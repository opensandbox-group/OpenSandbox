{{/*
Expand the name of the chart.
*/}}
{{- define "opensandbox.name" -}}
{{- default "opensandbox" .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "opensandbox.fullname" -}}
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
Create chart name and version as used by the chart label.
*/}}
{{- define "opensandbox.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "opensandbox.labels" -}}
helm.sh/chart: {{ include "opensandbox.chart" . }}
{{ include "opensandbox.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "opensandbox.selectorLabels" -}}
app.kubernetes.io/name: {{ include "opensandbox.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
control-plane: controller-manager
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "opensandbox.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default "opensandbox-controller-manager" .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Get the namespace to use
*/}}
{{- define "opensandbox.namespace" -}}
{{- if .Values.namespaceOverride }}
{{- .Values.namespaceOverride }}
{{- else }}
{{- print "opensandbox-system" }}
{{- end }}
{{- end }}

{{/*
Controller image with automatic version prefix handling.
Prepends 'v' to semantic version tags (e.g., 0.0.1 -> v0.0.1) but preserves
special tags like 'latest', 'dev', 'main', etc. as-is.
*/}}
{{- define "opensandbox.controllerImage" -}}
{{- $tag := .Values.controller.image.tag | default .Chart.AppVersion }}
{{- $finalTag := $tag }}
{{- if and (not (hasPrefix "v" $tag)) (regexMatch "^[0-9]+\\.[0-9]+\\.[0-9]+" $tag) }}
{{- $finalTag = printf "v%s" $tag }}
{{- end }}
{{- printf "%s:%s" .Values.controller.image.repository $finalTag }}
{{- end }}

{{/*
Create the name for the leader election role
*/}}
{{- define "opensandbox.leaderElectionRoleName" -}}
{{- print "opensandbox-leader-election-role" }}
{{- end }}

{{/*
Create the name for the manager role
*/}}
{{- define "opensandbox.managerRoleName" -}}
{{- print "opensandbox-manager-role" }}
{{- end }}

{{/*
Create the name for the metrics auth role
*/}}
{{- define "opensandbox.metricsAuthRoleName" -}}
{{- print "opensandbox-metrics-auth-role" }}
{{- end }}

{{/*
Create the name for the metrics reader role (bind to your scraper's ServiceAccount)
*/}}
{{- define "opensandbox.metricsReaderRoleName" -}}
{{- print "opensandbox-metrics-reader" }}
{{- end }}

{{/*
Return the appropriate apiVersion for RBAC APIs
*/}}
{{- define "opensandbox.rbac.apiVersion" -}}
{{- if .Capabilities.APIVersions.Has "rbac.authorization.k8s.io/v1" }}
{{- print "rbac.authorization.k8s.io/v1" }}
{{- else }}
{{- print "rbac.authorization.k8s.io/v1beta1" }}
{{- end }}
{{- end }}

{{/*
Return image pull policy
*/}}
{{- define "opensandbox.imagePullPolicy" -}}
{{- .Values.controller.image.pullPolicy | default "IfNotPresent" }}
{{- end }}
