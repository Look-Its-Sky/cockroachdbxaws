package main

import (
	"context"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/clock"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/journal"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/pipeline"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/redact"
)

func silentLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func noEnv(string) string { return "" }

// ingestArgs is a complete ingest configuration. Each case below corrupts
// exactly one thing, so the exit status it asserts is the only difference from
// a replica that would have started.
func ingestArgs(dir string, overrides ...string) []string {
	args := []string{"ingest",
		"-region=us-east-1", "-tenant-id=tenant-a", "-classification=SENSITIVE",
		"-journal-dir=" + dir, "-journal-max-bytes=67108864",
		"-otlp-grpc-listen=127.0.0.1:0", "-otlp-http-listen=127.0.0.1:0",
		"-otlp-trust-source=static_local", "-otlp-source-account=aws-account-a",
		"-otlp-source-instance=collector-a", "-otlp-credential-identity=workload-a",
		"-otlp-allowed-environments=production", "-otlp-allowed-services=paymentservice",
	}
	return append(args, overrides...)
}

// seedJournal creates a journal volume whose immutable manifest is whatever a
// misdeployed replica would find on the disk it was pointed at.
func seedJournal(t *testing.T, dir, region, classification string) {
	t.Helper()
	opened, err := journal.Open(journal.Config{Dir: dir, Owner: pipeline.DefaultWorkerOwner, TenantID: "tenant-a",
		Region: region, Classification: classification, Clock: clock.System(), Validator: redact.MinimalPolicy(),
		MaxBytes: 1 << 26, MinFreeBytes: 1, FreeSpace: func(string) (uint64, error) { return math.MaxUint64, nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
}

// runtime.md: configuration failures exit 2 and runtime failures exit 1, "so a
// supervisor does not restart-loop a process whose flags can never work". Every
// fault here is one an operator must change something to fix, and none of them
// is reported by the sentinel a flag-local error is reported by.
func TestConfigurationFaultsExitWithTheConfigurationStatus(t *testing.T) {
	cases := map[string]func(t *testing.T) []string{
		"missing region": func(t *testing.T) []string {
			args := ingestArgs(filepath.Join(t.TempDir(), "journal"))
			args[1] = "-region="
			return args
		},
		"journal volume belongs to another region": func(t *testing.T) []string {
			dir := filepath.Join(t.TempDir(), "journal")
			seedJournal(t, dir, "eu-central-1", "SENSITIVE")
			return ingestArgs(dir)
		},
		"journal volume was written under another classification": func(t *testing.T) []string {
			dir := filepath.Join(t.TempDir(), "journal")
			seedJournal(t, dir, "us-east-1", "RESTRICTED")
			return ingestArgs(dir)
		},
		"mutual_tls material is not on disk": func(t *testing.T) []string {
			dir := t.TempDir()
			return ingestArgs(filepath.Join(dir, "journal"),
				"-otlp-trust-source=mutual_tls", "-otlp-source-instance=", "-otlp-credential-identity=",
				"-otlp-tls-cert="+filepath.Join(dir, "missing.crt"), "-otlp-tls-key="+filepath.Join(dir, "missing.key"),
				"-otlp-client-ca="+filepath.Join(dir, "missing-ca.crt"))
		},
		"role that has no implementation": func(t *testing.T) []string {
			args := ingestArgs(filepath.Join(t.TempDir(), "journal"))
			args[0] = "outbox"
			return args
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			if status := run(build(t), noEnv, io.Discard, silentLogger()); status != exitConfiguration {
				t.Fatalf("exit status %d, want %d; a supervisor will restart-loop flags that can never work", status, exitConfiguration)
			}
		})
	}
}

// The separation is only worth anything if a failure a restart could clear is
// still reported as one. A journal path that is a file today may be a mounted
// volume a moment later.
func TestAFailureARestartMightClearExitsWithTheFailureStatus(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(path, []byte("this is where the volume should be mounted"), 0o600); err != nil {
		t.Fatal(err)
	}
	if status := run(ingestArgs(path), noEnv, io.Discard, silentLogger()); status != exitFailure {
		t.Fatalf("exit status %d, want %d", status, exitFailure)
	}
}

// The first signal begins the drain the shutdown budget bounds.
func TestTheFirstSignalBeginsADrainRatherThanStoppingTheProcess(t *testing.T) {
	signals := make(chan os.Signal, 1)
	escalated := make(chan struct{}, 1)
	ctx, release := watchSignals(context.Background(), signals, func() { escalated <- struct{}{} })
	defer release()

	signals <- syscall.SIGTERM
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("a signal did not begin the drain")
	}
	select {
	case <-escalated:
		t.Fatal("the first signal stopped the process instead of draining")
	case <-time.After(50 * time.Millisecond):
	}
}

// A drain that is not finishing leaves an operator with SIGKILL as the only
// escalation, which can land in the middle of a journal write. A second signal
// is the deliberate exit instead.
func TestASecondSignalDuringAHungDrainForcesAnImmediateExit(t *testing.T) {
	signals := make(chan os.Signal, 2)
	escalated := make(chan struct{}, 1)
	_, release := watchSignals(context.Background(), signals, func() { escalated <- struct{}{} })
	defer release()

	signals <- syscall.SIGTERM
	signals <- syscall.SIGTERM
	select {
	case <-escalated:
	case <-time.After(5 * time.Second):
		t.Fatal("a second signal during a hung drain was ignored")
	}
}
