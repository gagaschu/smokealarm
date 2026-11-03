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
| | `pod-image-pull` | critical, ImagePullBackOff / ErrImagePull |
| | `pod-oomkilled` | warning, OOMKilled (last termination < 15m ago) |
| | `pod-restarts` | warning, TooManyRestarts (> threshold and last restart < 1h ago) |
| | `pod-pending` | warning, reason from condition/event (e.g. Unschedulable) |
| workloads.go | `workload-replicas` | warning ReplicasUnavailable / critical NoReplicasAvailable |
| nodes.go | `node-not-ready` | critical, NotReady |
| | `node-pressure` | warning, MemoryPressure / DiskPressure / PIDPressure |
| jobs.go | `job-failed` | warning, JobFailed (standalone Job) / LastJobFailed (on the **CronJob**, newest job only → stable key) |
| | `cronjob-missed` | warning, MissedSchedule (robfig/cron, respects `spec.timeZone`, suspended CronJobs skipped) |
| storage.go | `pvc-pending` | warning, PVCPending |
| | `pvc-usage` | warning VolumeAlmostFull (>85 %) / critical VolumeFull (>95 %) |
| certs.go | `cert-not-ready` | warning, CertificateNotReady |
| | `cert-expiry` | warning ExpiresSoon (<14d) / critical ExpiresVerySoon (<3d) |
| argocd.go | `argocd-health` | critical, Degraded / Missing |
| | `argocd-sync` | warning, OutOfSync |

When severity escalates, the reason changes on purpose (so the key changes too): the escalation goes out as a new problem, and the old finding resolves. This keeps the state logic simple: one key always has one severity.

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

func New(cfg Config, logger) (*Cluster, error)
func (c *Cluster) Start(ctx) error          // start informers
func (c *Cluster) Synced() bool             // for /readyz
func (c *Cluster) BuildSnapshot(ctx, now, withVolumeStats bool) (*checks.Snapshot, []error)
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
    MailConfigured bool
    LastNotifyError   string
    LastNotifyErrorAt time.Time
}
func (e *Engine) Run(ctx)                       // ticker loop
func (e *Engine) RunOnce(ctx) error             // snapshot → checks → reconcile → notify → retention
func (e *Engine) Reconcile(ctx, findings []checks.Finding, ranChecks []string, now) error // testable without kube
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
}
func Open(path string, now func() time.Time) (*Store, error) // runs migrations
func (s *Store) OpenRecords(ctx) ([]Record, error)
func (s *Store) Insert/Update/Delete... ; Unnotified(ctx); MarkNotified(ctx, ids...)
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
type Mail struct{ ... }  // net/smtp; starttls | tls (port 465) | none; optional PLAIN auth; multiple recipients
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
`GET /login`, `POST /login`, `POST /logout`, `GET /` (overview), `GET /history?namespace=`, `GET /status`, `GET /static/style.css`, `GET /healthz`, `GET /readyz` (no auth for the last two and login/static). Templates: `layout.html` (meta refresh 30s, except on login), `login.html`, `overview.html`, `history.html`, `status.html`. CSS with `prefers-color-scheme`, mobile-friendly.

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
Migrations: files `NNN_name.sql` are embedded, applied in order, each in a transaction; the version goes into `schema_migrations`.

## 5. Environment variables
Durations: Go format (`30s`, `5m`, `2h`) plus a `d` suffix for days (`30d`). Lists: comma-separated.

| Variable | Default | Description |
|---|---|---|
| `SMOKEALARM_CHECK_INTERVAL` | `30s` | Interval between check runs |
| `SMOKEALARM_ALERT_AFTER` | `2m` | How long a problem must persist before it fires |
| `SMOKEALARM_RETENTION` | `30d` | Keep resolved findings this long |
| `SMOKEALARM_DB_PATH` | `/data/smokealarm.db` | SQLite file (locally e.g. `./smokealarm.db`) |
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

1. **phase-1-skeleton** – `go.mod`, `.gitignore`, minimal `.golangci.yml` (standard linters), `config` (core variables), slog setup, `kube` (clients, informers with transform, `BuildSnapshot` for core resources), `checks` types + `pods.go` + ignore helper with table-driven tests, `engine` loop that **logs** findings for now, `/healthz` and `/readyz` (cache sync). Try it: `KUBECONFIG=... SMOKEALARM_ADMIN_USER=x SMOKEALARM_ADMIN_PASSWORD_HASH=x go run ./cmd/smokealarm`.
2. **phase-2-core-checks** – `workloads.go`, `nodes.go`, `jobs.go` (incl. cronjob-missed with robfig/cron, `time/tzdata` import for distroless), `storage.go` (pvc-pending), `DISABLED_CHECKS`, tests incl. edge cases (just below/above threshold, ignored, suspended).
3. **phase-3-store** – `store` with migrations, `Reconcile`, retention, `notify.Log` as the notifier. Engine tests against a temp SQLite: pending→firing→resolved, flap suppressed, skipped check does not resolve, failed send gets retried.
4. **phase-4-mail** – `notify.Mail` (STARTTLS/TLS/auth/multiple recipients), subject/body templates, golden tests (`testdata/*.golden`, `-update` flag), SMTP config validation, last send error in `Status`. Try it locally with Mailpit (`SMTP_TLS=none`).
5. **phase-5-web** – `auth` (login, sessions, middleware, rate limit), `hash-password` subcommand, `web` (overview, history with namespace filter, status, CSS, auto-refresh), `/readyz` incl. DB ping. Handler tests with `httptest`.
6. **phase-6-optional-checks** – discovery of cert-manager/Argo CD, dynamic informers + conversion to `Certificate`/`Application`, `certs.go`, `argocd.go`, kubelet stats + `pvc-usage`, skip reasons on the status page. Tests incl. "CRD missing".
7. **phase-7-packaging** – Dockerfile (multi-stage, distroless/static:nonroot, buildx amd64/arm64), Makefile (build, test, lint, run, image), Helm chart (Recreate, PVC, RBAC switches, Secret/existingSecret, probes, resources, securityContext, optional Ingress), README, ARCHITECTURE.md, LICENSE (Apache-2.0), CHANGELOG.md.

## 7. Deliberate deviations / decisions
- **`KUBECONFIG` without prefix:** it is the standard variable used by kubectl/client-go; a separate `SMOKEALARM_KUBECONFIG` would only add confusion.
- **`notify.Log` as a second notifier:** a real second implementation (used when mail is off, and in phases 1–3), so the interface is not built "just in case".
- **Escalation = new key:** warning→critical comes with its own reason (see table) instead of an in-place severity change.
- **CronJob job failures are reported on the CronJob**, so the key stays stable across job runs.
- **Only check-specific thresholds marked "Default" in the spec are configurable**; all other values are constants (spec: "no configuration effort").
- **Rate limiting per client IP, in memory:** behind an ingress this effectively becomes one shared limit for everyone — acceptable for a single admin user.
- **Engine status in memory** (mutex-protected); after a restart, "last send error" is empty until the next attempt.
- **CSRF:** no token; SameSite=Lax plus POST-only for login/logout is enough for v0.1.
