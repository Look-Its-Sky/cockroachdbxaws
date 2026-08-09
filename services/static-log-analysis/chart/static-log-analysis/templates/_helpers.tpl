{{- define "static-log-analysis.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "static-log-analysis.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else if contains (include "static-log-analysis.name" .) .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (include "static-log-analysis.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "static-log-analysis.analysisServerName" -}}
{{- default (include "static-log-analysis.fullname" .) .Values.tls.analysisServerName -}}
{{- end -}}

{{- define "static-log-analysis.labels" -}}
app.kubernetes.io/name: {{ include "static-log-analysis.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{- define "static-log-analysis.selectorLabels" -}}
app.kubernetes.io/name: {{ include "static-log-analysis.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "static-log-analysis.image" -}}
{{- if .Values.image.digest -}}
{{- printf "%s:%s@%s" .Values.image.repository .Values.image.tag .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.image.repository .Values.image.tag -}}
{{- end -}}
{{- end -}}

{{- define "static-log-analysis.collectorImage" -}}
{{- if .Values.collector.image.digest -}}
{{- printf "%s:%s@%s" .Values.collector.image.repository .Values.collector.image.tag .Values.collector.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.collector.image.repository .Values.collector.image.tag -}}
{{- end -}}
{{- end -}}

{{- define "static-log-analysis.outboxServiceAccount" -}}
{{- default "static-log-analysis-outbox" .Values.outbox.serviceAccount.name -}}
{{- end -}}

{{- define "static-log-analysis.cloudwatchServiceAccount" -}}
{{- default "static-log-analysis-cloudwatch" .Values.cloudwatch.serviceAccount.name -}}
{{- end -}}

{{- define "static-log-analysis.cloudwatchGroups" -}}
{{- $seen := dict -}}
{{- range $index, $source := .Values.cloudwatch.sources -}}
{{- if hasKey $seen $source.logGroup -}}
{{- fail (printf "cloudwatch.sources names log group %q more than once" $source.logGroup) -}}
{{- end -}}
{{- $_ := set $seen $source.logGroup true -}}
{{- if $index }},{{ end -}}{{ $source.logGroup }}={{ $source.service }}={{ $source.environment }}
{{- end -}}
{{- end -}}
