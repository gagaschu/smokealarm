# Changelog

All notable changes to this project are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project aims to follow
[Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- **Reminders for critical problems**: `SMOKEALARM_CRITICAL_REPEAT_INTERVAL` reports critical problems that are still open again at that interval. Off by default.
- **Acknowledge critical problems** in the web UI: an acknowledged problem stays on the overview with a mark and sends no more reminders until it is resolved. "Resolved" is still reported. A first mail that has not gone out yet is dropped.
- An acknowledgement can be removed again. The problem then behaves like any other, so a mail can go out in the next run.
- **Downtimes** in the web UI: silence one problem until a fixed time, from the overview or the history. While it runs, the problem sends no mail, including "resolved". If it is still active when the downtime ends, it is reported again; if it went away, nothing is sent. The **Downtimes** page lists running downtimes and can end them early.
- Actions in the web UI that change state are protected against cross-site requests.
- **More notification targets** besides mail: a generic **webhook** (JSON, optional auth header), **ntfy** (ntfy.sh or self-hosted, optional access token), **Telegram** (one or more chats), **Slack** (incoming webhook) and **Microsoft Teams** (Workflows webhook with an Adaptive Card; the old Office 365 connector URLs no longer work and are refused). Any combination can be on. They get the same notifications as mail, at the same times. Other services can be reached with the webhook and a relay such as Apprise or n8n.
- Every notification target remembers on its own what it was told: a target that is down is retried without sending duplicates to the others, also after a restart. A target added later does not get the history.
- The status page lists every notification target with the time of its last message and its last error.
- Helm chart: `notifications.*` values; tokens and webhook URLs go into the Secret only. New example `notify-targets.yaml`.

### Changed

- **New look for the web UI**: a top bar with logo, slimmer buttons with icons and a clear main action, problem cards with severity counters above them, tables as cards, a status page with state pills, and a favicon. Still no JavaScript and no external files; light and dark mode and the phone layout are kept. The history shows the duration below the time and the severity above the reason, so the table fits on a small laptop screen.
- The README shows screenshots of the web interface.
- Database migration `005_deliveries.sql`. On the upgrade, the configured targets start from the current state, so nothing is sent twice.

## [0.1.0] - not released yet

First version.

### Added

- **Checks** (16): pod crash loops, image pull errors, OOM kills, many restarts and pods stuck pending; Deployments, StatefulSets and DaemonSets without enough available replicas; nodes that are not ready or under pressure; failed Jobs and missed CronJob runs; pending PVCs and volumes that are almost full; cert-manager certificates that are not ready or about to expire; Argo CD applications that are degraded, missing or out of sync. See the README for the full list with severities.
- **State handling**: problems are pending, then firing after `SMOKEALARM_ALERT_AFTER`, then resolved. Short flaps are never reported. A problem that gets worse stays the same problem and is reported again.
- **Mail notifications** over SMTP (STARTTLS, implicit TLS or plain), with several recipients, optional login and one combined mail per run. Failed sends are retried, also after a restart.
- **Web UI** with overview, history (filter by namespace) and status page; light and dark mode; usable on a phone. No JavaScript.
- **Login** for one local admin (bcrypt hash), server-side sessions, rate limit for failed logins, and the `hash-password` subcommand.
- **Ignore rules**: an annotation on an object or its namespace, a list of namespaces, and switching single checks off.
- Optional CRDs (cert-manager, Argo CD) are looked at only if an enabled check needs them. At startup smokealarm detects whether the CRD is installed and whether the service account may list it; checks that cannot run are skipped and shown with the reason. Without the permission their open problems stay open, because they may still exist.
- If the informer caches do not sync within two minutes (usually a missing permission), smokealarm exits with an error that names the resources, instead of staying unready forever.
- `/healthz` and `/readyz` endpoints.
- **Packaging**: a multi-arch container image (amd64, arm64) on a distroless base, a Makefile, and documentation (README, ARCHITECTURE).
- **Helm chart** with read RBAC, optional permissions behind switches (`rbac.nodesProxy`, `rbac.certManager`, `rbac.argocd`; a switch that is off also disables the checks that need it), a locked-down security context, startup, liveness and readiness probes, and example values files in `deploy/helm/examples/` (ingress with TLS, mail, minimal permissions, own Secret). The `nodes/proxy` rule exists only while `pvc-usage` is active; the README explains why `get` on `nodes/proxy` is not a read-only permission. The ClusterRole is named `<namespace>-<release>`, so the chart can be installed in several namespaces.
- `make test` runs without the race detector (it needs a C compiler on Linux); `make test-race` adds it. `make chart` lints and renders the chart with the defaults and every example file.

[Unreleased]: https://github.com/gagaschu/smokealarm/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/gagaschu/smokealarm/releases/tag/v0.1.0
