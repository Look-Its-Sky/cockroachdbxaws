package deployconfig_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestProductionChartDeclaresSafeTopologyAndDurability(t *testing.T) {
	chartRoot := filepath.Join(serviceRoot, "chart", "static-log-analysis")
	for _, name := range []string{
		"Chart.yaml",
		"values.yaml",
		"values.schema.json",
		"templates/migrate-job.yaml",
		"templates/analysis-statefulset.yaml",
		"templates/outbox-deployment.yaml",
		"templates/collector-statefulset.yaml",
		"templates/collector-configmap.yaml",
		"templates/cloudwatch-statefulset.yaml",
		"templates/cloudwatch-serviceaccount.yaml",
	} {
		if _, err := os.Stat(filepath.Join(chartRoot, name)); err != nil {
			t.Errorf("production chart is missing %s: %v", name, err)
		}
	}

	analysis := readProductionFile(t, "chart/static-log-analysis/templates/analysis-statefulset.yaml")
	for _, required := range []string{"kind: StatefulSet", "- all", "volumeClaimTemplates:", "/var/lib/static-log-analysis/journal", "-otlp-trust-source=mutual_tls"} {
		if !strings.Contains(analysis, required) {
			t.Errorf("analysis StatefulSet does not contain %q", required)
		}
	}
	for _, forbidden := range []string{"- ingest", "- process", "- source"} {
		if strings.Contains(analysis, forbidden) {
			t.Errorf("production chart deploys unsafe split role %q", forbidden)
		}
	}

	collector := readProductionFile(t, "chart/static-log-analysis/templates/collector-configmap.yaml")
	redaction := strings.Index(collector, "attributes/strip-credentials")
	queue := strings.Index(collector, "sending_queue:")
	if redaction < 0 || queue < 0 || redaction > queue {
		t.Fatal("Collector must define universal redaction before its persistent sending queue")
	}
	for _, required := range []string{"storage: file_storage/queue", "client_ca_file:", "cert_file:", "key_file:"} {
		if !strings.Contains(collector, required) {
			t.Errorf("production Collector config does not contain %q", required)
		}
	}

	outbox := readProductionFile(t, "chart/static-log-analysis/templates/outbox-deployment.yaml")
	for _, required := range []string{"kind: Deployment", "- outbox", "-outbox-queue-url=", "-outbox-dead-letter-queue-url="} {
		if !strings.Contains(outbox, required) {
			t.Errorf("outbox Deployment does not contain %q", required)
		}
	}

	cloudWatch := readProductionFile(t, "chart/static-log-analysis/templates/cloudwatch-statefulset.yaml")
	for _, required := range []string{"kind: StatefulSet", "- cloudwatch", "name: journal", "name: checkpoints", "-cloudwatch-log-groups="} {
		if !strings.Contains(cloudWatch, required) {
			t.Errorf("CloudWatch StatefulSet does not contain %q", required)
		}
	}
}

func TestProductionValuesAreSafeAndRepositoryAgnostic(t *testing.T) {
	raw := readProductionFile(t, "chart/static-log-analysis/values.yaml")
	var values map[string]any
	if err := yaml.Unmarshal([]byte(raw), &values); err != nil {
		t.Fatalf("production values are invalid YAML: %v", err)
	}
	for _, forbidden := range []string{"static_local", "sslmode=disable", "frontend-proxy", "paymentservice", "changeme"} {
		if strings.Contains(raw, forbidden) {
			t.Errorf("production values contain unsafe or project-specific value %q", forbidden)
		}
	}
	for _, required := range []string{"region: \"\"", "tenantID: \"\"", "allowedServices: []", "allowedEnvironments: []", "existingSecret:"} {
		if !strings.Contains(raw, required) {
			t.Errorf("production values do not make %q explicit", required)
		}
	}
}

func TestAWSInfrastructureCreatesRegionalQueuesAndLeastPrivilegeOutboxPolicy(t *testing.T) {
	main := readProductionFile(t, "infra/aws/main.tf")
	for _, required := range []string{
		`resource "aws_sqs_queue" "assignments"`,
		`resource "aws_sqs_queue" "dead_letter"`,
		"sqs_managed_sse_enabled",
		`"sqs:SendMessage"`,
		`resource "aws_eks_pod_identity_association" "outbox"`,
		`resource "aws_iam_role" "cloudwatch"`,
		`"logs:FilterLogEvents"`,
		`resource "aws_eks_pod_identity_association" "cloudwatch"`,
	} {
		if !strings.Contains(main, required) {
			t.Errorf("AWS module does not contain %q", required)
		}
	}
	if strings.Contains(main, `"sqs:*"`) || strings.Contains(main, `Resource = "*"`) {
		t.Fatal("outbox IAM policy is broader than its two queues")
	}
	if strings.Contains(main, `"logs:*"`) {
		t.Fatal("CloudWatch IAM policy is broader than FilterLogEvents")
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
