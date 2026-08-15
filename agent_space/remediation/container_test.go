package remediation

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func argsFor(spec Spec) []string {
	s := &ContainerSandbox{}
	return s.args(spec.withDefaults(), "sre-agent-test")
}

// the flag immediately after name, or "" if absent
func flagValue(args []string, name string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func TestArgsCapResources(t *testing.T) {
	args := argsFor(Spec{Image: "golang:1.24", Script: "go test ./..."})

	// this runs code a model wrote; the caps are the point
	if got := flagValue(args, "--memory"); got != "4096m" {
		t.Errorf("--memory = %q", got)
	}
	if got := flagValue(args, "--cpus"); got != "2" {
		t.Errorf("--cpus = %q", got)
	}
	if got := flagValue(args, "--pids-limit"); got == "" {
		t.Error("no pid limit; a fork bomb would take the host with it")
	}
	if got := flagValue(args, "--security-opt"); got != "no-new-privileges" {
		t.Errorf("--security-opt = %q", got)
	}
	if got := flagValue(args, "--cap-drop"); got != "ALL" {
		t.Errorf("--cap-drop = %q", got)
	}
	if !slices.Contains(args, "--rm") {
		t.Error("--rm is missing; containers would accumulate")
	}
}

func TestArgsNeverBindMountTheHost(t *testing.T) {
	args := argsFor(Spec{Image: "golang:1.24", Script: "true"})

	// the repository is cloned inside the container and dies with it; a mount
	// would put the host filesystem in reach of model-written code
	for _, flag := range []string{"-v", "--volume", "--mount"} {
		if slices.Contains(args, flag) {
			t.Errorf("sandbox bind-mounts the host via %s", flag)
		}
	}
}

func TestArgsSecretsAreNotInArgv(t *testing.T) {
	args := argsFor(Spec{
		Image:     "golang:1.24",
		Script:    "true",
		Env:       map[string]string{"SERVICE": "checkout"},
		SecretEnv: map[string]string{"GITHUB_TOKEN": "github_pat_supersecret"},
	})

	joined := strings.Join(args, " ")

	// the value is read from our environment by the runtime; argv is visible
	// to every other process on the machine
	if strings.Contains(joined, "github_pat_supersecret") {
		t.Errorf("a secret value reached argv: %s", joined)
	}
	if !strings.Contains(joined, "--env GITHUB_TOKEN") {
		t.Error("the secret was not passed by name")
	}
	// plain values are fine to pass through
	if !strings.Contains(joined, "SERVICE=checkout") {
		t.Error("plain env was not passed")
	}
}

func TestArgsNetworkIsOnUnlessRefused(t *testing.T) {
	// cloning and dependency resolution both need it, so network-off has to be
	// asked for explicitly rather than being the default
	on := argsFor(Spec{Image: "x", Script: "true"})
	if slices.Contains(on, "none") {
		t.Error("network was disabled by default")
	}

	off := argsFor(Spec{Image: "x", Script: "true", NoNetwork: true})
	if flagValue(off, "--network") != "none" {
		t.Error("NoNetwork did not disable the network")
	}
}

func TestArgsScriptIsTheFinalArgument(t *testing.T) {
	// the script must not be split or reordered into flags
	script := "echo 'hello --rm world'"
	args := argsFor(Spec{Image: "golang:1.24", Script: script})

	if got := args[len(args)-1]; got != script {
		t.Errorf("last argument = %q, want the script verbatim", got)
	}
	if args[len(args)-2] != "-c" || args[len(args)-3] != "sh" {
		t.Errorf("script is not invoked with sh -c: %v", args[len(args)-3:])
	}
	// and the image comes before the command, or the runtime reads it as a flag
	if args[len(args)-4] != "golang:1.24" {
		t.Errorf("image is not immediately before the command: %v", args[len(args)-4:])
	}
}

func TestSpecDefaults(t *testing.T) {
	got := Spec{}.withDefaults()

	if got.Timeout != DefaultSandboxTimeout {
		t.Errorf("timeout = %s", got.Timeout)
	}
	if got.MemoryMB != DefaultMemoryMB || got.CPUs != DefaultCPUs {
		t.Errorf("resources = %dm/%v", got.MemoryMB, got.CPUs)
	}
	if got.WorkingDir != "/workspace" {
		t.Errorf("working dir = %q", got.WorkingDir)
	}

	// explicit values survive
	custom := Spec{Timeout: time.Minute, MemoryMB: 512, CPUs: 0.5}.withDefaults()
	if custom.Timeout != time.Minute || custom.MemoryMB != 512 || custom.CPUs != 0.5 {
		t.Errorf("defaults overwrote explicit values: %+v", custom)
	}
}

func TestContainerNamesAreUnique(t *testing.T) {
	// two candidates run concurrently; a fixed name would collide and the
	// second would fail to start
	s := &ContainerSandbox{NamePrefix: "sre-test"}

	first, second := s.containerName(), s.containerName()
	if first == second {
		t.Errorf("container names collide: %q", first)
	}
	if !strings.HasPrefix(first, "sre-test-") {
		t.Errorf("name = %q, want the configured prefix", first)
	}
}

func TestBinaryDefaultsToDocker(t *testing.T) {
	if got := (&ContainerSandbox{}).binary(); got != "docker" {
		t.Errorf("binary = %q", got)
	}
	if got := (&ContainerSandbox{Binary: "podman"}).binary(); got != "podman" {
		t.Errorf("binary = %q", got)
	}
}
