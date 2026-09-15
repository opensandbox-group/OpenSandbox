{{/*
Expand the name of the chart.
*/}}
{{- define "opensandbox-ingress-gateway.name" -}}
{{- default "ingress-gateway" .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "opensandbox-ingress-gateway.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Chart name and version for labels.
*/}}
{{- define "opensandbox-ingress-gateway.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "opensandbox-ingress-gateway.labels" -}}
helm.sh/chart: {{ include "opensandbox-ingress-gateway.chart" . }}
{{ include "opensandbox-ingress-gateway.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/component: ingress-gateway
app.kubernetes.io/part-of: opensandbox
{{- end }}

{{/*
Selector labels
*/}}
{{- define "opensandbox-ingress-gateway.selectorLabels" -}}
app.kubernetes.io/name: {{ include "opensandbox-ingress-gateway.fullname" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Namespace to use
*/}}
{{- define "opensandbox-ingress-gateway.namespace" -}}
{{- if .Values.namespaceOverride }}
{{- .Values.namespaceOverride }}
{{- else }}
{{- print "opensandbox-system" }}
{{- end }}
{{- end }}

{{/*
ServiceAccount name (same as fullname, always created by chart)
*/}}
{{- define "opensandbox-ingress-gateway.serviceAccountName" -}}
{{- include "opensandbox-ingress-gateway.fullname" . }}
{{- end }}

{{/*
ClusterRole name for gateway
*/}}
{{- define "opensandbox-ingress-gateway.roleName" -}}
{{- include "opensandbox-ingress-gateway.fullname" . }}-reader-full
{{- end }}

{{/*
Gateway image with tag (prepend v to semver if missing)
*/}}
{{- define "opensandbox-ingress-gateway.image" -}}
{{- $tag := .Values.gateway.image.tag | default "v1.0.2" }}
{{- $finalTag := $tag }}
{{- if and (not (hasPrefix "v" $tag)) (regexMatch "^[0-9]+\\.[0-9]+\\.[0-9]+" $tag) }}
{{- $finalTag = printf "v%s" $tag }}
{{- end }}
{{- printf "%s:%s" .Values.gateway.image.repository $finalTag }}
{{- end }}

{{/*
RBAC apiVersion
*/}}
{{- define "opensandbox-ingress-gateway.rbac.apiVersion" -}}
{{- if .Capabilities.APIVersions.Has "rbac.authorization.k8s.io/v1" }}
{{- print "rbac.authorization.k8s.io/v1" }}
{{- else }}
{{- print "rbac.authorization.k8s.io/v1beta1" }}
{{- end }}
{{- end }}

{{/*
Validate secureAccess: keys and existingSecret are mutually exclusive.
*/}}
{{- define "opensandbox-ingress-gateway.secureAccess.validate" -}}
{{- if and .Values.gateway.secureAccess.keys .Values.gateway.secureAccess.existingSecret }}
{{- fail "gateway.secureAccess: set either keys or existingSecret, not both" }}
{{- end }}
{{- end }}
