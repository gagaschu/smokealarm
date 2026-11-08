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
- github.com/coreos/go-oidc/v3 and golang.org/x/oauth2 (since phase 24, only for the OIDC login: discovery, code exchange and the check of the ID token; the token check is not written by hand)
Explicitly not: controller-runtime, cobra, viper, web frameworks (gin, echo, chi), ORMs, Prometheus libraries, frontend build tooling.
HTTP routing with net/http (Go 1.22+ patterns), HTML with html/template, mail with net/smtp, text/template and, since phase 29, html/template for the HTML part, SQL with database/sql. The other notification targets (webhook, ntfy, Telegram, Slack, Teams) use net/http, encoding/json and text/template only: no notification library (shoutrrr, nikoksr/notify, ...) and no vendor SDK.
Further dependencies only after asking.

## Project structure
cmd/smokealarm/main.go   Wiring: load config, start clients, store, engine, web server; subcommand hash-password
internal/config          Config from environment variables, defaults, validation
internal/kube            Clients, informers (core + dynamic for CRDs), detection of optional CRDs
internal/checks          One file per area (pods.go, workloads.go, nodes.go, jobs.go, storage.go, certs.go, argocd.go) plus tests
internal/engine          Main loop: run checks, compare with stored state, trigger notifications
internal/store           SQLite access, schema migrations as embedded .sql files, retention
internal/notify          Notifier interface, mail, webhook, ntfy, Telegram, Slack and Teams (one file each)
internal/auth            Local login, OIDC login (since phase 24), sessions, middleware
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
- Targets (since phase 14): the notification targets from the targets annotation; the severity annotation can replace the check's severity. Both are applied after the checks, so checks stay unaware of them.
- CausedBy (since phase 15): the name of a node that is down, if the problem is a consequence of it (see Notifications, node down). Like Targets it is applied after the checks and is not part of the key.

Sequence every SMOKEALARM_CHECK_INTERVAL (default 30s):
1. Build the snapshot, run all enabled checks.
2. Compare with the state in SQLite: new problem → pending; lasts longer than SMOKEALARM_ALERT_AFTER (default 2m) → firing; gone for SMOKEALARM_RESOLVE_AFTER (default 5m, since phase 17) → resolved (only if it was firing before; a pending problem that is gone is discarded silently at once). This way short flaps are not reported.
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
- PVC usage via kubelet stats (since phase 21 directly from the kubelet, https://<node InternalIP>:<kubelet port>/stats/summary, with the ServiceAccount token and a verified certificate; no fallback through the API server proxy): > 85 % warning, > 95 % critical. Can be switched off, because it needs the RBAC permission nodes/stats. It needs kubelet serving certificates that can be verified (k3s, kubeadm with serverTLSBootstrap, most managed clusters); with self-signed ones (default kubeadm) the check is skipped with that reason, decided by the maintainer in the review of phase 21.
Certificates (only if cert-manager CRDs are present)
- Certificate not Ready → warning
- status.notAfter in < 14 days → warning, < 3 days → critical
- Do not read Secrets.
Argo CD (only if Argo CD CRDs are present)
- Application health Degraded or Missing → critical
- Sync OutOfSync → warning (ALERT_AFTER dampens short syncs)
Optional CRDs are detected at startup via the discovery API. If they are missing, the check is skipped, logged and shown on the status page.
Individual checks can be disabled via an environment variable.
API connection (since phase 17)
- api-connection → critical: the informer caches have not been kept up to date for 3 minutes (with ALERT_AFTER 5 minutes by default), because the API server does not answer or a watch fails with an error (a missing permission, a deleted CRD). A watch that merely ends (EOF, "expired") does not count. The API server is asked once per run. When it answers again after not answering, its outage is over at once: the failures of watches that began since its last answer are forgotten. A watch that was failing before stays reported, and one that still fails reports itself again within a minute, with its 3 minutes starting over. While the API server does answer, client-go does not report that a watch recovered, so a failing watch counts as working again after 2 minutes without an error. A lost permission only shows when the open watch ends, and an open watch lasts 5 to 10 minutes: the alarm then comes 10 to 15 minutes later (with ALERT_AFTER 2m).
- If nothing can be read (the API server does not answer, or all watches fail), all other checks count as unknown for that run: their problems stay as they are, without reminders, with the reason on the status page. If only a part fails, the other checks run as usual and only the alarm tells about it.
- The request to the API server is not a check: the kube package makes it while building the snapshot, like the kubelet stats, and checks still read only the snapshot.

Ignoring:
- The annotation smokealarm.<DOMAIN>/ignore: "true" on an object or on its namespace suppresses findings. For pods, the annotation in the pod template is enough. The annotation prefix is a single constant in the code. It wins over the routing annotations (see Notifications).
- In addition, an environment variable with a list of ignored namespaces.

## Notifications
- Mail via SMTP (STARTTLS and implicit TLS on port 465, optionally with auth, multiple recipients).
- Since phase 29, every mail has a text and an HTML part (multipart/alternative, text first). The HTML is built from html/template, so every text from the cluster is escaped, uses tables and inline CSS only and loads nothing from elsewhere. A user `mail.tmpl` without a `mail.html.tmpl` makes the mail text only, so the user's text is never hidden behind the built-in HTML. A resolved problem is listed with its last severity and how long it lasted, not with a [RESOLVED] tag.
- Since phase 13, also: a generic webhook (structured JSON, optional static auth header), ntfy (ntfy.sh or self-hosted, optional access token), Telegram (Bot API, one or more chats, plain text), Slack (incoming webhook) and Microsoft Teams (Power Automate Workflows webhook with an Adaptive Card; the old Office 365 connector webhooks no longer work). Any combination can be active; none configured means the log only. Every rule in this section (ALERT_AFTER, one combined message per run, reminders, acknowledge, downtimes) applies to every target exactly as to mail: the targets change only where a notification goes. Each target remembers on its own what it has been told. Secrets (tokens, webhook URLs, header values, the ntfy topic) never appear in logs, errors or on the status page; a target is shown by its name only.
- At most one message per run and target, with all new state changes and due reminders.
- Reminders are optional: SMOKEALARM_CRITICAL_REPEAT_INTERVAL (default 0, off; otherwise at least SMOKEALARM_CHECK_INTERVAL) reports a critical problem that is still firing again once its last report is that old. Each problem has its own clock. Warnings are not repeated. No reminders for checks whose state is unknown in this run. A failed reminder is retried in the next run.
- Since phase 17, a firing problem that gets worse (warning to critical) is reported again only once the critical state has lasted ALERT_AFTER. While it waits nothing new is sent about it, neither a reminder nor a first message to a target for critical problems only. If it falls back to warning first, nothing is sent. So a problem that flips faster than ALERT_AFTER sends one report; one that flips more slowly still sends one per change. If the problem goes away during the wait and comes back as critical, the wait starts again from its return; if it comes back as a warning, nothing is sent. With ALERT_AFTER 0 it is reported at once.
- Since phase 17, a firing problem that is no longer found counts as resolved only once it has been gone for SMOKEALARM_RESOLVE_AFTER (default 5m; 0 resolves it at once). This applies to all checks, also to NotReady and to checks that were switched off. If it comes back in that time, it simply goes on; the wait starts again the next time it is gone. While it waits there are no reminders and no first message, and its "resolved" gives the start of the wait. A pending problem is dropped at once, as before. A check whose state is unknown says nothing, so its problems do not start waiting, and a wait that is running starts again from the last run in which the state was unknown: only time in which the check looked and found nothing counts. The same goes for a pvc-usage problem whose volume was not measured. At the end of a downtime a problem that is not found in that run counts as gone during the downtime, also if it is still waiting: nothing is sent when it resolves, and if it comes back it is reported again as new.
- A firing critical problem can be acknowledged in the web UI. Warnings cannot, because they are not repeated anyway. An acknowledged problem sends no reminders. The acknowledgement lasts until the problem is resolved or until it is removed in the web UI, and "resolved" is still reported. If its first mail has not gone out yet (for example because sending failed), that mail is dropped, but "resolved" is reported anyway. If it becomes a warning and then critical again, it stays acknowledged and no mail is sent. If the problem returns after "resolved", it is a new alarm with a new mail. After removing an acknowledgement the problem behaves like any other: an overdue reminder or a first mail that was held back goes out in the next run, unless a downtime is running.
- A downtime silences one problem (one finding key, warning or critical) until a fixed date and time in the future. It can be set for firing problems and from the history, because a flapping problem is often "resolved" right now.
  - While it runs, the problem sends no mail at all: no new problem, no reminder, no "resolved". This also applies to an acknowledged problem.
  - If the problem is still active when the downtime ends, it is reported again as new in the next run, and the reminder interval runs from that mail. An acknowledged problem stays silent and only reports its end.
  - If the problem went away during the downtime, nothing is sent, not even after it ends. A problem that goes away in the run in which the downtime ends counts as gone during the downtime, unless it is acknowledged.
  - Ending a downtime early works like reaching its end at that moment. Ended downtimes are removed. A downtime of a check whose state is unknown waits until the check runs again, and stays silent until then.
  - A new reason or a new object name is a new key and is not covered. The UI and the docs say so.
- Since phase 31, a notification target can be switched off (muted) in the web UI, one target at a time (one Telegram chat is one target). It stays off, also across restarts, until it is switched on again; there is no automatic end. The other targets are not affected.
  - While it is off, the target sends nothing at all: no new problem, no reminder, no "resolved".
  - Switching it on works like the end of a downtime, for this target only: a problem that is still firing and meant for the target is reported again as new in the next run, and the reminder interval runs from that message. A problem that ended while the target was off sends nothing, also if the target heard its start. An acknowledged problem stays silent and only reports its end; a running downtime still wins. A problem that began and ended while the target was off was never heard there and sends nothing, as before.
  - The web UI only asks for the target to be switched on; the engine finishes it in its next run. If the state of a check is unknown in that run, it waits for a run in which no state is unknown, so that no problem is reported that may be gone; until then the target stays off and the UI says that it is being switched on.
  - Switching on forgets the last sending error of the target, because nothing from before is owed any more. Switching off and on is logged with the target's description, never with secrets.
  - A muted target that is no longer configured is forgotten at startup. The log-only fallback (no target configured) has no switch.
  - Message formats, the webhook JSON, the chart and the environment variables do not change.
- The subject summarizes: specific for one change, e.g. "[smokealarm] CRITICAL: CrashLoopBackOff media/jellyfin" or, for a reminder, "[smokealarm] STILL CRITICAL: CrashLoopBackOff media/jellyfin"; for several, e.g. "[smokealarm] 3 new problems, 1 still critical, 1 resolved". A node that is newly down comes first instead (since phase 15): "[smokealarm] NODE DOWN: pi-2", with the other changes counted after it, e.g. "[smokealarm] NODE DOWN: pi-2 (+ 1 new problem, 1 resolved)"; at most three nodes are named, the rest counted ("pi-1, pi-2, pi-3, and 2 more").
- Body as plain text: new problems sorted by severity, then reminders, then resolved ones; each with object, reason, message, since when; link to the web UI (SMOKEALARM_BASE_URL).
- Chat targets (ntfy, Telegram, Slack, Teams) get a short version: the subject as the headline, then one line per problem (object, reason, since when) and the link to the web UI, cut to the length limit of the service with "… and N more". The webhook gets all fields as JSON.
- Since phase 22, message templates: the subject and the text of mail, ntfy, Telegram, Slack and Teams can be replaced by Go text/template files in SMOKEALARM_TEMPLATE_DIR (Helm: `templates`, as a ConfigMap): `subject.tmpl` for every target, `mail.tmpl`, `mail.html.tmpl` (since phase 29, html/template, the HTML part of the mail), `chat.tmpl` for the chat targets, and `<target>.subject.tmpl` / `<target>.chat.tmpl` for one target, which win over the general file; without a file the built-in message stays, unchanged. The webhook keeps its JSON. All templates get the same data (the lists of the notification, each text from the cluster on one line, times in the configured time zone, UTC by default) and only the functions of text/template; a subject is put on one line and cut at 200 bytes. A chat text that is too long is rendered again with fewer problems, and the template learns how many were left out. Every file is checked at startup with made-up notifications; a mistake or an unknown file name stops the start with the file and the line. A template that fails with a real notification, or renders nothing, sends that message with the built-in template instead, and the log and the status page say so: an alarm never stays silent because of a template.
- Since phase 14, routing: SMOKEALARM_<TARGET>_MIN_SEVERITY (SMTP, WEBHOOK, NTFY, TELEGRAM, SLACK, TEAMS; default warning = everything) limits a target to critical problems. The annotations smokealarm.<DOMAIN>/targets (comma-separated kinds: mail, webhook, ntfy, telegram, slack, teams; telegram = all chats) and smokealarm.<DOMAIN>/severity (warning | critical) on an object or its namespace choose the targets and replace the severity of its problems; the object wins over the namespace, each annotation on its own. A target hears about a problem if its severity is high enough for the target and the targets annotation, if any, names it. The filter applies to new problems and reminders; a target that heard the start of a problem always hears its end, also after an escalation or the end of a downtime (only the targets the problem is meant for then hear it again as new). A targets annotation that names no configured target counts as none (every target), so a typo never silences an alarm; unusable annotation values, and an annotation that makes a problem reach no target at all (a warning for critical-only targets, which is allowed), are logged once, naming the object or namespace that carries it. The UI says who is notified when it is not every target. For a CronJob the annotations of its job template count too. Annotations do not change the key. Every other rule (ALERT_AFTER, combined message, reminders, acknowledge, downtimes) is unchanged. Not yet: Kubernetes labels, several instances of one kind of target, webhook signatures.
- Since phase 15, node down: when a node is down, the problems it causes are held back, so the node is the alarm and not the noise around it.
  - A node is down when its Ready condition is False or Unknown (the rule of the NotReady check). Its own ignore, targets and severity annotations decide whether and where NotReady is reported; the annotations of the pods on it do not.
  - A consequence is a problem of a pod that runs on a down node, or of a Deployment, StatefulSet or DaemonSet whose missing replicas are all explained by down nodes: no more replicas unavailable than it has pods (matched by its selector) on down nodes. A workload that misses more (a DaemonSet whose rollout fails on the healthy nodes too) is not a consequence. Nothing else is a consequence; a pod that waits for a node (Pending, no node yet) is not, so a lack of room after the failure is reported as usual.
  - A consequence that is not firing yet stays pending as long as it is one, whatever ALERT_AFTER says, also if the node itself is ignored or its NotReady goes to other targets. A consequence that goes away meanwhile is dropped silently, as any pending problem.
  - Nothing is held back while the NotReady check does not run (switched off, or unable to run in this run), because then nobody would hear about the node.
  - When the node is back, or the problem is no longer a consequence (the down node no longer explains it), the ALERT_AFTER clock of a held-back problem starts again from that run: if it goes away in that time, nothing is sent; if it lasts, it is reported as a problem of its own.
  - A problem that was already firing when the node went down is not touched: it keeps its reminders, acknowledgement and downtime, and its end is reported.
  - While a node is down, a pvc-usage problem whose volume was not measured in this run stays as it is, because it may live on that node: "not measured" is not "fine". A volume that was measured is judged as usual. Its downtime waits as for a check whose state is unknown.
  - Notifications list NotReady first. In the mail body new NotReady problems get their own first section NODE DOWN; a NotReady problem (new or reminder) lists the problems held back because of it that the target would hear about, as "Affected". Chat targets get a first section "Node down:" with the number of affected problems; the webhook gets an "affected" list on every alert (new field, format version unchanged).
  - The overview lists NotReady first, says how many problems a down node holds back, and shows the held-back problems grouped by node, also for an ignored node. "All good" only when nothing is firing and nothing is held back.

## Web UI
- Server-rendered HTML, no JavaScript framework, no build step. Auto-refresh via meta tag (30s). Plain custom CSS, light/dark via prefers-color-scheme, works well on a phone.
- Pages:
  - Login: the password form; with OIDC on (since phase 24) also a button "Login with <name>" (SMOKEALARM_OIDC_NAME, default "OIDC")
  - Overview: current problems by severity with object, reason, message, since when. Empty state "All good" with the time of the last run, only if the API connection is fine (since phase 17, a problem with it is shown on top with its start, and "All good" is not). A problem that waits for RESOLVE_AFTER stays on the page with "Not seen since". Firing critical problems have an acknowledge button (plain POST form); acknowledged problems stay on the page and are marked, with the time in the configured time zone, and have a button to remove the acknowledgement. Every problem links to the downtime form; problems in a downtime stay on the page and are marked. Since phase 31, a notification target that is off is named at the top with the time it was switched off and a link to the status page, also next to "All good".
  - History: resolved findings, filterable by namespace, with a link to the downtime form and a mark for a running downtime
  - Downtimes: running downtimes, each with its end and a button to end it now
  - Downtime form: an own page without auto-refresh, so a reload cannot discard the input. A POST form with `datetime-local`; the time is entered and shown in the configured time zone (SMOKEALARM_TIME_ZONE). Times that are not in the future are refused.
  - Status: last run, a problem with the API connection, active/skipped checks with reason, every configured notification target with its last success and its last sending error; since phase 31 also a button per target to switch it off or on, and whether it is off (since when) or being switched on
- Actions that change state need a valid session and are protected against cross-site requests (http.CrossOriginProtection from the standard library), in addition to the SameSite=Lax cookie.
- /healthz (liveness) and /readyz (informer caches synced, DB reachable), both without auth. A lost API connection does not change /readyz, because a new pod would not help.

## Authentication
- v0.1: one local admin user; username and bcrypt hash from the environment (in the cluster from a Secret).
- Login form, server-side sessions in SQLite with a random session ID. Cookie HttpOnly, Secure (can be switched off for local development), SameSite=Lax, configurable expiry (default 7d). Logout. Simple rate limiting for failed logins.
- Subcommand `smokealarm hash-password` reads a password from stdin and prints the bcrypt hash (simple os.Args switch, no CLI framework).
- The middleware only checks "is there a valid session?"; how the session comes about (password, OIDC) is separate from that. Every login method ends in `Sessions.Start`.
- OIDC (since phase 24) is a second login method next to the password, never instead of it: the local admin stays as the way in when the identity provider is down, so SMOKEALARM_ADMIN_USER and SMOKEALARM_ADMIN_PASSWORD_HASH stay required.
  - It is on when SMOKEALARM_OIDC_ISSUER_URL is set. One provider only. Authorization code flow with PKCE; the ID token is checked by go-oidc (signature, issuer, audience, expiry, nonce). No refresh tokens, no single logout at the provider: logout ends the smokealarm session only.
  - Access is by allowlist, and it fails closed. SMOKEALARM_OIDC_ALLOWED_EMAILS (only an email with email_verified=true counts, compared without regard to case) and SMOKEALARM_OIDC_ALLOWED_GROUPS (values of the claim SMOKEALARM_OIDC_GROUPS_CLAIM, default groups): one match is enough. With OIDC on, at least one of the two lists is required. There are no roles: everybody who gets in may do everything the admin may.
  - The state, nonce and PKCE verifier of a login live in a short-lived cookie (10 minutes, HttpOnly, Secure like the session cookie, SameSite=Lax), deleted at the callback. No table.
  - The redirect URI is SMOKEALARM_BASE_URL + /login/oidc/callback, so the base URL is required while OIDC is on.
  - The identity provider does not have to be reachable at startup: the discovery runs at the first login and is retried after an error, so the monitoring never depends on it. Wrong settings still stop the start with a clear message. A setting of OIDC without SMOKEALARM_OIDC_ISSUER_URL is an error, so a typo cannot switch OIDC off silently.
  - A session carries a fingerprint of the login data it was started with. The OIDC fingerprint covers issuer, client ID and both allowlists, so changing the allowlist ends the OIDC sessions at the next start, as changing the password ends the password sessions. No migration.
  - The client secret, tokens, codes and the cookie are never logged, never put into an error and never shown on the status page. The login page says only that the login failed; the reason is in the log.
  - Settings (table in docs/configuration.md): SMOKEALARM_OIDC_ISSUER_URL (https; http only for localhost), _CLIENT_ID, _CLIENT_SECRET, _NAME (shown in the button, at most 40 characters, no control characters), _SCOPES (default "openid email"), _GROUPS_CLAIM, _ALLOWED_EMAILS, _ALLOWED_GROUPS and _CA_FILE (a CA for a provider with an internal certificate).
  - Not included: several providers, roles or rights per user, single logout, refresh tokens, creating users.

## Configuration
- Only environment variables with the prefix SMOKEALARM_. Everything has sensible defaults except required values (admin user, password hash, SMTP data if mail is active).
- Validate at startup; abort with a clear message on errors.
- Time zone (since phase 25): SMOKEALARM_TIME_ZONE (IANA name, default UTC; `Local` is refused) is the zone for every time shown or entered: web UI, downtime form, mails, chat messages and the times in check messages. Only the display changes: stored times and all comparisons stay absolute (Unix seconds), and the time fields of the webhook JSON (since, resolved_at) stay in UTC; its subject and message are the same texts as for the other targets, with times in the configured zone. In the downtime form, a wall-clock time that exists twice (the clocks go back) is read as the earlier moment that is still in the future, unless the other one is the end of the running downtime, which is then kept. Message templates get their times in this zone.
- Runs in the cluster (in-cluster config) and locally against KUBECONFIG – for development on macOS (Apple Silicon) against a local k3s.
- Complete table of all variables in docs/configuration.md.

## Security
- RBAC: get/list/watch only, minimal list of resources. nodes/stats (get) only with an active PVC usage check, never nodes/proxy; CRD permissions only for cert-manager Certificates and Argo CD Applications, each switchable via a Helm value.
- Container: nonroot, readOnlyRootFilesystem, all capabilities dropped, seccompProfile RuntimeDefault, allowPrivilegeEscalation false. Only the PVC is writable.
- No passwords, hashes, session IDs, OIDC tokens or the OIDC client secret in the log.
- Keep informer memory small: remove managedFields and unneeded fields via SetTransform.

## Packaging & open-source basics
- Dockerfile: multi-stage, CGO_ENABLED=0, target gcr.io/distroless/static:nonroot, multi-arch (linux/amd64, linux/arm64) via docker buildx.
- Makefile with build, test, lint, run (local), image.
- Helm chart: Deployment (1 replica, strategy Recreate because of SQLite on an RWO PVC), PVC, Service, optional Ingress, ServiceAccount, RBAC with switches for optional checks, Secret or existingSecret for password hash and SMTP credentials, liveness/readiness probes, resources (guideline requests 64Mi/10m, limit 128Mi), securityContext as above.
- LICENSE (Apache-2.0)
- README: motto, a note at the top that the project was built with AI, what/why, clear scope (no Prometheus replacement, no root-cause analysis, no traces – deliberately few, reliable checks), a quickstart with Helm and links to the docs. Since phase 23 the details are articles in docs/: installation, how it works, checks (all checks with severity), notifications, routing, message templates, configuration table, security, troubleshooting, local development. Plain, factual writing.
- ARCHITECTURE.md: one page for readers without Go experience – data flow, packages and their purpose, step by step "How do I add a new check".
- CHANGELOG.md, .gitignore.
- For the Git workflow see CLAUDE.md.

## Tests
- Table-driven tests for every check with hand-built objects (no real API), including edge cases: just below/above the threshold, ignored via annotation, optional CRDs missing.
- Tests for the engine's state logic against a temporary SQLite file: pending → firing → resolved, flap is suppressed, failed sending is retried, combined mail for several changes, acknowledged problems, downtimes.
- Golden test for the subject and body of the mail, text and HTML.

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
