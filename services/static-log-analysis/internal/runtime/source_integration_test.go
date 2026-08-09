//go:build integration

package runtime_test

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	cwltypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/clock"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/runtime"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/localstacktest"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/testids"
)

// testWriter routes the replica's own logs into the test output.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", p)
	return len(p), nil
}

// TestTheSourceRoleReadsARealLogGroupIntoItsJournal is the end-to-end evidence
// that a deployed replica actually retrieves logs: the real role, the real
// adapter, a real CloudWatch Logs API, and a real Pebble journal, with no
// double anywhere in the path.
//
// Everything narrower - the adapter's windowing, the checkpoint store, the sink
// conversion - is covered in its own package. What only this test shows is that
// a replica started from a command line does the whole thing.
func TestTheSourceRoleReadsARealLogGroupIntoItsJournal(t *testing.T) {
	admin := localstacktest.Admin(t)
	group := localstacktest.LogGroup(t, admin)
	const stream = "ecs/paymentservice/0e1f2a3b"
	localstacktest.LogStream(t, admin, group, stream)

	// A local endpoint has no instance role, so the AWS default credential chain
	// has nothing to find and every read fails on identity rather than on
	// anything this service did. Supplying the standard environment variables is
	// how a local or non-AWS deployment is expected to provide them; LocalStack
	// accepts any well-formed pair.
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")

	now := time.Now().UTC()
	events := make([]cwltypes.InputLogEvent, 0, 4)
	for i := 0; i < 4; i++ {
		events = append(events, cwltypes.InputLogEvent{
			Timestamp: awssdk.Int64(now.Add(-time.Duration(4-i) * time.Second).UnixMilli()),
			Message:   awssdk.String(`{"level":"ERROR","message":"payment declined"}`),
		})
	}
	if _, err := admin.PutLogEvents(context.Background(), &cloudwatchlogs.PutLogEventsInput{
		LogGroupName: awssdk.String(group), LogStreamName: awssdk.String(stream), LogEvents: events,
	}); err != nil {
		t.Fatal(err)
	}

	args := []string{"source",
		"-region=" + localstacktest.Region, "-tenant-id=tenant-a", "-classification=SENSITIVE",
		"-journal-dir=" + filepath.Join(t.TempDir(), "journal"), "-journal-max-bytes=67108864",
		"-cloudwatch-account=" + localstacktest.Account,
		"-cloudwatch-log-groups=" + group + "=paymentservice=production",
		"-cloudwatch-source-instance=cw-adapter-a",
		"-cloudwatch-credential-identity=workload-a",
		"-cloudwatch-checkpoint-dir=" + filepath.Join(t.TempDir(), "checkpoints"),
		"-cloudwatch-endpoint-url=" + localstacktest.Endpoint(t),
		"-cloudwatch-interval=20ms", "-cloudwatch-idle-interval=20ms",
		"-cloudwatch-backoff-min=20ms", "-cloudwatch-backoff-max=40ms",
		"-admin-listen=127.0.0.1:0",
	}
	config, err := runtime.Parse(args, func(string) string { return "" }, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	// The poller's failures are logged, not returned, so a discarded logger
	// would turn "read nothing" into a mystery. This is the only place they can
	// be seen.
	deps := runtime.Deps{Clock: clock.System(), IDs: testids.New(),
		Logger: slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))}

	server, err := runtime.Start(context.Background(), config, deps)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	})

	// A source replica binds no listener: nothing pushes to it.
	if server.GRPCAddr() != "" || server.HTTPAddr() != "" {
		t.Fatalf("the source role opened %q and %q", server.GRPCAddr(), server.HTTPAddr())
	}

	// The poller is on its own cadence, so the assertion waits for the durable
	// result rather than for a number of cycles.
	deadline := time.Now().Add(60 * time.Second)
	for {
		stats, err := server.JournalStats()
		if err == nil && stats.Pending >= 4 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the source role never wrote the four events to its journal (stats=%+v err=%v)", stats, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
