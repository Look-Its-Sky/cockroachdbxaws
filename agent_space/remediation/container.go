package remediation

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// a Sandbox backed by a container CLI rather than the Docker SDK: podman and
// nerdctl work by changing one string, and every invocation is pasteable
type ContainerSandbox struct {
	// docker, podman or nerdctl; defaults to docker
	Binary string
	// Names every container this process starts, so an orphan is traceable
	// back to the run that leaked it.
	NamePrefix string
}

var _ Sandbox = (*ContainerSandbox)(nil)

// how long to wait for a forced removal after a timeout
const removeGrace = 20 * time.Second

// whether the runtime can be reached, called at boot so a missing one is a log
// line rather than a surprise twenty minutes into an incident
func (s *ContainerSandbox) Available(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, s.binary(), "version", "--format", "{{.Server.Version}}").CombinedOutput()
	if err != nil {
		return fmt.Errorf("remediation: %s is not usable: %w: %s",
			s.binary(), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Run executes one container and returns whatever it printed.
func (s *ContainerSandbox) Run(ctx context.Context, spec Spec) (Run, error) {
	spec = spec.withDefaults()

	if strings.TrimSpace(spec.Image) == "" {
		return Run{}, errors.New("remediation: sandbox spec has no image")
	}

	name := s.containerName()

	// the timeout belongs to the container, not to the caller's context, so a
	// harness that hangs is killed on our schedule rather than the queue's
	runCtx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, s.binary(), s.args(spec, name)...)

	// named on the command line but read from this process's environment, so a
	// value never appears in argv where another process could read it
	cmd.Env = append(os.Environ(), formatEnv(spec.SecretEnv)...)

	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output

	start := time.Now()
	err := cmd.Run()
	duration := time.Since(start)

	// a killed CLI does not stop the container and --rm only covers a clean
	// exit, so remove unconditionally or a timeout leaks one per incident
	s.forceRemove(name)

	run := Run{
		Output:   output.String(),
		Duration: duration,
		ExitCode: exitCodeOf(err),
	}

	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		run.TimedOut = true
		// a timed-out run has partial output worth keeping, so this is not an
		// error: the caller decides what a half-finished candidate is worth
		return run, nil
	}

	// the caller's context going away is a real error, unlike a container
	// exiting non-zero, which is an ordinary outcome here
	if ctx.Err() != nil {
		return run, ctx.Err()
	}

	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return run, fmt.Errorf("remediation: run container: %w", err)
	}
	return run, nil
}

// the argument list, kept separate from execution so it can be asserted on
// without a container runtime present
func (s *ContainerSandbox) args(spec Spec, name string) []string {
	args := []string{
		"run", "--rm", "--name", name,
		// this runs code a model wrote: cap what it can take, and stop it
		// gaining anything it was not given
		"--memory", fmt.Sprintf("%dm", spec.MemoryMB),
		"--cpus", strconv.FormatFloat(spec.CPUs, 'f', -1, 64),
		"--pids-limit", "512",
		"--security-opt", "no-new-privileges",
		"--cap-drop", "ALL",
	}

	if spec.NoNetwork {
		args = append(args, "--network", "none")
	}

	// Deliberately no bind mounts anywhere in here. The repository is cloned
	// inside the container and dies with it, which is the whole arrangement.

	for _, kv := range formatEnv(spec.Env) {
		args = append(args, "--env", kv)
	}
	// name only: the value is inherited from this process's environment
	for name := range spec.SecretEnv {
		args = append(args, "--env", name)
	}

	args = append(args, spec.Image, "sh", "-c", spec.Script)
	return args
}

// stop and delete a container regardless of what state it is in
func (s *ContainerSandbox) forceRemove(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), removeGrace)
	defer cancel()

	// errors are expected and ignored: the usual case is a container that
	// already removed itself
	_ = exec.CommandContext(ctx, s.binary(), "rm", "--force", name).Run()
}

func (s *ContainerSandbox) binary() string {
	if strings.TrimSpace(s.Binary) == "" {
		return "docker"
	}
	return s.Binary
}

func (s *ContainerSandbox) containerName() string {
	prefix := s.NamePrefix
	if prefix == "" {
		prefix = "sre-agent"
	}
	return fmt.Sprintf("%s-%s", prefix, uuid.NewString()[:8])
}

func formatEnv(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}

	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// a container that exits non-zero is an ordinary result here, so its code is
// data rather than an error
func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}
