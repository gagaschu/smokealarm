{{/* Name of the chart. */}}
{{- define "smokealarm.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Name used for all objects: the release name, unless it already contains the chart name. */}}
{{- define "smokealarm.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/* Labels that identify the objects of this release. */}}
{{- define "smokealarm.selectorLabels" -}}
app.kubernetes.io/name: {{ include "smokealarm.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "smokealarm.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "smokealarm.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "smokealarm.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "smokealarm.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/* The Secret with the password hash (and SMTP login): the user's own, or the one this chart creates. */}}
{{- define "smokealarm.secretName" -}}
{{- default (include "smokealarm.fullname" .) .Values.existingSecret -}}
{{- end -}}

{{/* The claim used for /data. */}}
{{- define "smokealarm.claimName" -}}
{{- default (include "smokealarm.fullname" .) .Values.persistence.existingClaim -}}
{{- end -}}

{{/*
The link to the web UI for mails: the configured one, or one built from the
ingress host, or empty.
*/}}
{{- define "smokealarm.baseUrl" -}}
{{- if .Values.config.baseUrl -}}
{{- .Values.config.baseUrl -}}
{{- else if and .Values.ingress.enabled .Values.ingress.host -}}
{{- printf "%s://%s" (ternary "https" "http" .Values.ingress.tls.enabled) .Values.ingress.host -}}
{{- end -}}
{{- end -}}

{{/*
The checks to switch off: the user's list plus the checks whose permission is
off. Without the permission they could not read their data.
*/}}
{{- define "smokealarm.disabledChecks" -}}
{{- $list := list -}}
{{- range .Values.disabledChecks -}}
{{- $list = append $list . -}}
{{- end -}}
{{- if not .Values.rbac.nodesProxy -}}
{{- $list = append $list "pvc-usage" -}}
{{- end -}}
{{- if not .Values.rbac.certManager -}}
{{- $list = concat $list (list "cert-not-ready" "cert-expiry") -}}
{{- end -}}
{{- if not .Values.rbac.argocd -}}
{{- $list = concat $list (list "argocd-health" "argocd-sync") -}}
{{- end -}}
{{- $list | uniq | join "," -}}
{{- end -}}
