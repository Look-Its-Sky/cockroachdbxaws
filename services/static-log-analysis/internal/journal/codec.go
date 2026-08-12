package journal

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"sort"
	"time"

	internalv1 "github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/gen/internalv1"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var errDecode = errors.New("journal: decode failure")

type storedRecord struct {
	state       State
	priority    Priority
	received    time.Time
	claimExpiry time.Time
	committedAt time.Time
	attempt     uint32
	claimOwner  string
	claimToken  string
	semantic    [32]byte
	envelope    []byte
}

// NormalizedRecordSize returns the exact nested Protobuf size enforced by the
// journal's per-record boundary. It lets ingestion preserve partial rejection
// without attempting an all-or-nothing append containing an oversized sibling.
func NormalizedRecordSize(record model.NormalizedLog) int { return encodedNormalizedSize(record) }

type storedBatchMember struct {
	recordID string
	priority Priority
	semantic [32]byte
}

type storedBatch struct {
	original []storedBatchMember
	live     []string
}

func encodeBatch(batch storedBatch) []byte {
	payload := binary.AppendUvarint(nil, uint64(len(batch.original)))
	for _, member := range batch.original {
		payload = appendBytes(payload, []byte(member.recordID))
		payload = append(payload, byte(member.priority))
		payload = append(payload, member.semantic[:]...)
	}
	payload = binary.AppendUvarint(payload, uint64(len(batch.live)))
	for _, recordID := range batch.live {
		payload = appendBytes(payload, []byte(recordID))
	}
	return checked([]byte("JBV2"), payload)
}

func decodeBatch(data []byte) (storedBatch, error) {
	payload, ok := unchecked(data, "JBV2")
	if !ok {
		return storedBatch{}, errDecode
	}
	memberCount, n := binary.Uvarint(payload)
	if n <= 0 || memberCount == 0 || memberCount > MaxAppendRecords {
		return storedBatch{}, errDecode
	}
	payload = payload[n:]
	batch := storedBatch{original: make([]storedBatchMember, 0, memberCount)}
	for i := uint64(0); i < memberCount; i++ {
		id, rest, ok := takeBytes(payload)
		if !ok || !validRecordID(string(id)) || len(rest) < 33 {
			return storedBatch{}, errDecode
		}
		member := storedBatchMember{recordID: string(id), priority: Priority(rest[0])}
		if member.priority > PriorityLow {
			return storedBatch{}, errDecode
		}
		copy(member.semantic[:], rest[1:33])
		batch.original = append(batch.original, member)
		payload = rest[33:]
	}
	liveCount, n := binary.Uvarint(payload)
	if n <= 0 || liveCount > memberCount {
		return storedBatch{}, errDecode
	}
	payload = payload[n:]
	batch.live = make([]string, 0, liveCount)
	for i := uint64(0); i < liveCount; i++ {
		id, rest, ok := takeBytes(payload)
		if !ok || !validRecordID(string(id)) {
			return storedBatch{}, errDecode
		}
		batch.live = append(batch.live, string(id))
		payload = rest
	}
	if len(payload) != 0 || !sortedUnique(batchOriginalIDs(batch.original)) || !sortedUnique(batch.live) {
		return storedBatch{}, errDecode
	}
	for _, id := range batch.live {
		if _, found := batchMemberByID(batch, id); !found {
			return storedBatch{}, errDecode
		}
	}
	return batch, nil
}

func batchOriginalIDs(members []storedBatchMember) []string {
	ids := make([]string, len(members))
	for i := range members {
		ids[i] = members[i].recordID
	}
	return ids
}

func sortedUnique(values []string) bool {
	if !sort.StringsAreSorted(values) {
		return false
	}
	for i := 1; i < len(values); i++ {
		if values[i] == values[i-1] {
			return false
		}
	}
	return true
}

func batchMemberByID(batch storedBatch, recordID string) (storedBatchMember, bool) {
	i := sort.Search(len(batch.original), func(i int) bool { return batch.original[i].recordID >= recordID })
	if i == len(batch.original) || batch.original[i].recordID != recordID {
		return storedBatchMember{}, false
	}
	return batch.original[i], true
}

func encodeDurable(record model.NormalizedLog, tenantID, classification string) ([]byte, error) {
	if record.Validate() != nil {
		return nil, errDecode
	}
	message := &internalv1.DurableNormalizedLog{
		SchemaVersion:  model.NormalizedLogSchemaVersion,
		MessageId:      durableMessageID(record.RecordID),
		CreatedAt:      timeProto(record.Source.ReceivedAt),
		Region:         record.Region,
		TenantId:       tenantID,
		Classification: classification,
		Producer:       record.Source.SourceInstance,
		CorrelationId:  record.Correlation.TraceID,
		Log:            toProto(record),
	}
	return proto.MarshalOptions{Deterministic: true}.Marshal(message)
}

func encodedNormalizedSize(record model.NormalizedLog) int {
	return proto.Size(toProto(record))
}

func decodeDurable(data []byte, tenantID, classification string) (model.NormalizedLog, error) {
	var message internalv1.DurableNormalizedLog
	if err := (proto.UnmarshalOptions{DiscardUnknown: false, RecursionLimit: 100}).Unmarshal(data, &message); err != nil || message.Log == nil {
		return model.NormalizedLog{}, errDecode
	}
	version, versionErr := model.ParseSchemaVersion(message.SchemaVersion)
	parsedID, idErr := uuid.Parse(message.MessageId)
	if versionErr != nil || version.Major != model.NormalizedLogSchemaMajor || idErr != nil || parsedID.String() != message.MessageId || message.CreatedAt == nil || message.CreatedAt.CheckValid() != nil ||
		message.Region == "" || message.TenantId == "" || message.Classification == "" || message.Producer == "" {
		return model.NormalizedLog{}, errDecode
	}
	if message.TenantId != tenantID || message.Classification != classification {
		return model.NormalizedLog{}, errDecode
	}
	record, err := fromProto(message.Log)
	if err != nil || durableMessageID(record.RecordID) != message.MessageId || record.Region != message.Region ||
		record.Source.SourceInstance != message.Producer || record.Correlation.TraceID != message.CorrelationId || !record.Source.ReceivedAt.Equal(message.CreatedAt.AsTime()) {
		return model.NormalizedLog{}, errDecode
	}
	if !validRecordTimes(record) {
		return model.NormalizedLog{}, errDecode
	}
	return record, nil
}

func semanticDigestEnvelope(data []byte) ([32]byte, error) {
	var message internalv1.DurableNormalizedLog
	if err := (proto.UnmarshalOptions{DiscardUnknown: false, RecursionLimit: 100}).Unmarshal(data, &message); err != nil || message.Log == nil || message.Log.Source == nil {
		return [32]byte{}, errDecode
	}
	// BatchID and both copies of the trusted envelope's ReceivedAt describe a
	// transport attempt, not the occurrence. Marshal the whole durable message
	// so same-major unknown fields remain identity-relevant.
	message.CreatedAt = nil
	message.Log.BatchId = ""
	message.Log.Source.ReceivedAt = nil
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(&message)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(encoded), nil
}

// NewReplayIdentity computes the exact sealed identity M3 stores for replay
// conflicts, including the durable outer boundary and admission priority.
func NewReplayIdentity(record model.NormalizedLog, tenantID, classification string, priority Priority) (ReplayIdentity, error) {
	if priority > PriorityLow {
		return ReplayIdentity{}, ErrInvalidInput
	}
	encoded, err := encodeDurable(record, tenantID, classification)
	if err != nil {
		return ReplayIdentity{}, err
	}
	digest, err := semanticDigestEnvelope(encoded)
	if err != nil {
		return ReplayIdentity{}, err
	}
	return ReplayIdentity{version: ReplayIdentityVersion, digest: digest, priority: priority}, nil
}

func durableMessageID(recordID string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("journal-record:"+recordID)).String()
}

func encodeStored(r storedRecord) []byte {
	payload := []byte{byte(r.state), byte(r.priority)}
	payload = binary.BigEndian.AppendUint64(payload, uint64(r.received.UnixNano()))
	payload = appendOptionalTime(payload, r.claimExpiry)
	payload = appendOptionalTime(payload, r.committedAt)
	payload = binary.BigEndian.AppendUint32(payload, r.attempt)
	payload = appendBytes(payload, []byte(r.claimOwner))
	payload = appendBytes(payload, []byte(r.claimToken))
	payload = append(payload, r.semantic[:]...)
	payload = appendBytes(payload, r.envelope)
	return checked([]byte("JRV1"), payload)
}

func decodeStored(data []byte) (storedRecord, error) {
	payload, ok := unchecked(data, "JRV1")
	if !ok || len(payload) < 64 {
		return storedRecord{}, errDecode
	}
	r := storedRecord{state: State(payload[0]), priority: Priority(payload[1])}
	r.received = time.Unix(0, int64(binary.BigEndian.Uint64(payload[2:10]))).UTC()
	var validTime bool
	r.claimExpiry, validTime = readOptionalTime(payload[10], payload[11:19])
	if !validTime {
		return storedRecord{}, errDecode
	}
	r.committedAt, validTime = readOptionalTime(payload[19], payload[20:28])
	if !validTime {
		return storedRecord{}, errDecode
	}
	r.attempt = binary.BigEndian.Uint32(payload[28:32])
	payload = payload[32:]
	var okRead bool
	var part []byte
	part, payload, okRead = takeBytes(payload)
	if !okRead {
		return storedRecord{}, errDecode
	}
	r.claimOwner = string(part)
	part, payload, okRead = takeBytes(payload)
	if !okRead || len(payload) < 32 {
		return storedRecord{}, errDecode
	}
	r.claimToken = string(part)
	copy(r.semantic[:], payload[:32])
	payload = payload[32:]
	part, payload, okRead = takeBytes(payload)
	if !okRead || len(payload) != 0 {
		return storedRecord{}, errDecode
	}
	r.envelope = append([]byte(nil), part...)
	if r.state < StatePending || r.state > StateCommitted || r.priority > PriorityLow || !validJournalTime(r.received) || len(r.envelope) == 0 {
		return storedRecord{}, errDecode
	}
	switch r.state {
	case StatePending:
		if r.claimOwner != "" || r.claimToken != "" || !r.claimExpiry.IsZero() || !r.committedAt.IsZero() {
			return storedRecord{}, errDecode
		}
	case StateClaimed:
		if !validIdentifier(r.claimOwner, MaxOwnerBytes) || !validClaimToken(r.claimToken) || !validJournalTime(r.claimExpiry) || !r.committedAt.IsZero() || r.attempt == 0 {
			return storedRecord{}, errDecode
		}
	case StateCommitted:
		if r.claimOwner != "" || !validClaimToken(r.claimToken) || !r.claimExpiry.IsZero() || !validJournalTime(r.committedAt) || r.attempt == 0 {
			return storedRecord{}, errDecode
		}
	}
	return r, nil
}

func checked(magic, payload []byte) []byte {
	out := append(append([]byte(nil), magic...), payload...)
	sum := sha256.Sum256(out)
	return append(out, sum[:]...)
}

func unchecked(data []byte, magic string) ([]byte, bool) {
	if len(data) < len(magic)+sha256.Size || string(data[:len(magic)]) != magic {
		return nil, false
	}
	payloadEnd := len(data) - sha256.Size
	sum := sha256.Sum256(data[:payloadEnd])
	if !equal32(sum[:], data[payloadEnd:]) {
		return nil, false
	}
	return data[len(magic):payloadEnd], true
}

func equal32(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var different byte
	for i := range a {
		different |= a[i] ^ b[i]
	}
	return different == 0
}

func appendBytes(out, value []byte) []byte {
	out = binary.AppendUvarint(out, uint64(len(value)))
	return append(out, value...)
}
func takeBytes(in []byte) ([]byte, []byte, bool) {
	n, read := binary.Uvarint(in)
	if read <= 0 || n > uint64(len(in[read:])) {
		return nil, nil, false
	}
	return in[read : read+int(n)], in[read+int(n):], true
}
func appendOptionalTime(out []byte, at time.Time) []byte {
	if at.IsZero() {
		out = append(out, 0)
		return binary.BigEndian.AppendUint64(out, 0)
	}
	out = append(out, 1)
	return binary.BigEndian.AppendUint64(out, uint64(at.UnixNano()))
}

func readOptionalTime(present byte, nanos []byte) (time.Time, bool) {
	if len(nanos) != 8 {
		return time.Time{}, false
	}
	encoded := binary.BigEndian.Uint64(nanos)
	switch present {
	case 0:
		return time.Time{}, encoded == 0
	case 1:
		at := time.Unix(0, int64(encoded)).UTC()
		return at, validJournalTime(at)
	default:
		return time.Time{}, false
	}
}

func encodeStrings(magic string, values []string) []byte {
	payload := binary.AppendUvarint(nil, uint64(len(values)))
	for _, v := range values {
		payload = appendBytes(payload, []byte(v))
	}
	return checked([]byte(magic), payload)
}
func decodeStrings(data []byte, magic string) ([]string, error) {
	payload, ok := unchecked(data, magic)
	if !ok {
		return nil, errDecode
	}
	count, n := binary.Uvarint(payload)
	if n <= 0 {
		return nil, errDecode
	}
	payload = payload[n:]
	out := make([]string, 0, count)
	for i := uint64(0); i < count; i++ {
		v, rest, ok := takeBytes(payload)
		if !ok {
			return nil, errDecode
		}
		out = append(out, string(v))
		payload = rest
	}
	if len(payload) != 0 {
		return nil, errDecode
	}
	return out, nil
}

type quarantineMetadata struct {
	recordID string
	reason   QuarantineReason
	at       time.Time
	attempt  uint32
	semantic [32]byte
	priority Priority
	legacy   bool
}

func encodeQuarantine(recordID string, reason QuarantineReason, at time.Time, attempt uint32, semantic [32]byte, priority Priority) []byte {
	present := byte(0)
	if !at.IsZero() {
		present = 1
	}
	payload := []byte{byte(reason), present}
	payload = binary.BigEndian.AppendUint64(payload, uint64(at.UnixNano()))
	payload = binary.BigEndian.AppendUint32(payload, attempt)
	payload = append(payload, byte(priority))
	payload = append(payload, semantic[:]...)
	payload = appendBytes(payload, []byte(recordID))
	return checked([]byte("JQV2"), payload)
}

// encodeLegacyQuarantine exists only for opening and explicitly migrating
// journals written before quarantine tombstones retained replay identity.
func encodeLegacyQuarantine(recordID string, reason QuarantineReason, at time.Time, attempt uint32) []byte {
	present := byte(0)
	if !at.IsZero() {
		present = 1
	}
	payload := []byte{byte(reason), present}
	payload = binary.BigEndian.AppendUint64(payload, uint64(at.UnixNano()))
	payload = binary.BigEndian.AppendUint32(payload, attempt)
	payload = appendBytes(payload, []byte(recordID))
	return checked([]byte("JQV1"), payload)
}

func decodeQuarantine(data []byte) (quarantineMetadata, error) {
	payload, current := unchecked(data, "JQV2")
	legacy := false
	if !current {
		payload, legacy = unchecked(data, "JQV1")
	}
	minimum := 48
	if legacy {
		minimum = 15
	}
	if (!current && !legacy) || len(payload) < minimum || payload[1] != 1 {
		return quarantineMetadata{}, errDecode
	}
	reason := QuarantineReason(payload[0])
	if reason < QuarantineInvalidNormalizedRecord || reason > QuarantineUnsupportedData {
		return quarantineMetadata{}, errDecode
	}
	at := time.Unix(0, int64(binary.BigEndian.Uint64(payload[2:10]))).UTC()
	attempt := binary.BigEndian.Uint32(payload[10:14])
	metadata := quarantineMetadata{reason: reason, at: at, attempt: attempt, legacy: legacy}
	rest := payload[14:]
	if !legacy {
		metadata.priority = Priority(rest[0])
		copy(metadata.semantic[:], rest[1:33])
		rest = rest[33:]
	}
	id, rest, validID := takeBytes(rest)
	if !validJournalTime(at) || attempt == 0 || !validID || len(rest) != 0 || !validRecordID(string(id)) {
		return quarantineMetadata{}, errDecode
	}
	if !legacy && metadata.priority > PriorityLow {
		return quarantineMetadata{}, errDecode
	}
	metadata.recordID = string(id)
	return metadata, nil
}

func timeProto(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}
func protoTime(t *timestamppb.Timestamp) (time.Time, error) {
	if t == nil || t.CheckValid() != nil {
		return time.Time{}, errDecode
	}
	return t.AsTime(), nil
}
func optionalProtoTime(t *timestamppb.Timestamp) (time.Time, error) {
	if t == nil {
		return time.Time{}, nil
	}
	return protoTime(t)
}

func toProto(r model.NormalizedLog) *internalv1.NormalizedLog {
	p := &internalv1.NormalizedLog{SchemaVersion: r.SchemaVersion, RecordId: r.RecordID, RecordIdVersion: r.RecordIDVersion, IdentityQuality: string(r.IdentityQuality), BatchId: r.BatchID, Source: envelopeProto(r.Source), Region: r.Region, EventTime: timeProto(r.EventTime), ObservedTime: timeProto(r.ObservedTime), TimestampInferred: r.TimestampInferred, TimestampInferenceReason: r.TimestampInferenceReason, ObservedTimeInferred: r.ObservedTimeInferred, ObservedTimeInferenceReason: r.ObservedTimeInferenceReason, SeverityNumber: r.SeverityNumber, SeverityText: r.SeverityText, SeverityClass: string(r.SeverityClass), Body: valueProto(r.Body), EventName: r.EventName, Attributes: mapProto(r.Attributes), ResourceAttributes: mapProto(r.ResourceAttributes), ScopeAttributes: mapProto(r.ScopeAttributes), Service: &internalv1.ServiceIdentity{Name: r.Service.Name, Namespace: r.Service.Namespace, InstanceId: r.Service.InstanceID, Environment: r.Service.Environment, Status: string(r.Service.Status)}, Deployment: &internalv1.DeploymentIdentity{Id: r.Deployment.ID, Version: r.Deployment.Version, Status: string(r.Deployment.Status)}, Correlation: &internalv1.CorrelationIdentity{TraceId: r.Correlation.TraceID, SpanId: r.Correlation.SpanID}, Redaction: &internalv1.RedactionMetadata{PolicyVersion: r.Redaction.PolicyVersion, RuleIds: append([]string(nil), r.Redaction.RuleIDs...), WithheldFields: append([]string(nil), r.Redaction.WithheldFields...)}}
	if r.Exception != nil {
		p.Exception = &internalv1.NormalizedException{Type: r.Exception.Type, SafeMessage: r.Exception.SafeMessage}
		for _, f := range r.Exception.StackFrames {
			p.Exception.StackFrames = append(p.Exception.StackFrames, &internalv1.StackFrame{Function: f.Function, Module: f.Module, File: f.File, InApplication: f.InApplication})
		}
	}
	if r.RawReference != nil {
		x := r.RawReference
		p.RawReference = &internalv1.RegionalLogReference{SourceType: string(x.SourceType), Region: x.Region, Locator: x.Locator, From: timeProto(x.From), To: timeProto(x.To), Classification: x.Classification, ExpiresAt: timeProto(x.ExpiresAt)}
	}
	return p
}

func envelopeProto(e model.TrustedEnvelope) *internalv1.TrustedEnvelope {
	return &internalv1.TrustedEnvelope{SourceType: string(e.SourceType), SourceAccount: e.SourceAccount, Region: e.Region, AllowedEnvironments: append([]string(nil), e.AllowedEnvironments...), AllowedServices: append([]string(nil), e.AllowedServices...), SourceInstance: e.SourceInstance, CredentialIdentity: e.CredentialIdentity, ReceivedAt: timeProto(e.ReceivedAt)}
}
func mapProto(m map[string]model.SafeValue) map[string]*internalv1.SafeValue {
	if m == nil {
		return nil
	}
	out := make(map[string]*internalv1.SafeValue, len(m))
	for k, v := range m {
		out[k] = valueProto(v)
	}
	return out
}
func valueProto(v model.SafeValue) *internalv1.SafeValue {
	switch v.Kind {
	case model.SafeKindEmpty:
		return nil
	case model.SafeKindString:
		return &internalv1.SafeValue{Value: &internalv1.SafeValue_StringValue{StringValue: v.String}}
	case model.SafeKindInt:
		return &internalv1.SafeValue{Value: &internalv1.SafeValue_IntValue{IntValue: v.Int}}
	case model.SafeKindDouble:
		return &internalv1.SafeValue{Value: &internalv1.SafeValue_DoubleValue{DoubleValue: v.Double}}
	case model.SafeKindBool:
		return &internalv1.SafeValue{Value: &internalv1.SafeValue_BoolValue{BoolValue: v.Bool}}
	case model.SafeKindMap:
		return &internalv1.SafeValue{Value: &internalv1.SafeValue_MapValue{MapValue: &internalv1.SafeMap{Values: mapProto(v.Map)}}}
	case model.SafeKindSlice:
		x := &internalv1.SafeList{}
		for _, child := range v.Slice {
			x.Values = append(x.Values, valueProto(child))
		}
		return &internalv1.SafeValue{Value: &internalv1.SafeValue_ListValue{ListValue: x}}
	case model.SafeKindWithheld:
		return &internalv1.SafeValue{Value: &internalv1.SafeValue_WithheldReason{WithheldReason: v.Withheld}}
	default:
		return nil
	}
}

func fromProto(p *internalv1.NormalizedLog) (model.NormalizedLog, error) {
	if p.Source == nil || p.Service == nil || p.Deployment == nil || p.Correlation == nil || p.Redaction == nil {
		return model.NormalizedLog{}, errDecode
	}
	event, err := protoTime(p.EventTime)
	if err != nil {
		return model.NormalizedLog{}, err
	}
	observed, err := protoTime(p.ObservedTime)
	if err != nil {
		return model.NormalizedLog{}, err
	}
	received, err := protoTime(p.Source.ReceivedAt)
	if err != nil {
		return model.NormalizedLog{}, err
	}
	body, err := valueModel(p.Body, true)
	if err != nil {
		return model.NormalizedLog{}, err
	}
	attrs, err := mapModel(p.Attributes)
	if err != nil {
		return model.NormalizedLog{}, err
	}
	resource, err := mapModel(p.ResourceAttributes)
	if err != nil {
		return model.NormalizedLog{}, err
	}
	scope, err := mapModel(p.ScopeAttributes)
	if err != nil {
		return model.NormalizedLog{}, err
	}
	r := model.NormalizedLog{SchemaVersion: p.SchemaVersion, RecordID: p.RecordId, RecordIDVersion: p.RecordIdVersion, IdentityQuality: model.IdentityQuality(p.IdentityQuality), BatchID: p.BatchId, Source: model.TrustedEnvelope{SourceType: model.SourceType(p.Source.SourceType), SourceAccount: p.Source.SourceAccount, Region: p.Source.Region, AllowedEnvironments: append([]string(nil), p.Source.AllowedEnvironments...), AllowedServices: append([]string(nil), p.Source.AllowedServices...), SourceInstance: p.Source.SourceInstance, CredentialIdentity: p.Source.CredentialIdentity, ReceivedAt: received}, Region: p.Region, EventTime: event, ObservedTime: observed, TimestampInferred: p.TimestampInferred, TimestampInferenceReason: p.TimestampInferenceReason, ObservedTimeInferred: p.ObservedTimeInferred, ObservedTimeInferenceReason: p.ObservedTimeInferenceReason, SeverityNumber: p.SeverityNumber, SeverityText: p.SeverityText, SeverityClass: model.SeverityClass(p.SeverityClass), Body: body, EventName: p.EventName, Attributes: attrs, ResourceAttributes: resource, ScopeAttributes: scope, Service: model.ServiceIdentity{Name: p.Service.Name, Namespace: p.Service.Namespace, InstanceID: p.Service.InstanceId, Environment: p.Service.Environment, Status: model.EnrichmentStatus(p.Service.Status)}, Deployment: model.DeploymentIdentity{ID: p.Deployment.Id, Version: p.Deployment.Version, Status: model.EnrichmentStatus(p.Deployment.Status)}, Correlation: model.CorrelationIdentity{TraceID: p.Correlation.TraceId, SpanID: p.Correlation.SpanId}, Redaction: model.RedactionMetadata{PolicyVersion: p.Redaction.PolicyVersion, RuleIDs: append([]string(nil), p.Redaction.RuleIds...), WithheldFields: append([]string(nil), p.Redaction.WithheldFields...)}}
	if p.Exception != nil {
		r.Exception = &model.NormalizedException{Type: p.Exception.Type, SafeMessage: p.Exception.SafeMessage}
		for _, f := range p.Exception.StackFrames {
			if f == nil {
				return model.NormalizedLog{}, errDecode
			}
			r.Exception.StackFrames = append(r.Exception.StackFrames, model.StackFrame{Function: f.Function, Module: f.Module, File: f.File, InApplication: f.InApplication})
		}
	}
	if p.RawReference != nil {
		x := p.RawReference
		from, err := optionalProtoTime(x.From)
		if err != nil {
			return model.NormalizedLog{}, err
		}
		to, err := optionalProtoTime(x.To)
		if err != nil {
			return model.NormalizedLog{}, err
		}
		expires, err := protoTime(x.ExpiresAt)
		if err != nil {
			return model.NormalizedLog{}, err
		}
		r.RawReference = &model.RegionalLogReference{SourceType: model.SourceType(x.SourceType), Region: x.Region, Locator: x.Locator, From: from, To: to, Classification: x.Classification, ExpiresAt: expires}
	}
	if err := r.Validate(); err != nil {
		return model.NormalizedLog{}, errDecode
	}
	return r, nil
}

func mapModel(m map[string]*internalv1.SafeValue) (map[string]model.SafeValue, error) {
	if m == nil {
		return nil, nil
	}
	out := make(map[string]model.SafeValue, len(m))
	for k, v := range m {
		x, err := valueModel(v, false)
		if err != nil {
			return nil, err
		}
		out[k] = x
	}
	return out, nil
}
func valueModel(p *internalv1.SafeValue, allowNil bool) (model.SafeValue, error) {
	if p == nil {
		if allowNil {
			return model.SafeValue{}, nil
		}
		return model.SafeValue{}, errDecode
	}
	switch x := p.Value.(type) {
	case *internalv1.SafeValue_StringValue:
		return model.SafeString(x.StringValue), nil
	case *internalv1.SafeValue_IntValue:
		return model.SafeInt(x.IntValue), nil
	case *internalv1.SafeValue_DoubleValue:
		return model.SafeDouble(x.DoubleValue), nil
	case *internalv1.SafeValue_BoolValue:
		return model.SafeBool(x.BoolValue), nil
	case *internalv1.SafeValue_MapValue:
		if x.MapValue == nil {
			return model.SafeValue{}, errDecode
		}
		v, err := mapModel(x.MapValue.Values)
		return model.SafeMap(v), err
	case *internalv1.SafeValue_ListValue:
		if x.ListValue == nil {
			return model.SafeValue{}, errDecode
		}
		out := make([]model.SafeValue, 0, len(x.ListValue.Values))
		for _, child := range x.ListValue.Values {
			v, err := valueModel(child, false)
			if err != nil {
				return model.SafeValue{}, err
			}
			out = append(out, v)
		}
		return model.SafeSlice(out...), nil
	case *internalv1.SafeValue_WithheldReason:
		return model.Withheld(x.WithheldReason), nil
	default:
		return model.SafeValue{}, errDecode
	}
}
