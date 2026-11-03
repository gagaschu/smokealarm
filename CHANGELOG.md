# Changelog

All notable changes to this project are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project aims to follow
[Semantic Versioning](https://semver.org/).

## [Unreleased]

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
