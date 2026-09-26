{{/*
Expand the name of the chart.
*/}}
{{- define "fast-sandbox.name" -}}
{{- default "fast-sandbox" .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "fast-sandbox.fullname" -}}
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
{{- define "fast-sandbox.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "fast-sandbox.labels" -}}
helm.sh/chart: {{ include "fast-sandbox.chart" . }}
app.kubernetes.io/name: {{ include "fast-sandbox.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/part-of: fast-sandbox
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels for the all-in-one controller (also selected by the
fast-sandbox-fastpath Service).
*/}}
{{- define "fast-sandbox.controllerSelectorLabels" -}}
app: fast-sandbox-controller
control-plane: controller-manager
fast-sandbox.io/control-plane-role: all
{{- end }}

{{/*
Component image reference. Call with a dict: repository, tag, appVersion. An
empty tag falls back to the release-<appVersion> companion image tag built
from the source pinned in manifests/third-party/fast-sandbox.commit.
*/}}
{{- define "fast-sandbox.image" -}}
{{- $tag := .tag | default (printf "release-%s" .appVersion) -}}
{{- printf "%s:%s" .repository $tag -}}
{{- end }}

{{/*
Namespace the workloads run in.
*/}}
{{- define "fast-sandbox.namespace" -}}
{{- default "opensandbox-system" .Values.systemNamespace }}
{{- end }}
