# Brief: smokealarm – lightweight Kubernetes health monitoring in Go

## Goal
Build a small open-source tool called smokealarm that runs in the Kubernetes cluster and answers exactly one question: "What is broken right now?"
Motto: "A smoke alarm for your Kubernetes cluster – silent until something's on fire."
No metrics database, no graphs, no logs, no traces, no root-cause analysis. Instead: a few fixed, reliable checks with no configuration effort, a simple web UI with current problems and history, and mail notifications on state changes.
Primary test environment: k3s on Raspberry Pi (arm64). However, it must run on any standard Kubernetes and must not require anything k3s-specific.

## Most important requirement: readable, simple code
The maintainer is not a Go developer. The code must be understandable for someone with an ops background and no Go experience. Readability takes priority over elegance, performance and abstraction.
- Idiomatic, plain Go (Effective Go, Go Code Review Comments). gofmt, go vet and golangci-lint with the standard linters must be clean.
- Explicit rather than clever: no generics, no reflection, no init() functions, no global variables except constants. Dependencies are created in main() and passed on as struct fields.
- Interfaces only where there really are several implementations (e.g. Notifier: mail, webhook, ntfy, Telegram, Slack, Teams). No interfaces just in case.
- Minimal concurrency: informers, one main loop with a ticker, the HTTP server. No channel pipelines, no worker pools.
- Short functions (guideline < 50 lines), descriptive names. Always wrap errors with context (fmt.Errorf("load config: %w", err)), never swallow them.
- Comments explain the why, not the what. Every package has a package doc comment. Every check has a doc comment: what is checked, why it is a problem, which severity.
- Code, comments, log messages and documentation in English.
- Logging with log/slog (JSON in the cluster, text locally).
- Time comes from an injectable now function, so that logic and tests are deterministic.

## Dependencies
Allowed, otherwise standard library only:
- k8s.io/client-go, k8s.io/api, k8s.io/apimachinery
- modernc.org/sqlite (pure Go, for CGO_ENABLED=0 and easy cross-compiling)
- golang.org/x/crypto (bcrypt)
- github.com/robfig/cron/v3 (only for parsing CronJob schedules)
Explicitly not: controller-runtime, cobra, viper, web frameworks (gin, echo, chi), ORMs, Prometheus libraries, frontend build tooling.
HTTP routing with net/http (Go 1.22+ patterns), HTML with html/template, mail with net/smtp and text/template, SQL with database/sql. The other notification targets (webhook, ntfy, Telegram, Slack, Teams) use net/http, encoding/json and text/template only: no notification library (shoutrrr, nikoksr/notify, ...) and no vendor SDK.
Further dependencies only after asking.

## Project structure
cmd/smokealarm/main.go   Wiring: load config, start clients, store, engine, web server; subcommand hash-password
internal/config          Config from environment variables, defaults, validation
internal/kube            Clients, informers (core + dynamic for CRDs), detection of optional CRDs
internal/checks          One file per area (pods.go, workloads.go, nodes.go, jobs.go, storage.go, certs.go, argocd.go) plus tests
internal/engine          Main loop: run checks, compare with stored state, trigger notifications
internal/store           SQLite access, schema migrations as embedded .sql files, retention
internal/notify          Notifier interface, mail, webhook, ntfy, Telegram, Slack and Teams (one file each)
internal/auth            Local login, sessions, middleware
internal/web             Handlers, templates, CSS (all via go:embed)
deploy/helm/smokealarm   Helm chart
No further packages without a good reason.

## Core concept
A check is a simple function: it receives a snapshot of the cluster objects from the informer cache and returns a list of findings. Checks never call the API themselves and have no state of their own, which makes them trivial to test with fake objects. (Exception: PVC usage, see below – the kubelet stats are loaded into the snapshot before the checks run.)

Finding (guideline):
- Key: stable, from check + kind/namespace/name + reason; identifies "the same problem" across runs
- Check, Severity (critical | warning), Object (Kind, Namespace, Name)
- Reason: short, e.g. "CrashLoopBackOff"
- Message: human-readable with relevant context (e.g. last exit code, matching warning event)

Sequence every SMOKEALARM_CHECK_INTERVAL (default 30s):
1. Build the snapshot, run all enabled checks.
2. Compare with the state in SQLite: new problem → pending; lasts longer than SMOKEALARM_ALERT_AFTER (default 2m) → firing; gone → resolved (only if it was firing before, otherwise discard silently). This way short flaps are not reported.
3. Send all changes not yet reported (firing/resolved), plus any reminders that are due, as one combined notification. An acknowledged problem only reports its end, and a problem in a downtime reports nothing (see Notifications). Mark them as reported only after sending succeeded, separately for every notification target; if sending to a target fails, it is retried for that target only in the next run, so the other targets get no duplicates. This also survives restarts.
4. Delete resolved findings older than SMOKEALARM_RETENTION (default 30d).

## Checks (v0.1)
Pods
- CrashLoopBackOff → critical
- ImagePullBackOff / ErrImagePull → critical
- OOMKilled (last termination within the last 15 min) → warning
- Many restarts: restartCount > threshold (default 5) and last restart within the last hour → warning
- Pending longer than threshold (default 5m), with the reason from conditions/events (e.g. Unschedulable) → warning
Workloads
- Deployment/StatefulSet/DaemonSet with fewer available than desired replicas → warning; 0 available with desired > 0 → critical
Nodes
- NotReady → critical
- MemoryPressure / DiskPressure / PIDPressure → warning
Jobs
- Job failed (condition Failed) → warning. For CronJobs only the newest job counts, so that old failures do not alarm permanently.
- CronJob missed a scheduled run (due according to the cron expression, lastScheduleTime older than expected + tolerance) → warning. Ignore suspended CronJobs.
Storage
- PVC Pending longer than threshold → warning
- PVC usage via kubelet stats (/api/v1/nodes/<node>/proxy/stats/summary): > 85 % warning, > 95 % critical. Can be switched off, because it needs the RBAC permission nodes/proxy.
Certificates (only if cert-manager CRDs are present)
- Certificate not Ready → warning
- status.notAfter in < 14 days → warning, < 3 days → critical
- Do not read Secrets.
Argo CD (only if Argo CD CRDs are present)
- Application health Degraded or Missing → critical
- Sync OutOfSync → warning (ALERT_AFTER dampens short syncs)
Optional CRDs are detected at startup via the discovery API. If they are missing, the check is skipped, logged and shown on the status page.
Individual checks can be disabled via an environment variable.

Ignoring:
- The annotation smokealarm.<DOMAIN>/ignore: "true" on an object or on its namespace suppresses findings. For pods, the annotation in the pod template is enough. The annotation prefix is a single constant in the code.
- In addition, an environment variable with a list of ignored namespaces.

## Notifications
- Mail via SMTP (STARTTLS and implicit TLS on port 465, optionally with auth, multiple recipients).
- Since phase 13, also: a generic webhook (structured JSON, optional static auth header), ntfy (ntfy.sh or self-hosted, optional access token), Telegram (Bot API, one or more chats, plain text), Slack (incoming webhook) and Microsoft Teams (Power Automate Workflows webhook with an Adaptive Card; the old Office 365 connector webhooks no longer work). Any combination can be active; none configured means the log only. Every rule in this section (ALERT_AFTER, one combined message per run, reminders, acknowledge, downtimes) applies to every target exactly as to mail: the targets change only where a notification goes. Each target remembers on its own what it has been told. Secrets (tokens, webhook URLs, header values, the ntfy topic) never appear in logs, errors or on the status page; a target is shown by its name only.
- At most one message per run and target, with all new state changes and due reminders.
- Reminders are optional: SMOKEALARM_CRITICAL_REPEAT_INTERVAL (default 0, off; otherwise at least SMOKEALARM_CHECK_INTERVAL) reports a critical problem that is still firing again once its last report is that old. Each problem has its own clock. Warnings are not repeated. No reminders for checks whose state is unknown in this run. A failed reminder is retried in the next run.
- A firing critical problem can be acknowledged in the web UI. Warnings cannot, because they are not repeated anyway. An acknowledged problem sends no reminders. The acknowledgement lasts until the problem is resolved or until it is removed in the web UI, and "resolved" is still reported. If its first mail has not gone out yet (for example because sending failed), that mail is dropped, but "resolved" is reported anyway. If it becomes a warning and then critical again, it stays acknowledged and no mail is sent. If the problem returns after "resolved", it is a new alarm with a new mail. After removing an acknowledgement the problem behaves like any other: an overdue reminder or a first mail that was held back goes out in the next run, unless a downtime is running.
- A downtime silences one problem (one finding key, warning or critical) until a fixed date and time in the future. It can be set for firing problems and from the history, because a flapping problem is often "resolved" right now.
  - While it runs, the problem sends no mail at all: no new problem, no reminder, no "resolved". This also applies to an acknowledged problem.
  - If the problem is still active when the downtime ends, it is reported again as new in the next run, and the reminder interval runs from that mail. An acknowledged problem stays silent and only reports its end.
  - If the problem went away during the downtime, nothing is sent, not even after it ends. A problem that goes away in the run in which the downtime ends counts as gone during the downtime, unless it is acknowledged.
  - Ending a downtime early works like reaching its end at that moment. Ended downtimes are removed. A downtime of a check whose state is unknown waits until the check runs again, and stays silent until then.
  - A new reason or a new object name is a new key and is not covered. The UI and the README say so.
- The subject summarizes: specific for one change, e.g. "[smokealarm] CRITICAL: CrashLoopBackOff media/jellyfin" or, for a reminder, "[smokealarm] STILL CRITICAL: CrashLoopBackOff media/jellyfin"; for several, e.g. "[smokealarm] 3 new problems, 1 still critical, 1 resolved".
- Body as plain text: new problems sorted by severity, then reminders, then resolved ones; each with object, reason, message, since when; link to the web UI (SMOKEALARM_BASE_URL).
- Chat targets (ntfy, Telegram, Slack, Teams) get a short version: the subject as the headline, then one line per problem (object, reason, since when) and the link to the web UI, cut to the length limit of the service with "… and N more". The webhook gets all fields as JSON.
- No routing by severity or label per target, no webhook signatures yet.

## Web UI
- Server-rendered HTML, no JavaScript framework, no build step. Auto-refresh via meta tag (30s). Plain custom CSS, light/dark via prefers-color-scheme, works well on a phone.
- Pages:
  - Login
  - Overview: current problems by severity with object, reason, message, since when. Empty state "All good" with the time of the last run. Firing critical problems have an acknowledge button (plain POST form); acknowledged problems stay on the page and are marked, with the time in UTC, and have a button to remove the acknowledgement. Every problem links to the downtime form; problems in a downtime stay on the page and are marked.
  - History: resolved findings, filterable by namespace, with a link to the downtime form and a mark for a running downtime
  - Downtimes: running downtimes, each with its end and a button to end it now
  - Downtime form: an own page without auto-refresh, so a reload cannot discard the input. A POST form with `datetime-local`; the time is entered and shown in UTC. Times that are not in the future are refused.
  - Status: last run, active/skipped checks with reason, every configured notification target with its last success and its last sending error
- Actions that change state need a valid session and are protected against cross-site requests (http.CrossOriginProtection from the standard library), in addition to the SameSite=Lax cookie.
- /healthz (liveness) and /readyz (informer caches synced, DB reachable), both without auth.

## Authentication
- v0.1: one local admin user; username and bcrypt hash from the environment (in the cluster from a Secret).
- Login form, server-side sessions in SQLite with a random session ID. Cookie HttpOnly, Secure (can be switched off for local development), SameSite=Lax, configurable expiry (default 7d). Logout. Simple rate limiting for failed logins.
- Subcommand `smokealarm hash-password` reads a password from stdin and prints the bcrypt hash (simple os.Args switch, no CLI framework).
- Structured so that OIDC can be added later as a second login method: the middleware only checks "is there a valid session?"; how the session comes about (password, later OIDC) is separate from that. Do not implement OIDC itself.

## Configuration
- Only environment variables with the prefix SMOKEALARM_. Everything has sensible defaults except required values (admin user, password hash, SMTP data if mail is active).
- Validate at startup; abort with a clear message on errors.
- Runs in the cluster (in-cluster config) and locally against KUBECONFIG – for development on macOS (Apple Silicon) against a local k3s.
- Complete table of all variables in the README.

## Security
- RBAC: get/list/watch only, minimal list of resources. nodes/proxy only with an active PVC usage check; CRD permissions only for cert-manager Certificates and Argo CD Applications, each switchable via a Helm value.
- Container: nonroot, readOnlyRootFilesystem, all capabilities dropped, seccompProfile RuntimeDefault, allowPrivilegeEscalation false. Only the PVC is writable.
- No passwords, hashes or session IDs in the log.
- Keep informer memory small: remove managedFields and unneeded fields via SetTransform.

## Packaging & open-source basics
- Dockerfile: multi-stage, CGO_ENABLED=0, target gcr.io/distroless/static:nonroot, multi-arch (linux/amd64, linux/arm64) via docker buildx.
- Makefile with build, test, lint, run (local), image.
- Helm chart: Deployment (1 replica, strategy Recreate because of SQLite on an RWO PVC), PVC, Service, optional Ingress, ServiceAccount, RBAC with switches for optional checks, Secret or existingSecret for password hash and SMTP credentials, liveness/readiness probes, resources (guideline requests 64Mi/10m, limit 128Mi), securityContext as above.
- LICENSE (Apache-2.0)
- README: motto, what/why, clear scope (no Prometheus replacement, no root-cause analysis, no traces – deliberately few, reliable checks), installation via Helm, configuration table, list of all checks with severity, local development.
- ARCHITECTURE.md: one page for readers without Go experience – data flow, packages and their purpose, step by step "How do I add a new check".
- CHANGELOG.md, .gitignore.
- For the Git workflow see CLAUDE.md.

## Tests
- Table-driven tests for every check with hand-built objects (no real API), including edge cases: just below/above the threshold, ignored via annotation, optional CRDs missing.
- Tests for the engine's state logic against a temporary SQLite file: pending → firing → resolved, flap is suppressed, failed sending is retried, combined mail for several changes, acknowledged problems, downtimes.
- Golden test for the subject and body of the mail.

## Procedure
First create PLAN.md with the package layout, central types, database schema, list of environment variables and phase plan. Then wait for my approval.
After that, work in phases. Each phase ends with a working state, green go build / go vet / go test / golangci-lint and a short summary: what is new, how do I try it locally. After each phase stop and wait for approval.
1. Skeleton: main, config, logging, informers, pod checks, findings to the log, /healthz. Runs locally against KUBECONFIG.
2. Remaining core checks (workloads, nodes, jobs, CronJobs, PVC pending) with tests.
3. SQLite store, state logic, retention.
4. Mail notifications with combined mails.
5. Web UI and login.
6. Optional checks: PVC usage, cert-manager, Argo CD.
7. Packaging: Dockerfile, Makefile, Helm chart, README, ARCHITECTURE.md, LICENSE, CHANGELOG.
If a requirement is unclear or contradictory: ask instead of guessing. Briefly justify deliberate deviations from the brief.
