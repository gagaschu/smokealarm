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
- Optional CRDs (cert-manager, Argo CD) are detected at startup; checks that cannot run are skipped and shown with the reason.
- `/healthz` and `/readyz` endpoints.
- **Packaging**: a multi-arch container image (amd64, arm64) on a distroless base, a Helm chart with read-only RBAC, optional permissions behind switches and a locked-down security context, a Makefile, and documentation (README, ARCHITECTURE).

[Unreleased]: https://github.com/gagaschu/smokealarm/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/gagaschu/smokealarm/releases/tag/v0.1.0
