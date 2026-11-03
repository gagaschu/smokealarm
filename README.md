# smokealarm

> A smoke alarm for your Kubernetes cluster – silent until something's on fire.

smokealarm runs inside your cluster and answers exactly one question: **"What is broken right now?"**

It runs a small set of fixed, reliable checks without any configuration effort, shows the current problems and their history on a simple web page, and sends you a mail when something starts or stops being broken.

## What it is, and what it is not

smokealarm is deliberately small. It is **not**:

- a replacement for Prometheus (no metrics database, no graphs, no query language),
- a log or tracing system (no logs, no traces),
- a root-cause analysis tool (it tells you *that* a pod is crash-looping, not *why*).

It does a few things and tries to do them reliably: a handful of well-chosen checks, a state machine that avoids false alarms, one combined mail per change, and a web page that is readable on a phone. It was built and tested on k3s on Raspberry Pi (arm64), but needs nothing k3s-specific and runs on any standard Kubernetes.

How it works in one paragraph: every 30 seconds smokealarm looks at the objects in its in-memory cache of the cluster, runs all checks, and compares the findings with what it saw before. A new problem is **pending**; if it lasts longer than `SMOKEALARM_ALERT_AFTER` (2 minutes) it becomes **firing** and is reported; when it disappears it is **resolved** and reported again. Problems that come and go within the waiting time are never reported, so short flaps stay quiet. All changes found in one run go out as **one** mail. For the details see [ARCHITECTURE.md](ARCHITECTURE.md).

## Installation

You need a Kubernetes cluster, `kubectl` and [Helm](https://helm.sh/) 3 or 4.

### 1. The image

smokealarm is a single static binary in a minimal image (`linux/amd64` and `linux/arm64`). There is no published image yet, so build it and push it to a registry your cluster can pull from:

```sh
make image IMAGE=ghcr.io/<you>/smokealarm TAG=0.1.0 PUSH=1
```

(The multi-arch build needs a buildx builder that supports several platforms, see the comment in the [Makefile](Makefile). `make image-local` builds for your own machine only.)

### 2. The admin password

smokealarm has one local admin user. You give it the **bcrypt hash** of the password, never the password itself:

```sh
read -s PW; printf '%s' "$PW" | docker run -i --rm ghcr.io/<you>/smokealarm:0.1.0 hash-password
```

(Without Docker: `make hash-password` in a checkout.) The hash contains `$` characters, so put it in single quotes or in a variable and use `--set-string` as shown below.

### 3. Install the chart

```sh
HASH='<the hash from step 2>'
helm install smokealarm ./deploy/helm/smokealarm \
  --namespace smokealarm --create-namespace \
  --set image.repository=ghcr.io/<you>/smokealarm \
  --set-string auth.passwordHash="$HASH"
```

The hash then lives in a Kubernetes Secret created by the chart. If you prefer to manage the Secret yourself:

```sh
kubectl -n smokealarm create secret generic smokealarm-auth \
  --from-literal=SMOKEALARM_ADMIN_PASSWORD_HASH="$HASH"
helm install smokealarm ./deploy/helm/smokealarm -n smokealarm \
  --set image.repository=ghcr.io/<you>/smokealarm --set existingSecret=smokealarm-auth
```

(The same Secret may also hold `SMOKEALARM_SMTP_USERNAME` and `SMOKEALARM_SMTP_PASSWORD`.)

Ready-made values files for typical setups are in [`deploy/helm/examples/`](deploy/helm/examples): an ingress with TLS, mail, minimal permissions and your own Secret. Use one with `-f deploy/helm/examples/<file>`; each file starts with a comment that shows the full command.

### 4. Open it

```sh
kubectl -n smokealarm port-forward svc/smokealarm 8080:80
```

Then open <http://localhost:8080> and log in as `admin`. The login cookie is only sent over HTTPS by default; if the login does not stick over a plain-HTTP port-forward, install with `--set config.cookieSecure=false` for local use. For permanent access, use the ingress:

```yaml
# my-values.yaml
ingress:
  enabled: true
  className: nginx
  host: smokealarm.example.com
  tls:
    enabled: true
    secretName: smokealarm-tls
```

### 5. Mail

Mail is switched on by setting an SMTP host. Without it, state changes are only written to the log.

```yaml
smtp:
  host: smtp.example.com
  port: 587
  tls: starttls            # starttls, tls (port 465) or none (local testing only)
  from: smokealarm@example.com
  to: [me@example.com]
  username: smokealarm     # optional; stored in the Secret together with the password
  password: "..."
```

A mail looks like this:

```
Subject: [smokealarm] CRITICAL: CrashLoopBackOff media/jellyfin

1 new problem

NEW PROBLEMS

[CRITICAL] CrashLoopBackOff
  Object:  Pod media/jellyfin
  Message: container jellyfin keeps crashing (7 restarts), last exit code 137 (Error)
  Since:   2026-11-03 10:00:00 UTC

Details: https://smokealarm.example.com
```

With several changes in one run, the subject is a count (`[smokealarm] 3 new problems, 1 resolved`) and the body lists everything, critical problems first. If a problem that you were already told about becomes critical, you get a new mail for it. Sending is retried at the next run if the mail server is down, also after a restart.

## Configuration

Everything is configured with environment variables that start with `SMOKEALARM_`. The Helm chart sets them from its values (see [values.yaml](deploy/helm/smokealarm/values.yaml)). The program checks all values at startup and refuses to start with a clear message if something is wrong. Durations use Go syntax (`30s`, `5m`, `2h`) plus `d` for days (`30d`).

| Variable | Default | Description |
|---|---|---|
| `SMOKEALARM_ADMIN_USER` | – (required) | Name of the admin user. |
| `SMOKEALARM_ADMIN_PASSWORD_HASH` | – (required) | bcrypt hash of the admin password. Create it with `smokealarm hash-password`. |
| `SMOKEALARM_CHECK_INTERVAL` | `30s` | How often the checks run. |
| `SMOKEALARM_ALERT_AFTER` | `2m` | How long a problem must last before it is reported. `0s` reports at once. |
| `SMOKEALARM_RETENTION` | `30d` | How long resolved problems are kept in the history. |
| `SMOKEALARM_DB_PATH` | `smokealarm.db` | The SQLite database file. The container image uses `/data/smokealarm.db`. |
| `SMOKEALARM_LISTEN_ADDR` | `:8080` | Address of the web server. |
| `SMOKEALARM_BASE_URL` | – | Public URL of the web UI, used for the link in mails. |
| `SMOKEALARM_LOG_FORMAT` | `auto` | `json`, `text` or `auto` (JSON inside a cluster, text otherwise). |
| `SMOKEALARM_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. |
| `SMOKEALARM_DISABLED_CHECKS` | – | Comma-separated check names to switch off, for example `pod-restarts,pvc-usage`. An unknown name is an error. Open problems of a switched-off check are closed and reported as resolved, see [The checks](#the-checks). |
| `SMOKEALARM_IGNORE_NAMESPACES` | – | Comma-separated namespaces that never produce findings. |
| `SMOKEALARM_RESTART_THRESHOLD` | `5` | `pod-restarts` reports containers with more restarts than this. |
| `SMOKEALARM_POD_PENDING_AFTER` | `5m` | `pod-pending` reports pods that are pending longer than this. |
| `SMOKEALARM_PVC_PENDING_AFTER` | `5m` | `pvc-pending` reports claims that are pending longer than this. |
| `SMOKEALARM_CRONJOB_GRACE` | `5m` | How late a CronJob run may be before `cronjob-missed` reports it. |
| `SMOKEALARM_SESSION_TTL` | `7d` | How long a login stays valid. |
| `SMOKEALARM_COOKIE_SECURE` | `true` | Send the login cookie only over HTTPS. Set `false` only for local development over plain HTTP. |
| `SMOKEALARM_SMTP_HOST` | – | Mail server. Setting it switches mail on. |
| `SMOKEALARM_SMTP_PORT` | `587` | Mail server port. |
| `SMOKEALARM_SMTP_TLS` | `starttls` | `starttls`, `tls` (encrypted from the start, usually port 465) or `none` (local testing only). With `starttls`, a server that does not offer it is an error; there is no silent downgrade. |
| `SMOKEALARM_SMTP_USERNAME` | – | Login for the mail server (optional). |
| `SMOKEALARM_SMTP_PASSWORD` | – | Password for the login. Required if a username is set. |
| `SMOKEALARM_SMTP_FROM` | – (required if mail is on) | Sender address. |
| `SMOKEALARM_SMTP_TO` | – (required if mail is on) | Comma-separated recipient addresses. |

For local runs, smokealarm also reads the standard `KUBECONFIG` variable (and uses the service account when it runs in a pod).

## The checks

Every check has a fixed purpose and severity. A **critical** problem needs action now, a **warning** deserves a look soon. The status page lists which checks are active and why one is skipped.

| Check | Severity | Reports |
|---|---|---|
| `pod-crashloop` | critical | A container is in `CrashLoopBackOff`. (The kubelet shows this state only after several crashes, so it can take a few minutes.) |
| `pod-image-pull` | critical | A container cannot pull its image (`ImagePullBackOff` / `ErrImagePull`). |
| `pod-oomkilled` | warning | A container was killed for exceeding its memory limit in the last 15 minutes. |
| `pod-restarts` | warning | More restarts than `SMOKEALARM_RESTART_THRESHOLD`, with the last restart within the past hour. |
| `pod-pending` | warning | A pod has been pending longer than `SMOKEALARM_POD_PENDING_AFTER`; the message says why (for example `Unschedulable`). |
| `workload-replicas` | warning / critical | A Deployment, StatefulSet or DaemonSet has fewer available replicas than desired (warning), or none at all (critical). |
| `node-not-ready` | critical | A node is `NotReady` or stopped reporting. |
| `node-pressure` | warning | A node reports `MemoryPressure`, `DiskPressure` or `PIDPressure`. |
| `job-failed` | warning | A Job failed. For CronJobs only the newest Job counts, so old failures do not alarm forever. |
| `cronjob-missed` | warning | A scheduled CronJob run did not happen (suspended CronJobs are ignored). |
| `pvc-pending` | warning | A claim is pending longer than `SMOKEALARM_PVC_PENDING_AFTER`. Claims of a `WaitForFirstConsumer` storage class are skipped, because they stay pending until a pod uses them. |
| `pvc-usage` | warning / critical | A volume is more than 85 % (warning) or 95 % (critical) full. Needs the `nodes/proxy` permission; see the note below. |
| `cert-not-ready` | warning | A cert-manager Certificate is not `Ready`. Only if cert-manager is installed. |
| `cert-expiry` | warning / critical | A certificate expires in less than 14 days (warning) or 3 days (critical), or has expired. Only if cert-manager is installed. |
| `argocd-health` | critical | An Argo CD Application is `Degraded` or `Missing`. Only if Argo CD is installed. |
| `argocd-sync` | warning | An Argo CD Application is `OutOfSync`. Only if Argo CD is installed. |

Notes:

- **Optional CRDs** (cert-manager, Argo CD) are detected once at startup, and only if an enabled check needs them. If a CRD is missing, or the service account may not list it, the matching checks are skipped and the status page says why. Install the CRD or grant the permission later and restart smokealarm. While a permission is missing, the open problems of those checks stay as they are, because they may still exist; if a CRD is not installed, they count as gone.
- **Switching a check off closes its problems.** If you disable a check (`SMOKEALARM_DISABLED_CHECKS`, or an `rbac.*` switch of the chart, which disables the checks that need the permission), its open problems count as gone: they are reported as *resolved* in the next mail, although the cause may still exist. A check that is skipped only for a moment (for example the kubelet does not answer) leaves its problems alone.
- **A problem that gets worse** (`pvc-usage`, `cert-expiry` and `workload-replicas` can go from warning to critical) stays the same problem: it is not reported as resolved, and you get a new mail saying it is critical now.
- **`pvc-usage` and `local-path`:** the kubelet reports the usage of the filesystem the volume lives on. For k3s `local-path` volumes that is the node's disk (the requested size is not enforced), so the check only alarms when the node disk is nearly full. It is most useful for volumes with their own filesystem (Longhorn, Ceph, block volumes).
- **If the volume stats cannot be read** (no permission, a node does not answer), `pvc-usage` is skipped for that run instead of guessing; its open problems stay as they are, and the status page shows the reason.

### Ignoring things

- Put the annotation `smokealarm.gagaschu.github.io/ignore: "true"` on an object, or on its namespace, to silence all findings for it. For pods it is enough to set it in the pod template of the Deployment (or StatefulSet, Job, ...).
- Or list namespaces in `SMOKEALARM_IGNORE_NAMESPACES`.
- Or switch a whole check off with `SMOKEALARM_DISABLED_CHECKS`.

## Security

- **Read access.** smokealarm only needs `get`, `list` and `watch`, on a short list of resources. The Helm chart creates a `ClusterRole` with exactly that (the one exception is `get` on `nodes/proxy`, see the next point). The optional permissions (`nodes/proxy` for `pvc-usage`, cert-manager Certificates, Argo CD Applications) each have a switch (`rbac.nodesProxy`, `rbac.certManager`, `rbac.argocd`); a switch that is off disables the checks that need it. smokealarm never reads Secrets. The ClusterRole is named `<namespace>-<release>`, so the chart can be installed in several namespaces.
- **`nodes/proxy` is more than read access.** `pvc-usage` reads the volume usage from the kubelets, which needs `get` on `nodes/proxy`. The Kubernetes documentation ([RBAC good practices](https://kubernetes.io/docs/concepts/security/rbac-good-practices/#access-to-proxy-subresource-of-nodes)) warns that this permission reaches the kubelet API, including container logs and command execution in pods, and bypasses audit logging and admission control; it says that `get` on `nodes/proxy` "is not a read-only permission". smokealarm only calls `stats/summary`, but anyone who steals its service account token gets the same reach. The rule is created only while `pvc-usage` is active. If you do not need the check, set `rbac.nodesProxy=false` or add `pvc-usage` to `disabledChecks`, and the permission is gone.
- **Locked-down container.** It runs as a non-root user, with a read-only root filesystem, all capabilities dropped, no privilege escalation and the default seccomp profile. The only writable place is the data volume.
- **Login.** One local admin; the password is stored as a bcrypt hash. Sessions live in the database (only a hash of the session id is stored), the cookie is `HttpOnly` and `SameSite=Lax`, and `Secure` unless you switch that off. Failed logins are rate-limited (5 per 15 minutes per client address; behind a proxy all clients share one limit).
- **Nothing sensitive in logs:** no passwords, hashes or session ids.

`/healthz` and `/readyz` need no login, so Kubernetes can probe them.

## Local development

You need Go (the version in [go.mod](go.mod)), `golangci-lint`, and for the image and chart tasks Docker and Helm.

```sh
make test        # go vet and all tests, including the chart tests (skipped without helm)
make test-race   # the same with the race detector (on Linux this needs a C compiler)
make lint        # golangci-lint
make build       # bin/smokealarm
make chart       # lint and render the Helm chart: the defaults and every file in deploy/helm/examples

export SMOKEALARM_ADMIN_PASSWORD_HASH="$(make -s hash-password)"
make run         # runs against the cluster from KUBECONFIG, UI on http://localhost:8080
```

`make run` uses `admin` as the user, readable log output and a cookie that works over plain HTTP; all of it can be overridden with the variables above. Use a throw-away cluster or namespace to try things: for example `kubectl create deployment brokendep --image=does-not-exist/nope` produces a critical finding after `SMOKEALARM_ALERT_AFTER`.

Run `make help` for all tasks. The code is written to be read by people who do not know Go; start with [ARCHITECTURE.md](ARCHITECTURE.md).

## Troubleshooting

- **The pod restarts about two minutes after it starts; the log says `informer caches did not sync within 2m0s: <resources>` and, before that, `forbidden`.** smokealarm cannot read the listed resources. The Helm chart grants them, so this happens with `rbac.create=false` and your own RBAC: compare it with `deploy/helm/smokealarm/templates/clusterrole.yaml`. Missing permissions for cert-manager or Argo CD do not cause this; the matching checks are skipped and the status page shows the reason.
- **Nothing is reported although something is broken.** A problem is reported only after `SMOKEALARM_ALERT_AFTER` (2 minutes). Look at the overview page and the log (`finding pending`, `finding firing`). Without `SMOKEALARM_SMTP_HOST` the notifications only go to the log.
- **I see a skipped check on the status page.** The reason is shown next to it: disabled by configuration, a CRD that is not installed, or missing permission.
- **Login works but I am logged out again at once.** The cookie is `Secure` but you use plain HTTP. Use HTTPS or set `SMOKEALARM_COOKIE_SECURE=false` for local use.
- **The history is gone after a restart.** The database was not on a persistent volume (`persistence.enabled=false`, or `SMOKEALARM_DB_PATH` outside the volume).

## License

Apache License 2.0, see [LICENSE](LICENSE).
