# Changelog

All notable changes to this project are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project aims to follow
[Semantic Versioning](https://semver.org/).

## [Unreleased]

## [0.1.0] - 2026-11-06

The first public release. smokealarm runs inside your cluster, checks it with a fixed set of
checks that need no configuration, and tells you when a problem starts or ends.

### Checks

- Pods: `pod-crashloop`, `pod-image-pull`, `pod-oomkilled`, `pod-restarts`, `pod-pending`.
- Workloads, nodes and jobs: `workload-replicas`, `node-not-ready`, `node-pressure`, `job-failed`, `cronjob-missed`.
- Storage: `pvc-pending`, and `pvc-usage`, which asks the kubelets directly and needs only `get` on `nodes/stats`.
- cert-manager and Argo CD, if they are installed: `cert-not-ready`, `cert-expiry`, `argocd-health`, `argocd-sync`.
- `api-connection`: smokealarm itself cannot keep its copy of the cluster up to date.
- Checks can be switched off, and objects or namespaces ignored with annotations ([docs/checks.md](docs/checks.md)).

### Alerting

- A problem is reported once it has lasted `SMOKEALARM_ALERT_AFTER`, and only reported as resolved after `SMOKEALARM_RESOLVE_AFTER`, so short flaps stay quiet.
- Warning and critical. A problem that goes from warning to critical stays the same problem and is reported again.
- Reminders for critical problems that stay open (`SMOKEALARM_CRITICAL_REPEAT_INTERVAL`, off by default).
- A node that is down is the alarm: the problems it causes are held back and listed under it.
- Acknowledge a critical problem (no more reminders), and silence one problem until a fixed time with a downtime.

### Notifications

- Mail (SMTP with STARTTLS or TLS), ntfy, Telegram, Slack, Microsoft Teams (Workflows) and a webhook with versioned JSON ([docs/notifications.md](docs/notifications.md)).
- Routing by severity per target and by annotations on the object or namespace ([docs/routing.md](docs/routing.md)).
- Your own wording with Go templates per target ([docs/message-templates.md](docs/message-templates.md)).
- `SMOKEALARM_TIME_ZONE` for all times shown or entered; the webhook's time fields and the stored data stay in UTC.

### Web interface

- Current problems, history with a namespace filter, and a status page with the active checks, skipped checks and the last error of each target. Server-rendered, light and dark, works on a phone.
- Login with a local admin password (bcrypt hash) and optionally with OIDC next to it ([docs/oidc.md](docs/oidc.md)).

### Packaging

- Container image `ghcr.io/gagaschu/smokealarm` for `linux/amd64` and `linux/arm64`: one static binary on `distroless/static:nonroot`, writes only to `/data` (SQLite).
- Helm chart in `deploy/helm/smokealarm` with read-only RBAC, switches for the optional permissions, and examples in `deploy/helm/examples` ([docs/installation.md](docs/installation.md)).

[Unreleased]: https://github.com/gagaschu/smokealarm/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/gagaschu/smokealarm/releases/tag/v0.1.0
