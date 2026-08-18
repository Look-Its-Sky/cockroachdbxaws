package queue

import (
	"testing"
	"time"
)

func TestConfigFromEnvDefaults(t *testing.T) {
	t.Setenv("SQS_QUEUE_URL", "http://localhost:4566/000000000000/static-log-analysis")

	cfg := ConfigFromEnv()
	if !cfg.Configured() {
		t.Fatal("a set SQS_QUEUE_URL should be Configured()")
	}
	if cfg.Region != DefaultRegion {
		t.Errorf("region = %q, want %q", cfg.Region, DefaultRegion)
	}
	if cfg.MaxAttempts != DefaultMaxAttempts {
		t.Errorf("max attempts = %d, want %d", cfg.MaxAttempts, DefaultMaxAttempts)
	}
	if cfg.VisibilityTimeout != DefaultVisibilityTimeout {
		t.Errorf("visibility timeout = %s, want %s", cfg.VisibilityTimeout, DefaultVisibilityTimeout)
	}
}

func TestConfigFromEnvUnconfigured(t *testing.T) {
	t.Setenv("SQS_QUEUE_URL", "")
	if ConfigFromEnv().Configured() {
		t.Error("an empty SQS_QUEUE_URL must leave the worker off")
	}
}

func TestConfigFromEnvClampsBadValues(t *testing.T) {
	t.Setenv("SQS_QUEUE_URL", "http://localhost:4566/q")
	// 0 read literally would give up on every message before running it, and
	// SQS rejects a wait longer than 20s outright
	t.Setenv("WORKER_MAX_ATTEMPTS", "0")
	t.Setenv("WORKER_WAIT_TIME", "5m")
	t.Setenv("WORKER_VISIBILITY_TIMEOUT", "nonsense")

	cfg := ConfigFromEnv()
	if cfg.MaxAttempts != DefaultMaxAttempts {
		t.Errorf("max attempts = %d, want the default %d", cfg.MaxAttempts, DefaultMaxAttempts)
	}
	if cfg.WaitTime > 20*time.Second {
		t.Errorf("wait time = %s, want it clamped to SQS's 20s ceiling", cfg.WaitTime)
	}
	if cfg.VisibilityTimeout != DefaultVisibilityTimeout {
		t.Errorf("visibility timeout = %s, want the default %s", cfg.VisibilityTimeout, DefaultVisibilityTimeout)
	}
}

func TestConfigFromEnvOverrides(t *testing.T) {
	t.Setenv("SQS_QUEUE_URL", "http://localhost:4566/q")
	t.Setenv("AWS_ENDPOINT_URL", "http://localhost:4566")
	t.Setenv("AWS_REGION", "eu-west-2")
	t.Setenv("WORKER_RUN_TIMEOUT", "90s")
	t.Setenv("AGENT_TENANT_ID", "tenant-a")
	t.Setenv("AGENT_CLASSIFICATION", "SENSITIVE")

	cfg := ConfigFromEnv()
	if cfg.Endpoint != "http://localhost:4566" {
		t.Errorf("endpoint = %q", cfg.Endpoint)
	}
	if cfg.Region != "eu-west-2" {
		t.Errorf("region = %q", cfg.Region)
	}
	if cfg.RunTimeout != 90*time.Second {
		t.Errorf("run timeout = %s, want 90s", cfg.RunTimeout)
	}
	if !cfg.BoundaryConfigured() || cfg.Boundary() != (Boundary{Region: "eu-west-2", TenantID: "tenant-a", Classification: "SENSITIVE"}) {
		t.Errorf("boundary = %+v, want configured deployment scope", cfg.Boundary())
	}
}

// switching to real SQS is two edits and the second is a deletion; forget it
// and every call goes to a LocalStack that may not be running
func TestRealQueueURLIgnoresAStaleLocalStackEndpoint(t *testing.T) {
	t.Setenv("SQS_QUEUE_URL", "https://sqs.us-east-1.amazonaws.com/000000000000/static-log-analysis")
	t.Setenv("AWS_ENDPOINT_URL", "http://localhost:4566")

	if endpoint := ConfigFromEnv().Endpoint; endpoint != "" {
		t.Errorf("endpoint = %q, want it dropped for an AWS queue URL", endpoint)
	}
}

func TestLocalStackEndpointIsKeptForALocalQueue(t *testing.T) {
	t.Setenv("SQS_QUEUE_URL", "http://localhost:4566/000000000000/static-log-analysis")
	t.Setenv("AWS_ENDPOINT_URL", "http://localhost:4566")

	// the whole local stack depends on this override surviving
	if endpoint := ConfigFromEnv().Endpoint; endpoint != "http://localhost:4566" {
		t.Errorf("endpoint = %q, want the override kept for a local queue", endpoint)
	}
}

func TestIsAWSQueue(t *testing.T) {
	cases := []struct {
		queueURL string
		want     bool
	}{
		{"https://sqs.us-east-1.amazonaws.com/123456789012/q", true},
		{"https://sqs-fips.us-east-1.amazonaws.com/123456789012/q", true},
		// China's hosts do not end .amazonaws.com, so they need matching separately
		{"https://sqs.cn-north-1.amazonaws.com.cn/123456789012/q", true},
		{"HTTPS://SQS.US-EAST-1.AMAZONAWS.COM/123456789012/q", true},

		{"http://localhost:4566/000000000000/q", false},
		{"http://localstack:4566/000000000000/q", false},
		// a host that merely contains the string is not AWS, and treating it as
		// one would drop a deliberate endpoint override
		{"https://evil-amazonaws.com/123456789012/q", false},
		{"https://amazonaws.com.attacker.net/123456789012/q", false},
		{"", false},
		{"://nonsense", false},
	}

	for _, tc := range cases {
		if got := isAWSQueue(tc.queueURL); got != tc.want {
			t.Errorf("isAWSQueue(%q) = %v, want %v", tc.queueURL, got, tc.want)
		}
	}
}
