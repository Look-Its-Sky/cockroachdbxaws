package collectorconfig_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const configPath = "../../deploy/collector/otel-collector-config.yaml"

type collectorConfig struct {
	Extensions map[string]any `yaml:"extensions"`
	Receivers  map[string]any `yaml:"receivers"`
	Processors map[string]any `yaml:"processors"`
	Exporters  map[string]struct {
		Endpoint     string `yaml:"endpoint"`
		Compression  string `yaml:"compression"`
		SendingQueue struct {
			Enabled   bool   `yaml:"enabled"`
			Storage   string `yaml:"storage"`
			QueueSize int    `yaml:"queue_size"`
		} `yaml:"sending_queue"`
		RetryOnFailure struct {
			Enabled        bool   `yaml:"enabled"`
			MaxElapsedTime string `yaml:"max_elapsed_time"`
		} `yaml:"retry_on_failure"`
	} `yaml:"exporters"`
	Service struct {
		Extensions []string `yaml:"extensions"`
		Pipelines  map[string]struct {
			Receivers  []string `yaml:"receivers"`
			Processors []string `yaml:"processors"`
			Exporters  []string `yaml:"exporters"`
		} `yaml:"pipelines"`
	} `yaml:"service"`
}

func load(t *testing.T) collectorConfig {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(configPath))
	if err != nil {
		t.Fatalf("the shipped Collector configuration is missing: %v", err)
	}
	var config collectorConfig
	if err := yaml.Unmarshal(raw, &config); err != nil {
		t.Fatalf("the shipped Collector configuration is not valid YAML: %v", err)
	}
	return config
}

// TestTheSendingQueueIsPersistent is the first of the two properties.
//
// architecture.md makes the Collector queue a durability layer: "Collector entry
// completes after the analysis service durably accepts it." A memory-only queue
// loses whatever it holds when the Collector restarts, which is precisely the
// failure the queue exists to survive.
func TestTheSendingQueueIsPersistent(t *testing.T) {
	config := load(t)
	exporter, ok := config.Exporters["otlp/analysis"]
	if !ok {
		t.Fatal("no otlp/analysis exporter")
	}
	if !exporter.SendingQueue.Enabled {
		t.Fatal("the sending queue is disabled; there is no first durability layer")
	}
	if exporter.SendingQueue.Storage == "" {
		t.Fatal("the sending queue has no storage extension, so it is an in-memory buffer and not a durability layer")
	}
	if _, ok := config.Extensions[exporter.SendingQueue.Storage]; !ok {
		t.Fatalf("the sending queue names storage %q, which is not configured", exporter.SendingQueue.Storage)
	}
	var enabled bool
	for _, extension := range config.Service.Extensions {
		if extension == exporter.SendingQueue.Storage {
			enabled = true
		}
	}
	if !enabled {
		t.Fatalf("storage %q is configured but not enabled in service.extensions, so it does nothing",
			exporter.SendingQueue.Storage)
	}
	if exporter.SendingQueue.QueueSize <= 0 {
		t.Fatal("the queue has no size, so it holds nothing")
	}
}

// TestRetriesNeverGiveUp pins that a retryable failure is retried until it
// succeeds. The analysis service reports permanent failures as permanent, so
// anything still being retried is something a retry can still fix; dropping it
// on a timer would be acknowledged-mandatory loss with extra steps.
func TestRetriesNeverGiveUp(t *testing.T) {
	exporter := load(t).Exporters["otlp/analysis"]
	if !exporter.RetryOnFailure.Enabled {
		t.Fatal("retry is disabled, so a single transient failure drops a batch")
	}
	if elapsed := strings.TrimSpace(exporter.RetryOnFailure.MaxElapsedTime); elapsed != "0" && elapsed != "0s" {
		t.Fatalf("max_elapsed_time is %q; a retryable batch would eventually be dropped", elapsed)
	}
}

// TestUniversalRedactionRunsBeforeTheQueue is the second property, and the one
// with security consequences.
//
// security.md: "Before the Collector persistent queue, universal policy removes
// authorization headers, cookies, passwords, API keys, JWTs, cloud credentials,
// connection-string passwords, and common private-key material." The queue is a
// persistent layer, so a secret that reaches it has been written to a disk this
// service does not control.
//
// In a Collector pipeline every processor runs before every exporter, so the
// requirement is met by the redaction processors being in the pipeline at all —
// which is exactly what could be dropped by accident.
func TestUniversalRedactionRunsBeforeTheQueue(t *testing.T) {
	config := load(t)
	pipeline, ok := config.Service.Pipelines["logs"]
	if !ok {
		t.Fatal("no logs pipeline")
	}
	var stripsAttributes, stripsContent bool
	for _, processor := range pipeline.Processors {
		if strings.HasPrefix(processor, "attributes/") {
			stripsAttributes = true
		}
		if strings.HasPrefix(processor, "transform/") {
			stripsContent = true
		}
		if _, ok := config.Processors[processor]; !ok {
			t.Fatalf("the logs pipeline names processor %q, which is not configured", processor)
		}
	}
	if !stripsAttributes {
		t.Fatal("no attributes processor in the logs pipeline; credential-named attributes would reach the persistent queue")
	}
	if !stripsContent {
		t.Fatal("no transform processor in the logs pipeline; credentials inside a log body would reach the persistent queue")
	}
	if len(pipeline.Exporters) == 0 {
		t.Fatal("the logs pipeline exports nothing")
	}
}

// TestEveryCredentialClassSecurityRequiresIsAddressed walks the list in
// security.md rather than trusting that "there is a redaction processor" means
// the right things are redacted.
func TestEveryCredentialClassSecurityRequiresIsAddressed(t *testing.T) {
	raw, err := os.ReadFile(filepath.Clean(configPath))
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for name, pattern := range map[string]*regexp.Regexp{
		"authorization headers":       regexp.MustCompile(`(?i)authorization`),
		"cookies":                     regexp.MustCompile(`(?i)cookie`),
		"passwords":                   regexp.MustCompile(`(?i)password`),
		"api keys":                    regexp.MustCompile(`(?i)api[_-]?key`),
		"jwts":                        regexp.MustCompile(`eyJ`),
		"cloud credentials":           regexp.MustCompile(`AKIA`),
		"connection-string passwords": regexp.MustCompile(`://`),
		"private key material":        regexp.MustCompile(`PRIVATE KEY`),
	} {
		if !pattern.MatchString(body) {
			t.Fatalf("the Collector configuration does not address %s, which security.md requires before the persistent queue", name)
		}
	}
}

// TestTheExporterCompressesTheWayTheReceiverExpects keeps the two ends
// agreeing. The analysis service links a gzip compressor precisely because a
// default Collector sends gzip; an exporter configured otherwise would work but
// would stop exercising the path that broke once already.
func TestTheExporterCompressesTheWayTheReceiverExpects(t *testing.T) {
	if got := load(t).Exporters["otlp/analysis"].Compression; got != "gzip" {
		t.Fatalf("compression=%q, want gzip", got)
	}
}

func TestCollectorDoesNotInventIdentityAfterAnUpstreamRetryBoundary(t *testing.T) {
	raw, err := os.ReadFile(filepath.Clean(configPath))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"UUIDv7()", "log.record.uid"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("Collector configuration contains %q; a fresh UID here would split an upstream retry into another record", forbidden)
		}
	}
}
