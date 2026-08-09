package journal

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
	"github.com/cockroachdb/pebble/v2"
	"github.com/cockroachdb/pebble/v2/vfs"
)

type Journal struct {
	mu            sync.Mutex
	db            *pebble.DB
	lock          *pebble.Lock
	cfg           Config
	usage         uint64
	priorities    [4]uint64
	healthy       bool
	closed        bool
	commit        func(*pebble.Batch, *pebble.WriteOptions) error
	policyVersion string
}

func Open(cfg Config) (*Journal, error) {
	return openWithOptions(cfg, nil)
}

// openWithOptions is the storage-test seam. It remains package-private so
// Pebble and filesystem details do not leak into the journal's domain API.
func openWithOptions(cfg Config, supplied *pebble.Options) (*Journal, error) {
	if strings.TrimSpace(cfg.Dir) == "" || !validIdentifier(cfg.Owner, MaxOwnerBytes) || !validIdentifier(cfg.TenantID, MaxBoundaryBytes) || !validIdentifier(cfg.Region, MaxBoundaryBytes) || !validClassification(cfg.Classification) || nilInterface(cfg.Clock) || nilInterface(cfg.Validator) || cfg.MaxBytes == 0 || cfg.MinFreeBytes == 0 || cfg.FreeSpace == nil {
		return nil, ErrInvalidConfig
	}
	if cfg.ClaimTTL == 0 {
		cfg.ClaimTTL = DefaultClaimTTL
	}
	if cfg.SafetyDelay == 0 {
		cfg.SafetyDelay = DefaultSafetyDelay
	}
	if cfg.EntryOverhead == 0 {
		cfg.EntryOverhead = DefaultEntryOverhead
	}
	if cfg.MaxBatchRefs == 0 {
		cfg.MaxBatchRefs = DefaultMaxBatchRefs
	}
	if cfg.ClaimTTL <= 0 || cfg.SafetyDelay <= 0 || cfg.MaxBatchRefs < 1 {
		return nil, ErrInvalidConfig
	}
	policyVersion := cfg.Validator.Version()
	if !validIdentifier(policyVersion, MaxBoundaryBytes) {
		return nil, ErrInvalidConfig
	}
	now := cfg.Clock.Now()
	if !validJournalTime(now) {
		return nil, ErrInvalidConfig
	}
	if _, ok := safeAdd(now, cfg.ClaimTTL); !ok {
		return nil, ErrInvalidConfig
	}
	if _, ok := safeAdd(now, cfg.SafetyDelay); !ok {
		return nil, ErrInvalidConfig
	}
	options := &pebble.Options{FS: vfs.Default, Logger: discardLogger{}}
	if supplied != nil {
		options = supplied.Clone()
		if options.FS == nil {
			options.FS = vfs.Default
		}
		if options.Logger == nil {
			options.Logger = discardLogger{}
		}
	}
	if err := options.FS.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, ErrInvalidConfig
	}
	lock, err := pebble.LockDirectory(cfg.Dir, options.FS)
	if err != nil {
		return nil, ErrLocked
	}
	options.Lock = lock
	db, err := pebble.Open(cfg.Dir, options)
	if err != nil {
		_ = lock.Close()
		return nil, ErrCorruption
	}
	j := &Journal{db: db, lock: lock, cfg: cfg, healthy: true, policyVersion: policyVersion, commit: func(batch *pebble.Batch, options *pebble.WriteOptions) error {
		return batch.Commit(options)
	}}
	if err := j.openManifest(); err != nil {
		_ = db.Close()
		_ = lock.Close()
		return nil, err
	}
	if err := j.verify(); err != nil {
		j.healthy = false
		_ = db.Close()
		_ = lock.Close()
		return nil, ErrCorruption
	}
	return j, nil
}

func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil
	}
	j.closed = true
	if err := j.db.Close(); err != nil {
		return ErrCorruption
	}
	if err := j.lock.Close(); err != nil {
		return ErrCorruption
	}
	return nil
}
func (j *Journal) Healthy() bool { j.mu.Lock(); defer j.mu.Unlock(); return j.healthy && !j.closed }
func (j *Journal) Ready() bool   { return j.Healthy() }

// Retention returns the post-commit safety delay during which safe records stay
// available for deterministic restart recovery.
func (j *Journal) Retention() time.Duration {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.cfg.SafetyDelay
}

// ClaimTTL returns how long a claim is held before it lapses back to pending.
// A worker needs it to decide when to renew work it is still retrying.
func (j *Journal) ClaimTTL() time.Duration {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.cfg.ClaimTTL
}

// Manifest returns the immutable region, tenant, maximum classification, and
// redaction-policy version this directory was opened with and verified against.
func (j *Journal) Manifest() Manifest {
	j.mu.Lock()
	defer j.mu.Unlock()
	return Manifest{Region: j.cfg.Region, TenantID: j.cfg.TenantID,
		Classification: j.cfg.Classification, PolicyVersion: j.policyVersion}
}

// RecoverRetained returns one integrity-checked page of still-retained safe
// records. after is the last record ID from the prior page; an empty next value
// means recovery is complete. Pagination bounds memory without truncating a
// high-volume journal's deterministic restart state.
func (j *Journal) RecoverRetained(after string, limit int) (records []RecoveredRecord, next string, err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.usable(); err != nil {
		return nil, "", err
	}
	if limit < 1 || limit > MaxTransitionRecords || (after != "" && !validRecordID(after)) {
		return nil, "", ErrInvalidInput
	}
	iter, err := j.db.NewIter(&pebble.IterOptions{LowerBound: prefix(nsRecord), UpperBound: prefix(nsRecord + 1)})
	if err != nil {
		return nil, "", j.corrupt()
	}
	defer iter.Close()
	recovered := make([]RecoveredRecord, 0, limit)
	valid := iter.First()
	if after != "" {
		valid = iter.SeekGE(recordKey(after))
		if valid && string(iter.Key()) == string(recordKey(after)) {
			valid = iter.Next()
		}
	}
	lastID := ""
	for ; valid && len(recovered) < limit; valid = iter.Next() {
		id, rest, ok := readComponent(iter.Key()[2:])
		if !ok || len(rest) != 0 || !validRecordID(id) {
			return nil, "", j.corrupt()
		}
		stored, err := decodeStored(iter.Value())
		if err != nil {
			return nil, "", j.corrupt()
		}
		record, err := decodeDurable(stored.envelope, j.cfg.TenantID, j.cfg.Classification)
		if err != nil || record.RecordID != id || !record.Source.ReceivedAt.Equal(stored.received) ||
			!validRecordBoundary(record, j.cfg, j.policyVersion) || j.cfg.Validator.ValidateRecord(record) != nil {
			return nil, "", j.corrupt()
		}
		semantic, err := semanticDigestEnvelope(stored.envelope)
		if err != nil || semantic != stored.semantic {
			return nil, "", j.corrupt()
		}
		var index []byte
		switch stored.state {
		case StatePending:
			index = pendingKey(stored.priority, stored.received, id)
		case StateClaimed:
			index = claimKey(stored.claimExpiry, id)
		case StateCommitted:
			index = committedKey(stored.committedAt, id)
		default:
			return nil, "", j.corrupt()
		}
		if err := j.validateEmptyIndex(index); err != nil {
			return nil, "", j.corrupt()
		}
		recovered = append(recovered, RecoveredRecord{Record: record, State: stored.state})
		lastID = id
	}
	if err := iter.Error(); err != nil {
		return nil, "", j.corrupt()
	}
	if valid {
		next = lastID
	}
	return recovered, next, nil
}

func (j *Journal) openManifest() error {
	format, found, err := get(j.db, keyFormat)
	if err != nil {
		return ErrCorruption
	}
	if !found {
		// A non-empty database without a format marker is never initialized in
		// place; that could overwrite an older or corrupt layout.
		iter, e := j.db.NewIter(nil)
		if e != nil {
			return ErrCorruption
		}
		nonempty := iter.First()
		closeErr := iter.Close()
		if closeErr != nil {
			return ErrCorruption
		}
		if nonempty {
			return ErrIncompatibleFormat
		}
		batch := j.db.NewBatch()
		defer batch.Close()
		if batch.Set(keyFormat, formatValue(), nil) != nil ||
			batch.Set(keyOwner, checked([]byte("JOW1"), []byte(j.cfg.Owner)), nil) != nil ||
			batch.Set(keyBoundary, boundaryValue(j.cfg, j.policyVersion), nil) != nil || batch.Set(keyUsage, usageValue(0), nil) != nil || batch.Set(keyPriority, priorityValue([4]uint64{}), nil) != nil {
			return ErrCorruption
		}
		if err := j.commit(batch, pebble.Sync); err != nil {
			return ErrCorruption
		}
		j.usage = 0
		return nil
	}
	payload, ok := unchecked(format, "JFM1")
	if !ok || len(payload) != 4 {
		return ErrCorruption
	}
	// Directory formats are strict: unlike protobuf payload schemas, any future
	// minor may change key/state semantics and requires an explicit migration.
	if binary.BigEndian.Uint16(payload[:2]) != FormatMajor || binary.BigEndian.Uint16(payload[2:]) != FormatMinor {
		return ErrIncompatibleFormat
	}
	owner, found, err := get(j.db, keyOwner)
	if err != nil || !found {
		return ErrCorruption
	}
	ownerPayload, ok := unchecked(owner, "JOW1")
	if !ok {
		return ErrCorruption
	}
	if string(ownerPayload) != j.cfg.Owner {
		return ErrOwnerMismatch
	}
	boundary, found, err := get(j.db, keyBoundary)
	if err != nil || !found {
		return ErrCorruption
	}
	boundaryPayload, ok := unchecked(boundary, "JBD1")
	if !ok {
		return ErrCorruption
	}
	boundaryFields, decodeErr := decodeStrings(boundaryPayload, "JBS1")
	if decodeErr != nil || len(boundaryFields) != 4 {
		return ErrCorruption
	}
	if !bytes.Equal(boundary, boundaryValue(j.cfg, j.policyVersion)) {
		return ErrBoundaryMismatch
	}
	usage, found, err := get(j.db, keyUsage)
	if err != nil || !found {
		return ErrCorruption
	}
	value, ok := decodeUsage(usage)
	if !ok {
		return ErrCorruption
	}
	j.usage = value
	priorityData, found, err := get(j.db, keyPriority)
	if err != nil || !found {
		return ErrCorruption
	}
	priorities, ok := decodePriority(priorityData)
	if !ok {
		return ErrCorruption
	}
	j.priorities = priorities
	return nil
}

func boundaryValue(cfg Config, policyVersion string) []byte {
	return checked([]byte("JBD1"), encodeStrings("JBS1", []string{cfg.Region, cfg.TenantID, cfg.Classification, policyVersion}))
}

var minJournalTime = time.Unix(0, math.MinInt64).UTC()
var maxJournalTime = time.Unix(0, math.MaxInt64).UTC()

func validJournalTime(t time.Time) bool {
	return !t.IsZero() && t.Location() == time.UTC && !t.Before(minJournalTime) && !t.After(maxJournalTime) &&
		t.Equal(time.Unix(0, t.UnixNano()).UTC())
}
func safeAdd(t time.Time, d time.Duration) (time.Time, bool) {
	if !validJournalTime(t) || d <= 0 || t.After(maxJournalTime.Add(-d)) {
		return time.Time{}, false
	}
	return t.Add(d), true
}
func nilInterface(v any) bool {
	if v == nil {
		return true
	}
	x := reflect.ValueOf(v)
	switch x.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return x.IsNil()
	}
	return false
}
func validIdentifier(s string, max int) bool {
	if len(s) == 0 || len(s) > max {
		return false
	}
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("-_.:/", r)) {
			return false
		}
	}
	return true
}

func validRecordID(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

func validClaimToken(s string) bool {
	if len(s) != claimTokenBytes || !strings.HasPrefix(s, "claim:") {
		return false
	}
	for _, r := range s[len("claim:"):] {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}
func validClassification(s string) bool {
	switch s {
	case "PUBLIC", "INTERNAL", "SENSITIVE", "RESTRICTED":
		return true
	}
	return false
}

func classificationRank(s string) int {
	switch s {
	case "PUBLIC":
		return 1
	case "INTERNAL":
		return 2
	case "SENSITIVE":
		return 3
	case "RESTRICTED":
		return 4
	default:
		return 0
	}
}

func validRecordBoundary(r model.NormalizedLog, cfg Config, policyVersion string) bool {
	if r.Region != cfg.Region || r.Redaction.PolicyVersion != policyVersion {
		return false
	}
	if ref := r.RawReference; ref != nil {
		refRank := classificationRank(ref.Classification)
		if ref.Region != cfg.Region || refRank == 0 || refRank > classificationRank(cfg.Classification) {
			return false
		}
	}
	return true
}

func validRecordTimes(r model.NormalizedLog) bool {
	times := []time.Time{r.Source.ReceivedAt, r.EventTime, r.ObservedTime}
	if ref := r.RawReference; ref != nil {
		times = append(times, ref.From, ref.To, ref.ExpiresAt)
		if ref.To.Before(ref.From) || !ref.ExpiresAt.After(ref.To) {
			return false
		}
	}
	for _, t := range times {
		if !validJournalTime(t) {
			return false
		}
	}
	return true
}

func formatValue() []byte {
	payload := binary.BigEndian.AppendUint16(nil, FormatMajor)
	payload = binary.BigEndian.AppendUint16(payload, FormatMinor)
	return checked([]byte("JFM1"), payload)
}
func usageValue(n uint64) []byte {
	return checked([]byte("JUS1"), binary.BigEndian.AppendUint64(nil, n))
}
func decodeUsage(data []byte) (uint64, bool) {
	p, ok := unchecked(data, "JUS1")
	return func() (uint64, bool) {
		if !ok || len(p) != 8 {
			return 0, false
		}
		return binary.BigEndian.Uint64(p), true
	}()
}
func priorityValue(values [4]uint64) []byte {
	payload := make([]byte, 0, 32)
	for _, v := range values {
		payload = binary.BigEndian.AppendUint64(payload, v)
	}
	return checked([]byte("JPA1"), payload)
}
func decodePriority(data []byte) ([4]uint64, bool) {
	var out [4]uint64
	p, ok := unchecked(data, "JPA1")
	if !ok || len(p) != 32 {
		return out, false
	}
	for i := range out {
		out[i] = binary.BigEndian.Uint64(p[i*8 : i*8+8])
	}
	return out, true
}

func (j *Journal) AppendBatch(batchID string, admissions []Admission) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.usable(); err != nil {
		return err
	}
	if !validIdentifier(batchID, MaxBoundaryBytes) || len(admissions) == 0 || len(admissions) > MaxAppendRecords {
		return ErrInvalidInput
	}
	type prepared struct {
		admission Admission
		encoded   []byte
		semantic  [32]byte
	}
	unique := make(map[string]prepared, len(admissions))
	ids := make([]string, 0, len(admissions))
	for _, a := range admissions {
		if a.Priority > PriorityLow || a.Record.BatchID != batchID || !validRecordBoundary(a.Record, j.cfg, j.policyVersion) || !validRecordTimes(a.Record) || a.Record.Validate() != nil {
			return ErrInvalidInput
		}
		if encodedNormalizedSize(a.Record) > MaxEncodedRecordBytes {
			return ErrInvalidInput
		}
		if j.cfg.Validator.ValidateRecord(a.Record) != nil {
			return ErrInvalidInput
		}
		encoded, err := encodeDurable(a.Record, j.cfg.TenantID, j.cfg.Classification)
		if err != nil {
			return ErrInvalidInput
		}
		decoded, err := decodeDurable(encoded, j.cfg.TenantID, j.cfg.Classification)
		if err != nil || decoded.RecordID != a.Record.RecordID {
			return ErrInvalidInput
		}
		semantic, err := semanticDigestEnvelope(encoded)
		if err != nil {
			return ErrInvalidInput
		}
		if prior, ok := unique[a.Record.RecordID]; ok {
			if prior.semantic != semantic || prior.admission.Priority != a.Priority {
				return ErrDuplicateConflict
			}
			continue
		}
		unique[a.Record.RecordID] = prepared{a, encoded, semantic}
		ids = append(ids, a.Record.RecordID)
	}
	sort.Strings(ids)
	if existing, found, err := get(j.db, batchKey(batchID)); err != nil {
		return j.corrupt()
	} else if found {
		batch, err := decodeBatch(existing)
		if err != nil {
			return j.corrupt()
		}
		if !equalStrings(batchOriginalIDs(batch.original), ids) {
			return ErrDuplicateConflict
		}
		for _, member := range batch.original {
			prepared := unique[member.recordID]
			if member.semantic != prepared.semantic || member.priority != prepared.admission.Priority {
				return ErrDuplicateConflict
			}
		}
		if err := j.validateStoredBatch(batchID, batch); errors.Is(err, ErrDuplicateConflict) {
			return ErrDuplicateConflict
		} else if err != nil {
			return j.corrupt()
		}
		return nil
	}
	m := newMutation(j)
	for _, id := range ids {
		p := unique[id]
		historyCount, err := j.validateHistoricalIdentity(id, p.semantic, p.admission.Priority)
		if errors.Is(err, ErrDuplicateConflict) {
			return ErrDuplicateConflict
		}
		if err != nil {
			return j.corrupt()
		}
		if historyCount >= j.cfg.MaxBatchRefs {
			return ErrCapacity
		}
		m.set(batchOriginalRefKey(id, batchID), []byte{})
		stored, found, err := j.loadRecord(id)
		if err != nil {
			return j.corrupt()
		}
		if found {
			if stored.semantic != p.semantic || stored.priority != p.admission.Priority {
				return ErrDuplicateConflict
			}
			refs, err := j.batchRefs(id)
			if err != nil {
				return j.corrupt()
			}
			if len(refs) >= j.cfg.MaxBatchRefs {
				return ErrCapacity
			}
			m.set(batchRefKey(id, batchID), []byte{})
			continue
		}
		if _, quarantined, err := get(j.db, quarantineKey(id)); err != nil {
			return j.corrupt()
		} else if quarantined {
			return ErrDuplicateConflict
		}
		received := p.admission.Record.Source.ReceivedAt
		stored = storedRecord{state: StatePending, priority: p.admission.Priority, received: received, semantic: p.semantic, envelope: p.encoded}
		m.set(recordKey(id), encodeStored(stored))
		m.set(pendingKey(stored.priority, received, id), []byte{})
		m.set(batchRefKey(id, batchID), []byte{})
		m.set(transitionReserveKey(id), reserveValue())
		m.addPriority(stored.priority, 1)
	}
	batch := storedBatch{original: make([]storedBatchMember, 0, len(ids)), live: append([]string(nil), ids...)}
	for _, id := range ids {
		prepared := unique[id]
		batch.original = append(batch.original, storedBatchMember{recordID: id, priority: prepared.admission.Priority, semantic: prepared.semantic})
	}
	m.set(batchKey(batchID), encodeBatch(batch))
	m.set(keyFormat, formatValue())
	return j.commitMutation(m, true)
}

func (j *Journal) Claim(limit int, owner string) ([]ClaimedRecord, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.usable(); err != nil {
		return nil, err
	}
	if limit < 1 || limit > MaxTransitionRecords || strings.TrimSpace(owner) == "" {
		return nil, ErrInvalidInput
	}
	if !validIdentifier(owner, MaxOwnerBytes) {
		return nil, ErrInvalidInput
	}
	expired := newMutation(j)
	expiredCount, err := j.expireClaims(expired, MaxTransitionRecords)
	if err != nil {
		return nil, j.corrupt()
	}
	if err := j.commitMutation(expired, false); err != nil {
		return nil, err
	}
	remainingBudget := MaxTransitionRecords - expiredCount
	if remainingBudget == 0 {
		return []ClaimedRecord{}, nil
	}
	if limit > remainingBudget {
		limit = remainingBudget
	}
	m := newMutation(j)
	iter, err := j.db.NewIter(&pebble.IterOptions{LowerBound: prefix(nsPending), UpperBound: prefix(nsPending + 1)})
	if err != nil {
		return nil, j.corrupt()
	}
	defer iter.Close()
	now := j.cfg.Clock.Now()
	expiry, ok := safeAdd(now, j.cfg.ClaimTTL)
	if !ok {
		return nil, j.corrupt()
	}
	claimed := make([]ClaimedRecord, 0, limit)
	for iter.First(); iter.Valid() && len(claimed) < limit; iter.Next() {
		if len(iter.Value()) != 0 {
			return nil, j.corrupt()
		}
		priority, received, id, ok := parsePendingKey(iter.Key())
		if !ok {
			return nil, j.corrupt()
		}
		stored, found, err := j.loadRecord(id)
		if err != nil || !found || stored.state != StatePending || stored.priority != priority || !stored.received.Equal(received) {
			return nil, j.corrupt()
		}
		if stored.attempt == math.MaxUint32 {
			return nil, j.corrupt()
		}
		if err := j.validateTransitionReserve(id, true); err != nil {
			return nil, j.corrupt()
		}
		stored.state = StateClaimed
		m.del(transitionReserveKey(id))
		stored.attempt++
		stored.claimOwner = owner
		stored.claimExpiry = expiry
		stored.claimToken = claimToken(j.cfg.Owner, owner, id, stored.attempt, stored.claimExpiry)
		m.del(append([]byte(nil), iter.Key()...))
		m.set(claimKey(stored.claimExpiry, id), []byte{})
		m.set(recordKey(id), encodeStored(stored))
		record, err := decodeDurable(stored.envelope, j.cfg.TenantID, j.cfg.Classification)
		if err != nil {
			return nil, j.corrupt()
		}
		claimed = append(claimed, ClaimedRecord{Record: record, Priority: priority,
			Replay: ReplayIdentity{version: ReplayIdentityVersion, digest: stored.semantic, priority: priority},
			Token:  stored.claimToken, Owner: owner, Attempt: stored.attempt, ExpiresAt: stored.claimExpiry})
	}
	if err := iter.Error(); err != nil {
		return nil, j.corrupt()
	}
	if err := j.commitMutation(m, false); err != nil {
		return nil, err
	}
	return claimed, nil
}

// AdoptClaims returns the unexpired claims owner already holds, with the exact
// tokens it was given. A worker which restarts still owns whatever it claimed
// before it died; without adoption those records wait out a whole claim TTL
// before anyone may touch them, which is time the required outage tolerance
// cannot spend. Adoption observes only: it is not a state transition and it
// does not extend an expiry or count an attempt.
func (j *Journal) AdoptClaims(owner string, limit int) ([]ClaimedRecord, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.usable(); err != nil {
		return nil, err
	}
	if limit < 1 || limit > MaxTransitionRecords || !validIdentifier(owner, MaxOwnerBytes) {
		return nil, ErrInvalidInput
	}
	iter, err := j.db.NewIter(&pebble.IterOptions{LowerBound: prefix(nsClaim), UpperBound: prefix(nsClaim + 1)})
	if err != nil {
		return nil, j.corrupt()
	}
	defer iter.Close()
	now := j.cfg.Clock.Now()
	if !validJournalTime(now) {
		return nil, j.corrupt()
	}
	adopted := make([]ClaimedRecord, 0, limit)
	for iter.First(); iter.Valid() && len(adopted) < limit; iter.Next() {
		if len(iter.Value()) != 0 {
			return nil, j.corrupt()
		}
		expiry, id, ok := parseTimedKey(iter.Key(), nsClaim)
		if !ok {
			return nil, j.corrupt()
		}
		stored, found, err := j.loadRecord(id)
		if err != nil || !found || stored.state != StateClaimed || !stored.claimExpiry.Equal(expiry) {
			return nil, j.corrupt()
		}
		// Claim keys are ordered by expiry, so everything at or before now has
		// already lapsed and belongs to whoever claims next, not to this owner.
		if !now.Before(expiry) || stored.claimOwner != owner {
			continue
		}
		if err := j.validateTransitionReserve(id, false); err != nil {
			return nil, j.corrupt()
		}
		record, err := decodeDurable(stored.envelope, j.cfg.TenantID, j.cfg.Classification)
		if err != nil {
			return nil, j.corrupt()
		}
		adopted = append(adopted, ClaimedRecord{Record: record, Priority: stored.priority,
			Replay: ReplayIdentity{version: ReplayIdentityVersion, digest: stored.semantic, priority: stored.priority},
			Token:  stored.claimToken, Owner: stored.claimOwner, Attempt: stored.attempt, ExpiresAt: stored.claimExpiry})
	}
	if err := iter.Error(); err != nil {
		return nil, j.corrupt()
	}
	return adopted, nil
}

// RenewClaims extends every supplied unexpired claim by a further claim TTL and
// returns the refreshed claims. A worker retrying one cohort through a long
// CockroachDB outage would otherwise let its claims lapse every TTL, churning
// attempt counts and re-deriving the same work. One stale member rejects the
// whole transition, exactly as committing does; and exactly as committing does,
// a member this journal has already committed under the same token is satisfied
// rather than stale, so a lost response cannot make its cohort stale too. Only
// the still-claimed members come back. The claim token is derived from the
// expiry, so renewal retires the previous token: only the renewing worker can
// continue, and a holder of the old token fails closed.
func (j *Journal) RenewClaims(claims []CommitClaim) ([]ClaimedRecord, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.usable(); err != nil {
		return nil, err
	}
	if len(claims) == 0 || len(claims) > MaxTransitionRecords {
		return nil, ErrInvalidInput
	}
	now := j.cfg.Clock.Now()
	if !validJournalTime(now) {
		return nil, j.corrupt()
	}
	expiry, ok := safeAdd(now, j.cfg.ClaimTTL)
	if !ok {
		return nil, j.corrupt()
	}
	m := newMutation(j)
	seen := map[string]bool{}
	renewed := make([]ClaimedRecord, 0, len(claims))
	for _, claim := range claims {
		if !validRecordID(claim.RecordID) || !validClaimToken(claim.Token) || seen[claim.RecordID] {
			return nil, ErrInvalidInput
		}
		seen[claim.RecordID] = true
		stored, found, err := j.loadRecord(claim.RecordID)
		if err != nil {
			return nil, j.corrupt()
		}
		if !found {
			return nil, ErrStaleClaim
		}
		if stored.state == StateCommitted {
			// A lost MarkCommittedBatch response leaves the holder renewing a
			// claim this journal has already retired. There is nothing left to
			// renew and nothing is wrong, so the member is satisfied rather
			// than stale: rejecting it would make one lost response forfeit
			// every valid claim in its cohort. A token which does not match is
			// still someone else's work.
			if stored.claimToken != claim.Token {
				return nil, ErrStaleClaim
			}
			continue
		}
		if stored.state != StateClaimed || stored.claimToken != claim.Token || !now.Before(stored.claimExpiry) {
			return nil, ErrStaleClaim
		}
		if err := j.validateEmptyIndex(claimKey(stored.claimExpiry, claim.RecordID)); err != nil {
			return nil, j.corrupt()
		}
		if err := j.validateTransitionReserve(claim.RecordID, false); err != nil {
			return nil, j.corrupt()
		}
		record, err := decodeDurable(stored.envelope, j.cfg.TenantID, j.cfg.Classification)
		if err != nil {
			return nil, j.corrupt()
		}
		m.del(claimKey(stored.claimExpiry, claim.RecordID))
		stored.claimExpiry = expiry
		stored.claimToken = claimToken(j.cfg.Owner, stored.claimOwner, claim.RecordID, stored.attempt, expiry)
		m.set(claimKey(expiry, claim.RecordID), []byte{})
		m.set(recordKey(claim.RecordID), encodeStored(stored))
		renewed = append(renewed, ClaimedRecord{Record: record, Priority: stored.priority,
			Replay: ReplayIdentity{version: ReplayIdentityVersion, digest: stored.semantic, priority: stored.priority},
			Token:  stored.claimToken, Owner: stored.claimOwner, Attempt: stored.attempt, ExpiresAt: expiry})
	}
	if err := j.commitMutation(m, false); err != nil {
		return nil, err
	}
	return renewed, nil
}

func (j *Journal) MarkCommitted(recordID, token string) error {
	return j.MarkCommittedBatch([]CommitClaim{{RecordID: recordID, Token: token}})
}

// MarkCommittedBatch advances every supplied claim in one synchronized batch.
// One stale member rejects the whole transition.
func (j *Journal) MarkCommittedBatch(claims []CommitClaim) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.usable(); err != nil {
		return err
	}
	if len(claims) == 0 || len(claims) > MaxTransitionRecords {
		return ErrInvalidInput
	}
	m := newMutation(j)
	now := j.cfg.Clock.Now()
	if !validJournalTime(now) {
		return j.corrupt()
	}
	if _, ok := safeAdd(now, j.cfg.SafetyDelay); !ok {
		return j.corrupt()
	}
	seen := map[string]bool{}
	for _, claim := range claims {
		if !validRecordID(claim.RecordID) || !validClaimToken(claim.Token) || seen[claim.RecordID] {
			return ErrInvalidInput
		}
		seen[claim.RecordID] = true
		stored, found, err := j.loadRecord(claim.RecordID)
		if err != nil {
			return j.corrupt()
		}
		if !found {
			return ErrStaleClaim
		}
		if stored.state == StateCommitted {
			if err := j.validateEmptyIndex(committedKey(stored.committedAt, claim.RecordID)); err != nil {
				return j.corrupt()
			}
			if err := j.validateTransitionReserve(claim.RecordID, false); err != nil {
				return j.corrupt()
			}
			if stored.claimToken != claim.Token {
				return ErrStaleClaim
			}
			continue
		}
		if stored.state != StateClaimed || stored.claimToken != claim.Token || !now.Before(stored.claimExpiry) {
			return ErrStaleClaim
		}
		if err := j.validateEmptyIndex(claimKey(stored.claimExpiry, claim.RecordID)); err != nil {
			return j.corrupt()
		}
		if err := j.validateTransitionReserve(claim.RecordID, false); err != nil {
			return j.corrupt()
		}
		m.del(claimKey(stored.claimExpiry, claim.RecordID))
		stored.state = StateCommitted
		stored.committedAt = now
		stored.claimOwner = ""
		stored.claimExpiry = time.Time{}
		m.set(recordKey(claim.RecordID), encodeStored(stored))
		m.set(committedKey(stored.committedAt, claim.RecordID), []byte{})
	}
	return j.commitMutation(m, false)
}

func (j *Journal) Quarantine(recordID, token string, reason QuarantineReason) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.usable(); err != nil {
		return err
	}
	if !validRecordID(recordID) || !validClaimToken(token) || reason < QuarantineInvalidNormalizedRecord || reason > QuarantineUnsupportedData {
		return ErrInvalidInput
	}
	stored, found, err := j.loadRecord(recordID)
	if err != nil {
		return j.corrupt()
	}
	now := j.cfg.Clock.Now()
	if !validJournalTime(now) {
		return j.corrupt()
	}
	if !found || stored.state != StateClaimed || stored.claimToken != token || !now.Before(stored.claimExpiry) {
		return ErrStaleClaim
	}
	if err := j.validateEmptyIndex(claimKey(stored.claimExpiry, recordID)); err != nil {
		return j.corrupt()
	}
	if err := j.validateTransitionReserve(recordID, false); err != nil {
		return j.corrupt()
	}
	m := newMutation(j)
	m.del(claimKey(stored.claimExpiry, recordID))
	m.del(recordKey(recordID))
	m.addPriority(stored.priority, -1)
	if err := j.removeBatchRefs(m, recordID); err != nil {
		return j.corrupt()
	}
	m.set(quarantineKey(recordID), encodeQuarantine(recordID, reason, now, stored.attempt))
	return j.commitMutation(m, false)
}

func (j *Journal) Compact(limit int) (int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.usable(); err != nil {
		return 0, err
	}
	if limit < 1 || limit > MaxTransitionRecords {
		return 0, ErrInvalidInput
	}
	m := newMutation(j)
	iter, err := j.db.NewIter(&pebble.IterOptions{LowerBound: prefix(nsCommitted), UpperBound: prefix(nsCommitted + 1)})
	if err != nil {
		return 0, j.corrupt()
	}
	defer iter.Close()
	now := j.cfg.Clock.Now()
	if !validJournalTime(now) {
		return 0, j.corrupt()
	}
	removed := 0
	for iter.First(); iter.Valid() && removed < limit; iter.Next() {
		if len(iter.Value()) != 0 {
			return 0, j.corrupt()
		}
		at, id, ok := parseTimedKey(iter.Key(), nsCommitted)
		if !ok {
			return 0, j.corrupt()
		}
		deadline, ok := safeAdd(at, j.cfg.SafetyDelay)
		if !ok {
			return 0, j.corrupt()
		}
		if !now.After(deadline) {
			break
		}
		stored, found, err := j.loadRecord(id)
		if err != nil || !found || stored.state != StateCommitted || !stored.committedAt.Equal(at) {
			return 0, j.corrupt()
		}
		if err := j.validateTransitionReserve(id, false); err != nil {
			return 0, j.corrupt()
		}
		m.del(append([]byte(nil), iter.Key()...))
		m.del(recordKey(id))
		m.addPriority(stored.priority, -1)
		if err := j.removeBatchRefs(m, id); err != nil {
			return 0, j.corrupt()
		}
		removed++
	}
	if err := iter.Error(); err != nil {
		return 0, j.corrupt()
	}
	if err := j.commitMutation(m, false); err != nil {
		return 0, err
	}
	return removed, nil
}

// Capacity is a point-in-time observation of how full this journal is.
//
// It is cheap on purpose: the accounted byte total is maintained as records are
// written, so this reads state rather than scanning. Ingestion consults it per
// batch, and a capacity check that had to walk the keyspace would be a capacity
// check nobody could afford to make.
func (j *Journal) Capacity() Capacity {
	j.mu.Lock()
	defer j.mu.Unlock()
	observed := Capacity{Used: j.usage, Total: j.cfg.MaxBytes}
	if j.usable() != nil {
		// An unusable journal cannot persist anything, and saying so is what
		// turns a corrupted volume into backpressure rather than into a silent
		// acknowledgement.
		return observed
	}
	if j.usage >= j.cfg.MaxBytes {
		return observed
	}
	free, err := j.cfg.FreeSpace(j.cfg.Dir)
	if err != nil || free < j.cfg.MinFreeBytes {
		return observed
	}
	observed.CanPersist = true
	return observed
}

func (j *Journal) Stats() (Stats, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.usable(); err != nil {
		return Stats{}, err
	}
	stats := Stats{AccountedBytes: j.usage, ByPriority: j.priorities}
	iter, err := j.db.NewIter(&pebble.IterOptions{LowerBound: prefix(nsRecord), UpperBound: prefix(nsRecord + 1)})
	if err != nil {
		return Stats{}, j.corrupt()
	}
	defer iter.Close()
	for iter.First(); iter.Valid(); iter.Next() {
		stored, err := decodeStored(iter.Value())
		if err != nil {
			return Stats{}, j.corrupt()
		}
		switch stored.state {
		case StatePending:
			stats.Pending++
		case StateClaimed:
			stats.Claimed++
		case StateCommitted:
			stats.Committed++
		default:
			return Stats{}, j.corrupt()
		}
	}
	if err := iter.Error(); err != nil {
		return Stats{}, j.corrupt()
	}
	q, err := j.countNamespace(nsQuarantine)
	if err != nil {
		return Stats{}, j.corrupt()
	}
	stats.Quarantined = q
	return stats, nil
}

func (j *Journal) expireClaims(m *mutation, limit int) (int, error) {
	if limit < 1 || limit > MaxTransitionRecords {
		return 0, errDecode
	}
	iter, err := j.db.NewIter(&pebble.IterOptions{LowerBound: prefix(nsClaim), UpperBound: prefix(nsClaim + 1)})
	if err != nil {
		return 0, err
	}
	defer iter.Close()
	now := j.cfg.Clock.Now()
	if !validJournalTime(now) {
		return 0, errDecode
	}
	expired := 0
	for iter.First(); iter.Valid() && expired < limit; iter.Next() {
		if len(iter.Value()) != 0 {
			return 0, errDecode
		}
		expiry, id, ok := parseTimedKey(iter.Key(), nsClaim)
		if !ok {
			return 0, errDecode
		}
		if now.Before(expiry) {
			break
		}
		stored, found, err := j.loadRecord(id)
		if err != nil || !found || stored.state != StateClaimed || !stored.claimExpiry.Equal(expiry) {
			return 0, errDecode
		}
		if err := j.validateTransitionReserve(id, false); err != nil {
			return 0, errDecode
		}
		m.del(append([]byte(nil), iter.Key()...))
		stored.state = StatePending
		stored.claimOwner = ""
		stored.claimToken = ""
		stored.claimExpiry = time.Time{}
		m.set(recordKey(id), encodeStored(stored))
		m.set(pendingKey(stored.priority, stored.received, id), []byte{})
		m.set(transitionReserveKey(id), reserveValue())
		expired++
	}
	return expired, iter.Error()
}

func claimToken(journalOwner, worker, id string, attempt uint32, expiry time.Time) string {
	h := sha256.New()
	for _, v := range []string{journalOwner, worker, id} {
		_ = binary.Write(h, binary.BigEndian, uint32(len(v)))
		_, _ = h.Write([]byte(v))
	}
	_ = binary.Write(h, binary.BigEndian, attempt)
	_ = binary.Write(h, binary.BigEndian, expiry.UnixNano())
	return string([]byte("claim:")) + fmtHex(h.Sum(nil))
}
func fmtHex(in []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(in)*2)
	for i, b := range in {
		out[i*2] = digits[b>>4]
		out[i*2+1] = digits[b&15]
	}
	return string(out)
}

func (j *Journal) removeBatchRefs(m *mutation, recordID string) error {
	refs, err := j.batchRefs(recordID)
	if err != nil {
		return err
	}
	for _, batchID := range refs {
		value, found, err := m.get(batchKey(batchID))
		if err != nil || !found {
			return errDecode
		}
		batch, err := decodeBatch(value)
		if err != nil {
			return err
		}
		if _, found := batchMemberByID(batch, recordID); !found || !containsString(batch.live, recordID) {
			return errDecode
		}
		next := batch.live[:0]
		for _, id := range batch.live {
			if id != recordID {
				next = append(next, id)
			}
		}
		batch.live = next
		m.del(batchRefKey(recordID, batchID))
		if len(next) == 0 {
			for _, member := range batch.original {
				m.del(batchOriginalRefKey(member.recordID, batchID))
			}
			m.del(batchKey(batchID))
		} else {
			m.set(batchKey(batchID), encodeBatch(batch))
		}
	}
	return nil
}
func (j *Journal) batchRefs(recordID string) ([]string, error) {
	stored, found, err := j.loadRecord(recordID)
	if err != nil || !found {
		return nil, errDecode
	}
	p := batchRefPrefix(recordID)
	iter, err := j.db.NewIter(&pebble.IterOptions{LowerBound: p, UpperBound: prefixEnd(p)})
	if err != nil {
		return nil, err
	}
	defer iter.Close()
	var refs []string
	for iter.First(); iter.Valid(); iter.Next() {
		id, batch, ok := parseTwoComponentKey(iter.Key(), nsBatchRef)
		if !ok || id != recordID || len(iter.Value()) != 0 {
			return nil, errDecode
		}
		value, found, err := get(j.db, batchKey(batch))
		if err != nil || !found {
			return nil, errDecode
		}
		metadata, err := decodeBatch(value)
		member, memberFound := batchMemberByID(metadata, recordID)
		if err != nil || !memberFound || !containsString(metadata.live, recordID) || member.semantic != stored.semantic || member.priority != stored.priority {
			return nil, errDecode
		}
		originalRef, originalRefFound, err := get(j.db, batchOriginalRefKey(recordID, batch))
		if err != nil || !originalRefFound || len(originalRef) != 0 {
			return nil, errDecode
		}
		refs = append(refs, batch)
		if len(refs) > j.cfg.MaxBatchRefs {
			return nil, errDecode
		}
	}
	return refs, iter.Error()
}

func (j *Journal) validateHistoricalIdentity(recordID string, semantic [32]byte, priority Priority) (int, error) {
	p := batchOriginalRefPrefix(recordID)
	iter, err := j.db.NewIter(&pebble.IterOptions{LowerBound: p, UpperBound: prefixEnd(p)})
	if err != nil {
		return 0, err
	}
	defer iter.Close()
	count := 0
	for iter.First(); iter.Valid(); iter.Next() {
		id, batchID, ok := parseTwoComponentKey(iter.Key(), nsBatchOriginalRef)
		if !ok || id != recordID || len(iter.Value()) != 0 {
			return 0, errDecode
		}
		value, found, err := get(j.db, batchKey(batchID))
		if err != nil || !found {
			return 0, errDecode
		}
		batch, err := decodeBatch(value)
		if err != nil {
			return 0, errDecode
		}
		member, found := batchMemberByID(batch, recordID)
		if !found {
			return 0, errDecode
		}
		if member.semantic != semantic || member.priority != priority {
			return 0, ErrDuplicateConflict
		}
		stored, recordFound, err := j.loadRecord(recordID)
		if err != nil {
			return 0, errDecode
		}
		refValue, refFound, err := get(j.db, batchRefKey(recordID, batchID))
		if err != nil {
			return 0, errDecode
		}
		if containsString(batch.live, recordID) {
			if !recordFound || stored.semantic != member.semantic || stored.priority != member.priority || !refFound || len(refValue) != 0 {
				return 0, errDecode
			}
		} else {
			if refFound {
				return 0, errDecode
			}
			if recordFound && (stored.semantic != member.semantic || stored.priority != member.priority) {
				return 0, ErrDuplicateConflict
			}
		}
		count++
		if count > j.cfg.MaxBatchRefs {
			return 0, errDecode
		}
	}
	if err := iter.Error(); err != nil {
		return 0, err
	}
	return count, nil
}

func (j *Journal) validateStoredBatch(batchID string, batch storedBatch) error {
	if len(batch.live) == 0 || len(batch.original) == 0 {
		return errDecode
	}
	for _, member := range batch.original {
		originalRef, originalRefFound, err := get(j.db, batchOriginalRefKey(member.recordID, batchID))
		if err != nil || !originalRefFound || len(originalRef) != 0 {
			return errDecode
		}
		live := containsString(batch.live, member.recordID)
		stored, found, err := j.loadRecord(member.recordID)
		if err != nil {
			return errDecode
		}
		refValue, refFound, err := get(j.db, batchRefKey(member.recordID, batchID))
		if err != nil {
			return errDecode
		}
		if live {
			if !found || stored.semantic != member.semantic || stored.priority != member.priority || !refFound || len(refValue) != 0 {
				return errDecode
			}
			refs, err := j.batchRefs(member.recordID)
			if err != nil || !containsString(refs, batchID) {
				return errDecode
			}
		} else {
			if refFound {
				return errDecode
			}
			if found && (stored.semantic != member.semantic || stored.priority != member.priority) {
				return ErrDuplicateConflict
			}
		}
	}
	return nil
}
func prefixEnd(p []byte) []byte {
	out := append([]byte(nil), p...)
	for i := len(out) - 1; i >= 0; i-- {
		if out[i] != 0xff {
			out[i]++
			return out[:i+1]
		}
	}
	return nil
}

func (j *Journal) loadRecord(id string) (storedRecord, bool, error) {
	value, found, err := get(j.db, recordKey(id))
	if err != nil || !found {
		return storedRecord{}, found, err
	}
	r, err := decodeStored(value)
	return r, true, err
}

func (j *Journal) validateEmptyIndex(indexKey []byte) error {
	value, found, err := get(j.db, indexKey)
	if err != nil || !found || len(value) != 0 {
		return errDecode
	}
	return nil
}

func (j *Journal) validateTransitionReserve(recordID string, expected bool) error {
	value, found, err := get(j.db, transitionReserveKey(recordID))
	if err != nil || found != expected {
		return errDecode
	}
	if !expected {
		return nil
	}
	if !bytes.Equal(value, reserveValue()) {
		return errDecode
	}
	return nil
}

func get(db *pebble.DB, key []byte) ([]byte, bool, error) {
	value, closer, err := db.Get(key)
	if errors.Is(err, pebble.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer closer.Close()
	return append([]byte(nil), value...), true, nil
}
func (j *Journal) usable() error {
	if j.closed {
		return ErrClosed
	}
	if !j.healthy {
		return ErrNotHealthy
	}
	if err := j.verifyManifest(); err != nil {
		return j.corrupt()
	}
	return nil
}

// verifyManifest is the constant-time live health check used on the hot path.
// Full keyspace verification remains a startup responsibility; transitions
// additionally validate every state/index key they touch.
func (j *Journal) verifyManifest() error {
	expected := []struct {
		key   []byte
		value []byte
	}{
		{keyFormat, formatValue()},
		{keyOwner, checked([]byte("JOW1"), []byte(j.cfg.Owner))},
		{keyBoundary, boundaryValue(j.cfg, j.policyVersion)},
		{keyUsage, usageValue(j.usage)},
		{keyPriority, priorityValue(j.priorities)},
	}
	for _, item := range expected {
		value, found, err := get(j.db, item.key)
		if err != nil || !found || !bytes.Equal(value, item.value) {
			return errDecode
		}
	}
	return nil
}
func (j *Journal) corrupt() error { j.healthy = false; return ErrCorruption }
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

type mutation struct {
	j             *Journal
	ops           map[string]*[]byte
	priorityDelta [4]int64
}

func newMutation(j *Journal) *mutation { return &mutation{j: j, ops: map[string]*[]byte{}} }
func (m *mutation) set(k, v []byte) {
	copyValue := append([]byte(nil), v...)
	m.ops[string(k)] = &copyValue
}
func (m *mutation) del(k []byte)                        { m.ops[string(k)] = nil }
func (m *mutation) addPriority(p Priority, delta int64) { m.priorityDelta[int(p)] += delta }
func (m *mutation) get(k []byte) ([]byte, bool, error) {
	if value, ok := m.ops[string(k)]; ok {
		if value == nil {
			return nil, false, nil
		}
		return append([]byte(nil), (*value)...), true, nil
	}
	return get(m.j.db, k)
}

func (j *Journal) commitMutation(m *mutation, capacity bool) error {
	if len(m.ops) == 0 {
		return nil
	}
	next := j.usage
	nextPriorities := j.priorities
	for i, delta := range m.priorityDelta {
		if delta < 0 {
			amount := uint64(-delta)
			if amount > nextPriorities[i] {
				return j.corrupt()
			}
			nextPriorities[i] -= amount
		} else if delta > 0 {
			amount := uint64(delta)
			if nextPriorities[i] > math.MaxUint64-amount {
				return ErrCapacity
			}
			nextPriorities[i] += amount
		}
	}
	keys := make([]string, 0, len(m.ops))
	for key := range m.ops {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if len(key) >= 2 && key[0] == keyVersion && key[1] == nsManifest {
			continue
		}
		old, found, err := get(j.db, []byte(key))
		if err != nil {
			return j.corrupt()
		}
		if found {
			cost, ok := j.cost([]byte(key), old)
			if !ok || cost > next {
				return j.corrupt()
			}
			next -= cost
		}
		if value := m.ops[key]; value != nil {
			cost, ok := j.cost([]byte(key), *value)
			if !ok || next > math.MaxUint64-cost {
				return ErrCapacity
			}
			next += cost
		}
	}
	if capacity && next > j.usage {
		delta := next - j.usage
		if next > j.cfg.MaxBytes {
			return ErrCapacity
		}
		free, err := j.cfg.FreeSpace(j.cfg.Dir)
		if err != nil {
			return ErrCapacity
		}
		if free < j.cfg.MinFreeBytes || delta > free-j.cfg.MinFreeBytes {
			return ErrCapacity
		}
	}
	batch := j.db.NewBatch()
	defer batch.Close()
	for _, key := range keys {
		if value := m.ops[key]; value == nil {
			if err := batch.Delete([]byte(key), nil); err != nil {
				return j.corrupt()
			}
		} else {
			if err := batch.Set([]byte(key), *value, nil); err != nil {
				return j.corrupt()
			}
		}
	}
	if err := batch.Set(keyUsage, usageValue(next), nil); err != nil {
		return j.corrupt()
	}
	if err := batch.Set(keyPriority, priorityValue(nextPriorities), nil); err != nil {
		return j.corrupt()
	}
	if err := j.commit(batch, pebble.Sync); err != nil {
		return j.corrupt()
	}
	j.usage = next
	j.priorities = nextPriorities
	return nil
}
func (j *Journal) cost(k, v []byte) (uint64, bool) {
	if uint64(len(k)) > math.MaxUint64-uint64(len(v)) {
		return 0, false
	}
	n := uint64(len(k)) + uint64(len(v))
	if n > math.MaxUint64-j.cfg.EntryOverhead {
		return 0, false
	}
	return n + j.cfg.EntryOverhead, true
}

func (j *Journal) countNamespace(ns byte) (uint64, error) {
	iter, err := j.db.NewIter(&pebble.IterOptions{LowerBound: prefix(ns), UpperBound: prefix(ns + 1)})
	if err != nil {
		return 0, err
	}
	defer iter.Close()
	var count uint64
	for iter.First(); iter.Valid(); iter.Next() {
		count++
	}
	return count, iter.Error()
}

func (j *Journal) verify() error {
	var calculated uint64
	iter, err := j.db.NewIter(nil)
	if err != nil {
		return err
	}
	defer iter.Close()
	records := map[string]storedRecord{}
	pending := map[string]string{}
	claims := map[string]string{}
	committed := map[string]string{}
	batches := map[string]storedBatch{}
	refs := map[string]map[string]bool{}
	originalRefs := map[string]map[string]bool{}
	reserves := map[string]bool{}
	quarantines := map[string]bool{}
	var manifests [5]bool
	for iter.First(); iter.Valid(); iter.Next() {
		k := append([]byte(nil), iter.Key()...)
		v := append([]byte(nil), iter.Value()...)
		switch {
		case bytes.Equal(k, keyFormat):
			if !bytes.Equal(v, formatValue()) {
				return errDecode
			}
			manifests[0] = true
			continue
		case bytes.Equal(k, keyOwner):
			if !bytes.Equal(v, checked([]byte("JOW1"), []byte(j.cfg.Owner))) {
				return errDecode
			}
			manifests[1] = true
			continue
		case bytes.Equal(k, keyBoundary):
			if !bytes.Equal(v, boundaryValue(j.cfg, j.policyVersion)) {
				return errDecode
			}
			manifests[2] = true
			continue
		case bytes.Equal(k, keyUsage):
			usage, ok := decodeUsage(v)
			if !ok || usage != j.usage {
				return errDecode
			}
			manifests[3] = true
			continue
		case bytes.Equal(k, keyPriority):
			priorities, ok := decodePriority(v)
			if !ok || priorities != j.priorities {
				return errDecode
			}
			manifests[4] = true
			continue
		}
		cost, ok := j.cost(k, v)
		if !ok || calculated > math.MaxUint64-cost {
			return errDecode
		}
		calculated += cost
		if len(k) < 2 || k[0] != keyVersion {
			return errDecode
		}
		switch k[1] {
		case nsRecord:
			id, rest, ok := readComponent(k[2:])
			if !ok || len(rest) != 0 || !validRecordID(id) {
				return errDecode
			}
			r, err := decodeStored(v)
			if err != nil {
				return err
			}
			record, err := decodeDurable(r.envelope, j.cfg.TenantID, j.cfg.Classification)
			if err != nil || record.RecordID != id || !record.Source.ReceivedAt.Equal(r.received) || !validRecordBoundary(record, j.cfg, j.policyVersion) {
				return errDecode
			}
			semantic, err := semanticDigestEnvelope(r.envelope)
			if err != nil || semantic != r.semantic {
				return errDecode
			}
			records[id] = r
		case nsPending:
			_, _, id, ok := parsePendingKey(k)
			if !ok || len(v) != 0 {
				return errDecode
			}
			if _, exists := pending[id]; exists {
				return errDecode
			}
			pending[id] = string(k)
		case nsClaim:
			_, id, ok := parseTimedKey(k, nsClaim)
			if !ok || len(v) != 0 {
				return errDecode
			}
			if _, exists := claims[id]; exists {
				return errDecode
			}
			claims[id] = string(k)
		case nsCommitted:
			_, id, ok := parseTimedKey(k, nsCommitted)
			if !ok || len(v) != 0 {
				return errDecode
			}
			if _, exists := committed[id]; exists {
				return errDecode
			}
			committed[id] = string(k)
		case nsBatch:
			id, rest, ok := readComponent(k[2:])
			if !ok || len(rest) != 0 || !validIdentifier(id, MaxBoundaryBytes) {
				return errDecode
			}
			batch, err := decodeBatch(v)
			if err != nil {
				return err
			}
			if len(batch.live) == 0 {
				return errDecode
			}
			batches[id] = batch
		case nsBatchRef:
			id, batch, ok := parseTwoComponentKey(k, nsBatchRef)
			if !ok || len(v) != 0 {
				return errDecode
			}
			if refs[id] == nil {
				refs[id] = map[string]bool{}
			}
			refs[id][batch] = true
		case nsBatchOriginalRef:
			id, batch, ok := parseTwoComponentKey(k, nsBatchOriginalRef)
			if !ok || len(v) != 0 {
				return errDecode
			}
			if originalRefs[id] == nil {
				originalRefs[id] = map[string]bool{}
			}
			if originalRefs[id][batch] {
				return errDecode
			}
			originalRefs[id][batch] = true
		case nsQuarantine:
			id, rest, ok := readComponent(k[2:])
			if !ok || len(rest) != 0 || !validRecordID(id) {
				return errDecode
			}
			valueID, _, _, _, err := decodeQuarantine(v)
			if err != nil || valueID != id {
				return errDecode
			}
			if quarantines[id] {
				return errDecode
			}
			quarantines[id] = true
		case nsTransitionReserve:
			id, rest, ok := readComponent(k[2:])
			if !ok || len(rest) != 0 || !bytes.Equal(v, reserveValue()) || reserves[id] {
				return errDecode
			}
			reserves[id] = true
		default:
			return errDecode
		}
	}
	if err := iter.Error(); err != nil {
		return err
	}
	for _, found := range manifests {
		if !found {
			return errDecode
		}
	}
	if calculated != j.usage {
		return errDecode
	}
	var calculatedPriorities [4]uint64
	for _, r := range records {
		calculatedPriorities[int(r.priority)]++
	}
	if calculatedPriorities != j.priorities {
		return errDecode
	}
	for id, r := range records {
		if quarantines[id] {
			return errDecode
		}
		indexes := 0
		if pending[id] != "" {
			indexes++
		}
		if claims[id] != "" {
			indexes++
		}
		if committed[id] != "" {
			indexes++
		}
		if indexes != 1 {
			return errDecode
		}
		if (r.state == StatePending) != (pending[id] != "") || (r.state == StateClaimed) != (claims[id] != "") || (r.state == StateCommitted) != (committed[id] != "") {
			return errDecode
		}
		var expected []byte
		switch r.state {
		case StatePending:
			expected = pendingKey(r.priority, r.received, id)
		case StateClaimed:
			expected = claimKey(r.claimExpiry, id)
		case StateCommitted:
			expected = committedKey(r.committedAt, id)
		}
		actual := pending[id]
		if r.state == StateClaimed {
			actual = claims[id]
		} else if r.state == StateCommitted {
			actual = committed[id]
		}
		if actual != string(expected) {
			return errDecode
		}
		if len(refs[id]) == 0 {
			return errDecode
		}
		if len(refs[id]) > j.cfg.MaxBatchRefs {
			return errDecode
		}
		if len(originalRefs[id]) == 0 || len(originalRefs[id]) > j.cfg.MaxBatchRefs {
			return errDecode
		}
		if (r.state == StatePending) != reserves[id] {
			return errDecode
		}
		if r.state == StateClaimed && r.claimToken != claimToken(j.cfg.Owner, r.claimOwner, id, r.attempt, r.claimExpiry) {
			return errDecode
		}
	}
	for id := range reserves {
		if _, ok := records[id]; !ok {
			return errDecode
		}
	}
	for batchID, batch := range batches {
		for _, member := range batch.original {
			if !originalRefs[member.recordID][batchID] {
				return errDecode
			}
			record, found := records[member.recordID]
			if containsString(batch.live, member.recordID) {
				if !found {
					return errDecode
				}
				if record.semantic != member.semantic || record.priority != member.priority || !refs[member.recordID][batchID] {
					return errDecode
				}
			} else {
				if refs[member.recordID][batchID] {
					return errDecode
				}
				if found && (record.semantic != member.semantic || record.priority != member.priority) {
					return errDecode
				}
			}
		}
	}
	for id, byBatch := range refs {
		if _, ok := records[id]; !ok {
			return errDecode
		}
		for batchID := range byBatch {
			batch, ok := batches[batchID]
			if !ok || !containsString(batch.live, id) {
				return errDecode
			}
		}
	}
	for id, byBatch := range originalRefs {
		if len(byBatch) > j.cfg.MaxBatchRefs {
			return errDecode
		}
		for batchID := range byBatch {
			batch, ok := batches[batchID]
			if !ok {
				return errDecode
			}
			if _, found := batchMemberByID(batch, id); !found {
				return errDecode
			}
		}
	}
	return nil
}
func reserveValue() []byte { return checked([]byte("JTR1"), make([]byte, transitionReserveBytes)) }
func containsString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

// Pebble's informational WAL messages are not journal telemetry and can carry
// filesystem context. Production reports categorical health separately.
type discardLogger struct{}

func (discardLogger) Infof(string, ...any)  {}
func (discardLogger) Errorf(string, ...any) {}
func (discardLogger) Fatalf(string, ...any) {}
