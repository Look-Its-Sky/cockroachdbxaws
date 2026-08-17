package main

import (
	"os"
	"strings"
	"testing"
)

func TestAppRunnerDeploymentPinsTheIntegratedWorkerBoundary(t *testing.T) {
	raw, err := os.ReadFile("scripts/deploy.sh")
	if err != nil {
		t.Fatalf("read deployment script: %v", err)
	}
	deploy := string(raw)

	for _, required := range []string{
		"ANALYSIS_DATABASE_URL",
		"SQS_QUEUE_URL",
		"AGENT_TENANT_ID",
		"AGENT_CLASSIFICATION",
		"API_TOKEN",
		`env["CORS_ORIGINS"] == "*"`,
		"APPRUNNER_INSTANCE_ROLE_ARN",
		`env["REMEDIATION_ENABLED"] = "false"`,
		"--instance-configuration \"$INSTANCE_CONFIG\"",
		"--auto-scaling-configuration-arn \"$AUTOSCALING_ARN\"",
		"--min-size 1",
		"--max-size 1",
	} {
		if !strings.Contains(deploy, required) {
			t.Errorf("App Runner deployment does not contain %q", required)
		}
	}
	if strings.Contains(deploy, "warning — COCKROACH_API_KEY unset") {
		t.Error("deployment still treats a required worker dependency as optional")
	}
}

func TestLocalIntegrationUsesTheStaticStackWithoutSharingAgentState(t *testing.T) {
	raw, err := os.ReadFile("../compose.local-integration.yaml")
	if err != nil {
		t.Fatalf("read local integration deployment: %v", err)
	}
	compose := string(raw)

	for _, required := range []string{
		`name: agent-local-integration`,
		`name: static-log-analysis_default`,
		`CREATE DATABASE IF NOT EXISTS agent_space`,
		`CREATE USER IF NOT EXISTS agent_context_reader`,
		`GRANT SELECT ON TABLE investigations, incident_families, investigation_contexts TO agent_context_reader`,
		`DATABASE_URL: postgresql://root@cockroachdb:26257/agent_space?sslmode=disable`,
		`ANALYSIS_DATABASE_URL: postgresql://agent_context_reader@cockroachdb:26257/static_log_analysis?sslmode=disable`,
		`SQS_QUEUE_URL: http://localstack:4566/queue/${SLA_REGION:-us-east-1}/000000000000/static-log-analysis`,
		`AGENT_TENANT_ID: ${SLA_TENANT_ID:-local}`,
		`AGENT_CLASSIFICATION: SENSITIVE`,
		`COCKROACH_MCP_URL: http://agent-mcp:8443`,
		`REMEDIATION_ENABLED: "false"`,
		`127.0.0.1:${AGENT_MCP_PORT:-18443}:8443`,
		`127.0.0.1:${AGENT_API_PORT:-18081}:8080`,
		`host.docker.internal:host-gateway`,
	} {
		if !strings.Contains(compose, required) {
			t.Errorf("local integration deployment does not contain %q", required)
		}
	}
	if strings.Contains(compose, `/var/run/docker.sock`) {
		t.Error("local agent API must not receive the host Docker socket")
	}
}

func TestLocalModelsOverlayKeepsTheAgentOnLoopbackAndRemediationOff(t *testing.T) {
	raw, err := os.ReadFile("../compose.local-models.yaml")
	if err != nil {
		t.Fatalf("read local models deployment: %v", err)
	}
	compose := string(raw)

	for _, required := range []string{
		`name: agent-local-models`,
		`network_mode: host`,
		`OPENAI_BASE_URL: http://127.0.0.1:11434/v1`,
		`EMBEDDING_BASE_URL: http://127.0.0.1:11434/v1`,
		`DATABASE_URL: postgresql://root@127.0.0.1:26257/agent_space?sslmode=disable`,
		`ANALYSIS_DATABASE_URL: postgresql://agent_context_reader@127.0.0.1:26257/static_log_analysis?sslmode=disable`,
		`COCKROACH_MCP_URL: http://127.0.0.1:${AGENT_MCP_PORT:-18443}`,
		`SQS_QUEUE_URL: http://127.0.0.1:4566/queue/${SLA_REGION:-us-east-1}/000000000000/static-log-analysis`,
		`PORT: ${AGENT_API_PORT:-18081}`,
		`REMEDIATION_ENABLED: "false"`,
	} {
		if !strings.Contains(compose, required) {
			t.Errorf("local models deployment does not contain %q", required)
		}
	}
	if strings.Contains(compose, `/var/run/docker.sock`) {
		t.Error("local model agent must not receive the host Docker socket")
	}
}

func TestAgentImagePinsBuildAndRuntimeBases(t *testing.T) {
	raw, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	dockerfile := string(raw)
	for _, required := range []string{
		"golang:1.26.5-alpine3.23@sha256:",
		"alpine:3.23@sha256:",
	} {
		if !strings.Contains(dockerfile, required) {
			t.Errorf("agent image does not pin %q", required)
		}
	}
	if strings.Contains(dockerfile, "alpine:latest") {
		t.Error("agent runtime base still floats on alpine:latest")
	}
}
