# smokealarm – Implementation Plan

## 1. Overview
smokealarm runs as a single Deployment (1 replica). Every `CHECK_INTERVAL` it builds a **snapshot** from the informer caches, runs all enabled **checks** (pure functions → `[]Finding`), **reconciles** the findings with the state in SQLite (pending → firing → resolved), sends **one combined notification** for all unreported changes, and cleans up old data. A small `net/http` server shows the current state.

```
informers ──► kube.BuildSnapshot ──► checks.Run* ──► engine.Reconcile ──► store (SQLite)
                                                           │
                                                           └──► notify.Notifier (mail / log)
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
| `internal/notify` | `Notifier` interface, `Mail` (SMTP), `Log` (slog) implementations, subject/body templates | `checks` |
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
    notifier notify.Notifier
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
    Checks         []CheckStatus // Name, Enabled bool, SkipReason string
    LastNotifyError   string
    LastNotifyErrorAt time.Time
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
4. Notify: all firing records with `firing_notified_at IS NULL` plus all resolved records with `resolved_notified_at IS NULL`. Resolved records whose firing was never reported get marked silently (the user never heard about them). Only after `Send` succeeds, set the `*_notified_at` fields; on error they stay open and get retried in the next run (survives restarts).
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
    Name() string
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
Telegram/webhook later as separate files `telegram.go` / `webhook.go` that implement `Notifier`; the engine does not change.

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
Implementation note (phase 3): `001_findings.sql` contains only the `findings` table; `schema_migrations` is created by the migration code itself; the `sessions` table arrives with phase 5 as `002_sessions.sql`. Phase 9 adds the column `findings.acknowledged_at` (INTEGER, nullable) in `003_acknowledge.sql`. Phase 10 adds the table `downtimes` in `004_downtimes.sql`.
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

## 7. Deliberate deviations / decisions
- **New look (phase 12):** icons are inline SVG in `templates/icons.html` instead of image files or an icon font, so they need no extra request, take the text color and work with the CSP `default-src 'self'`. That policy also blocks `style` attributes (a test checks the templates) and `data:` images, so color and line width of the icons are set in the stylesheet. No web fonts: the system font is used, as before. The favicon is a static SVG file under `/static/`, which needs no login. The login page has no top bar; its card shows the logo.
- **README screenshots (phase 12):** PNG files in `docs/screenshots/`, rendered from made-up data through the real handlers and taken with a headless browser at twice the pixel density, reduced to 256 colors to keep them small. The helper for that is not in the repository. When the look changes, the screenshots have to be taken again by hand. `docs` is in `.dockerignore`, so the images do not go into the build context.
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
- **Status page data (phase 5):** `engine.Status` has no `MailConfigured` field; "mail configured" is derived from `NotifierName == "mail"`. Skipped checks come from `Engine.SkippedChecks`, which main fills (phase 6 adds "CRD not installed" without changing the engine).
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
