package otlpreceiver_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	collectorlogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	rpcstatus "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/admission"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/journal"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/otlpreceiver"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/pipeline"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/redact"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/fakeclock"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/otlpgen"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/testids"
)

// fakeIngester answers exactly what a test wants the coordinator to have
// answered, and records what the receiver handed it. It never inspects the
// payload, because deciding an outcome from the payload is the coordinator's
// job and not the transport's.
type fakeIngester struct {
	mu        sync.Mutex
	result    pipeline.IngestResult
	err       error
	before    func()
	envelopes []model.TrustedEnvelope
	encodings []admission.Encoding
	payloads  [][]byte
}

func (f *fakeIngester) Ingest(_ context.Context, envelope model.TrustedEnvelope, payload []byte, encoding admission.Encoding) (pipeline.IngestResult, error) {
	if f.before != nil {
		f.before()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.envelopes = append(f.envelopes, envelope)
	f.encodings = append(f.encodings, encoding)
	f.payloads = append(f.payloads, append([]byte(nil), payload...))
	return f.result, f.err
}

func (f *fakeIngester) calls() ([]model.TrustedEnvelope, []admission.Encoding, [][]byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.envelopes, f.encodings, f.payloads
}

func startReceiver(t *testing.T, ingester otlpreceiver.Ingester, mutate ...func(*otlpreceiver.Config)) *otlpreceiver.Receiver {
	t.Helper()
	config := otlpreceiver.Config{
		GRPCListen: "127.0.0.1:0", HTTPListen: "127.0.0.1:0", Trust: staticTrust(),
		Policy: redact.MinimalPolicy(), Clock: fakeclock.NewAtOrigin(), Limits: defaultLimits(),
	}
	for _, m := range mutate {
		m(&config)
	}
	receiver, err := otlpreceiver.New(config, ingester)
	if err != nil {
		t.Fatal(err)
	}
	if err := receiver.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = receiver.Shutdown(ctx)
	})
	return receiver
}

func defaultLimits() admission.Limits {
	return admission.Limits{
		MaxCompressedBytes: admission.DefaultMaxCompressedBytes, MaxUncompressedBytes: admission.DefaultMaxUncompressedBytes,
		MaxRecords: admission.DefaultMaxRecords, MaxNestingDepth: admission.DefaultMaxNestingDepth,
		MaxNormalizedBytes: admission.DefaultMaxNormalizedBytes, MaxMaterializedBytes: admission.DefaultMaxMaterializedBytes,
		MaxStructuralNodes: admission.DefaultMaxStructuralNodes,
	}
}

func grpcExport(t *testing.T, receiver *otlpreceiver.Receiver, payload []byte) (*collectorlogs.ExportLogsServiceResponse, error) {
	t.Helper()
	conn, err := grpc.NewClient(receiver.GRPCAddr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	request := &collectorlogs.ExportLogsServiceRequest{}
	if err := proto.Unmarshal(payload, request); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return collectorlogs.NewLogsServiceClient(conn).Export(ctx, request)
}

// httpExport speaks the OTLP/HTTP binary Protobuf contract directly: POST
// /v1/logs with application/x-protobuf, which is what the Collector's otlphttp
// exporter sends.
func httpExport(t *testing.T, receiver *otlpreceiver.Receiver, payload []byte, encoding string) (int, []byte) {
	t.Helper()
	body := payload
	if encoding == "gzip" {
		var buffer bytes.Buffer
		writer := gzip.NewWriter(&buffer)
		if _, err := writer.Write(payload); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		body = buffer.Bytes()
	}
	request, err := http.NewRequest(http.MethodPost, "http://"+receiver.HTTPAddr()+"/v1/logs", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-protobuf")
	if encoding != "" {
		request.Header.Set("Content-Encoding", encoding)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, responseBody
}

func samplePayload(t *testing.T) []byte {
	t.Helper()
	clock := fakeclock.NewAtOrigin()
	producer := otlpgen.New(otlpgen.WithClock(clock), otlpgen.WithIDs(testids.New(testids.WithClock(clock))))
	payload, err := otlpgen.Encode(producer.Request(producer.PaymentError(otlpgen.WithBody("card declined"))))
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func acknowledged(accepted int, rejected ...pipeline.RecordRejection) pipeline.IngestResult {
	return pipeline.IngestResult{Acknowledged: true, Accepted: accepted, Rejected: rejected}
}

func TestSuccessfulExportOverBothTransportsCarriesNoPartialSuccess(t *testing.T) {
	ingester := &fakeIngester{result: acknowledged(1)}
	receiver := startReceiver(t, ingester)
	payload := samplePayload(t)

	response, err := grpcExport(t, receiver, payload)
	if err != nil {
		t.Fatalf("gRPC export: %v", err)
	}
	if response.GetPartialSuccess().GetRejectedLogRecords() != 0 || response.GetPartialSuccess().GetErrorMessage() != "" {
		t.Fatalf("a fully accepted batch reported partial success: %+v", response.GetPartialSuccess())
	}
	code, body := httpExport(t, receiver, payload, "")
	if code != http.StatusOK {
		t.Fatalf("HTTP export: status=%d body=%q", code, body)
	}
	var decoded collectorlogs.ExportLogsServiceResponse
	if err := proto.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("HTTP success body is not an ExportLogsServiceResponse: %v", err)
	}
	if decoded.GetPartialSuccess().GetRejectedLogRecords() != 0 {
		t.Fatalf("a fully accepted batch reported partial success over HTTP: %+v", decoded.GetPartialSuccess())
	}
}

// architecture.md: "A record held only in memory must never be acknowledged."
// A result the coordinator did not mark acknowledged is exactly that record, so
// the transport must ask for it again rather than report success.
func TestAResultWithoutAJournalCommitIsNeverReportedAsSuccess(t *testing.T) {
	ingester := &fakeIngester{result: pipeline.IngestResult{Acknowledged: false, Accepted: 1}}
	receiver := startReceiver(t, ingester)
	payload := samplePayload(t)

	if _, err := grpcExport(t, receiver, payload); status.Code(err) != codes.Unavailable {
		t.Fatalf("an unacknowledged batch answered gRPC %s", status.Code(err))
	}
	if code, body := httpExport(t, receiver, payload, ""); code != http.StatusServiceUnavailable {
		t.Fatalf("an unacknowledged batch answered HTTP %d: %q", code, body)
	}
}

// A permanent failure returned as retryable makes a Collector replay a poisoned
// batch forever and starve its valid siblings.
func TestPermanentOutcomesAreNotRetryableOnEitherTransport(t *testing.T) {
	cases := map[string]error{
		"request rejected":  pipeline.ErrRequestRejected,
		"foreign region":    pipeline.ErrScopeNotPermitted,
		"journal refused":   pipeline.ErrJournalRejected,
		"identity conflict": journal.ErrDuplicateConflict,
	}
	for name, ingestErr := range cases {
		t.Run(name, func(t *testing.T) {
			receiver := startReceiver(t, &fakeIngester{err: ingestErr})
			payload := samplePayload(t)
			if _, err := grpcExport(t, receiver, payload); status.Code(err) != codes.InvalidArgument {
				t.Fatalf("%s answered gRPC %s, which a Collector retries", name, status.Code(err))
			}
			code, body := httpExport(t, receiver, payload, "")
			if code != http.StatusBadRequest {
				t.Fatalf("%s answered HTTP %d, which a Collector retries", name, code)
			}
			var failure rpcstatus.Status
			if err := proto.Unmarshal(body, &failure); err != nil {
				t.Fatalf("HTTP failure body is not a google.rpc.Status: %v", err)
			}
			if failure.GetCode() != int32(codes.InvalidArgument) {
				t.Fatalf("HTTP failure body reported code %d", failure.GetCode())
			}
		})
	}
}

func TestJournalUnavailabilityIsRetryableOnEitherTransport(t *testing.T) {
	receiver := startReceiver(t, &fakeIngester{err: pipeline.ErrJournalUnavailable})
	payload := samplePayload(t)
	if _, err := grpcExport(t, receiver, payload); status.Code(err) != codes.Unavailable {
		t.Fatalf("journal unavailability answered gRPC %s", status.Code(err))
	}
	if code, _ := httpExport(t, receiver, payload, ""); code != http.StatusServiceUnavailable {
		t.Fatalf("journal unavailability answered HTTP %d", code)
	}
}

// An unexpected error class is a bug, not a verdict about the batch. It is
// reported as retryable so an acknowledged-looking loss is impossible, and the
// Collector's own queue holds the data while it is diagnosed.
func TestAnUnrecognizedIngestFailureIsTreatedAsRetryable(t *testing.T) {
	receiver := startReceiver(t, &fakeIngester{err: errors.New("something nobody classified")})
	if _, err := grpcExport(t, receiver, samplePayload(t)); status.Code(err) != codes.Unavailable {
		t.Fatalf("an unclassified failure answered gRPC %s", status.Code(err))
	}
}

func TestRejectedRecordsBecomeOTLPPartialSuccess(t *testing.T) {
	ingester := &fakeIngester{result: acknowledged(2,
		pipeline.RecordRejection{Index: 1, Reason: pipeline.RejectionClaimNotPermitted},
		pipeline.RecordRejection{Index: 3, Reason: pipeline.RejectionNestingTooDeep},
		pipeline.RecordRejection{Index: 4, Reason: pipeline.RejectionClaimNotPermitted})}
	receiver := startReceiver(t, ingester)
	response, err := grpcExport(t, receiver, samplePayload(t))
	if err != nil {
		t.Fatalf("a partially rejected batch failed instead of partially succeeding: %v", err)
	}
	partial := response.GetPartialSuccess()
	if partial.GetRejectedLogRecords() != 3 {
		t.Fatalf("rejected_log_records=%d, want 3", partial.GetRejectedLogRecords())
	}
	message := partial.GetErrorMessage()
	if message == "" {
		t.Fatal("partial success carried no explanation at all")
	}
	if !strings.Contains(message, string(pipeline.RejectionClaimNotPermitted)) || !strings.Contains(message, string(pipeline.RejectionNestingTooDeep)) {
		t.Fatalf("the message does not name the categorical reasons: %q", message)
	}
	code, body := httpExport(t, receiver, samplePayload(t), "")
	if code != http.StatusOK {
		t.Fatalf("a partially rejected batch answered HTTP %d", code)
	}
	var decoded collectorlogs.ExportLogsServiceResponse
	if err := proto.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.GetPartialSuccess().GetRejectedLogRecords() != 3 {
		t.Fatalf("HTTP partial success reported %d rejected records", decoded.GetPartialSuccess().GetRejectedLogRecords())
	}
}

// The partial-success message is returned to the Collector, which logs it. It
// is built from categorical reasons the service owns, and is put through the
// redaction policy's text validation before it leaves the process.
func TestPartialSuccessMessageIsCategoricalAndPolicyValidated(t *testing.T) {
	policy, err := redact.MinimalPolicy().WithForbiddenValues("customer-secret")
	if err != nil {
		t.Fatal(err)
	}
	ingester := &fakeIngester{result: acknowledged(0, pipeline.RecordRejection{Index: 0, Reason: pipeline.RejectionInvalidRecord})}
	receiver := startReceiver(t, ingester, func(c *otlpreceiver.Config) { c.Policy = policy })
	clock := fakeclock.NewAtOrigin()
	producer := otlpgen.New(otlpgen.WithClock(clock), otlpgen.WithIDs(testids.New(testids.WithClock(clock))))
	payload, err := otlpgen.Encode(producer.Request(producer.PaymentError(otlpgen.WithBody("card declined customer-secret"))))
	if err != nil {
		t.Fatal(err)
	}
	response, err := grpcExport(t, receiver, payload)
	if err != nil {
		t.Fatal(err)
	}
	message := response.GetPartialSuccess().GetErrorMessage()
	if strings.Contains(message, "customer-secret") || strings.Contains(message, "card declined") {
		t.Fatalf("log content reached the partial-success message: %q", message)
	}
	if err := policy.ValidateText(message); err != nil {
		t.Fatalf("the partial-success message does not pass the policy that produced it: %v", err)
	}
}

// security.md: service, environment, and region attributes inside application
// logs are untrusted. The receiver reads none of them; the envelope it hands to
// the coordinator is exactly the configured, authenticated one.
func TestPayloadClaimsCannotInfluenceTheTrustedEnvelope(t *testing.T) {
	ingester := &fakeIngester{result: acknowledged(1)}
	receiver := startReceiver(t, ingester)
	clock := fakeclock.NewAtOrigin()
	forged := otlpgen.New(otlpgen.WithClock(clock), otlpgen.WithIDs(testids.New(testids.WithClock(clock))),
		otlpgen.WithRegion("eu-central-1"), otlpgen.WithService("attacker-service"), otlpgen.WithEnvironment("staging"))
	payload, err := otlpgen.Encode(forged.Request(forged.PaymentError(otlpgen.WithBody("card declined"))))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := grpcExport(t, receiver, payload); err != nil {
		t.Fatal(err)
	}
	if code, body := httpExport(t, receiver, payload, ""); code != http.StatusOK {
		t.Fatalf("forged-claim export answered HTTP %d: %q", code, body)
	}
	envelopes, _, _ := ingester.calls()
	if len(envelopes) != 2 {
		t.Fatalf("the receiver made %d ingest calls", len(envelopes))
	}
	want := model.TrustedEnvelope{
		SourceType: model.SourceTypeOTLP, SourceAccount: "aws-account-a", Region: "us-east-1",
		AllowedEnvironments: []string{"production"}, AllowedServices: []string{"paymentservice"},
		SourceInstance: "collector-a", CredentialIdentity: "workload-a", ReceivedAt: fakeclock.Origin,
	}
	for i, envelope := range envelopes {
		if !reflect.DeepEqual(envelope, want) {
			t.Fatalf("call %d admitted under a payload-influenced envelope:\n got %+v\nwant %+v", i, envelope, want)
		}
	}
}

func TestHTTPHonoursGzipContentEncoding(t *testing.T) {
	ingester := &fakeIngester{result: acknowledged(1)}
	receiver := startReceiver(t, ingester)
	if code, body := httpExport(t, receiver, samplePayload(t), "gzip"); code != http.StatusOK {
		t.Fatalf("gzip export answered %d: %q", code, body)
	}
	_, encodings, _ := ingester.calls()
	if len(encodings) != 1 || encodings[0] != admission.EncodingGZIP {
		t.Fatalf("the receiver decompressed instead of declaring the encoding: %v", encodings)
	}
}

// source-adapters.md makes the regional Collector over OTLP/gRPC the initial
// source, and the Collector's otlp exporter compresses with gzip by default. An
// encoding grpc-go has no compressor for is answered UNIMPLEMENTED, which is
// permanent: a default Collector would drop every batch instead of queueing it.
func TestGRPCAcceptsTheGzipCompressionADefaultCollectorSends(t *testing.T) {
	ingester := &fakeIngester{result: acknowledged(1)}
	receiver := startReceiver(t, ingester)
	payload := samplePayload(t)
	if err := gzipGRPCExport(receiver.GRPCAddr(), payload, defaultLimits().MaxCompressedBytes); err != nil {
		t.Fatalf("a gzip-compressed export was refused: %v", err)
	}
	envelopes, encodings, payloads := ingester.calls()
	if len(envelopes) != 1 {
		t.Fatalf("the coordinator saw %d gzip exports", len(envelopes))
	}
	// The transport decompressed the frame, so admission is handed the same
	// identity-encoded bytes an uncompressed export produces.
	if encodings[0] != admission.EncodingIdentity {
		t.Fatalf("a gzip frame reached admission declared as %q", encodings[0])
	}
	if len(payloads[0]) == 0 {
		t.Fatal("the decompressed export carried no payload")
	}
}

// A compressor must not become a way around the byte limits the identity path
// is held to. A frame small enough to accept that expands past the limit is
// refused, and refused permanently: it is the same frame on every retry.
func TestGRPCGzipIsBoundedByTheSameCompressedLimitAsTheIdentityPath(t *testing.T) {
	const limit = 4096
	ingester := &fakeIngester{result: acknowledged(1)}
	receiver := startReceiver(t, ingester, func(c *otlpreceiver.Config) { c.Limits.MaxCompressedBytes = limit })
	// Highly compressible: the frame on the wire is a few hundred bytes and
	// expands to far more than the receiver will accept.
	bomb := samplePayloadOfSize(t, 8<<20)
	err := gzipGRPCExport(receiver.GRPCAddr(), bomb, limit)
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("a gzip bomb answered %s: %v", status.Code(err), err)
	}
	if envelopes, _, _ := ingester.calls(); len(envelopes) != 0 {
		t.Fatalf("a gzip bomb reached the coordinator %d times", len(envelopes))
	}
}

// gzipGRPCExport sends a gzip-compressed export. It uses the deprecated
// client-side compressor deliberately: grpc-go's codec registry is global to a
// process, so a test that registered gzip through encoding/gzip would install
// the very decompressor it is meant to prove the receiver installs.
func gzipGRPCExport(addr string, payload []byte, maxSend int64) error {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithCompressor(grpc.NewGZIPCompressor()))
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	request := &collectorlogs.ExportLogsServiceRequest{}
	if err := proto.Unmarshal(payload, request); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err = collectorlogs.NewLogsServiceClient(conn).Export(ctx, request,
		grpc.MaxCallSendMsgSize(int(maxSend)*4096))
	return err
}

// samplePayloadOfSize builds a valid export whose body is padded to roughly
// size bytes with one repeated character, so it compresses to almost nothing.
func samplePayloadOfSize(t *testing.T, size int) []byte {
	t.Helper()
	clock := fakeclock.NewAtOrigin()
	producer := otlpgen.New(otlpgen.WithClock(clock), otlpgen.WithIDs(testids.New(testids.WithClock(clock))))
	payload, err := otlpgen.Encode(producer.Request(producer.PaymentError(otlpgen.WithBody(strings.Repeat("a", size)))))
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func TestHTTPRefusesWhatIsNotAnOTLPProtobufExport(t *testing.T) {
	receiver := startReceiver(t, &fakeIngester{result: acknowledged(1)})
	base := "http://" + receiver.HTTPAddr()
	cases := []struct {
		name, method, path, contentType, encoding string
		want                                      int
	}{
		{"json body", http.MethodPost, "/v1/logs", "application/json", "", http.StatusUnsupportedMediaType},
		{"no content type", http.MethodPost, "/v1/logs", "", "", http.StatusUnsupportedMediaType},
		{"unknown encoding", http.MethodPost, "/v1/logs", "application/x-protobuf", "deflate", http.StatusUnsupportedMediaType},
		{"wrong method", http.MethodGet, "/v1/logs", "application/x-protobuf", "", http.StatusMethodNotAllowed},
		{"traces path", http.MethodPost, "/v1/traces", "application/x-protobuf", "", http.StatusNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			request, err := http.NewRequest(c.method, base+c.path, bytes.NewReader(samplePayload(t)))
			if err != nil {
				t.Fatal(err)
			}
			if c.contentType != "" {
				request.Header.Set("Content-Type", c.contentType)
			}
			if c.encoding != "" {
				request.Header.Set("Content-Encoding", c.encoding)
			}
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = response.Body.Close() }()
			if response.StatusCode != c.want {
				t.Fatalf("%s answered %d, want %d", c.name, response.StatusCode, c.want)
			}
		})
	}
}

// A request larger than the configured compressed limit is refused before it is
// read into memory, and refused permanently: it will be the same size on retry.
func TestHTTPRefusesABodyOverTheConfiguredCompressedLimit(t *testing.T) {
	ingester := &fakeIngester{result: acknowledged(1)}
	receiver := startReceiver(t, ingester, func(c *otlpreceiver.Config) { c.Limits.MaxCompressedBytes = 16 })
	code, _ := httpExport(t, receiver, samplePayload(t), "")
	if code != http.StatusBadRequest {
		t.Fatalf("an oversized body answered %d", code)
	}
	if envelopes, _, _ := ingester.calls(); len(envelopes) != 0 {
		t.Fatalf("an oversized body reached the coordinator %d times", len(envelopes))
	}
}

func TestNewRefusesAReceiverThatCannotBeBuiltSafely(t *testing.T) {
	valid := otlpreceiver.Config{GRPCListen: "127.0.0.1:0", HTTPListen: "127.0.0.1:0", Trust: staticTrust(),
		Policy: redact.MinimalPolicy(), Clock: fakeclock.NewAtOrigin(), Limits: defaultLimits()}
	if _, err := otlpreceiver.New(valid, &fakeIngester{}); err != nil {
		t.Fatalf("a complete configuration was refused: %v", err)
	}
	for name, mutate := range map[string]func(*otlpreceiver.Config){
		"no policy":  func(c *otlpreceiver.Config) { c.Policy = nil },
		"no clock":   func(c *otlpreceiver.Config) { c.Clock = nil },
		"no listens": func(c *otlpreceiver.Config) { c.GRPCListen, c.HTTPListen = "", "" },
		"bad trust":  func(c *otlpreceiver.Config) { c.Trust.Source = "payload" },
		"bad limits": func(c *otlpreceiver.Config) { c.Limits.MaxRecords = -1 },
	} {
		t.Run(name, func(t *testing.T) {
			config := valid
			mutate(&config)
			if _, err := otlpreceiver.New(config, &fakeIngester{}); err == nil {
				t.Fatalf("%s was accepted", name)
			}
		})
	}
	if _, err := otlpreceiver.New(valid, nil); err == nil {
		t.Fatal("a receiver with no coordinator was accepted")
	}
}

// The admission package reads an unset limit as "use the default". The receiver
// enforces the compressed bound itself on the transport, where an unset limit
// would instead read as "accept nothing" or "accept anything".
func TestUnsetLimitsBecomeTheDocumentedDefaultsRatherThanZero(t *testing.T) {
	ingester := &fakeIngester{result: acknowledged(1)}
	receiver := startReceiver(t, ingester, func(c *otlpreceiver.Config) { c.Limits = admission.Limits{} })
	if code, body := httpExport(t, receiver, samplePayload(t), ""); code != http.StatusOK {
		t.Fatalf("an unset compressed limit refused an ordinary export: %d %q", code, body)
	}
	if _, err := grpcExport(t, receiver, samplePayload(t)); err != nil {
		t.Fatalf("an unset compressed limit refused an ordinary gRPC export: %v", err)
	}
}

// The mutual_tls listener is security-critical wiring, not just an envelope
// decision: a receiver that verified nothing would still hand the coordinator a
// perfectly well-formed envelope.
func TestMutualTLSListenersRefuseAnUnverifiedClientAndAdmitAVerifiedOne(t *testing.T) {
	material := issueTLSMaterial(t)
	ingester := &fakeIngester{result: acknowledged(1)}
	receiver := startReceiver(t, ingester, func(c *otlpreceiver.Config) {
		c.Trust = mutualTrust()
		c.Trust.TLS = otlpreceiver.TLSConfig{CertificateFile: material.serverCert, KeyFile: material.serverKey, ClientCAFile: material.caCert}
	})
	payload := samplePayload(t)

	anonymous := &tls.Config{RootCAs: material.pool, ServerName: "localhost", MinVersion: tls.VersionTLS13}
	if err := tlsGRPCExport(receiver.GRPCAddr(), anonymous, payload); err == nil {
		t.Fatal("a client with no certificate exported over gRPC")
	}
	if err := tlsHTTPExport(receiver.HTTPAddr(), anonymous, payload); err == nil {
		t.Fatal("a client with no certificate exported over HTTP")
	}
	if envelopes, _, _ := ingester.calls(); len(envelopes) != 0 {
		t.Fatalf("an unverified client reached the coordinator %d times", len(envelopes))
	}

	verified := anonymous.Clone()
	verified.Certificates = []tls.Certificate{material.client}
	if err := tlsGRPCExport(receiver.GRPCAddr(), verified, payload); err != nil {
		t.Fatalf("a verified client was refused over gRPC: %v", err)
	}
	if err := tlsHTTPExport(receiver.HTTPAddr(), verified, payload); err != nil {
		t.Fatalf("a verified client was refused over HTTP: %v", err)
	}
	envelopes, _, _ := ingester.calls()
	if len(envelopes) != 2 {
		t.Fatalf("the coordinator saw %d verified exports", len(envelopes))
	}
	for i, envelope := range envelopes {
		if envelope.SourceInstance != "collector-a.otel.svc" || envelope.CredentialIdentity != "spiffe://cluster.local/ns/otel/sa/collector" {
			t.Fatalf("export %d was admitted under %+v rather than the certificate's identity", i, envelope)
		}
	}
}

func tlsGRPCExport(addr string, config *tls.Config, payload []byte) error {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(config)))
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	request := &collectorlogs.ExportLogsServiceRequest{}
	if err := proto.Unmarshal(payload, request); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err = collectorlogs.NewLogsServiceClient(conn).Export(ctx, request)
	return err
}

func tlsHTTPExport(addr string, config *tls.Config, payload []byte) error {
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: config}}
	defer client.CloseIdleConnections()
	request, err := http.NewRequest(http.MethodPost, "https://"+addr+"/v1/logs", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Host = "localhost"
	request.Header.Set("Content-Type", "application/x-protobuf")
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if _, err := io.ReadAll(response.Body); err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", response.StatusCode)
	}
	return nil
}

type tlsMaterial struct {
	caCert     string
	serverCert string
	serverKey  string
	client     tls.Certificate
	pool       *x509.CertPool
}

// issueTLSMaterial mints a throwaway CA, server certificate, and client
// certificate. Generating them keeps the test free of a checked-in fixture that
// would one day expire.
func issueTLSMaterial(t *testing.T) tlsMaterial {
	t.Helper()
	dir := t.TempDir()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, IsCA: true, BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCertificate, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(caCertificate)

	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "receiver"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	clientURI, err := url.Parse("spiffe://cluster.local/ns/otel/sa/collector")
	if err != nil {
		t.Fatal(err)
	}
	clientTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(3), Subject: pkix.Name{CommonName: "collector"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		DNSNames: []string{"collector-a.otel.svc"}, URIs: []*url.URL{clientURI},
	}
	caPath := filepath.Join(dir, "ca.crt")
	writePEM(t, caPath, "CERTIFICATE", caDER)
	serverCertPath, serverKeyPath := issueSigned(t, dir, "server", serverTemplate, caTemplate, caKey)
	clientCertPath, clientKeyPath := issueSigned(t, dir, "client", clientTemplate, caTemplate, caKey)
	clientCertificate, err := tls.LoadX509KeyPair(clientCertPath, clientKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	return tlsMaterial{caCert: caPath, serverCert: serverCertPath, serverKey: serverKeyPath,
		client: clientCertificate, pool: pool}
}

func issueSigned(t *testing.T, dir, name string, template, parent *x509.Certificate, parentKey *ecdsa.PrivateKey) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificate(rand.Reader, template, parent, &key.PublicKey, parentKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath := filepath.Join(dir, name+".crt"), filepath.Join(dir, name+".key")
	writePEM(t, certPath, "CERTIFICATE", der)
	writePEM(t, keyPath, "PRIVATE KEY", keyDER)
	return certPath, keyPath
}

func writePEM(t *testing.T, path, blockType string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}
