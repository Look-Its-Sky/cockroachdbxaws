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

func TestLocalComposeCanSelectAFreshJournalWithoutDeletingTheOldVolume(t *testing.T) {
	compose := readProductionFile(t, "compose.yaml")
	if !strings.Contains(compose, `name: ${SLA_ANALYSIS_JOURNAL_VOLUME:-static-log-analysis_analysis-journal}`) {
		t.Fatal("local Compose cannot select a fresh named journal volume for a bounded smoke deployment")
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
		`base64gzip(file("${path.module}/../../deploy/aws/deploy-images.sh"))`,
	} {
		if !strings.Contains(main, required) {
			t.Errorf("AWS infrastructure does not compress embedded deployment file through %q", required)
		}
	}
	if strings.Count(bootstrap, "base64 --decode | gzip --decompress") != 3 {
		t.Fatal("AWS bootstrap does not decompress all embedded deployment files")
	}
}

func TestAWSBootstrapRefreshSkipsSatisfiedHostTooling(t *testing.T) {
	bootstrap := readProductionFile(t, "infra/aws/user-data.sh.tftpl")
	for _, required := range []string{
		`command -v docker`,
		`command -v aws`,
		`command -v openssl`,
		`command -v curl`,
		`if [ "$${#missing_packages[@]}" -gt 0 ]`,
		`if ! printf '%s  %s\n' "$compose_sha256" "$compose_plugin" | sha256sum --check`,
		`install -o root -g root -m 0755 "$temporary_compose" "$compose_plugin"`,
	} {
		if !strings.Contains(bootstrap, required) {
			t.Errorf("in-place bootstrap refresh does not contain %q", required)
		}
	}
}

func TestAWSServiceBootstrapChangesPreserveTheEncryptedInstanceDisk(t *testing.T) {
	main := readProductionFile(t, "infra/aws/main.tf")
	service := terraformResourceBlock(t, main, `resource "aws_instance" "service"`)
	if !strings.Contains(service, "user_data_replace_on_change = false") {
		t.Fatal("service bootstrap changes can replace the instance and delete its local durable state")
	}
}

func TestAWSInstancesUseAnExplicitPinnedMachineImage(t *testing.T) {
	main := readProductionFile(t, "infra/aws/main.tf")
	variables := readProductionFile(t, "infra/aws/variables.tf")
	if strings.Contains(main, `data "aws_ami"`) || strings.Contains(main, "most_recent = true") {
		t.Fatal("a moving most-recent AMI can replace stateful hosts during an application deployment")
	}
	if !strings.Contains(variables, `variable "machine_image_id"`) {
		t.Fatal("AWS infrastructure does not require an explicitly pinned machine image")
	}
	for _, resource := range []string{
		`resource "aws_instance" "service"`,
		`resource "aws_instance" "demo"`,
	} {
		instance := terraformResourceBlock(t, main, resource)
		if !strings.Contains(instance, "ami                         = var.machine_image_id") {
			t.Errorf("%s does not use the explicitly pinned machine image", resource)
		}
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
		"deploy-static-log-analysis-images",
		"read -r -s",
		"STATIC_LOG_ANALYSIS_DATABASE_DSN",
		"sslrootcert=system",
		"autocommit_before_ddl=false",
		"sed -E",
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

func TestAWSServiceRestartUsesOnlyDigestPinnedImages(t *testing.T) {
	bootstrap := readProductionFile(t, "infra/aws/user-data.sh.tftpl")
	startMarker := "cat > /usr/local/sbin/start-static-log-analysis <<'SCRIPT'"
	unitMarker := "cat > /etc/systemd/system/static-log-analysis.service <<'UNIT'"

	start := scriptBetween(t, bootstrap, startMarker, unitMarker)
	for _, forbidden := range []string{"git ", "--build", "SLA_REPOSITORY_URL", "docker compose build"} {
		if strings.Contains(start, forbidden) {
			t.Errorf("routine service startup can redeploy source through %q", forbidden)
		}
	}
	for _, required := range []string{
		"/etc/static-log-analysis/release.env",
		"SLA_ANALYSIS_IMAGE",
		"SLA_DASHBOARD_IMAGE",
		"SLA_DASHBOARD_ADMIN_IMAGE",
		"@sha256:",
		"--no-build",
	} {
		if !strings.Contains(start, required) {
			t.Errorf("routine service startup does not enforce %q", required)
		}
	}
	if !strings.Contains(bootstrap, "EnvironmentFile=-/etc/static-log-analysis/release.env") {
		t.Fatal("systemd stop and start do not share the selected digest release environment")
	}
}

func TestAWSImagePublisherBuildsEveryArtifactFromOneCleanCommit(t *testing.T) {
	publisher := readProductionFile(t, "scripts/publish-aws-images.sh")
	for _, required := range []string{
		`git -C "$repository_root" rev-parse HEAD`,
		`git -C "$repository_root" status --porcelain`,
		`export DOCKER_CONFIG=$docker_config`,
		`buildx-v0.34.1.linux-amd64`,
		`sha256sum --check`,
		`--platform linux/amd64`,
		`existing_digest=$(published_digest "$repository_url")`,
		`reusing the existing immutable commit artifact`,
		`publish_image "$analysis_repository" "$service_root" ""`,
		`publish_image "$dashboard_repository" "$repository_root/services/dashboard" runtime`,
		`publish_image "$dashboard_admin_repository" "$repository_root/services/dashboard" admin`,
		`aws ecr describe-images`,
		`--push`,
	} {
		if !strings.Contains(publisher, required) {
			t.Errorf("off-host image publisher does not contain %q", required)
		}
	}
}

func TestAWSComposeRequiresPrebuiltImmutableImages(t *testing.T) {
	compose := readProductionFile(t, "deploy/aws/compose.yaml")
	for _, required := range []string{
		"${SLA_ANALYSIS_IMAGE:?",
		"${SLA_DASHBOARD_IMAGE:?",
		"${SLA_DASHBOARD_ADMIN_IMAGE:?",
	} {
		if !strings.Contains(compose, required) {
			t.Errorf("production Compose does not require %q", required)
		}
	}
	if strings.Contains(compose, "build:") {
		t.Fatal("production Compose still builds application source on the stateful host")
	}
}

func TestAWSRegistryIsImmutableAndPullAccessIsRepositoryScoped(t *testing.T) {
	main := readProductionFile(t, "infra/aws/main.tf")
	for _, required := range []string{
		`resource "aws_ecr_repository" "release"`,
		`image_tag_mutability = "IMMUTABLE"`,
		`scan_on_push = true`,
		`"analysis"`,
		`"dashboard"`,
		`"dashboard-admin"`,
		`actions   = ["ecr:GetAuthorizationToken"]`,
		`resources = ["*"]`,
		`"ecr:BatchCheckLayerAvailability"`,
		`"ecr:GetDownloadUrlForLayer"`,
		`"ecr:BatchGetImage"`,
		`resources = [for repository in aws_ecr_repository.release : repository.arn]`,
	} {
		if !strings.Contains(main, required) {
			t.Errorf("immutable registry contract does not contain %q", required)
		}
	}
}

func TestAWSImageCutoverIsBoundedAndRollsBack(t *testing.T) {
	deployer := readProductionFile(t, "deploy/aws/deploy-images.sh")
	for _, required := range []string{
		`@sha256:[0-9a-f]{64}$`,
		`aws ecr get-login-password`,
		`docker pull "$analysis_image"`,
		`docker pull "$dashboard_image"`,
		`docker pull "$dashboard_admin_image"`,
		`previous_release_file`,
		`health_attempts=24`,
		`restore_previous_release`,
		`systemctl restart static-log-analysis.service`,
	} {
		if !strings.Contains(deployer, required) {
			t.Errorf("image deployment helper does not contain %q", required)
		}
	}
	for _, forbidden := range []string{"git ", "docker compose build", "latest"} {
		if strings.Contains(deployer, forbidden) {
			t.Errorf("image deployment helper contains mutable source deployment through %q", forbidden)
		}
	}
}

func TestPublicDashboardIsAuthenticatedAndReadOnlyAtItsBoundaries(t *testing.T) {
	main := readProductionFile(t, "infra/aws/main.tf")
	servicePolicy := scriptBetween(
		t,
		main,
		`data "aws_iam_policy_document" "service"`,
		`resource "aws_iam_role_policy" "service"`,
	)
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
		if strings.Contains(servicePolicy, forbidden) {
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
		"AGENT_API_URL",
		"AGENT_API_TOKEN",
		"SLA_RELEASE_SHA",
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
		"set -a",
	} {
		if !strings.Contains(bootstrap, required) {
			t.Errorf("dashboard bootstrap does not contain %q", required)
		}
	}
	for _, forbidden := range []string{"docker buildx", "docker compose build", "/swapfile"} {
		if strings.Contains(bootstrap, forbidden) {
			t.Errorf("dashboard bootstrap still builds source on the host through %q", forbidden)
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

func TestAgentRuntimeRoleCanOnlyConsumeAssignments(t *testing.T) {
	main := readProductionFile(t, "infra/aws/main.tf")
	trust := scriptBetween(
		t,
		main,
		`data "aws_iam_policy_document" "agent_runtime_trust"`,
		`resource "aws_iam_role" "agent_runtime"`,
	)
	if !strings.Contains(trust, `identifiers = ["tasks.apprunner.amazonaws.com"]`) {
		t.Error("agent runtime role is not restricted to the App Runner task service")
	}

	policy := scriptBetween(
		t,
		main,
		`data "aws_iam_policy_document" "agent_runtime"`,
		`resource "aws_iam_role_policy" "agent_runtime"`,
	)
	for _, required := range []string{
		`"sqs:ReceiveMessage"`,
		`"sqs:DeleteMessage"`,
		`"sqs:ChangeMessageVisibility"`,
		`"sqs:GetQueueAttributes"`,
		`resources = [aws_sqs_queue.assignments.arn]`,
	} {
		if !strings.Contains(policy, required) {
			t.Errorf("agent runtime policy does not contain %q", required)
		}
	}
	for _, forbidden := range []string{
		`"sqs:SendMessage"`,
		`aws_sqs_queue.dead_letter.arn`,
		`Resource = "*"`,
	} {
		if strings.Contains(policy, forbidden) {
			t.Errorf("agent runtime policy contains over-broad capability %q", forbidden)
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
