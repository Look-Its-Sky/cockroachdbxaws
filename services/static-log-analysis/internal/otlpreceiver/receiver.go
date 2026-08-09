// Package otlpreceiver is the OTLP ingress. It owns the acknowledgement
// boundary the whole delivery contract rests on: an export is answered
// successfully only after the coordinator reports that the accepted records are
// committed to the journal, and every other outcome is answered with the retry
// semantics that outcome actually has.
//
// Raw OTLP bytes pass through this package opaquely. Decoding, redaction, and
// identity belong to admission and normalization; nothing here reads a payload.
package otlpreceiver

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	collectorlogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	rpcstatus "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	// The Collector's otlp exporter compresses with gzip by default, and grpc-go
	// installs no compressor unless one is linked in. Without this import a
	// default-configured Collector is answered UNIMPLEMENTED, which is permanent,
	// so it drops every batch instead of queueing it. Decompression stays bounded
	// by MaxRecvMsgSize, so this is not a way around the compressed-byte limit.
	_ "google.golang.org/grpc/encoding/gzip"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/admission"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/clock"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/journal"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/pipeline"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/redact"
)

// LogsPath is the OTLP/HTTP path for logs. Only this path exists: a traces or
// metrics export delivered here is a misconfigured Collector, and answering it
// would hide that.
const LogsPath = "/v1/logs"

const (
	protobufContentType = "application/x-protobuf"
	// unsafeMessageReplacement is used if a message this package composed from
	// its own categorical reasons somehow fails policy validation. It is the
	// fail-closed branch, not the normal one.
	unsafeMessageReplacement = "some log records were rejected"
)

var ErrInvalidConfig = errors.New("otlpreceiver: invalid configuration")

// Ingester is the acknowledgement boundary. It is satisfied by
// *pipeline.Service; the interface exists so this package can be exercised
// against every outcome the coordinator can report, including the ones a real
// journal produces only under failure.
type Ingester interface {
	Ingest(context.Context, model.TrustedEnvelope, []byte, admission.Encoding) (pipeline.IngestResult, error)
}

type Config struct {
	// GRPCListen and HTTPListen are host:port. source-adapters.md makes gRPC the
	// primary transport with HTTP a supported fallback; either may be empty to
	// run only the other, but not both.
	GRPCListen string
	HTTPListen string
	Trust      TrustConfig
	Policy     *redact.Policy
	Clock      clock.Clock
	Limits     admission.Limits
	Logger     *slog.Logger
	// ReadHeaderTimeout bounds how long a connection may hold a handler slot
	// before it has even declared what it is sending.
	ReadHeaderTimeout time.Duration
	// ReadTimeout bounds the whole request read, body included. Without it a
	// caller that declares a body and then dribbles it holds a handler, its
	// goroutine, and its buffer for as long as it likes, for the price of one
	// socket. The static_local trust mode has no client certificate by design,
	// so on that socket this is the only thing bounding that cost.
	ReadTimeout time.Duration
	// WriteTimeout bounds writing one response, so a caller that stops reading
	// cannot pin a handler on a blocked write.
	WriteTimeout time.Duration
	// IdleTimeout bounds a keep-alive connection between requests.
	IdleTimeout time.Duration
	// MaxHeaderBytes bounds the header block, which is read before any of this
	// service's own limits can apply.
	MaxHeaderBytes int
	// MaxInFlight bounds exports admitted at once across BOTH transports. Each
	// admitted export holds its compressed body, its decoded form, and its
	// materialized form at the same time, so this is the multiplier on
	// admission's byte limits that decides the replica's peak memory. Two
	// independent bounds would let a caller take twice what was configured by
	// splitting traffic across the transports, so there is one.
	MaxInFlight int
	// MaxConcurrentStreams bounds concurrent gRPC streams on one connection.
	// grpc-go's own default is math.MaxUint32, which is no bound at all.
	MaxConcurrentStreams uint32
}

// Bounds are the resolved limits a receiver is actually running with. It is
// exported so an operator, and a test, can see that an unconfigured receiver
// got the documented defaults rather than the zero values, which net/http reads
// as "no deadline".
type Bounds struct {
	ReadHeaderTimeout    time.Duration
	ReadTimeout          time.Duration
	WriteTimeout         time.Duration
	IdleTimeout          time.Duration
	MaxHeaderBytes       int
	MaxInFlight          int
	MaxConcurrentStreams uint32
}

// The defaults are deliberately generous enough for a healthy Collector and
// nowhere near generous enough to be a resource for an attacker.
const (
	defaultReadHeaderTimeout = 10 * time.Second
	// A 4 MiB compressed body over a slow but real link fits comfortably; a
	// body that has not finished in thirty seconds is not a batch this replica
	// is losing anything by refusing.
	defaultReadTimeout    = 30 * time.Second
	defaultWriteTimeout   = 30 * time.Second
	defaultIdleTimeout    = 60 * time.Second
	defaultMaxHeaderBytes = 64 << 10
	// With admission's defaults each in-flight export can hold roughly 4 MiB
	// compressed plus 16 MiB decoded plus 16 MiB materialized, so 64 bounds the
	// receiver's peak at something an operator can size a container against
	// rather than at whatever its callers choose.
	defaultMaxInFlight          = 64
	defaultMaxConcurrentStreams = 256
	// gRPC connection establishment, including the TLS handshake.
	defaultConnectionTimeout = 20 * time.Second
	// A client that pings more often than this is refused, so keepalive pings
	// cannot themselves become the flood.
	minimumClientKeepalive = 30 * time.Second
)

type Receiver struct {
	config        Config
	ingester      Ingester
	authenticator Authenticator
	logger        *slog.Logger

	grpcServer   *grpc.Server
	httpServer   *http.Server
	grpcListener net.Listener
	httpListener net.Listener

	// slots is the shared in-flight bound. A receive is an admission and a send
	// is a release, so the two transports draw from one pool.
	slots chan struct{}

	mu      sync.Mutex
	started bool
	serving sync.WaitGroup
	failed  chan error
	collectorlogs.UnimplementedLogsServiceServer
}

func New(config Config, ingester Ingester) (*Receiver, error) {
	if ingester == nil {
		return nil, fmt.Errorf("%w: no coordinator to ingest into", ErrInvalidConfig)
	}
	if config.Policy == nil {
		return nil, fmt.Errorf("%w: a redaction policy is required to validate returned messages", ErrInvalidConfig)
	}
	if config.Clock == nil {
		return nil, fmt.Errorf("%w: a clock is required to stamp received_at", ErrInvalidConfig)
	}
	if strings.TrimSpace(config.GRPCListen) == "" && strings.TrimSpace(config.HTTPListen) == "" {
		return nil, fmt.Errorf("%w: a receiver with no listener would look healthy and receive nothing", ErrInvalidConfig)
	}
	if err := config.Limits.Validate(); err != nil {
		return nil, fmt.Errorf("%w: admission limits: %v", ErrInvalidConfig, err)
	}
	config.Limits = withLimitDefaults(config.Limits)
	authenticator, err := NewAuthenticator(config.Trust)
	if err != nil {
		return nil, err
	}
	config = withBoundDefaults(config)
	logger := config.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return &Receiver{config: config, ingester: ingester, authenticator: authenticator, logger: logger,
		slots: make(chan struct{}, config.MaxInFlight), failed: make(chan error, 2)}, nil
}

// withBoundDefaults resolves every unset bound. A zero deadline means "wait
// forever" to net/http and an unbounded stream count to grpc-go, so leaving one
// unset is not a smaller bound but no bound at all.
func withBoundDefaults(config Config) Config {
	if config.ReadHeaderTimeout <= 0 {
		config.ReadHeaderTimeout = defaultReadHeaderTimeout
	}
	if config.ReadTimeout <= 0 {
		config.ReadTimeout = defaultReadTimeout
	}
	if config.WriteTimeout <= 0 {
		config.WriteTimeout = defaultWriteTimeout
	}
	if config.IdleTimeout <= 0 {
		config.IdleTimeout = defaultIdleTimeout
	}
	if config.MaxHeaderBytes <= 0 {
		config.MaxHeaderBytes = defaultMaxHeaderBytes
	}
	if config.MaxInFlight <= 0 {
		config.MaxInFlight = defaultMaxInFlight
	}
	if config.MaxConcurrentStreams == 0 {
		config.MaxConcurrentStreams = defaultMaxConcurrentStreams
	}
	return config
}

// Bounds reports the limits this receiver resolved to.
func (r *Receiver) Bounds() Bounds {
	return Bounds{
		ReadHeaderTimeout: r.config.ReadHeaderTimeout, ReadTimeout: r.config.ReadTimeout,
		WriteTimeout: r.config.WriteTimeout, IdleTimeout: r.config.IdleTimeout,
		MaxHeaderBytes: r.config.MaxHeaderBytes, MaxInFlight: r.config.MaxInFlight,
		MaxConcurrentStreams: r.config.MaxConcurrentStreams,
	}
}

// acquire takes one of the shared in-flight slots. It never waits: shedding is
// the point, and a caller made to queue would hold the socket and the memory
// this bound exists to cap. The refusal is retryable, so the Collector's
// persistent queue holds the batch instead of this replica's heap.
func (r *Receiver) acquire() (release func(), ok bool) {
	select {
	case r.slots <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-r.slots }) }, true
	default:
		return nil, false
	}
}

// withLimitDefaults resolves the "unset means default" convention admission
// uses. The transport enforces the compressed bound before admission ever runs,
// and there an unset limit would mean zero bytes rather than the default.
func withLimitDefaults(limits admission.Limits) admission.Limits {
	if limits.MaxCompressedBytes == 0 {
		limits.MaxCompressedBytes = admission.DefaultMaxCompressedBytes
	}
	if limits.MaxUncompressedBytes == 0 {
		limits.MaxUncompressedBytes = admission.DefaultMaxUncompressedBytes
	}
	return limits
}

// Start binds both listeners and begins serving. Binding happens here rather
// than inside the serving goroutines so a port conflict is a startup error an
// operator sees, not a background failure after the process looks healthy.
func (r *Receiver) Start() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started {
		return fmt.Errorf("%w: receiver already started", ErrInvalidConfig)
	}
	tlsConfig, err := r.serverTLS()
	if err != nil {
		return err
	}
	if err := r.startGRPC(tlsConfig); err != nil {
		return err
	}
	if err := r.startHTTP(tlsConfig); err != nil {
		r.closeListeners()
		return err
	}
	r.started = true
	return nil
}

func (r *Receiver) startGRPC(tlsConfig *tls.Config) error {
	if strings.TrimSpace(r.config.GRPCListen) == "" {
		return nil
	}
	listener, err := net.Listen("tcp", r.config.GRPCListen)
	if err != nil {
		return fmt.Errorf("otlpreceiver: listen gRPC on %s: %w", r.config.GRPCListen, err)
	}
	options := []grpc.ServerOption{
		// The compressed-request limit is the transport's own bound. Admission
		// enforces it again on the decoded payload; this one keeps an oversized
		// frame from being buffered at all.
		grpc.MaxRecvMsgSize(int(r.config.Limits.MaxCompressedBytes)),
		// grpc-go's default is math.MaxUint32 streams per connection, so one
		// connection could hold as many decoded requests as it liked.
		grpc.MaxConcurrentStreams(r.config.MaxConcurrentStreams),
		grpc.ConnectionTimeout(defaultConnectionTimeout),
		grpc.KeepaliveParams(keepalive.ServerParameters{MaxConnectionIdle: r.config.IdleTimeout}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime: minimumClientKeepalive, PermitWithoutStream: false,
		}),
		// grpc-go does not recover a handler panic: it unwinds through the
		// serving goroutine and takes the whole replica with it, along with
		// every export in flight and the journal drain behind them.
		grpc.ChainUnaryInterceptor(r.recoverUnary),
	}
	if tlsConfig != nil {
		options = append(options, grpc.Creds(credentials.NewTLS(tlsConfig)))
	}
	server := grpc.NewServer(options...)
	collectorlogs.RegisterLogsServiceServer(server, r)
	r.grpcServer, r.grpcListener = server, listener
	r.serving.Add(1)
	go func() {
		defer r.serving.Done()
		if err := server.Serve(listener); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			r.fail(fmt.Errorf("otlpreceiver: gRPC serve: %w", err))
		}
	}()
	return nil
}

func (r *Receiver) startHTTP(tlsConfig *tls.Config) error {
	if strings.TrimSpace(r.config.HTTPListen) == "" {
		return nil
	}
	listener, err := net.Listen("tcp", r.config.HTTPListen)
	if err != nil {
		return fmt.Errorf("otlpreceiver: listen HTTP on %s: %w", r.config.HTTPListen, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc(LogsPath, r.serveHTTPLogs)
	server := &http.Server{
		Handler:           r.recoverHTTP(mux),
		ReadHeaderTimeout: r.config.ReadHeaderTimeout,
		ReadTimeout:       r.config.ReadTimeout,
		WriteTimeout:      r.config.WriteTimeout,
		IdleTimeout:       r.config.IdleTimeout,
		MaxHeaderBytes:    r.config.MaxHeaderBytes,
		TLSConfig:         tlsConfig,
	}
	r.httpServer, r.httpListener = server, listener
	r.serving.Add(1)
	go func() {
		defer r.serving.Done()
		var err error
		if tlsConfig != nil {
			err = server.ServeTLS(listener, "", "")
		} else {
			err = server.Serve(listener)
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			r.fail(fmt.Errorf("otlpreceiver: HTTP serve: %w", err))
		}
	}()
	return nil
}

// serverTLS builds the listener credentials mutual_tls needs. A receiver whose
// authenticator requires a client certificate must never be able to serve a
// plaintext listener, so this is derived from the authenticator rather than
// configured beside it.
func (r *Receiver) serverTLS() (*tls.Config, error) {
	if !r.authenticator.RequiresClientCertificate() {
		return nil, nil
	}
	// Missing or unreadable material is a deployment fault, not a transient one:
	// no retry of this process turns an absent certificate into a present one,
	// so it carries the configuration sentinel the runtime maps to its
	// configuration exit status.
	certificate, err := tls.LoadX509KeyPair(r.config.Trust.TLS.CertificateFile, r.config.Trust.TLS.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("%w: load server certificate: %v", ErrInvalidConfig, err)
	}
	pemBytes, err := os.ReadFile(r.config.Trust.TLS.ClientCAFile)
	if err != nil {
		return nil, fmt.Errorf("%w: read client CA: %v", ErrInvalidConfig, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("%w: client CA file contains no certificate", ErrInvalidConfig)
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{certificate},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		NextProtos:   []string{"h2", "http/1.1"},
	}, nil
}

// recoverUnary keeps a defect under the handler from costing more than the one
// export it happened under. The answer is retryable because a panic is a defect
// in this service, not a verdict about the caller's batch: reporting it
// permanent would discard acknowledged-mandatory data on the strength of a bug.
func (r *Receiver) recoverUnary(ctx context.Context, request any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (response any, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			r.logPanic(recovered)
			response, err = nil, status.Error(codes.Unavailable, "ingestion is temporarily unavailable")
		}
	}()
	return handler(ctx, request)
}

// recoverHTTP does the same for the HTTP path. net/http recovers a handler
// panic on its own, but it answers nothing, so a Collector would see a dropped
// connection rather than the retryable status the mapping table promises.
func (r *Receiver) recoverHTTP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		tracked := &trackedWriter{ResponseWriter: writer}
		defer func() {
			recovered := recover()
			if recovered == nil {
				return
			}
			if failure, ok := recovered.(error); ok && errors.Is(failure, http.ErrAbortHandler) {
				// The documented way for a handler to abandon a connection
				// deliberately. Passing it on is what net/http expects.
				panic(recovered)
			}
			r.logPanic(recovered)
			if !tracked.wrote {
				r.writeHTTPFailure(tracked, http.StatusServiceUnavailable, codes.Unavailable,
					"ingestion is temporarily unavailable")
			}
		}()
		next.ServeHTTP(tracked, request)
	})
}

func (r *Receiver) logPanic(recovered any) {
	r.logger.Error("recovered a panic under an export handler",
		slog.Any("panic", recovered), slog.String("stack", string(debug.Stack())))
}

// trackedWriter remembers whether anything was already sent, so the recovery
// path does not try to write a second set of headers over a partial response.
type trackedWriter struct {
	http.ResponseWriter
	wrote bool
}

func (w *trackedWriter) WriteHeader(status int) {
	w.wrote = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *trackedWriter) Write(body []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(body)
}

func (r *Receiver) fail(err error) {
	select {
	case r.failed <- err:
	default:
	}
}

// Failed reports a listener that stopped serving for a reason other than an
// orderly shutdown. A supervisor selects on it so a half-dead process exits
// instead of accepting nothing while looking healthy.
func (r *Receiver) Failed() <-chan error { return r.failed }

func (r *Receiver) GRPCAddr() string { return listenerAddr(r.grpcListener) }
func (r *Receiver) HTTPAddr() string { return listenerAddr(r.httpListener) }

func listenerAddr(listener net.Listener) string {
	if listener == nil {
		return ""
	}
	return listener.Addr().String()
}

// Shutdown stops accepting new exports and lets admitted handlers finish. Every
// admitted handler is inside Ingest, so draining is draining through journal
// commit exactly as operations.md requires. If the deadline passes first the
// stop becomes forced, which stays safe: whatever was acknowledged was already
// synchronized before its response was written.
func (r *Receiver) Shutdown(ctx context.Context) error {
	r.mu.Lock()
	grpcServer, httpServer := r.grpcServer, r.httpServer
	r.mu.Unlock()

	graceful := make(chan struct{})
	go func() {
		defer close(graceful)
		if grpcServer != nil {
			grpcServer.GracefulStop()
		}
	}()
	var httpErr error
	if httpServer != nil {
		httpErr = httpServer.Shutdown(ctx)
	}
	select {
	case <-graceful:
	case <-ctx.Done():
		if grpcServer != nil {
			grpcServer.Stop()
		}
		<-graceful
		httpErr = errors.Join(httpErr, ctx.Err())
	}
	if httpServer != nil {
		_ = httpServer.Close()
	}
	r.serving.Wait()
	return httpErr
}

func (r *Receiver) closeListeners() {
	if r.grpcListener != nil {
		_ = r.grpcListener.Close()
	}
	if r.httpListener != nil {
		_ = r.httpListener.Close()
	}
}

// Export is the OTLP/gRPC entry point. The request is re-marshaled rather than
// handed on as a decoded message: admission owns bounded decoding, including
// the nesting and structural-node limits that a generated decoder has already
// spent before it could enforce them.
func (r *Receiver) Export(ctx context.Context, request *collectorlogs.ExportLogsServiceRequest) (*collectorlogs.ExportLogsServiceResponse, error) {
	// Authenticate first, so an unauthenticated caller cannot occupy an
	// in-flight slot, then take the slot before re-marshaling, because the
	// payload that allocates is what the slot is accounting for. The HTTP path
	// takes the same two steps in the same order.
	envelope, err := r.authenticator.Envelope(grpcPeer(ctx), r.config.Clock.Now())
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, "caller is not authenticated")
	}
	release, ok := r.acquire()
	if !ok {
		return nil, status.Error(codes.Unavailable, "receiver is at its in-flight export limit")
	}
	defer release()
	payload, err := proto.Marshal(request)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "request could not be re-encoded for bounded admission")
	}
	response, outcome := r.admit(ctx, envelope, payload, admission.EncodingIdentity)
	if outcome != nil {
		return nil, status.Error(outcome.code, outcome.message)
	}
	return response, nil
}

func grpcPeer(ctx context.Context) Peer {
	value, ok := peer.FromContext(ctx)
	if !ok {
		return Peer{}
	}
	result := Peer{RemoteAddr: value.Addr.String()}
	if info, ok := value.AuthInfo.(credentials.TLSInfo); ok {
		state := info.State
		result.TLS = &state
	}
	return result
}

func (r *Receiver) serveHTTPLogs(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		r.writeHTTPFailure(writer, http.StatusMethodNotAllowed, codes.InvalidArgument, "only POST is accepted")
		return
	}
	if mediaType, _, _ := strings.Cut(request.Header.Get("Content-Type"), ";"); strings.TrimSpace(mediaType) != protobufContentType {
		// JSON OTLP is a valid protocol variant this service does not implement.
		// Answering 415 says so, instead of failing later as if the batch were
		// malformed and inviting a retry that cannot succeed.
		r.writeHTTPFailure(writer, http.StatusUnsupportedMediaType, codes.InvalidArgument, "only "+protobufContentType+" is accepted")
		return
	}
	encoding, ok := requestEncoding(request.Header.Get("Content-Encoding"))
	if !ok {
		r.writeHTTPFailure(writer, http.StatusUnsupportedMediaType, codes.InvalidArgument, "unsupported content encoding")
		return
	}
	envelope, err := r.authenticator.Envelope(Peer{TLS: request.TLS, RemoteAddr: request.RemoteAddr}, r.config.Clock.Now())
	if err != nil {
		r.writeHTTPFailure(writer, http.StatusUnauthorized, codes.Unauthenticated, "caller is not authenticated")
		return
	}
	// The slot is taken before the body is read, not after: the buffer the read
	// accumulates is most of what this bound exists to cap, so acquiring
	// afterwards would bound only the cheap part.
	release, ok := r.acquire()
	if !ok {
		r.writeHTTPFailure(writer, http.StatusServiceUnavailable, codes.Unavailable,
			"receiver is at its in-flight export limit")
		return
	}
	defer release()
	// The limit is enforced while reading, so an oversized body never becomes an
	// oversized allocation. It is permanent: the same body is the same size on
	// every retry.
	payload, err := readBounded(request, r.config.Limits.MaxCompressedBytes)
	if err != nil {
		r.writeHTTPFailure(writer, http.StatusBadRequest, codes.InvalidArgument, "request body exceeds the configured limit or ended early")
		return
	}
	response, outcome := r.admit(request.Context(), envelope, payload, encoding)
	if outcome != nil {
		r.writeHTTPFailure(writer, outcome.httpStatus, outcome.code, outcome.message)
		return
	}
	body, err := proto.Marshal(response)
	if err != nil {
		r.writeHTTPFailure(writer, http.StatusInternalServerError, codes.Internal, "response could not be encoded")
		return
	}
	writer.Header().Set("Content-Type", protobufContentType)
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(body)
}

func readBounded(request *http.Request, maximum int64) ([]byte, error) {
	body := http.MaxBytesReader(nil, request.Body, maximum)
	defer func() { _ = body.Close() }()
	buffer := make([]byte, 0, 4096)
	chunk := make([]byte, 32*1024)
	for {
		n, err := body.Read(chunk)
		buffer = append(buffer, chunk[:n]...)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return buffer, nil
			}
			return nil, err
		}
	}
}

func requestEncoding(header string) (admission.Encoding, bool) {
	switch strings.TrimSpace(strings.ToLower(header)) {
	case "":
		return admission.EncodingIdentity, true
	case "identity":
		return admission.EncodingIdentity, true
	case "gzip":
		return admission.EncodingGZIP, true
	default:
		return "", false
	}
}

// outcome is one export failure carrying both transports' representations of
// the same retry semantics, so the two can never drift apart.
type outcome struct {
	code       codes.Code
	httpStatus int
	message    string
}

var (
	// retryable means the batch may succeed unchanged later. The Collector's
	// persistent queue holds it in the meantime.
	retryableOutcome = func(message string) *outcome {
		return &outcome{code: codes.Unavailable, httpStatus: http.StatusServiceUnavailable, message: message}
	}
	// permanent means no retry of this batch can succeed. Reporting it as
	// retryable would make a Collector replay a poisoned batch forever and
	// starve the valid batches queued behind it.
	permanentOutcome = func(message string) *outcome {
		return &outcome{code: codes.InvalidArgument, httpStatus: http.StatusBadRequest, message: message}
	}
)

// admit is the single place both transports decide what an ingest result means.
func (r *Receiver) admit(ctx context.Context, envelope model.TrustedEnvelope, payload []byte, encoding admission.Encoding) (*collectorlogs.ExportLogsServiceResponse, *outcome) {
	result, err := r.ingester.Ingest(ctx, envelope, payload, encoding)
	if err != nil {
		return nil, r.classify(err)
	}
	if !result.Acknowledged {
		// The coordinator did not report a journal commit. Whatever it holds is
		// in memory only, and acknowledging it here would be the one loss the
		// delivery contract does not permit.
		r.logger.Error("ingest returned no error and no acknowledgement", slog.Int("accepted", result.Accepted))
		return nil, retryableOutcome("batch was not durably accepted")
	}
	response := &collectorlogs.ExportLogsServiceResponse{}
	if len(result.Rejected) > 0 {
		// architecture.md: permanently malformed records must not make an
		// otherwise valid batch retry forever. They are reported as partial
		// success so the Collector drops exactly them and keeps the rest.
		response.PartialSuccess = &collectorlogs.ExportLogsPartialSuccess{
			RejectedLogRecords: int64(len(result.Rejected)),
			ErrorMessage:       r.rejectionMessage(result.Rejected),
		}
	}
	return response, nil
}

func (r *Receiver) classify(err error) *outcome {
	switch {
	case errors.Is(err, pipeline.ErrScopeNotPermitted):
		return permanentOutcome("envelope region is not served by this replica")
	case errors.Is(err, pipeline.ErrJournalRejected):
		return permanentOutcome("journal permanently refused the batch")
	case errors.Is(err, journal.ErrDuplicateConflict):
		// The same record identity arrived with different content. Retrying
		// replays the same contradiction.
		return permanentOutcome("batch conflicts with a previously accepted record identity")
	case errors.Is(err, pipeline.ErrRequestRejected):
		return permanentOutcome("request is not admissible")
	case errors.Is(err, pipeline.ErrJournalUnavailable):
		return retryableOutcome("journal is unavailable")
	default:
		// An unclassified failure is a defect, not a verdict about the batch.
		// Calling it permanent would discard data on the strength of a bug, so
		// it is retryable and loud.
		r.logger.Error("unclassified ingest failure", slog.String("error", err.Error()))
		return retryableOutcome("ingestion is temporarily unavailable")
	}
}

// rejectionMessage summarizes rejections using only the categorical reasons
// this service defines. It never reads record content, and the composed result
// is put through the same policy that guards persistence before it is returned.
func (r *Receiver) rejectionMessage(rejections []pipeline.RecordRejection) string {
	counts := map[pipeline.RejectionReason]int{}
	for _, rejection := range rejections {
		counts[rejection.Reason]++
	}
	reasons := make([]string, 0, len(counts))
	for reason := range counts {
		reasons = append(reasons, string(reason))
	}
	sort.Strings(reasons)
	parts := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		parts = append(parts, fmt.Sprintf("%s=%d", reason, counts[pipeline.RejectionReason(reason)]))
	}
	message := fmt.Sprintf("%d log records rejected: %s", len(rejections), strings.Join(parts, " "))
	if err := r.config.Policy.ValidateText(message); err != nil {
		r.logger.Error("composed rejection message failed policy validation")
		return unsafeMessageReplacement
	}
	return message
}

// writeHTTPFailure answers with the google.rpc.Status body the OTLP/HTTP
// contract defines, so a Collector reads the same categorical failure it would
// have received over gRPC.
func (r *Receiver) writeHTTPFailure(writer http.ResponseWriter, httpStatus int, code codes.Code, message string) {
	body, err := proto.Marshal(&rpcstatus.Status{Code: int32(code), Message: message})
	if err != nil {
		http.Error(writer, "", httpStatus)
		return
	}
	writer.Header().Set("Content-Type", protobufContentType)
	writer.WriteHeader(httpStatus)
	_, _ = writer.Write(body)
}
