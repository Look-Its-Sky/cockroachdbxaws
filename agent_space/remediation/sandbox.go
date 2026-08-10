package remediation

import (
	"context"
	"time"
)

// a throwaway container: one script, one exit code, whatever it printed.
//
// Deliberately this small. Everything interesting — cloning, running a coding
// harness, building, testing, extracting a diff — is composed as shell in
// Script and parsed back out of Output, which keeps the runtime adapter thin
// enough to be obviously correct and puts the logic somewhere a fake can drive.
type Sandbox interface {
	Run(ctx context.Context, spec Spec) (Run, error)
}

// Spec is one container invocation.
type Spec struct {
	Image      string
	WorkingDir string
	// Script is executed with `sh -c`. It is the whole job: the container is
	// destroyed afterwards, so nothing can be carried between Runs.
	Script string
	Env    map[string]string
	// SecretEnv names variables whose values are taken from this process's
	// environment by the runtime, so a token is never written into argv where
	// any other process on the box could read it.
	SecretEnv map[string]string
	// Timeout kills the container. A coding harness with no ceiling will
	// happily spend an afternoon.
	Timeout time.Duration
	// hard caps, because this executes code a model wrote
	MemoryMB int64
	CPUs     float64
	// NoNetwork cuts the container off entirely. Only usable for stages that
	// need neither a registry nor a model API — see the note in runner.go
	// about why build and test rarely qualify.
	NoNetwork bool
}

// Run is the outcome of one container.
type Run struct {
	ExitCode int
	Output   string
	Duration time.Duration
	TimedOut bool
}

// defaults sized for a coding harness plus a build, not for a quick command
const (
	DefaultSandboxTimeout = 15 * time.Minute
	DefaultMemoryMB       = 4096
	DefaultCPUs           = 2.0
)

// fill in anything the caller left at zero
func (s Spec) withDefaults() Spec {
	if s.Timeout <= 0 {
		s.Timeout = DefaultSandboxTimeout
	}
	if s.MemoryMB <= 0 {
		s.MemoryMB = DefaultMemoryMB
	}
	if s.CPUs <= 0 {
		s.CPUs = DefaultCPUs
	}
	if s.WorkingDir == "" {
		s.WorkingDir = "/workspace"
	}
	return s
}
