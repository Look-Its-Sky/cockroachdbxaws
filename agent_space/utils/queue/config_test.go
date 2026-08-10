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
}
