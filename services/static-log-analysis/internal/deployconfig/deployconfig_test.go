package deployconfig_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const serviceRoot = "../.."

type composeFile struct {
	Services map[string]struct {
		Build       any               `yaml:"build"`
		Image       string            `yaml:"image"`
		Entrypoint  any               `yaml:"entrypoint"`
		Command     any               `yaml:"command"`
		DependsOn   map[string]any    `yaml:"depends_on"`
		Volumes     []string          `yaml:"volumes"`
		Environment map[string]string `yaml:"environment"`
		Networks    map[string]any    `yaml:"networks"`
	} `yaml:"services"`
	Volumes  map[string]any `yaml:"volumes"`
	Networks map[string]struct {
		Name string `yaml:"name"`
	} `yaml:"networks"`
}

func TestComposePublishesAStableCrossRepositoryIngressNetwork(t *testing.T) {
	config := loadCompose(t)
	collector := config.Services["otel-collector"]
	if _, ok := collector.Networks["ingress"]; !ok {
		t.Fatal("the Collector is not attached to the cross-repository ingress network")
	}
	if got := config.Networks["ingress"].Name; !strings.Contains(got, "static-log-analysis-ingress") {
		t.Fatalf("ingress network name=%q, want a stable non-project-scoped name", got)
	}
	raw, err := os.ReadFile(filepath.Join(serviceRoot, "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "static-log-analysis-collector") {
		t.Fatal("the ingress Collector has no stable network alias for other repositories")
	}
}

func TestDeploymentDefaultsAreRepositoryAgnostic(t *testing.T) {
	for _, name := range []string{"compose.yaml", ".env.example"} {
		raw, err := os.ReadFile(filepath.Join(serviceRoot, name))
		if err != nil {
			t.Fatal(err)
		}
		for _, demoSpecific := range []string{"local-demo", "compose-collector", "frontend-proxy", "payment,product-catalog"} {
			if strings.Contains(string(raw), demoSpecific) {
				t.Errorf("%s still embeds OpenTelemetry Demo-specific default %q", name, demoSpecific)
			}
		}
	}
}

func TestComposeIntegrationRendererProducesAValidRepositoryOverlay(t *testing.T) {
	command := exec.Command("sh", filepath.Join(serviceRoot, "scripts", "render-compose-integration.sh"), "api", "checkout", "staging")
	raw, err := command.Output()
	if err != nil {
		t.Fatalf("rendering integration overlay: %v", err)
	}
	var overlay struct {
		Services map[string]struct {
			Environment map[string]string `yaml:"environment"`
			Networks    []string          `yaml:"networks"`
		} `yaml:"services"`
		Networks map[string]struct {
			External bool   `yaml:"external"`
			Name     string `yaml:"name"`
		} `yaml:"networks"`
	}
	if err := yaml.Unmarshal(raw, &overlay); err != nil {
		t.Fatalf("renderer produced invalid YAML:\n%s\n%v", raw, err)
	}
	service, ok := overlay.Services["api"]
	if !ok {
		t.Fatalf("overlay does not modify requested Compose service:\n%s", raw)
	}
	if service.Environment["OTEL_SERVICE_NAME"] != "checkout" ||
		!strings.Contains(service.Environment["OTEL_RESOURCE_ATTRIBUTES"], "deployment.environment.name=staging") ||
		!strings.Contains(service.Environment["OTEL_EXPORTER_OTLP_ENDPOINT"], "static-log-analysis-collector") {
		t.Fatalf("overlay does not carry the requested service/environment/endpoint: %+v", service.Environment)
	}
	network := overlay.Networks["static-log-analysis-ingress"]
	if !network.External || network.Name != "${SLA_INGRESS_NETWORK:-static-log-analysis-ingress}" {
		t.Fatalf("overlay network=%+v, want the shared external ingress network", network)
	}
}

func loadCompose(t *testing.T) composeFile {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(serviceRoot, "compose.yaml"))
	if err != nil {
		t.Fatalf("the local Compose deployment is missing: %v", err)
	}
	var config composeFile
	if err := yaml.Unmarshal(raw, &config); err != nil {
		t.Fatalf("the local Compose deployment is not valid YAML: %v", err)
	}
	return config
}

func TestComposeRunsTheSafeCurrentTopology(t *testing.T) {
	config := loadCompose(t)
	for _, name := range []string{"cockroachdb", "cockroach-init", "migrate", "localstack", "static-log-analysis", "outbox", "otel-collector"} {
		if _, ok := config.Services[name]; !ok {
			t.Errorf("compose service %q is missing", name)
		}
	}
	if _, split := config.Services["ingest"]; split {
		t.Fatal("the local deployment must not strand work in a replica-local ingest journal")
	}
	if _, split := config.Services["process"]; split {
		t.Fatal("the local deployment must use the combined role until journal handoff exists")
	}

	analysis := config.Services["static-log-analysis"]
	if !containsString(analysis.Command, "all") {
		t.Fatalf("static-log-analysis command=%v, want the combined all role", analysis.Command)
	}
	if !containsString(config.Services["outbox"].Command, "outbox") {
		t.Fatalf("outbox command=%v, want the outbox role", config.Services["outbox"].Command)
	}
	if _, ok := analysis.DependsOn["migrate"]; !ok {
		t.Fatal("analysis service may start before reviewed migrations complete")
	}
	if _, ok := config.Services["outbox"].DependsOn["migrate"]; !ok {
		t.Fatal("outbox service may start before reviewed migrations complete")
	}
}

func TestComposePersistsBothDurabilityLayers(t *testing.T) {
	config := loadCompose(t)
	for service, volume := range map[string]string{
		"cockroachdb":         "cockroach-data",
		"static-log-analysis": "analysis-journal",
		"otel-collector":      "collector-queue",
	} {
		if !hasVolume(config.Services[service].Volumes, volume) {
			t.Errorf("%s does not mount durable volume %q", service, volume)
		}
		if _, ok := config.Volumes[volume]; !ok {
			t.Errorf("durable volume %q is not declared", volume)
		}
	}
}

func TestImageCarriesServiceAndReviewedMigratorAsNonRoot(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(serviceRoot, "Dockerfile"))
	if err != nil {
		t.Fatalf("the service Dockerfile is missing: %v", err)
	}
	dockerfile := string(raw)
	for _, required := range []string{
		"./cmd/log-analysis",
		"./cmd/log-analysis-migrate",
		"USER 10001:10001",
		`ENTRYPOINT ["/usr/local/bin/log-analysis"]`,
	} {
		if !strings.Contains(dockerfile, required) {
			t.Errorf("Dockerfile does not contain %q", required)
		}
	}
}

func containsString(value any, want string) bool {
	switch value := value.(type) {
	case string:
		return strings.Contains(value, want)
	case []any:
		for _, item := range value {
			if text, ok := item.(string); ok && text == want {
				return true
			}
		}
	}
	return false
}

func hasVolume(mounts []string, name string) bool {
	for _, mount := range mounts {
		if strings.HasPrefix(mount, name+":") {
			return true
		}
	}
	return false
}
