//go:build integration

package runtime_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	collectorlogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/clock"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/persistence"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/runtime"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/crdbtest"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/otlpgen"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/testids"
)

const forbiddenValue = "customer-secret"

// ingressArgs is the one configuration both phases share. The journal manifest
// is immutable, so the process replica that drains this volume must agree with
// the ingest replica that filled it.
func ingressArgs(role, dir string) []string {
	return []string{role,
		"-region=" + otlpgen.DefaultRegion, "-tenant-id=tenant-a", "-classification=SENSITIVE",
		"-journal-dir=" + dir, "-journal-max-bytes=67108864",
		"-redaction-forbidden-values=" + forbiddenValue,
		"-otlp-grpc-listen=127.0.0.1:0", "-otlp-http-listen=127.0.0.1:0",
		"-admin-listen=127.0.0.1:0",
		"-otlp-trust-source=static_local", "-otlp-source-account=aws-account-a",
		"-otlp-source-instance=collector-a", "-otlp-credential-identity=workload-a",
		"-otlp-allowed-environments=" + otlpgen.DefaultEnvironment, "-otlp-allowed-services=" + otlpgen.DefaultService,
		"-process-interval=50ms", "-process-idle-interval=50ms",
		"-process-backoff-min=100ms", "-process-backoff-max=500ms",
	}
}

// architecture.md: "A record held only in memory must never be acknowledged."
// The only way to observe that from outside is to remove everything that is not
// already on disk. The child exports over both transports, prints its
// acknowledgement, and then dies without draining or closing anything, so what
// the parent finds in the journal directory is exactly what was synchronized
// before each response was written.
func TestOTLPExportOverGRPCAndHTTPIsDurableBeforeTheResponseIsObserved(t *testing.T) {
	if os.Getenv("RUNTIME_ACK_HELPER") == "1" {
		t.Skip("helper is selected by its dedicated test name")
	}
	ctx := context.Background()
	pool := crdbtest.Pool(t)
	if err := persistence.ApplyMigrations(ctx, pool, persistence.TopologySingleRegion); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "journal")

	cmd := exec.Command(os.Args[0], "-test.run=^TestRuntimeAcknowledgementHelper$")
	cmd.Env = append(os.Environ(), "RUNTIME_ACK_HELPER=1", "RUNTIME_ACK_DIR="+dir)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper: %v: %s", err, output)
	}
	if !strings.Contains(string(output), "ACK grpc\n") || !strings.Contains(string(output), "ACK http\n") {
		t.Fatalf("the helper did not acknowledge both transports: %s", output)
	}

	// Nothing ran after the responses were observed. Both records are here or
	// they were acknowledged while held only in memory.
	durable := verifyJournal(t, dir)
	stats, err := durable.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Pending != 2 || stats.Committed != 0 {
		t.Fatalf("the acknowledged records were not durable before the response: %+v", stats)
	}
	if err := durable.Close(); err != nil {
		t.Fatal(err)
	}

	// The same volume now drains through a process replica, which is the other
	// half of the runnable service.
	config, err := runtime.Parse(ingressArgs("process", dir), func(string) string { return pool.Config().ConnString() }, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	server, err := runtime.Start(ctx, config, runtime.Deps{Clock: clock.System(), IDs: testids.New(),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	waitForOccurrences(t, ctx, pool, 2)
	shutdownCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	var occurrences, investigations int
	var bodies string
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM occurrences),
		(SELECT count(*) FROM investigations), (SELECT coalesce(string_agg(safe_summary::STRING, ' '), '') FROM occurrences)`).
		Scan(&occurrences, &investigations, &bodies); err != nil {
		t.Fatal(err)
	}
	if occurrences != 2 {
		t.Fatalf("%d occurrences reached CockroachDB", occurrences)
	}
	// Two ordinary errors are below the five-in-five threshold.
	if investigations != 0 {
		t.Fatalf("%d investigations were launched below the rule threshold", investigations)
	}
	if strings.Contains(bodies, forbiddenValue) {
		t.Fatal("a configured forbidden value crossed the persistence boundary")
	}
}

// TestRuntimeAcknowledgementHelper is the child process of the test above. It
// runs the real server, exports with real OTLP clients, and exits abruptly.
func TestRuntimeAcknowledgementHelper(t *testing.T) {
	if os.Getenv("RUNTIME_ACK_HELPER") != "1" {
		return
	}
	config, err := runtime.Parse(ingressArgs("ingest", os.Getenv("RUNTIME_ACK_DIR")), func(string) string { return "" }, io.Discard)
	if err != nil {
		fmt.Println("config:", err)
		os.Exit(20)
	}
	server, err := runtime.Start(context.Background(), config, runtime.Deps{Clock: clock.System(), IDs: testids.New(),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		fmt.Println("start:", err)
		os.Exit(21)
	}
	producer := otlpgen.New(otlpgen.WithIDs(testids.New()))
	// Event time is already outside the allowed-lateness horizon, so the
	// records finalize on the process replica's first cycle instead of making
	// the test wait out two minutes of wall clock.
	settled := uint64(time.Now().Add(-5 * time.Minute).UnixNano())

	grpcRecord := producer.PaymentError(otlpgen.WithBody("payment declined "+forbiddenValue), otlpgen.AtEventTime(settled))
	grpcRequest := producer.Request(grpcRecord)
	conn, err := grpc.NewClient(server.GRPCAddr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fmt.Println("dial:", err)
		os.Exit(22)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	response, err := collectorlogs.NewLogsServiceClient(conn).Export(ctx, grpcRequest)
	if err != nil || response.GetPartialSuccess().GetRejectedLogRecords() != 0 {
		fmt.Println("grpc export:", err, response.GetPartialSuccess())
		os.Exit(23)
	}
	fmt.Println("ACK grpc")

	httpRecord := producer.PaymentError(otlpgen.WithBody("payment declined again"), otlpgen.AtEventTime(settled))
	payload, err := proto.Marshal(producer.Request(httpRecord))
	if err != nil {
		os.Exit(24)
	}
	request, err := http.NewRequest(http.MethodPost, "http://"+server.HTTPAddr()+"/v1/logs", bytes.NewReader(payload))
	if err != nil {
		os.Exit(25)
	}
	request.Header.Set("Content-Type", "application/x-protobuf")
	httpResponse, err := http.DefaultClient.Do(request)
	if err != nil || httpResponse.StatusCode != http.StatusOK {
		fmt.Println("http export:", err)
		os.Exit(26)
	}
	_ = httpResponse.Body.Close()
	fmt.Println("ACK http")

	// No Shutdown, no journal close, no flush of anything. Whatever survives
	// was synchronized before its own response was written.
	os.Exit(0)
}

func waitForOccurrences(t *testing.T, ctx context.Context, pool *pgxpool.Pool, want int) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for {
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM occurrences`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the process worker persisted %d of %d records before the deadline", count, want)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
