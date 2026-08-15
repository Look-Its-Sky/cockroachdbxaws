package remediation

import (
	"context"
	"time"
)

// a throwaway container: one script, one exit code, whatever it printed.
// Deliberately this small, so the logic lives in shell a fake can drive.
type Sandbox interface {
	Run(ctx context.Context, spec Spec) (Run, error)
}

// one container invocation
type Spec struct {
	Image      string
	WorkingDir string
	// executed with `sh -c`, and the whole job: the container is
	// destroyed afterwards, so nothing can be carried between Runs.
	Script string
	Env    map[string]string
	// variables the runtime reads from this process's environment, so a token
	// is never written into argv
	SecretEnv map[string]string
	// kills the container; a coding harness with no ceiling will
	// happily spend an afternoon.
	Timeout time.Duration
	// hard caps, because this executes code a model wrote
	MemoryMB int64
	CPUs     float64
	// cuts the container off entirely; only for stages needing neither a
	// registry nor a model API, which build and test rarely are
	NoNetwork bool
}

// the outcome of one container
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
