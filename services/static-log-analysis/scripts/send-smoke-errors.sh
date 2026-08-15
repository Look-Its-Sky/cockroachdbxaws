#!/bin/sh
set -eu

service_name=${1:-example-service}
deployment_environment=${2:-development}
endpoint=${SLA_OTLP_HTTP_ENDPOINT:-http://127.0.0.1:4318/v1/logs}

for value in "$service_name" "$deployment_environment"; do
  case "$value" in
    ""|*[!A-Za-z0-9._-]*)
      echo "service and environment values may contain only letters, digits, dot, underscore, and hyphen" >&2
      exit 2
      ;;
  esac
done

# Put the events just behind the engine's two-minute allowed-lateness frontier
# so the smoke check finalizes promptly instead of waiting two minutes.
seconds=$(( $(date +%s) - 121 ))
base="${seconds}000000000"

# Five distinct event timestamps intentionally exercise the ordinary five-in-
# five rule. No log.record.uid is sent: this also proves the documented
# derived:v1 compatibility path used by repositories without native IDs.
curl --fail --silent --show-error \
  -H 'Content-Type: application/json' \
  --data-binary @- \
  "$endpoint" <<EOF
{
  "resourceLogs": [{
    "resource": {"attributes": [
      {"key": "service.name", "value": {"stringValue": "${service_name}"}},
      {"key": "deployment.environment.name", "value": {"stringValue": "${deployment_environment}"}}
    ]},
    "scopeLogs": [{
      "scope": {"name": "static-log-analysis-smoke", "version": "1.0"},
      "logRecords": [
        {"timeUnixNano": "${base}", "observedTimeUnixNano": "${base}", "severityNumber": 17, "severityText": "ERROR", "body": {"stringValue": "plug-in deployment smoke failure"}},
        {"timeUnixNano": "$((base + 1))", "observedTimeUnixNano": "$((base + 1))", "severityNumber": 17, "severityText": "ERROR", "body": {"stringValue": "plug-in deployment smoke failure"}},
        {"timeUnixNano": "$((base + 2))", "observedTimeUnixNano": "$((base + 2))", "severityNumber": 17, "severityText": "ERROR", "body": {"stringValue": "plug-in deployment smoke failure"}},
        {"timeUnixNano": "$((base + 3))", "observedTimeUnixNano": "$((base + 3))", "severityNumber": 17, "severityText": "ERROR", "body": {"stringValue": "plug-in deployment smoke failure"}},
        {"timeUnixNano": "$((base + 4))", "observedTimeUnixNano": "$((base + 4))", "severityNumber": 17, "severityText": "ERROR", "body": {"stringValue": "plug-in deployment smoke failure"}}
      ]
    }]
  }]
}
EOF

echo "sent five ${service_name}/${deployment_environment} error records to ${endpoint}"
echo "check http://127.0.0.1:9464/metrics and the configured assignment queue"
