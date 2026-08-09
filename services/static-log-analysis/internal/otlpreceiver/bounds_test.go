package otlpreceiver_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/otlpreceiver"
)

// These tests are about what a merely slow or merely numerous caller can cost
// this replica. The static_local trust mode has no client certificate by
// design, so on that socket the bounds below are the only thing between a
// caller and the process.

// gate blocks every export inside the handler until the test releases it, and
// reports each one as it arrives. It is how a test occupies an in-flight bound
// deterministically rather than by racing.
type gate struct {
	admitted chan struct{}
	release  chan struct{}
	once     sync.Once
}

func newGate(capacity int) *gate {
	return &gate{admitted: make(chan struct{}, capacity), release: make(chan struct{})}
}

func (g *gate) enter() {
	g.admitted <- struct{}{}
	<-g.release
}

func (g *gate) open() { g.once.Do(func() { close(g.release) }) }

// awaitAdmitted waits for count exports to be inside the handler.
func (g *gate) awaitAdmitted(t *testing.T, count int) {
	t.Helper()
	for i := 0; i < count; i++ {
		select {
		case <-g.admitted:
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of %d exports reached ingestion", i, count)
		}
	}
}

// admittedSince reports whether any further export slipped past the bound.
func (g *gate) admittedSince() bool {
	select {
	case <-g.admitted:
		return true
	default:
		return false
	}
}

func timedOut(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// shedWithin posts one export and insists on an answer. A receiver that admits
// it instead of shedding it would block on the gate forever, so the deadline is
// what turns "the bound is missing" into a failure rather than a hung test.
func shedWithin(t *testing.T, receiver *otlpreceiver.Receiver, payload []byte, within time.Duration) int {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, "http://"+receiver.HTTPAddr()+otlpreceiver.LogsPath,
		bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/x-protobuf")
	response, err := (&http.Client{Timeout: within}).Do(request)
	if err != nil {
		t.Fatalf("an export that should have been shed was admitted and never answered: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, response.Body)
	return response.StatusCode
}

// TestASlowBodyIsCutOffRatherThanHoldingAHandlerIndefinitely pins the request
// read bound. Without one, a caller that declares a body and then never sends
// it holds a handler, its goroutine, and its buffer for as long as it likes, at
// a cost to the caller of one socket.
func TestASlowBodyIsCutOffRatherThanHoldingAHandlerIndefinitely(t *testing.T) {
	ingester := &fakeIngester{result: acknowledged(1)}
	receiver := startReceiver(t, ingester, func(config *otlpreceiver.Config) {
		config.ReadTimeout = 250 * time.Millisecond
	})

	conn, err := net.DialTimeout("tcp", receiver.HTTPAddr(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	// A well-formed request that promises far more body than it will ever send.
	payload := samplePayload(t)
	header := fmt.Sprintf("POST /v1/logs HTTP/1.1\r\nHost: receiver\r\n"+
		"Content-Type: application/x-protobuf\r\nContent-Length: %d\r\n\r\n", len(payload))
	if _, err := conn.Write([]byte(header)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(payload[:1]); err != nil {
		t.Fatal(err)
	}

	// The server must give up on its own. A refusal or a closed connection is
	// the bound working; only our own deadline firing means it is still waiting.
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Read(make([]byte, 512)); err != nil && timedOut(err) {
		t.Fatal("a handler was still holding an unfinished request body five seconds after a 250ms read bound")
	}

	// A body that never arrived must not have been admitted as if it had.
	if envelopes, _, _ := ingester.calls(); len(envelopes) != 0 {
		t.Fatalf("a truncated body reached ingestion %d times; it must never be admitted", len(envelopes))
	}
}

// TestConcurrentExportsAreBoundedAndTheExcessIsRetryable pins that the number
// of exports admitted at once is bounded, and that a caller turned away is told
// to come back rather than told its batch is bad.
//
// The count matters because each admitted export holds its compressed body, its
// decoded form, and its materialized form at once. Unbounded, this replica's
// memory is whatever its callers choose it to be.
func TestConcurrentExportsAreBoundedAndTheExcessIsRetryable(t *testing.T) {
	const bound = 2
	blocked := newGate(bound + 2)
	ingester := &fakeIngester{result: acknowledged(1), before: blocked.enter}
	receiver := startReceiver(t, ingester, func(config *otlpreceiver.Config) {
		config.MaxInFlight = bound
	})
	t.Cleanup(blocked.open)

	payload := samplePayload(t)
	var running sync.WaitGroup
	for i := 0; i < bound; i++ {
		running.Add(1)
		go func() {
			defer running.Done()
			_, _ = httpExport(t, receiver, payload, "")
		}()
	}
	blocked.awaitAdmitted(t, bound)

	if code := shedWithin(t, receiver, payload, 10*time.Second); code != http.StatusServiceUnavailable {
		t.Fatalf("an export over the in-flight bound answered %d; shedding is retryable (503), never a verdict about the batch", code)
	}
	if blocked.admittedSince() {
		t.Fatal("an export over the in-flight bound was admitted anyway")
	}

	blocked.open()
	running.Wait()
}

// TestGRPCAndHTTPShareOneInFlightBound pins that the two transports are bounded
// together. Two independent bounds would let a caller take twice the memory an
// operator configured, just by splitting its traffic across both.
func TestGRPCAndHTTPShareOneInFlightBound(t *testing.T) {
	blocked := newGate(4)
	ingester := &fakeIngester{result: acknowledged(1), before: blocked.enter}
	receiver := startReceiver(t, ingester, func(config *otlpreceiver.Config) {
		config.MaxInFlight = 1
	})
	t.Cleanup(blocked.open)

	payload := samplePayload(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = grpcExport(t, receiver, payload)
	}()
	blocked.awaitAdmitted(t, 1)

	if code := shedWithin(t, receiver, payload, 10*time.Second); code != http.StatusServiceUnavailable {
		t.Fatalf("an HTTP export answered %d while the only in-flight slot was held over gRPC; the bound must be shared", code)
	}

	blocked.open()
	<-done
}

// TestAPanicInIngestionIsAnsweredRatherThanKillingTheReplica pins that a defect
// under the handler costs one export, not the process.
//
// grpc-go does not recover a handler panic: without an interceptor it unwinds
// through the serving goroutine and takes the whole replica with it, including
// every export in flight and the journal drain behind them.
func TestAPanicInIngestionIsAnsweredRatherThanKillingTheReplica(t *testing.T) {
	var first sync.Once
	ingester := &fakeIngester{result: acknowledged(1), before: func() {
		first.Do(func() { panic("defect under the handler") })
	}}
	receiver := startReceiver(t, ingester)
	payload := samplePayload(t)

	_, err := grpcExport(t, receiver, payload)
	if err == nil {
		t.Fatal("a panicking handler answered success")
	}
	if got := grpcstatus.Code(err); got != codes.Unavailable {
		t.Fatalf("a recovered panic answered %s; a defect is not a verdict about the batch, so it is retryable", got)
	}

	// Still serving is the whole point of recovering.
	if _, err := grpcExport(t, receiver, payload); err != nil {
		t.Fatalf("the receiver stopped serving after a handler panic: %v", err)
	}
}

// TestAPanicOnTheHTTPPathIsAlsoAnsweredAsRetryable keeps the two transports'
// answers identical, which the retry table requires of every other outcome.
func TestAPanicOnTheHTTPPathIsAlsoAnsweredAsRetryable(t *testing.T) {
	var first sync.Once
	ingester := &fakeIngester{result: acknowledged(1), before: func() {
		first.Do(func() { panic("defect under the handler") })
	}}
	receiver := startReceiver(t, ingester)
	payload := samplePayload(t)

	code, _ := httpExport(t, receiver, payload, "")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("a recovered panic answered %d over HTTP; gRPC answers UNAVAILABLE and the two must agree", code)
	}
	if code, _ := httpExport(t, receiver, payload, ""); code != http.StatusOK {
		t.Fatalf("the receiver stopped serving HTTP after a handler panic: %d", code)
	}
}

// TestAnIdleConnectionDoesNotHoldAListenerSlotForever pins the idle bound on
// keep-alive connections, which is what a caller that opens many connections
// and then says nothing would otherwise consume.
func TestAnIdleConnectionDoesNotHoldAListenerSlotForever(t *testing.T) {
	ingester := &fakeIngester{result: acknowledged(1)}
	receiver := startReceiver(t, ingester, func(config *otlpreceiver.Config) {
		config.IdleTimeout = 250 * time.Millisecond
	})

	conn, err := net.DialTimeout("tcp", receiver.HTTPAddr(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	// Complete one request first, so the connection is idle rather than new.
	payload := samplePayload(t)
	header := fmt.Sprintf("POST /v1/logs HTTP/1.1\r\nHost: receiver\r\n"+
		"Content-Type: application/x-protobuf\r\nContent-Length: %d\r\n\r\n", len(payload))
	if _, err := conn.Write(append([]byte(header), payload...)); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 4096)
	if _, err := conn.Read(buffer); err != nil {
		t.Fatalf("the first request on the connection was not answered: %v", err)
	}

	// Now say nothing. The server must close the idle connection on its own,
	// which the client observes as its read ending rather than as a timeout.
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := conn.Read(buffer); err != nil {
			if timedOut(err) {
				t.Fatal("an idle keep-alive connection was still open ten seconds after a 250ms idle bound")
			}
			return
		}
	}
}

// TestBoundsFallBackToDocumentedDefaultsRatherThanZero pins that an operator who
// configures nothing gets the bounds rather than their absence. Zero means "no
// deadline" to net/http, which is the exact state these tests exist to forbid.
func TestBoundsFallBackToDocumentedDefaultsRatherThanZero(t *testing.T) {
	receiver := startReceiver(t, &fakeIngester{result: acknowledged(1)})
	got := receiver.Bounds()
	if got.ReadTimeout <= 0 || got.WriteTimeout <= 0 || got.IdleTimeout <= 0 ||
		got.ReadHeaderTimeout <= 0 || got.MaxInFlight <= 0 || got.MaxHeaderBytes <= 0 ||
		got.MaxConcurrentStreams == 0 {
		t.Fatalf("an unconfigured receiver left a bound unset: %+v", got)
	}
}
