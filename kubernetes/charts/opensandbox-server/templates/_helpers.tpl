{{/*
Expand the name of the chart.
*/}}
{{- define "opensandbox-server.name" -}}
{{- default "opensandbox-server" .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "opensandbox-server.fullname" -}}
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
{{- define "opensandbox-server.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "opensandbox-server.labels" -}}
helm.sh/chart: {{ include "opensandbox-server.chart" . }}
{{ include "opensandbox-server.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/component: opensandbox-server
app.kubernetes.io/part-of: opensandbox
{{- end }}

{{/*
Selector labels
*/}}
{{- define "opensandbox-server.selectorLabels" -}}
app.kubernetes.io/name: {{ include "opensandbox-server.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Namespace to use
*/}}
{{- define "opensandbox-server.namespace" -}}
{{- if .Values.namespaceOverride }}
{{- .Values.namespaceOverride }}
{{- else }}
{{- print "opensandbox-system" }}
{{- end }}
{{- end }}

{{/*
ServiceAccount name (same as fullname, always created by chart)
*/}}
{{- define "opensandbox-server.serviceAccountName" -}}
{{- include "opensandbox-server.fullname" . }}
{{- end }}

{{/*
Server image with tag (prepend v to semver if missing)
*/}}
{{- define "opensandbox-server.serverImage" -}}
{{- $tag := .Values.server.image.tag | default .Chart.AppVersion }}
{{- $finalTag := $tag }}
{{- if and (not (hasPrefix "v" $tag)) (regexMatch "^[0-9]+\\.[0-9]+\\.[0-9]+" $tag) }}
{{- $finalTag = printf "v%s" $tag }}
{{- end }}
{{- printf "%s:%s" .Values.server.image.repository $finalTag }}
{{- end }}

{{/*
Image pull policy
*/}}
{{- define "opensandbox-server.imagePullPolicy" -}}
{{- .Values.server.image.pullPolicy | default "IfNotPresent" }}
{{- end }}

{{/*
RBAC apiVersion
*/}}
{{- define "opensandbox-server.rbac.apiVersion" -}}
{{- if .Capabilities.APIVersions.Has "rbac.authorization.k8s.io/v1" }}
{{- print "rbac.authorization.k8s.io/v1" }}
{{- else }}
{{- print "rbac.authorization.k8s.io/v1beta1" }}
{{- end }}
{{- end }}

{{/*
ClusterRole name for server
*/}}
{{- define "opensandbox-server.roleName" -}}
{{- include "opensandbox-server.fullname" . }}-role
{{- end }}

{{/*
Render [ingress] TOML block from server.gateway.
When server.gateway.enabled=true: mode=gateway + gateway.address + gateway.route.mode; otherwise mode=direct.
*/}}
{{- define "opensandbox-server.ingressConfigToml" -}}
[ingress]
mode = {{ .Values.server.gateway.enabled | ternary "gateway" "direct" | quote }}
{{- if .Values.server.gateway.enabled }}

gateway.address = {{ .Values.server.gateway.host | quote }}
gateway.route.mode = {{ .Values.server.gateway.gatewayRouteMode | quote }}
{{- end }}
{{- if and .Values.server.gateway.enabled .Values.server.gateway.secureAccess.keys }}

[ingress.secure_access]
active_key = {{ .Values.server.gateway.secureAccess.activeKey | quote }}
{{- range .Values.server.gateway.secureAccess.keys }}
[[ingress.secure_access.keys]]
key_id = {{ .key_id | quote }}
key = {{ .key | quote }}
{{- end }}
{{- end }}

{{- end }}

{{/*
Gateway fixed name (independent of server)
*/}}
{{- define "opensandbox-server.ingressGatewayFullname" -}}
opensandbox-ingress-gateway
{{- end }}

{{- define "opensandbox-server.ingressGatewaySelectorLabels" -}}
app.kubernetes.io/name: {{ include "opensandbox-server.ingressGatewayFullname" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "opensandbox-server.ingressGatewayImage" -}}
{{- $tag := .Values.server.gateway.image.tag | default "v1.0.2" }}
{{- printf "%s:%s" .Values.server.gateway.image.repository $tag }}
{{- end }}
