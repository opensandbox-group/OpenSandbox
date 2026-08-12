{{- define "opensandbox-node-agent.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "opensandbox-node-agent.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (include "opensandbox-node-agent.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "opensandbox-node-agent.namespace" -}}
{{- default .Release.Namespace .Values.namespaceOverride -}}
{{- end -}}

{{- define "opensandbox-node-agent.rbacName" -}}
{{- printf "%s-%s" (include "opensandbox-node-agent.namespace" .) (include "opensandbox-node-agent.fullname" .) | trunc 253 | trimSuffix "-" -}}
{{- end -}}

{{- define "opensandbox-node-agent.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
app.kubernetes.io/name: {{ include "opensandbox-node-agent.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/component: node-agent
{{- end -}}

{{- define "opensandbox-node-agent.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "opensandbox-node-agent.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "opensandbox-node-agent.image" -}}
{{- $tag := default .Chart.AppVersion .Values.image.tag -}}
{{- if and (not (hasPrefix "v" $tag)) (regexMatch "^[0-9]+\\.[0-9]+\\.[0-9]+" $tag) -}}
{{- $tag = printf "v%s" $tag -}}
{{- end -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end -}}
