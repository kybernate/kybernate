{{- define "kybernate.name" -}}
{{- .Chart.Name -}}
{{- end -}}

{{- define "kybernate.labels" -}}
app.kubernetes.io/name: {{ include "kybernate.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{- define "kybernate.nodeAgent.name" -}}
{{- printf "%s-node-agent" .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "kybernate.nodeAgent.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kybernate.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: node-agent
{{- end -}}

{{- define "kybernate.nodeAgent.image" -}}
{{- printf "%s:%s" .Values.nodeAgent.image.repository (.Values.nodeAgent.image.tag | default .Chart.AppVersion) -}}
{{- end -}}

{{/* Non-empty if GPU leases are enabled; they need the device plugin. */}}
{{- define "kybernate.leases" -}}
{{- if and .Values.nodeAgent.enabled .Values.nodeAgent.devicePlugin.enabled .Values.nodeAgent.leases.enabled -}}true{{- end -}}
{{- end -}}
