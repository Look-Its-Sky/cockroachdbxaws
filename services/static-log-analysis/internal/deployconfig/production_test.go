package deployconfig_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAWSComposeRunsOnlyTheCloudWatchProductionPath(t *testing.T) {
	compose := readProductionFile(t, "deploy/aws/compose.yaml")
	for _, required := range []string{
		"volume-init:",
		"migrate:",
		"cloudwatch:",
		"outbox:",
		"- cloudwatch",
		"- outbox",
		"analysis-journal:",
		"cloudwatch-checkpoints:",
		"restart: unless-stopped",
		"chown -R 10001:10001",
	} {
		if !strings.Contains(compose, required) {
			t.Errorf("AWS Compose deployment does not contain %q", required)
		}
	}
	for _, forbidden := range []string{
		"cockroachdb:",
		"localstack:",
		"AWS_ACCESS_KEY_ID",
		"AWS_SECRET_ACCESS_KEY",
		"sslmode=disable",
		"- ingest",
		"- process",
		"- source",
	} {
		if strings.Contains(compose, forbidden) {
			t.Errorf("AWS Compose deployment contains local-only or unsafe value %q", forbidden)
		}
	}
}

func TestAWSInfrastructureIsOnePrivateEC2HostWithLeastPrivilege(t *testing.T) {
	main := readProductionFile(t, "infra/aws/main.tf")
	for _, required := range []string{
		`resource "aws_instance" "service"`,
		`resource "aws_iam_instance_profile" "service"`,
		`resource "aws_security_group" "service"`,
		`resource "aws_sqs_queue" "assignments"`,
		`resource "aws_sqs_queue" "dead_letter"`,
		`"logs:FilterLogEvents"`,
		`"sqs:SendMessage"`,
		`"ssm:GetParameter"`,
		`metadata_options`,
		`http_tokens`,
		`encrypted`,
	} {
		if !strings.Contains(main, required) {
			t.Errorf("AWS EC2 module does not contain %q", required)
		}
	}
	for _, forbidden := range []string{
		`aws_eks_`,
		`pods.eks.amazonaws.com`,
		`ingress {`,
		`"logs:*"`,
		`"sqs:*"`,
		`Resource = "*"`,
	} {
		if strings.Contains(main, forbidden) {
			t.Errorf("AWS EC2 module contains obsolete or over-broad value %q", forbidden)
		}
	}
}

func TestBootstrapRetrievesTheDatabaseSecretAtRuntime(t *testing.T) {
	bootstrap := readProductionFile(t, "infra/aws/user-data.sh.tftpl")
	for _, required := range []string{
		"aws ssm get-parameter",
		"--with-decryption",
		"STATIC_LOG_ANALYSIS_DATABASE_DSN",
		"sha256sum --check",
		"systemctl enable --now static-log-analysis.service",
	} {
		if !strings.Contains(bootstrap, required) {
			t.Errorf("EC2 bootstrap does not contain %q", required)
		}
	}
	for _, forbidden := range []string{"postgresql://", "password=", "AWS_SECRET_ACCESS_KEY"} {
		if strings.Contains(bootstrap, forbidden) {
			t.Errorf("EC2 bootstrap appears to persist secret material through %q", forbidden)
		}
	}
}

func TestKubernetesIsNotASecondProductionPath(t *testing.T) {
	_, err := os.Stat(filepath.Join(serviceRoot, "chart"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the obsolete Kubernetes chart still exists: %v", err)
	}

	deployment := readRepositoryFile(t, "docs/static-log-analysis/deployment.md")
	for _, obsolete := range []string{"Production EKS", "helm upgrade", "kubectl", "Pod Identity"} {
		if strings.Contains(deployment, obsolete) {
			t.Errorf("the primary deployment guide still presents Kubernetes through %q", obsolete)
		}
	}
}

func readProductionFile(t *testing.T, relative string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(serviceRoot, filepath.FromSlash(relative)))
	if err != nil {
		t.Fatalf("reading %s: %v", relative, err)
	}
	return string(raw)
}

func readRepositoryFile(t *testing.T, relative string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(serviceRoot, "..", "..", filepath.FromSlash(relative)))
	if err != nil {
		t.Fatalf("reading %s: %v", relative, err)
	}
	return string(raw)
}
