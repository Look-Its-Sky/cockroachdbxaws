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
		"cockroach-ca.crt",
		"- ingest",
		"- process",
		"- source",
	} {
		if strings.Contains(compose, forbidden) {
			t.Errorf("AWS Compose deployment contains local-only or unsafe value %q", forbidden)
		}
	}
}

func TestAWSInfrastructureSeparatesPrivateAnalysisFromOptionalDemo(t *testing.T) {
	main := readProductionFile(t, "infra/aws/main.tf")
	for _, required := range []string{
		`resource "aws_instance" "service"`,
		`resource "aws_instance" "demo"`,
		`resource "aws_iam_instance_profile" "service"`,
		`resource "aws_iam_instance_profile" "demo"`,
		`resource "aws_security_group" "service"`,
		`resource "aws_security_group" "demo"`,
		`resource "aws_cloudwatch_log_group" "demo_payment"`,
		`resource "aws_sqs_queue" "assignments"`,
		`resource "aws_sqs_queue" "dead_letter"`,
		`"logs:FilterLogEvents"`,
		`"${source.log_group_arn}:*"`,
		`"sqs:SendMessage"`,
		`AmazonSSMManagedInstanceCore`,
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
		`"logs:*"`,
		`"sqs:*"`,
		`"ssm:GetParameter"`,
		`"kms:Decrypt"`,
		`data "aws_ssm_parameter"`,
		`Resource = "*"`,
	} {
		if strings.Contains(main, forbidden) {
			t.Errorf("AWS EC2 module contains obsolete or over-broad value %q", forbidden)
		}
	}

	serviceSecurityGroup := terraformResourceBlock(t, main, `resource "aws_security_group" "service"`)
	for _, required := range []string{`dynamic "ingress"`, "dashboard_ingress_ports", "dashboard_ingress_cidr"} {
		if !strings.Contains(serviceSecurityGroup, required) {
			t.Errorf("the analysis host's optional dashboard ingress does not contain %q", required)
		}
	}
	for _, forbidden := range []string{"from_port   = 26257", "from_port   = 9464", "from_port   = 9465"} {
		if strings.Contains(serviceSecurityGroup, forbidden) {
			t.Errorf("the analysis host exposes a private service port through %q", forbidden)
		}
	}
	demoSecurityGroup := terraformResourceBlock(t, main, `resource "aws_security_group" "demo"`)
	for _, required := range []string{"ingress {", "from_port", "8080", "demo_ingress_cidr"} {
		if !strings.Contains(demoSecurityGroup, required) {
			t.Errorf("the demo security group does not contain %q", required)
		}
	}
}

func TestAWSBootstrapCompressesEmbeddedDeploymentFiles(t *testing.T) {
	main := readProductionFile(t, "infra/aws/main.tf")
	bootstrap := readProductionFile(t, "infra/aws/user-data.sh.tftpl")
	for _, required := range []string{
		`base64gzip(file("${path.module}/../../deploy/aws/compose.yaml"))`,
		`base64gzip(file("${path.module}/../../deploy/aws/Caddyfile"))`,
	} {
		if !strings.Contains(main, required) {
			t.Errorf("AWS infrastructure does not compress embedded deployment file through %q", required)
		}
	}
	if strings.Count(bootstrap, "base64 --decode | gzip --decompress") != 2 {
		t.Fatal("AWS bootstrap does not decompress both embedded deployment files")
	}
}

func TestAWSProviderUsesTheDeclaredRegionalBoundary(t *testing.T) {
	versions := readProductionFile(t, "infra/aws/versions.tf")
	for _, required := range []string{
		`provider "aws"`,
		"region = var.region",
	} {
		if !strings.Contains(versions, required) {
			t.Errorf("AWS provider configuration does not contain %q", required)
		}
	}
}

func TestBootstrapRequiresARootOnlyComposeSecretsFile(t *testing.T) {
	bootstrap := readProductionFile(t, "infra/aws/user-data.sh.tftpl")
	for _, required := range []string{
		"/etc/static-log-analysis/secrets.env",
		"stat -c '%u:%g:%a'",
		"0:0:600",
		`--env-file "$database_secret_file"`,
		`STATIC_LOG_ANALYSIS_DATABASE_DSN='`,
		"set-static-log-analysis-database-dsn",
		"deploy-static-log-analysis",
		"read -r -s",
		"STATIC_LOG_ANALYSIS_DATABASE_DSN",
		"sslrootcert=system",
		"autocommit_before_ddl=false",
		"sed -E",
		"docker-buildx",
		"sha256sum --check",
		"systemctl enable static-log-analysis.service",
	} {
		if !strings.Contains(bootstrap, required) {
			t.Errorf("EC2 bootstrap does not contain %q", required)
		}
	}
	for _, forbidden := range []string{
		"aws ssm get-parameter",
		"--with-decryption",
		"password=",
		"AWS_SECRET_ACCESS_KEY",
		"systemctl enable --now static-log-analysis.service",
		"CockroachDB CA certificate URL",
		"cockroach-ca.crt",
	} {
		if strings.Contains(bootstrap, forbidden) {
			t.Errorf("EC2 bootstrap appears to persist secret material through %q", forbidden)
		}
	}
}

func TestAWSServiceRestartDoesNotRedeploySource(t *testing.T) {
	bootstrap := readProductionFile(t, "infra/aws/user-data.sh.tftpl")
	startMarker := "cat > /usr/local/sbin/start-static-log-analysis <<'SCRIPT'"
	deployMarker := "cat > /usr/local/sbin/deploy-static-log-analysis <<'SCRIPT'"
	unitMarker := "cat > /etc/systemd/system/static-log-analysis.service <<'UNIT'"

	start := scriptBetween(t, bootstrap, startMarker, deployMarker)
	for _, forbidden := range []string{"git ", "--build", "SLA_REPOSITORY_URL", "SLA_REPOSITORY_REF"} {
		if strings.Contains(start, forbidden) {
			t.Errorf("routine service startup can redeploy source through %q", forbidden)
		}
	}
	if !strings.Contains(start, "--no-build") {
		t.Fatal("routine service startup does not require the already-deployed image")
	}

	deploy := scriptBetween(t, bootstrap, deployMarker, unitMarker)
	for _, required := range []string{
		`git -C "$repository" fetch --depth 1 origin "$SLA_REPOSITORY_REF"`,
		`git -C "$repository" reset --hard FETCH_HEAD`,
		"docker compose",
		"build",
		"systemctl restart static-log-analysis.service",
	} {
		if !strings.Contains(deploy, required) {
			t.Errorf("explicit deployment helper does not contain %q", required)
		}
	}
}

func TestPublicDashboardIsAuthenticatedAndReadOnlyAtItsBoundaries(t *testing.T) {
	main := readProductionFile(t, "infra/aws/main.tf")
	compose := readProductionFile(t, "deploy/aws/compose.yaml")
	bootstrap := readProductionFile(t, "infra/aws/user-data.sh.tftpl")
	caddy := readProductionFile(t, "deploy/aws/Caddyfile")

	for _, required := range []string{
		`actions   = ["sqs:GetQueueAttributes"]`,
		`from_port   = ingress.value`,
		`dashboard_ingress_ports`,
		`dashboard_enabled`,
	} {
		if !strings.Contains(main, required) {
			t.Errorf("dashboard infrastructure does not contain %q", required)
		}
	}
	for _, forbidden := range []string{`sqs:ReceiveMessage`, `sqs:DeleteMessage`, `sqs:ChangeMessageVisibility`} {
		if strings.Contains(main, forbidden) {
			t.Errorf("dashboard can mutate the assignment queue through %q", forbidden)
		}
	}
	for _, required := range []string{
		"dashboard:",
		"dashboard-auth-migrate:",
		"dashboard-create-admin:",
		"dashboard-auth:",
		"caddy:",
		`profiles: ["dashboard"]`,
		"BETTER_AUTH_SECRET",
		"BETTER_AUTH_DATABASE_PATH",
		"ANALYSIS_OVERVIEW_URL",
	} {
		if !strings.Contains(compose, required) {
			t.Errorf("production Compose does not contain %q", required)
		}
	}
	for _, required := range []string{
		"set-static-log-analysis-dashboard-auth",
		"create-static-log-analysis-dashboard-admin",
		"/etc/static-log-analysis/dashboard.env",
		"read -r -s",
		"openssl rand -base64 48",
		"BETTER_AUTH_SECRET",
		"dashboard-create-admin",
	} {
		if !strings.Contains(bootstrap, required) {
			t.Errorf("dashboard bootstrap does not contain %q", required)
		}
	}
	for _, required := range []string{"reverse_proxy dashboard:3000", "Strict-Transport-Security", "X-Frame-Options"} {
		if !strings.Contains(caddy, required) {
			t.Errorf("dashboard TLS proxy does not contain %q", required)
		}
	}

	authConfig := readRepositoryFile(t, "services/dashboard/lib/auth.ts")
	for _, required := range []string{"disableSignUp: true", "minPasswordLength: 9", "plugins: [admin()", "BETTER_AUTH_DATABASE_PATH"} {
		if !strings.Contains(authConfig, required) {
			t.Errorf("dashboard authentication config does not contain %q", required)
		}
	}
	loginIdentity := readRepositoryFile(t, "services/dashboard/lib/login-identity.ts")
	for _, required := range []string{`normalized === "admin"`, `admin@static-log-analysis.local`} {
		if !strings.Contains(loginIdentity, required) {
			t.Errorf("temporary dashboard identity mapping does not contain %q", required)
		}
	}
	for _, file := range []string{
		"services/dashboard/package.json",
		"services/dashboard/lib/auth.ts",
		"services/static-log-analysis/deploy/aws/compose.yaml",
		"services/static-log-analysis/infra/aws/user-data.sh.tftpl",
	} {
		contents := readRepositoryFile(t, file)
		for _, forbidden := range []string{"@clerk", "CLERK_SECRET_KEY", "DASHBOARD_ALLOWED_EMAILS"} {
			if strings.Contains(contents, forbidden) {
				t.Errorf("%s still contains obsolete hosted-auth setting %q", file, forbidden)
			}
		}
	}
}

func TestProductionImageProvidesSystemCertificateAuthorities(t *testing.T) {
	dockerfile := readProductionFile(t, "Dockerfile")
	if !strings.Contains(dockerfile, "apk add --no-cache ca-certificates") {
		t.Fatal("production image does not install system certificate authorities")
	}
}

func TestAWSDeploymentDoesNotRequireAParameterStoreSecret(t *testing.T) {
	for _, file := range []string{
		"infra/aws/main.tf",
		"infra/aws/variables.tf",
		"infra/aws/terraform.tfvars.example",
		"infra/aws/user-data.sh.tftpl",
	} {
		contents := readProductionFile(t, file)
		for _, obsolete := range []string{
			"database_dsn_parameter_name",
			"database_kms_key_arn",
			"ReadCockroachConnection",
			"aws ssm get-parameter",
		} {
			if strings.Contains(contents, obsolete) {
				t.Errorf("%s still requires Parameter Store through %q", file, obsolete)
			}
		}
	}
}

func TestDemoBootstrapPinsTheUpstreamAppAndExportsPaymentLogs(t *testing.T) {
	bootstrap := readProductionFile(t, "infra/aws/demo-user-data.sh.tftpl")
	for _, required := range []string{
		"https://github.com/open-telemetry/opentelemetry-demo.git",
		"git -C \"$repository\" fetch --depth 1 origin",
		"driver: awslogs",
		"awslogs-group:",
		"payment:",
		"--no-build",
		"systemctl enable --now opentelemetry-demo.service",
	} {
		if !strings.Contains(bootstrap, required) {
			t.Errorf("demo bootstrap does not contain %q", required)
		}
	}
	for _, forbidden := range []string{
		"AWS_ACCESS_KEY_ID",
		"AWS_SECRET_ACCESS_KEY",
		"0.0.0.0/0",
		"latest-payment",
	} {
		if strings.Contains(bootstrap, forbidden) {
			t.Errorf("demo bootstrap contains unsafe or mutable value %q", forbidden)
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

func terraformResourceBlock(t *testing.T, contents, declaration string) string {
	t.Helper()
	start := strings.Index(contents, declaration)
	if start < 0 {
		t.Fatalf("Terraform declaration %q is missing", declaration)
	}
	rest := contents[start+len(declaration):]
	end := strings.Index(rest, "\nresource \"")
	if end < 0 {
		return contents[start:]
	}
	return contents[start : start+len(declaration)+end]
}

func scriptBetween(t *testing.T, contents, startMarker, endMarker string) string {
	t.Helper()
	start := strings.Index(contents, startMarker)
	if start < 0 {
		t.Fatalf("start marker %q is missing", startMarker)
	}
	end := strings.Index(contents[start+len(startMarker):], endMarker)
	if end < 0 {
		t.Fatalf("end marker %q is missing after %q", endMarker, startMarker)
	}
	return contents[start : start+len(startMarker)+end]
}
