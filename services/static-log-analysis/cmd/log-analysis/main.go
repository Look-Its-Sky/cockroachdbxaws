// Command log-analysis is the static log analysis service. ADR 0001 fixes one
// artifact with several roles:
//
//	log-analysis all
//	log-analysis ingest
//	log-analysis process
//	log-analysis outbox
//	log-analysis source
//	log-analysis cloudwatch
//
// Everything this command does is glue. Configuration, role selection, the OTLP
// acknowledgement boundary, the process cadence, and shutdown ordering all live
// in internal/runtime and internal/otlpreceiver, where they are tested.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/runtime"
)

const (
	// exitConfiguration separates "an operator must change something" from
	// "this ran and then failed", so a supervisor does not restart-loop a
	// process whose flags can never work.
	exitConfiguration = 2
	exitFailure       = 1
	exitSuccess       = 0
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	os.Exit(run(os.Args[1:], os.Getenv, os.Stderr, logger))
}

// run is main without the process exit, so the exit-status contract is
// something a test can hold this command to.
func run(args []string, getenv func(string) string, stderr io.Writer, logger *slog.Logger) int {
	config, err := runtime.Parse(args, getenv, stderr)
	if err != nil {
		// Written to stderr rather than logged: a configuration error happens
		// before there is a running service to attribute a log line to. The
		// error never contains the DSN.
		fmt.Fprintln(stderr, err)
		return exitConfiguration
	}

	// SIGTERM is what an orchestrator sends before it stops waiting, so it
	// begins the drain rather than killing the process mid-append.
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)
	ctx, release := watchSignals(context.Background(), signals, func() {
		os.Exit(exitFailure)
	})
	defer release()

	if err := runtime.Run(ctx, config, runtime.Deps{Logger: logger}); err != nil {
		logger.Error("static-log-analysis exited with an error", slog.String("error", err.Error()))
		if errors.Is(err, runtime.ErrInvalidConfig) || errors.Is(err, runtime.ErrRoleNotImplemented) {
			return exitConfiguration
		}
		return exitFailure
	}
	return exitSuccess
}

// watchSignals cancels the returned context on the first SIGINT or SIGTERM and
// escalates on the second. Without the second read, an operator whose drain has
// wedged has no escalation short of SIGKILL, which lands mid-journal-write; the
// drain exists precisely so that does not have to happen.
func watchSignals(parent context.Context, signals <-chan os.Signal, escalate func()) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	released := make(chan struct{})
	go func() {
		select {
		case <-released:
			return
		case <-signals:
			cancel()
		}
		select {
		case <-released:
		case <-signals:
			escalate()
		}
	}()
	return ctx, func() {
		close(released)
		cancel()
	}
}
