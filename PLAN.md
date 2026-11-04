# smokealarm – Implementation Plan

## 1. Overview
smokealarm runs as a single Deployment (1 replica). Every `CHECK_INTERVAL` it builds a **snapshot** from the informer caches, runs all enabled **checks** (pure functions → `[]Finding`), **reconciles** the findings with the state in SQLite (pending → firing → resolved), sends **one combined notification** for all unreported changes, and cleans up old data. A small `net/http` server shows the current state.

```
informers ──► kube.BuildSnapshot ──► checks.Run* ──► engine.Reconcile ──► store (SQLite)
                                                           │
                                                           └──► notify.Notifier, one per target
                                                                (mail, webhook, ntfy, Telegram, Slack, Teams / log)
web ◄── store + engine.Status()
```

Module path: `github.com/gagaschu/smokealarm`. Go: current stable release; `go.mod` pins the version.

## 2. Package layout and dependencies

| Package | Responsibility | Imports (internal) |
|---|---|---|
| `cmd/smokealarm` | `main()`: `hash-password` subcommand via `os.Args`, load config, create logger, clients, store, notifier, engine, web server; signal handling and shutdown | all |
| `internal/config` | Read `SMOKEALARM_*` env vars, defaults, validation, one clear error list | – |
| `internal/kube` | Create clients (in-cluster or `KUBECONFIG`), shared informers + transform, discovery of optional CRDs, dynamic informers, kubelet stats, `BuildSnapshot()` | `checks` |
| `internal/checks` | `Snapshot`, `Finding`, `Check`, all check functions, ignore helper. **No API access, no state.** | – |
| `internal/engine` | Main loop with ticker, `RunOnce`, `Reconcile`, notification dispatch, retention, in-memory `Status` | `checks`, `kube`, `store`, `notify` |
| `internal/store` | SQLite (`modernc.org/sqlite`), embedded migrations (`migrations/*.sql`), finding/session/queries, retention | `checks` |
| `internal/notify` | `Notifier` interface, `Mail` (SMTP), `Webhook`, `Ntfy`, `Telegram`, `Slack`, `Teams` (net/http + encoding/json), `Log` (slog), subject/body templates and the short chat text | `checks` |
| `internal/auth` | Password login (bcrypt), sessions, middleware, login rate limiter | `store` |
| `internal/web` | Handlers, `html/template` templates, CSS (`go:embed`), `/healthz`, `/readyz` | `store`, `auth`, `engine` |
| `deploy/helm/smokealarm` | Helm chart | – |

Dependency rule: `checks` sits at the bottom and imports only `k8s.io/api`/`apimachinery`; there are no cycles.

## 3. Core types

### checks
```go
type Severity string // "critical" | "warning"

type ObjectRef struct {
    Kind, Namespace, Name string // Namespace empty for cluster-scoped objects (Node)
}

type Finding struct {
    Key      string   // check + "/" + kind + "/" + namespace + "/" + name + "/" + reason
    Check    string   // e.g. "pod-crashloop"
    Severity Severity
    Object   ObjectRef
    Reason   string   // e.g. "CrashLoopBackOff"
    Message  string   // human-readable, with context (exit code, warning event, ...)
    Targets  []string // phase 14: from the targets annotation, empty = every target; not in the key
}

// Snapshot is a read-only copy of everything the checks need.
type Snapshot struct {
    Now          time.Time
    Namespaces   []*corev1.Namespace
    Pods         []*corev1.Pod
    Deployments  []*appsv1.Deployment
    StatefulSets []*appsv1.StatefulSet
    DaemonSets   []*appsv1.DaemonSet
    Nodes        []*corev1.Node
    Jobs         []*batchv1.Job
    CronJobs     []*batchv1.CronJob
    PVCs         []*corev1.PersistentVolumeClaim
    Events       []*corev1.Event    // Warning events only
    Certificates []Certificate      // filled only if cert-manager is installed
    Applications []Application      // filled only if Argo CD is installed
    VolumeUsage  map[string]VolumeUsage // key "namespace/pvcName", from kubelet stats
}

// Small structs converted from unstructured objects in package kube, so
// checks never deal with unstructured maps.
type Certificate struct {
    Meta        metav1.ObjectMeta
    Ready       bool
    ReadyReason, ReadyMessage string
    NotAfter    *time.Time
}
type Application struct {
    Meta         metav1.ObjectMeta
    HealthStatus string // Healthy, Degraded, Missing, ...
    SyncStatus   string // Synced, OutOfSync, ...
}
type VolumeUsage struct{ UsedBytes, CapacityBytes int64 }

// Options holds the configurable thresholds (from config).
type Options struct {
    RestartThreshold  int
    PodPendingAfter   time.Duration
    PVCPendingAfter   time.Duration
    CronJobGrace      time.Duration
    IgnoredNamespaces []string
}

type Check struct {
    Name     string // stable id, used in SMOKEALARM_DISABLED_CHECKS and Finding.Check
    Requires string // "", "cert-manager", "argocd", "kubelet-stats"
    Run      func(snap *Snapshot, opts Options) []Finding
}

func All() []Check // returns the list (a function, not a global variable)
```
Helpers in `checks`: `newFinding(...)` builds the key; `isIgnored(snap, opts, meta)` checks the annotation `smokealarm.gagaschu.github.io/ignore: "true"` on the object and its namespace, plus `IgnoredNamespaces`; `latestWarning(snap, kind, ns, name)` returns the newest matching Warning event. Prefix as a constant: `const AnnotationPrefix = "smokealarm.gagaschu.github.io"`. For pods the pod's own annotations are enough, since annotations in the pod template are copied onto the pod.

Fixed values (constants, per spec): OOM window 15m, restart window 1h, PVC usage 85 %/95 %, certificate 14d/3d.

**Check list** (one file per area):

| File | Check name | Severity / Reason |
|---|---|---|
| pods.go | `pod-crashloop` | critical, CrashLoopBackOff |
| | `pod-image-pull` | critical, ImagePullBackOff (also used for ErrImagePull so the key stays stable) |
| | `pod-oomkilled` | warning, OOMKilled (last termination < 15m ago) |
| | `pod-restarts` | warning, TooManyRestarts (> threshold and last restart < 1h ago) |
| | `pod-pending` | warning, PodPending (detail such as Unschedulable goes into the message) |
| workloads.go | `workload-replicas` | warning ReplicasUnavailable / critical NoReplicasAvailable |
| nodes.go | `node-not-ready` | critical, NotReady |
| | `node-pressure` | warning, MemoryPressure / DiskPressure / PIDPressure |
| jobs.go | `job-failed` | warning, JobFailed (standalone Job) / LastJobFailed (on the **CronJob**, newest job only → stable key) |
| | `cronjob-missed` | warning, MissedSchedule (robfig/cron, respects `spec.timeZone`, suspended CronJobs skipped) |
| storage.go | `pvc-pending` | warning, PVCPending (claims of a WaitForFirstConsumer StorageClass are skipped; a waiting pod is covered by `pod-pending`) |
| | `pvc-usage` | warning VolumeAlmostFull (>85 %) / critical VolumeFull (>95 %) |
| certs.go | `cert-not-ready` | warning, CertificateNotReady |
| | `cert-expiry` | warning ExpiresSoon (<14d) / critical ExpiresVerySoon (<3d) |
| argocd.go | `argocd-health` | critical, Degraded / Missing |
| | `argocd-sync` | warning, OutOfSync |

A problem that can get worse while it lasts (`pvc-usage`, `cert-expiry`, `workload-replicas`) is **one finding with one key**: the key is built from a stable part (`usage`, `expiry`, `replicas`), while `Reason` and `Severity` change with how bad it is (`VolumeAlmostFull` → `VolumeFull`). The engine updates the record in place and, if a firing problem becomes critical, clears its "reported" marker so it is announced again as a new CRITICAL problem (with the original start time). No "resolved" notification and no pending gap; getting better again is silent, the later resolution is reported. Checks use `newEscalatingFinding` for this; all other checks use `newFinding`, where the reason is part of the key.

### kube
```go
type Cluster struct { // created in main
    client      kubernetes.Interface
    dynamic     dynamic.Interface
    factory     informers.SharedInformerFactory
    dynFactory  dynamicinformer.DynamicSharedInformerFactory
    Features    Features
    logger      *slog.Logger
}
type Features struct{ CertManager, ArgoCD bool } // from the discovery API at startup

func New(client, dynamicClient, Options{Features, VolumeStats}, logger) (*Cluster, error)
func (c *Cluster) Start(ctx) error          // start informers
func (c *Cluster) Synced() bool             // for /readyz
func (c *Cluster) BuildSnapshot(ctx, now) *checks.Snapshot // Snapshot.Skipped says which checks could not get their data
```
- `SetTransform` on every informer: strip `managedFields` and the `kubectl.kubernetes.io/last-applied-configuration` annotation; for Pods also strip `spec.volumes`/`env` etc.
- Events informer only for `type=Warning` (field selector).
- Kubelet stats: `GET /api/v1/nodes/<node>/proxy/stats/summary` per Ready node; errors per node are collected, and on 403 `pvc-usage` is marked as skipped ("missing RBAC nodes/proxy").

### engine
```go
type Engine struct {
    cluster  *kube.Cluster
    store    *store.Store
    notifiers []notify.Notifier // one per target (phase 13), at least notify.Log
    checks   []checks.Check   // enabled checks only
    opts     checks.Options
    interval, alertAfter, retention time.Duration
    baseURL  string
    now      func() time.Time
    logger   *slog.Logger
    mu       sync.Mutex
    status   Status
}
type Status struct {
    LastRun        time.Time
    LastRunError   string
    Checks         []CheckStatus  // Name, Enabled bool, SkipReason string
    Targets        []TargetStatus // phase 13: Name, Description, LastSentAt, LastError, LastErrorAt
}
func (e *Engine) Run(ctx)                       // ticker loop
func (e *Engine) RunOnce(ctx) error             // snapshot → checks → reconcile → notify → retention
func (e *Engine) reconcile(open []store.Record, findings []checks.Finding, skipped []string, now) store.Changes // pure, no I/O
func (e *Engine) Status() Status
```
**Reconcile rules:**
1. Finding with an open record (pending/firing) → update `last_seen`, message, severity. Pending and `now - first_seen >= ALERT_AFTER` → firing (`firing_at = now`).
2. New finding → insert as pending (immediately firing if `ALERT_AFTER = 0`).
3. Open record no longer present **and its check ran successfully in this run** → pending: delete silently; firing: resolved (`resolved_at = now`). Records of skipped or failed checks stay untouched (no false resolves after an API error).
4. Notify (phase 13: once per target, with that target's markers from `deliveries`): all firing records with `firing_notified_at IS NULL` plus all resolved records with `resolved_notified_at IS NULL`. Resolved records whose firing was never reported get marked silently (the user never heard about them). Only after `Send` succeeds, set the `*_notified_at` fields; on error they stay open and get retried in the next run (survives restarts).
5. Retention: delete resolved records older than `RETENTION` and expired sessions.

### store
```go
type Store struct{ db *sql.DB; now func() time.Time }
type Record struct {
    ID int64
    checks.Finding
    State              string // pending | firing | resolved
    FirstSeen, LastSeen time.Time
    FiringAt, ResolvedAt               *time.Time
    FiringNotifiedAt, ResolvedNotifiedAt *time.Time
    AcknowledgedAt                       *time.Time // set from the web UI; never written by Apply
}
func Open(path string, now func() time.Time) (*Store, error) // runs migrations
func (s *Store) OpenRecords(ctx) ([]Record, error)
func (s *Store) Insert/Update/Delete... ; Unnotified(ctx); MarkNotified(ctx, ids...)
func (s *Store) Acknowledge(ctx, id, at) (key string, changed bool, err error) // firing critical only
func (s *Store) Unacknowledge(ctx, id) (key string, changed bool, err error)    // firing only
func (s *Store) Record(ctx, id) (Record, bool, error)                    // any state, for the downtime form
func (s *Store) SetDowntime / ActiveDowntimes / ExpiredDowntimes / EndDowntime / FinishDowntime / DowntimeKeys
func (s *Store) Resolved(ctx, namespace string, limit int) ([]Record, error) // history
func (s *Store) Namespaces(ctx) ([]string, error)                         // history filter
func (s *Store) Cleanup(ctx, olderThan time.Time) error
func (s *Store) CreateSession / GetSession / DeleteSession
func (s *Store) Ping(ctx) error
```
`db.SetMaxOpenConns(1)`, PRAGMAs `journal_mode=WAL`, `busy_timeout=5000`. Reconcile runs in one transaction.

### notify
```go
type Notifier interface {
    Name() string        // stable id, stored in deliveries: "mail", "slack", "telegram:<chat>"
    Description() string // phase 13: label without secrets for the log and the status page
    Send(ctx context.Context, n Notification) error
}
type Notification struct {
    New      []Alert // sorted: critical first, then by object
    Resolved []Alert
    BaseURL  string
}
type Alert struct {
    checks.Finding
    Since      time.Time
    ResolvedAt time.Time // zero for New
}
func Subject(n Notification) string // "[smokealarm] CRITICAL: CrashLoopBackOff media/jellyfin" or "[smokealarm] 3 new problems, 1 resolved"
func Body(n Notification) (string, error) // text/template
type Mail struct{ Host, Port, TLS, Username, Password, From, To, TLSConfig, Now } // net/smtp; starttls | tls (port 465) | none; optional PLAIN auth; multiple recipients
type Log struct{ logger *slog.Logger } // used when no SMTP is configured, and in phases 1–3
```
Phase 13 adds `webhook.go`, `ntfy.go`, `telegram.go`, `slack.go` and `teams.go`, each a `Notifier` built on `post` in `http.go` (shared `*http.Client` from `NewHTTPClient`, the run's context, non-2xx = error with status and a short excerpt, URL and secrets taken out of errors), plus `ChatBody(n, limit)` in `message.go` for the chat targets.

### auth
```go
type PasswordLogin struct{ Username string; Hash []byte }
func (p PasswordLogin) Verify(user, password string) bool // constant-time username compare + bcrypt
type Sessions struct{ store *store.Store; ttl time.Duration; secureCookie bool; now func() time.Time }
func (s *Sessions) Start(w, r, username) error   // creates the session and sets the cookie – called by any login method (later OIDC too)
func (s *Sessions) End(w, r) error
func (s *Sessions) Require(next http.Handler) http.Handler // only checks "is there a valid session?"
type RateLimiter struct{ mu sync.Mutex; failures map[string][]time.Time; now func() time.Time } // per client IP: max 5 failures / 15 min
func HashPassword(password string) (string, error) // for the subcommand
```
Session ID: 32 random bytes (crypto/rand), base64url in the cookie; **only the SHA-256 hash** is stored in the DB. Cookie `smokealarm_session`, HttpOnly, Secure (configurable), SameSite=Lax, Path=/.

### web
Routes (`net/http` 1.22 patterns):
`GET /login`, `POST /login`, `POST /logout`, `GET /` (overview), `POST /problems/{id}/acknowledge`, `POST /problems/{id}/unacknowledge`, `GET /history?namespace=`, `GET /downtimes`, `GET /downtimes/new?finding=`, `POST /downtimes`, `POST /downtimes/{id}/end`, `GET /status`, `GET /static/style.css`, `GET /healthz`, `GET /readyz` (no auth for the last two and login/static). Templates: `layout.html` (meta refresh 30s, except on login), `login.html`, `overview.html`, `history.html`, `status.html`. CSS with `prefers-color-scheme`, mobile-friendly.

## 4. Database schema (`internal/store/migrations/001_init.sql`)
All timestamps are INTEGER (Unix seconds, UTC).
```sql
CREATE TABLE schema_migrations (
    version    INTEGER PRIMARY KEY,
    applied_at INTEGER NOT NULL
);

CREATE TABLE findings (
    id                   INTEGER PRIMARY KEY,
    key                  TEXT    NOT NULL,
    check_name           TEXT    NOT NULL,
    severity             TEXT    NOT NULL CHECK (severity IN ('critical','warning')),
    kind                 TEXT    NOT NULL,
    namespace            TEXT    NOT NULL DEFAULT '',
    name                 TEXT    NOT NULL,
    reason               TEXT    NOT NULL,
    message              TEXT    NOT NULL,
    state                TEXT    NOT NULL CHECK (state IN ('pending','firing','resolved')),
    first_seen           INTEGER NOT NULL,
    last_seen            INTEGER NOT NULL,
    firing_at            INTEGER,
    resolved_at          INTEGER,
    firing_notified_at   INTEGER,
    resolved_notified_at INTEGER
);
-- At most one open record per problem; resolved records stay as history.
CREATE UNIQUE INDEX findings_open_key ON findings(key) WHERE state IN ('pending','firing');
CREATE INDEX findings_state ON findings(state);
CREATE INDEX findings_resolved ON findings(resolved_at) WHERE state = 'resolved';

CREATE TABLE sessions (
    id_hash    TEXT PRIMARY KEY,   -- SHA-256 of the session ID, never the ID itself
    username   TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
);
CREATE INDEX sessions_expires ON sessions(expires_at);
```
Implementation note (phase 3): `001_findings.sql` contains only the `findings` table; `schema_migrations` is created by the migration code itself; the `sessions` table arrives with phase 5 as `002_sessions.sql`. Phase 9 adds the column `findings.acknowledged_at` (INTEGER, nullable) in `003_acknowledge.sql`. Phase 10 adds the table `downtimes` in `004_downtimes.sql`. Phase 13 adds the tables `deliveries` (finding_id, target, firing_notified_at, resolved_notified_at; one row per finding and target) and `targets` (name) in `005_deliveries.sql`; the marker columns in `findings` stay as the summary over all targets. Phase 14 adds the column `findings.targets` (TEXT, default `''`) in `006_targets.sql`. Phase 15 adds the columns `findings.caused_by` (TEXT, default `''`) and `findings.pending_since` (INTEGER, nullable; NULL means `first_seen`) in `007_node_cause.sql`.
Migrations: files `NNN_name.sql` are embedded, applied in order, each in a transaction; the version goes into `schema_migrations`.

## 5. Environment variables
Durations: Go format (`30s`, `5m`, `2h`) plus a `d` suffix for days (`30d`). Lists: comma-separated.

| Variable | Default | Description |
|---|---|---|
| `SMOKEALARM_CHECK_INTERVAL` | `30s` | Interval between check runs |
| `SMOKEALARM_ALERT_AFTER` | `2m` | How long a problem must persist before it fires |
| `SMOKEALARM_RETENTION` | `30d` | Keep resolved findings this long |
| `SMOKEALARM_DB_PATH` | `smokealarm.db` | SQLite file; the Helm chart sets `/data/smokealarm.db` |
| `SMOKEALARM_LISTEN_ADDR` | `:8080` | HTTP listen address |
| `SMOKEALARM_BASE_URL` | – | Public URL of the UI, used for links in mails |
| `SMOKEALARM_LOG_FORMAT` | `auto` | `json`, `text`, `auto` (json in cluster, text locally) |
| `SMOKEALARM_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `SMOKEALARM_DISABLED_CHECKS` | – | List of check names, e.g. `pod-restarts,pvc-usage` |
| `SMOKEALARM_IGNORE_NAMESPACES` | – | Namespaces without findings |
| `SMOKEALARM_RESTART_THRESHOLD` | `5` | `pod-restarts`: restarts above this value |
| `SMOKEALARM_POD_PENDING_AFTER` | `5m` | `pod-pending` threshold |
| `SMOKEALARM_PVC_PENDING_AFTER` | `5m` | `pvc-pending` threshold |
| `SMOKEALARM_CRONJOB_GRACE` | `5m` | Tolerance for `cronjob-missed` |
| `SMOKEALARM_ADMIN_USER` | **required** | Admin username |
| `SMOKEALARM_ADMIN_PASSWORD_HASH` | **required** | bcrypt hash (`smokealarm hash-password`) |
| `SMOKEALARM_SESSION_TTL` | `7d` | Session lifetime |
| `SMOKEALARM_COOKIE_SECURE` | `true` | Set to `false` only for local HTTP |
| `SMOKEALARM_SMTP_HOST` | – | If set, mail is enabled |
| `SMOKEALARM_SMTP_PORT` | `587` | SMTP port |
| `SMOKEALARM_SMTP_TLS` | `starttls` | `starttls`, `tls` (implicit, port 465), `none` (local testing only) |
| `SMOKEALARM_SMTP_USERNAME` | – | Optional, enables auth |
| `SMOKEALARM_SMTP_PASSWORD` | – | Required if username is set |
| `SMOKEALARM_SMTP_FROM` | required when mail is on | Sender address |
| `SMOKEALARM_SMTP_TO` | required when mail is on | Recipient list |
| `SMOKEALARM_WEBHOOK_URL` | – | Secret; switches the webhook on (phase 13) |
| `SMOKEALARM_WEBHOOK_HEADER` | – | Secret; `Name: value` |
| `SMOKEALARM_NTFY_TOPIC` | – | Secret; switches ntfy on |
| `SMOKEALARM_NTFY_URL` | `https://ntfy.sh` | ntfy server |
| `SMOKEALARM_NTFY_TOKEN` | – | Secret; access token |
| `SMOKEALARM_TELEGRAM_BOT_TOKEN` | – | Secret; switches Telegram on |
| `SMOKEALARM_TELEGRAM_CHAT_IDS` | required when Telegram is on | List of numeric ids or `@channel` |
| `SMOKEALARM_SLACK_WEBHOOK_URL` | – | Secret; switches Slack on |
| `SMOKEALARM_TEAMS_WEBHOOK_URL` | – | Secret; switches Teams on (Workflows webhooks only) |
| `SMOKEALARM_NOTIFY_ALLOW_HTTP` | `false` | Plain http for the webhook and ntfy server |
| `SMOKEALARM_<TARGET>_MIN_SEVERITY` | `warning` | Phase 14; `<TARGET>` = `SMTP`, `WEBHOOK`, `NTFY`, `TELEGRAM`, `SLACK`, `TEAMS`; `critical` = critical problems only |
| `KUBECONFIG` | – | Standard variable for local runs (see deviations) |

Validation collects all errors and exits with one clear message (e.g. `config: SMOKEALARM_ADMIN_USER is required; SMOKEALARM_SMTP_PORT: invalid number "abc"`). Unknown check names in `DISABLED_CHECKS` are an error.

## 6. Phase plan
Every phase: own branch from main, unit tests with fake objects, `go build`/`go vet`/`go test`/`golangci-lint` green, PR with "what's new / how to try it locally", then stop.

1. **phase-1-skeleton** – `go.mod`, `.gitignore`, minimal `.golangci.yml` (standard linters), `config` (core variables), slog setup, `kube` (clients, informers with transform, `BuildSnapshot` for core resources), `checks` types + `pods.go` + ignore helper with table-driven tests, `engine` loop that **logs** findings for now, `/healthz` and `/readyz` (cache sync). Try it: `KUBECONFIG=... go run ./cmd/smokealarm` (admin variables are not needed before phase 5).
2. **phase-2-core-checks** – `workloads.go`, `nodes.go`, `jobs.go` (incl. cronjob-missed with robfig/cron, `time/tzdata` import for distroless), `storage.go` (pvc-pending), `DISABLED_CHECKS`, tests incl. edge cases (just below/above threshold, ignored, suspended).
3. **phase-3-store** – `store` with migrations, `Reconcile`, retention, `notify.Log` as the notifier. Engine tests against a temp SQLite: pending→firing→resolved, flap suppressed, skipped check does not resolve, failed send gets retried.
4. **phase-4-mail** – `notify.Mail` (STARTTLS/TLS/auth/multiple recipients), subject/body templates, golden tests (`testdata/*.golden`, updated with `UPDATE_GOLDEN=1`), SMTP config validation, last send error in `Status`. Try it locally with Mailpit (`SMTP_TLS=none`).
5. **phase-5-web** – `auth` (login, sessions, middleware, rate limit), `hash-password` subcommand, `web` (overview, history with namespace filter, status, CSS, auto-refresh), `/readyz` incl. DB ping. Handler tests with `httptest`.
6. **phase-6-optional-checks** – discovery of cert-manager/Argo CD, dynamic informers + conversion to `Certificate`/`Application`, `certs.go`, `argocd.go`, kubelet stats + `pvc-usage`, skip reasons on the status page. Tests incl. "CRD missing".
7. **phase-7-packaging** – Dockerfile (multi-stage, distroless/static:nonroot, buildx amd64/arm64), Makefile (build, test, lint, run, image), Helm chart (RBAC read access: namespaces, pods, events, nodes, deployments, statefulsets, daemonsets, jobs, cronjobs, persistentvolumeclaims, storageclasses; Recreate, PVC, RBAC switches, Secret/existingSecret, probes, resources, securityContext, optional Ingress), README, ARCHITECTURE.md, LICENSE (Apache-2.0), CHANGELOG.md.
8. **phase-8-critical-reminders** – `SMOKEALARM_CRITICAL_REPEAT_INTERVAL` (default off): critical problems that stay open are reported again. No migration; tests for engine, store, config, chart and golden mails.
9. **phase-9-acknowledge** – acknowledge firing critical problems in the web UI: no more reminders, "resolved" is still reported. Migration `003_acknowledge.sql`; cross-origin protection for the new action; tests for engine, store and handlers.
10. **phase-10-downtime** – silence one problem (one key) until a fixed time, set for firing problems and from the history. List and early end in the web UI. Migration `004_downtimes.sql`; tests for engine, store and handlers.
11. **phase-11-remove-acknowledge** – a button to take an acknowledgement back. No migration and no change to the engine; tests for engine, store and handlers.
12. **phase-12-ui-design** – a new look for the web UI: stylesheet, templates, inline SVG icons (`templates/icons.html`), favicon, and counters of critical problems and warnings on the overview. No change to behavior, texts or routes; tests for the counters, the favicon and templates without `style` attributes. The history table has five columns instead of seven (the duration below the time, the severity above the reason), so the downtime button stays visible on screens from about 1000 px. Screenshots for the README in `docs/screenshots/`.

13. **phase-13-notify-targets** – more notification targets besides mail: webhook, ntfy, Telegram, Slack, Teams (Workflows), standard library only. Per-target delivery tracking in migration `005_deliveries.sql`; status page per target; Helm values with secrets in the Secret only. Tests per target with `httptest` (no network), golden files for every format, config validation, engine and store tests for partial failure, restart, acknowledge and downtimes with two targets.
14. **phase-14-routing** – route problems by severity per target (`SMOKEALARM_<TARGET>_MIN_SEVERITY`) and by the annotations `smokealarm.gagaschu.github.io/targets` and `/severity` on the object or namespace. `checks.ApplyAnnotations` after the checks, `Finding.Targets`, migration `006_targets.sql`, `routedTo` in the engine, status page and overview/history show the routing, Helm `minSeverity` per target. Tests for annotations, store, config, engine (two targets: critical only, escalation, end after a change, typo, acknowledge), web and chart.
15. **phase-15-node-down** – a node that is down is the alarm; the problems it causes are held back. `checks.DownNodes` and `checks.ApplyNodeCauses` (after `ApplyAnnotations`) set `Finding.CausedBy` for pods on a down node and for Deployments/StatefulSets/DaemonSets with a pod there. The engine keeps such a problem pending while its node is down and restarts its ALERT_AFTER clock (`pending_since`) when the node is back; migration `007_node_cause.sql`. NotReady comes first in notifications, the mail gets a section NODE DOWN with the affected problems, chat a section "Node down:", the webhook an `affected` list; the overview shows the held-back problems by node. Tests for checks, store, engine (held back, late consequence, node back with and without recovery, ignored node, pod without node, already firing, routing of the affected list), notify goldens and web.

## 7. Deliberate deviations / decisions
- **Notification targets (phase 13), per-target delivery (decided by the maintainer):** each target has its own "reported" markers in `deliveries`, and the engine runs the existing notify logic once per target. A target that fails is retried on its own; the others get no duplicates, also after a restart. Every rule (ALERT_AFTER, combined message, reminders, acknowledge, downtimes) applies per target as it did to mail; each target has its own reminder clock. The alternatives were all-or-nothing (no schema change, but duplicates at every other target on every outage) and best effort (messages lost).
- **Phase 13, the summary markers:** `findings.firing_notified_at`/`resolved_notified_at` stay and mean "reported to at least one target". `reconcile`, the escalation rule and the web UI keep using them unchanged. An escalation is named explicitly in `Changes.Escalated`; `Apply` and `FinishDowntime` then clear the markers of every target, and all of them hear about it again.
- **Phase 13, adding and removing targets:** `SyncTargets` runs at startup. A new target gets no history, but it does hear about what is firing and not acknowledged: otherwise it would stay silent during an alarm and then report only its end. *(Changed in the review of phase 13; before, it copied the firing markers too.)* A resolved problem it never heard about is closed without a message; the end of an acknowledged one is still sent, as for every target. Only the first start after the upgrade from phase 12 (no target known yet) copies all summary markers, because they are what mail or log has already told, so nothing is sent twice; a target added in that same step is treated alike. A target no longer configured is forgotten with its rows; if it comes back, it counts as new again. The table `targets` is needed so that a target that has never delivered anything is not re-seeded on every restart.
- **Phase 13, no `notify.Multi`:** with per-target markers the targets can need different content (one has caught up, another has not), so the fan-out has to know about the markers. One loop in the engine is simpler than a type that hides it.
- **Phase 13, ids and labels:** `Notifier.Name()` is the stable id in the database; `Description()` is the label without secrets. Telegram gets one notifier per chat id (`telegram:<id>`), so a chat that fails does not repeat the message in the others. The ntfy topic is not shown anywhere, because on ntfy.sh it works like a password; the chart keeps it in the Secret.
- **Phase 13, ntfy priority:** a new critical problem is `urgent` (a smoke alarm must get through), reminders only are `high` (known, but still on fire), new warnings are `default`, resolved only is `low`. Tags: `rotating_light`, `warning`, `white_check_mark`.
- **Phase 13, length limits:** `ChatBody` counts bytes (exact for ntfy, on the safe side for character limits) and ends with "… and N more, see <base URL>". Telegram: 4096 characters per message, minus the headline. ntfy: 4096 bytes, a longer body becomes an attachment, so 4000. Slack: `text` is cut at 40,000 characters, but a section block holds 3000 and a header 150, so 3000 and 150. Teams: about 28 KB per message including the card. The Teams card has one TextBlock per line, because single line breaks inside one block are not reliably kept, and those blocks count too: the text starts at 20,000 and is shortened until the whole message is at most 26,000 bytes.
- **Phase 13, security:** no redirects are followed (a secret must not go to another URL, and a misconfigured URL shows up as an error). `http.go` replaces the `*url.Error` from net/http by its cause and removes the URL, its path and query and the known secrets from errors and response excerpts. Plain http only for localhost, or for the webhook and ntfy with `SMOKEALARM_NOTIFY_ALLOW_HTTP` (relays in the cluster usually speak plain http); Slack, Teams and Telegram always https. Config errors name the variable, never a secret value. Old Teams connector URLs (`*.webhook.office.com`) are refused with a hint.
- **Phase 13, Helm:** secrets (`webhook.url`, `webhook.header`, `ntfy.topic`, `ntfy.token`, `telegram.botToken`, `slack.webhookUrl`, `teams.webhookUrl`) go into the chart's Secret only; with `existingSecret` the chart refuses them in the values instead of ignoring them. Telegram chat ids must be strings, because Helm turns bare numbers into floats (`-1001234567890` → `-1.00123456789e+12`).
- **Phase 13, routing later:** because the engine builds one report per target, a filter per target (e.g. minimum severity) fits into `notifyTarget` before `Send`, configured by new optional variables per group (`SMOKEALARM_SLACK_MIN_SEVERITY`) whose defaults are today's behavior, so no configuration breaks. Several instances of one kind could get suffixed ids (`slack:ops`) without touching stored data. *(Done in phase 14, see below; several instances are still open.)*
- **Phase 14, annotations instead of labels (decided by the maintainer):** routing per app uses our own annotations, like `ignore`, not Kubernetes labels. They are read from the object and its namespace, which the snapshot already has; labels would have needed the labels in every finding and owner lookups for pods. `targets` names kinds of targets (`telegram` = all chats, via `notify.Kind`), `severity` replaces the check's severity in both directions. The object wins over the namespace, each annotation on its own; an empty value or a list without names counts as not set.
- **Phase 14, applied after the checks:** `checks.ApplyAnnotations` looks up each finding's object in the snapshot by kind, namespace and name, so the 21 `newFinding` calls and the checks stay unchanged and pure. Like `isIgnored`, a `workload-replicas` problem reads the Deployment's own annotations, a pod problem those of the pod (copied from the template), a CronJob also those of its job template (which `isIgnored` sees on the newest Job). *(Review: the kinds are `Kind…` constants shared by the checks and `objectAnnotations`, and a test covers every kind.)*
- **Phase 14, the targets are stored:** `findings.targets` (migration `006_targets.sql`, comma-separated, empty = every target), updated in every run, so reminders and the end of a problem know them also when the object is gone. The severity was already stored. Annotations never change the key.
- **Phase 14, the routing rule:** a target hears a new problem or a reminder if the severity is at least its `MinSeverity` and the targets annotation, if any, names its kind. A firing problem not meant for a target gets no marker there: it simply waits in `Unnotified`, so the target hears it as new once it is meant for it (escalation, annotation changed), without any extra state. The end is reported by marker: a target that heard the start hears the end, even after a change; one that never heard it is closed silently (the existing branch). An acknowledged problem whose first message never went out reports its end only where it is routed. *(Review, findings 1 and 2: an escalation and the end of a downtime cleared the markers of every target, so a target the problem had moved away from never heard the end. Now `Changes.Escalated` and `FinishDowntime` carry the targets the problem is meant for now, and only their markers are cleared. The end of a downtime uses the severity and targets of this run's finding, because it runs before they are saved. Consequence: for a target the problem moved away from, a downtime starts nothing over, so it hears the end even if the problem goes away in the run in which the downtime ends; otherwise its alarm would never end.)*
- **Phase 14, typos:** annotation values cannot be checked at startup. A targets list that names no configured target counts as no list (every target), so a mistake never silences an alarm; names that are not configured and an invalid severity are logged as a warning once, when they show up (like a skipped check), and the check's severity counts. *(Review: the warnings name the object or namespace that carries the annotation (`Finding.TargetsFrom`, not stored) instead of every problem, and are remembered for the lifetime of the process, so a problem that comes back does not log again.)*
- **Phase 14, a problem that reaches nobody (decided by the maintainer in the review):** `targets: ntfy` with a critical-only ntfy sends the object's warnings nowhere. The AND rule stays, because a fallback would send warnings to targets the annotation deliberately left out; instead it is logged once, and `Engine.Audience`, the same rule as the sending, lets the overview and the history say "Notifies nobody" (or "Notifies only: …" when not every target hears about it).
- **Phase 14, the branch (decided by the maintainer):** the cloud session is bound to `claude/trusting-brown-e0a7ty`, so this phase does not use `phase-14-routing`.
- **Phase 15, held back as pending (decided by the maintainer):** consequences of a down node are not reported, not even grouped; the node's NotReady carries them as "Affected". Keeping them pending reuses every existing rule: pending is never reported, a pending problem that goes away is dropped silently, and acknowledgements and downtimes need no change. The attribution is sticky while the node is down, because after the eviction a replacement pod starting elsewhere would otherwise let a Deployment problem through; when the node is back, the clock restarts (`pending_since`), because pods need a moment to become ready again, and `first_seen` stays the real start for the reader.
- **Phase 15, routing of a node (decided by the maintainer):** only the node's own annotations decide about its NotReady (as since phase 14). Consequences are held back whenever the node is down, also for an ignored test node, so a test node does not produce single pod and workload alarms instead.
- **Phase 15, workloads by selector:** a workload counts as a consequence if at least one of its pods (its label selector, same namespace) is on a down node. Owner references would need the ReplicaSets in the snapshot. A workload with a second, unrelated problem is held back too; the pod checks still report that pod on its own.
- **Phase 15, webhook:** NotReady stays in `new`, so receivers keep working; `affected` is a new field, so the format version stays 1.
- **Phase 15, the branch (decided by the maintainer):** the cloud session is bound to `claude/gracious-einstein-n8dbn4`, so this phase does not use `phase-15-node-down`.
- **Phase 14, `MIN_SEVERITY` for a target that is off is refused**, like `SMOKEALARM_NTFY_TOKEN` without a topic. Helm renders the variable only if `minSeverity` is set.
- **New look (phase 12):** icons are inline SVG in `templates/icons.html` instead of image files or an icon font, so they need no extra request, take the text color and work with the CSP `default-src 'self'`. That policy also blocks `style` attributes (a test checks the templates) and `data:` images, so color and line width of the icons are set in the stylesheet. No web fonts: the system font is used, as before. The favicon is a static SVG file under `/static/`, which needs no login. The login page has no top bar; its card shows the logo.
- **README screenshots (phase 12):** PNG files in `docs/screenshots/`, taken by `TestScreenshots` in `internal/web` (`make screenshots`, which sets `UPDATE_SCREENSHOTS=1` like `UPDATE_GOLDEN=1` for the mails). It renders the pages from made-up data through the real handlers, serves them with an `httptest` server and calls headless Chrome from the command line (`--screenshot`, `--force-dark-mode`), so it needs no Node or Playwright and no new Go dependency. Chrome can only take a picture of its window, so the window is taller than the page and the test cuts off the empty background with the standard `image` packages. `pngquant`, if installed, reduces the files to 256 colors (about 430 KB together instead of about three times that). Normal test runs skip it; with `UPDATE_SCREENSHOTS=1` a missing Chrome is an error. `docs` is in `.dockerignore`, so the images do not go into the build context.
- **Removing an acknowledgement (phase 11, rules agreed with the maintainer):** the problem then behaves like any other, and the engine needs no change. An overdue reminder or a first mail that was held back goes out in the next run, so a mail can follow right away. A grace period would need a new column or a false `firing_notified_at`. A problem that is only a warning at that moment cannot be acknowledged again until it is critical, because warnings cannot be acknowledged.
- **Downtimes (phase 10), own table:** a downtime belongs to a key, not to a row in `findings`, because it must outlive a "resolved" and cover the problem when it returns. Object and reason are copied, because the row can reach its retention while the downtime runs. One downtime per key; a new one replaces it. `AUTOINCREMENT`, so a button on an old page cannot hit a later downtime.
- **Downtimes (phase 10), the end:** `Unnotified` cannot find a problem that was reported before the downtime. `RunOnce` therefore runs `finishDowntimes` first: in one transaction it removes the ended downtime and clears `firing_notified_at` of a firing, unacknowledged problem. That reuses the path of an escalation (reported as new; a failed mail is retried, also after a restart), and the reminder clock runs from that mail. It has to happen before `saveState`, which writes back the markers it read. Ending early only sets `expires_at = now`, so there is one path for both.
- **Downtimes (phase 10), silent while it exists:** `notify` treats every row in the table as a downtime, whether its end has passed or not. That closes the gap between finishing and notifying. A downtime of a check whose state is unknown is not finished, so it can stay in the table past its end (hidden in the UI) until the check runs again; a mail would otherwise claim a problem that may be gone. For a check skipped with `KeepProblems` that lasts until the permission is back.
- **Downtimes (phase 10), together with acknowledge:** while it runs, the downtime wins: an acknowledged problem that goes away sends no "resolved". When it ends, the acknowledgement still counts: no mail, and "resolved" is reported later (decided by the maintainer).
- **Downtimes (phase 10), the last run:** a problem that goes away in the run in which the downtime ends counts as gone during the downtime and stays silent, because nobody can tell which came first. An acknowledged problem sends its "resolved" here, because it keeps its marker.
- **Downtimes (phase 10), the form has its own page:** the overview and the history reload every 30 seconds, which would discard the input. No upper limit for the end.
- **Downtimes (phase 10), changing keys:** a new reason or a new object name is a new key and is not covered. That includes pods recreated by a Deployment, which get a new name. A downtime on the `workload-replicas` problem keeps working, but does not silence the problems of the pods themselves. The form, the list and the README say so.
- **Acknowledge (phase 9):** the mark belongs to the row in `findings`, not to the key, so a problem that returns after "resolved" is automatically a new, unacknowledged alarm. The store leaves acknowledged problems out of `Unnotified` (while firing) and `CriticalDueForReminder`. `Apply` never writes `acknowledged_at`, so a run that works on an older copy cannot undo an acknowledgement. It is not recorded who acknowledged (one admin). *(Phase 11: an acknowledgement can be removed.)*
- **Acknowledged before the first mail (phase 9, decided by the maintainer):** the first mail is dropped, because somebody already knows; a late "CRITICAL" after a mail outage would only be noise. "Resolved" is still reported, so the silent branch in `notify` applies only to unacknowledged problems. Other recipients therefore learn about such a problem from the "resolved" mail.
- **An escalation does not clear the marker of an acknowledged problem (phase 9):** `Unnotified` would already keep the mail back, but `firing_notified_at` should keep saying when the problem was last reported, and the log should not claim an escalation is being reported.
- **Changing keys and acknowledge (phase 9):** only `pvc-usage`, `cert-expiry` and `workload-replicas` keep their key when the reason changes. For the other checks a new reason is a new alarm, which is not acknowledged and gets its own mail. Accepted and documented, not worked around.
- **Reminders (phase 8):** only for critical problems, with one clock per problem. `firing_notified_at` now means "last reported", so no new column is needed and a failed reminder is retried like any other send. Checks whose state is unknown get no reminder, because the mail would claim the problem still exists. The interval must be at least `CHECK_INTERVAL`, so a typo cannot cause a mail per run.
- **`KUBECONFIG` without prefix:** it is the standard variable used by kubectl/client-go; a separate `SMOKEALARM_KUBECONFIG` would only add confusion.
- **`notify.Log` as a second notifier:** a real second implementation (used when mail is off, and in phases 1–3), so the interface is not built "just in case".
- **Escalation = same problem (changed after phase 6):** the first design (warning→critical as a new key) showed "All good" and sent a "resolved" mail while a volume was getting fuller. Now an escalation updates the existing record and is reported again; see the paragraph under the check table. Keys of the three affected checks changed (`…/usage`, `…/expiry`, `…/replicas`); no migration, because v0.1 is unreleased and old rows resolve on the next run.
- **CronJob job failures are reported on the CronJob**, so the key stays stable across job runs.
- **Only check-specific thresholds marked "Default" in the spec are configurable**; all other values are constants (spec: "no configuration effort").
- **Rate limiting per client IP, in memory:** behind an ingress this effectively becomes one shared limit for everyone — acceptable for a single admin user.
- **Engine status in memory** (mutex-protected); after a restart, "last send error" is empty until the next attempt.
- **CSRF:** no token; SameSite=Lax plus POST-only for login/logout is enough for v0.1. *(Phase 9: SameSite=Lax treats every host of the same site as "same site", so another application under the same domain could submit a form with the session cookie. Actions that change state therefore also go through `http.CrossOriginProtection`, which checks `Sec-Fetch-Site` and falls back to comparing `Origin` with `Host`. It needs no token, no state, no JavaScript and no new dependency. Limit: on plain HTTP behind a proxy that rewrites `Host`, actions are refused with 403. Login and logout are unchanged.)*
- **Store persists, engine decides (phase 3):** `store.Apply(Changes)` writes inserts, updates and deletes in one transaction; the pending/firing/resolved rules live in the pure function `engine.reconcile`.
- **`skipped` instead of `ranChecks` (phase 3):** `reconcile` gets the checks that could not run this time. Problems of checks that were disabled (or removed) are resolved normally instead of staying "firing" forever.
- **Per-run finding log line is Debug (phase 3):** state transitions are logged at Info, so the log does not repeat every 30 s.
- **`go:embed` needs one package-level variable** (`migrationFiles` in `internal/store`); it is never modified.
- **Mail (phase 4):** STARTTLS is required in `starttls` mode (a server without it is an error, no silent downgrade); `none` is for local testing. The body is quoted-printable. There is no connection test at startup, so a mail server that is briefly down at boot does not stop monitoring.
- **Golden tests use `UPDATE_GOLDEN=1`** instead of a `-update` flag, because a `flag` variable would be a package-level variable.
- **`Status` starts minimal (phase 4):** only `NotifierName`, `LastNotifyError` and `LastNotifyErrorAt`; the error is cleared by the next successful send. `LastRun`, checks and the rest follow with the status page in phase 5.
- **Status page data (phase 5):** `engine.Status` has no `MailConfigured` field; "mail configured" is derived from `NotifierName == "mail"`. *(Phase 13: replaced by `Status.Targets`, one entry per target.)* Skipped checks come from `Engine.SkippedChecks`, which main fills (phase 6 adds "CRD not installed" without changing the engine).
- **Overview shows firing problems only (phase 5):** pending ones have not lasted `ALERT_AFTER` yet; listing them would contradict the mails. Before the first run it says "Starting up" instead of "All good".
- **`web.Server` (phase 5):** dependencies are plain fields (`Ready func(ctx) error`, `Status func() engine.Status`, `Store`, `Sessions`, ...). `Handler()` parses the templates and returns an error if one is broken. `/readyz` in main = informer caches synced **and** database ping.
- **Second `go:embed` variable (phase 5):** `assets` in `internal/web` (templates and CSS), never modified. The first one is `migrationFiles` in `internal/store`.
- **`hash-password` reads stdin with echo on (phase 5):** hiding the typed password needs `golang.org/x/term`, which is not on the allowed list. Use `read -s PW; printf '%s' "$PW" | smokealarm hash-password`.
- **Sessions (phase 5):** fixed expiry (no sliding renewal); only the SHA-256 hash of the session id is stored; the login handler never accepts a redirect target. The rate limit uses `RemoteAddr` only, so behind an ingress all clients share one limit.
- **Security headers (phase 5):** CSP `default-src 'self'` (the pages need no inline script or style), `X-Content-Type-Options`, `X-Frame-Options`, `Referrer-Policy`, `Cache-Control: no-store` on pages.
- **Phase 6, skipping checks:** two kinds. *Static*: checks whose CRDs are missing (`Check.Requires`, detected once at startup) are not in `Engine.Checks` and show up as skipped with the reason; problems left over from an earlier run are resolved normally. *Temporary*: `Snapshot.Skipped` (check name → reason) is set by the data collection; the engine does not run those checks and leaves their open problems untouched (this is what the `skipped` parameter of `reconcile` is for). A change in the skipped set is logged once, not every run.
- **Phase 6, `pvc-usage`:** only Ready nodes are asked for kubelet stats. If any of them fails (403 or other), the whole check is skipped for that run, because a missing node would make its full volumes look "fine again" and resolve their problems. NotReady nodes are not asked and do not cause a skip; their volume findings may resolve (`node-not-ready` reports the node). Without the check enabled no kubelet request is made and `nodes/proxy` is not needed.
- **Phase 6, memory:** the dynamic informers keep only the fields the checks read (Argo CD Applications are large).
- **Phase 6, RBAC list for the Helm chart (phase 7), all get/list/watch unless noted:** namespaces, pods, events, nodes, deployments, statefulsets, daemonsets, jobs, cronjobs, persistentvolumeclaims, storageclasses; `nodes/proxy` (`get`, only if `pvc-usage` is enabled); `certificates.cert-manager.io` and `applications.argoproj.io` (each behind its own Helm switch). Discovery needs no extra rule.
- **Phase 6, CRD detection:** a 404 means "not installed"; any other discovery error aborts startup instead of silently switching checks off. CRDs installed later are noticed after a restart. *(Phase 7 review: `DetectFeatures` only looks at the CRDs that an enabled check requires, and it also lists one object of each. A 403 means "no permission": the checks are skipped with a reason, like for a missing CRD, instead of an informer that never syncs. `Features.Has` and `featuresInUse` are gone; `main` passes `Check.Requires` of the enabled checks to `DetectFeatures` and splits the checks by the reasons it returns.)*
- **Phase 6, `pvc-usage` and `local-path`:** the kubelet reports the usage of the filesystem the volume lives on. For k3s `local-path` volumes that is the node's disk (the requested size is not enforced), so every such claim shows the same numbers and the check only alarms when the node disk is nearly full. It is meaningful for volumes with their own filesystem (Longhorn, Ceph, iSCSI/block volumes, a dedicated mount). To test it, use a static `local` PV on a small tmpfs (see the PR description).
- **Phase 7, no version in the binary:** `-X main.version` needs a package-level variable, which the spec forbids. The image tag and the chart's `appVersion` carry the version.
- **Phase 7, the container writes only to `/data`:** `SMOKEALARM_DB_PATH=/data/smokealarm.db` and `TMPDIR=/data` (SQLite temporary files), so `readOnlyRootFilesystem` works and the volume is the only writable place. `GOMEMLIMIT=100MiB` (below the 128Mi limit) makes the Go garbage collector respect the limit.
- **Phase 7, RBAC switches and checks stay in sync:** the chart's `rbac.nodesProxy`, `rbac.certManager`, `rbac.argocd` remove the permission **and** add the matching checks to `SMOKEALARM_DISABLED_CHECKS`. Together with the change in `main` (informers only for CRDs that an active check needs), an install without a permission cannot hang on caches that never sync. *(Phase 7 review: `TestRBACSwitchesCoverEveryCheckThatNeedsThem` ties the check names in `_helpers.tpl` to `checks.All()`, so a new check cannot run without its permission.)*
- **Phase 7, image name:** the chart defaults to `ghcr.io/gagaschu/smokealarm` (derived from the GitHub account, an assumption). No image is published; the README says how to build and push one. Multi-arch builds need a buildx builder with multi-platform support.
- **Phase 7, tests for packaging:** the chart is tested by rendering it with `helm template` and checking typed objects (skipped without `helm`); a docs test checks that the README mentions every `SMOKEALARM_*` variable of the config code and every check name.
- **Phase 7 review, `nodes/proxy` (SPEC: only with an active `pvc-usage`):** the rule is rendered only if `rbac.nodesProxy` is on **and** `pvc-usage` is not in `disabledChecks`. The default of `rbac.nodesProxy` stays `true` (decided by the maintainer); the README, `values.yaml` and the rule's comment say that `get` on `nodes/proxy` is not read-only (Kubernetes RBAC good practices) and how to leave it out.
- **Phase 7 review, sync timeout:** `Cluster.WaitForSync` gives the caches two minutes. After that smokealarm exits with an error that names the caches that did not fill (usually a missing permission for a core resource with own RBAC), so the pod fails visibly with the cause in the log instead of staying unready forever. Shutdown during the wait keeps its own message.
- **Phase 7 review, switching a check off still resolves its problems:** unchanged engine behavior (decision of the maintainer). It is documented in the README, `values.yaml` and the install notes, because an `rbac.*` switch now switches checks off implicitly.
- **Phase 7 review, no permission is not "gone":** a 403 on an optional CRD at startup says nothing about the objects, so those checks are skipped with `SkippedCheck.KeepProblems`, and their open problems stay as they are instead of being mailed as resolved. A CRD that is not installed and a check that is switched off still resolve their problems.
- **Phase 7 review, ClusterRole name:** `<namespace>-<fullname>` for the ClusterRole and its binding, so one release name can be installed in several namespaces. Upgrading from the first chart creates the new objects and removes the old ones.
- **Phase 7 review, probes:** a `startupProbe` on `/healthz` (up to 150 s) protects a slow start (database, migrations, cache sync on a Raspberry Pi) from the liveness probe; all probes use a 3 s timeout.
- **Phase 7 review, consistency tests:** every `SMOKEALARM_*` name in the chart must be read by the config code (`config.Load` ignores unknown variables), one chart test names every variable the chart can render, and the rendered environment is fed to `config.Load`. The Dockerfile keeps a default `GO_VERSION` so a plain `docker build .` works; a test makes sure it equals `go.mod`. The chart tests read the chart files themselves, otherwise Go's test cache would not notice a changed template.
- **Phase 7 review, Makefile:** `make test` no longer uses `-race` (on Linux it needs cgo, while the project builds with `CGO_ENABLED=0`); `make test-race` keeps it. `make chart` lints and renders the defaults and each file in `deploy/helm/examples/`.
