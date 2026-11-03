# smokealarm

> A smoke alarm for your Kubernetes cluster – silent until something's on fire.

smokealarm runs inside your cluster and tells you what is broken right now.

It runs a fixed set of checks that need no configuration, and shows current problems and their history on a web page that also works on a phone. When a problem starts or ends, it sends you a mail.

## Scope

The list of checks is short on purpose, so that each one can be reliable. smokealarm has no metrics database, no graphs and no query language, so it won't replace Prometheus. It doesn't collect logs or traces. It also doesn't do root-cause analysis: it can tell you that a pod is crash-looping, but finding out why is up to you.

It was developed and tested with k3s on a Raspberry Pi (arm64). Nothing in it depends on k3s, so it runs on any standard Kubernetes.

## How it works

Every 30 seconds smokealarm runs all checks against its in-memory copy of the cluster state and compares the result with the previous run. A new problem starts as **pending**. If it is still there after `SMOKEALARM_ALERT_AFTER` (2 minutes by default), it becomes **firing** and is reported. When it goes away, it is **resolved** and reported again. A problem that disappears before the wait is over is never reported. All changes from one run go into a single mail.

If you already know about a critical problem, press **Acknowledge** on the overview. It stays on the page with a mark and sends no more reminders. You still get a mail when it is resolved. See [Acknowledging a problem](#acknowledging-a-problem) for the details.

To silence one problem until a fixed time, for example during planned work or while it flaps, set a [downtime](#downtimes).

[ARCHITECTURE.md](ARCHITECTURE.md) has the details.

## Installation

You need a Kubernetes cluster, `kubectl` and [Helm](https://helm.sh/) 3 or 4.

### 1. The image

The image contains one static binary and is built for `linux/amd64` and `linux/arm64`. Nothing is published yet, so build it yourself and push it to a registry your cluster can pull from:

```sh
make image IMAGE=ghcr.io/<you>/smokealarm TAG=0.1.0 PUSH=1
```

The multi-arch build needs a buildx builder that supports several platforms; the comment in the [Makefile](Makefile) explains this. To build for your own machine only, use `make image-local`.

### 2. The admin password

There is one local admin user. smokealarm takes the bcrypt hash of the password, not the password itself, so create the hash first:

```sh
read -s PW; printf '%s' "$PW" | docker run -i --rm ghcr.io/<you>/smokealarm:0.1.0 hash-password
```

Without Docker, run `make hash-password` in a checkout. The hash contains `$` characters, so keep it in single quotes or a variable and pass it with `--set-string`, as below.

### 3. Install the chart

```sh
HASH='<the hash from step 2>'
helm install smokealarm ./deploy/helm/smokealarm \
  --namespace smokealarm --create-namespace \
  --set image.repository=ghcr.io/<you>/smokealarm \
  --set-string auth.passwordHash="$HASH"
```

The chart stores the hash in a Kubernetes Secret. To manage the Secret yourself instead:

```sh
kubectl -n smokealarm create secret generic smokealarm-auth \
  --from-literal=SMOKEALARM_ADMIN_PASSWORD_HASH="$HASH"
helm install smokealarm ./deploy/helm/smokealarm -n smokealarm \
  --set image.repository=ghcr.io/<you>/smokealarm --set existingSecret=smokealarm-auth
```

The same Secret can also hold `SMOKEALARM_SMTP_USERNAME` and `SMOKEALARM_SMTP_PASSWORD`.

[`deploy/helm/examples/`](deploy/helm/examples) has values files for common setups: ingress with TLS, mail, minimal permissions, and your own Secret. Pass one with `-f deploy/helm/examples/<file>`. The comment at the top of each file shows the full command.

### 4. Open it

```sh
kubectl -n smokealarm port-forward svc/smokealarm 8080:80
```

Open <http://localhost:8080> and log in as `admin`. By default the login cookie is only sent over HTTPS, so if the login doesn't stick over the port-forward, install with `--set config.cookieSecure=false` for local use. For permanent access, use the ingress:

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

Set an SMTP host to turn mail on. Without one, state changes only go to the log.

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

If one run finds several changes, the subject is a count (`[smokealarm] 3 new problems, 1 resolved`) and the body lists all of them, critical problems first. A problem you were already told about triggers a new mail if it becomes critical. Set `SMOKEALARM_CRITICAL_REPEAT_INTERVAL` (chart: `config.criticalRepeatInterval`) to be reminded of critical problems that are still open: each one is reported again as `[smokealarm] STILL CRITICAL: ...` once its last report is that old. Warnings are not repeated. If the mail server is down, smokealarm tries again on the next run, even after a restart.

## Acknowledging a problem

Only firing critical problems can be acknowledged. Warnings are not repeated anyway.

- An acknowledged problem sends no reminders, and the mark lasts until the problem is resolved or until you remove it.
- "Resolved" is always reported, even if the first mail never went out. A first mail that is still waiting, for example during a mail server outage, is dropped once the problem is acknowledged. If you have several recipients, the others then only hear about it from the "resolved" mail.
- If an acknowledged problem becomes a warning and then critical again, it stays acknowledged and no mail is sent.
- If the problem returns after "resolved", it is a new alarm with a new mail.
- **Remove acknowledgement** takes it back, and the problem behaves like any other again. A reminder that is overdue, or a first mail that was held back, goes out in the next run, unless a downtime is running. If the problem is only a warning at that moment, nothing is sent until it is critical again, and only then can you acknowledge it again.

An acknowledgement covers one problem, identified by its key: check, object and reason. Only `pvc-usage`, `cert-expiry` and `workload-replicas` keep their key when the reason changes. For the other checks a new reason counts as a new problem, and so does a new object name, such as a pod that was recreated by its Deployment. The new problem gets its own mail and is not acknowledged.

## Downtimes

A downtime silences one problem, warning or critical, until a fixed date and time. Set it from the overview or, if the problem is resolved right now, from the history. The **Downtimes** page lists the running ones, and you can end them early there. Times are entered and shown in UTC, like in the mails, and the end must be in the future. A new downtime for the same problem replaces the old one.

- While it runs, the problem sends no mail: not when it is new, no reminder and no "resolved". It stays visible in the web UI, with a mark.
- If the problem is still active when the downtime ends, you get a mail in the next run, and the reminder interval runs from that mail. Ending a downtime early works the same way.
- If the problem went away during the downtime, nothing is sent, not even after it ends. If it returns after the end, it is a new alarm with a new mail.
- A downtime wins over an acknowledgement while it runs: an acknowledged problem that goes away during a downtime sends no "resolved". An acknowledgement outlasts a downtime: if the problem is still active when the downtime ends, no mail is sent, and "resolved" is reported later.
- If a check cannot run when the downtime ends, for example because the kubelet does not answer, the mail waits until the check runs again.

A downtime covers exactly one problem, like an acknowledgement: the same check, object and reason. A new reason is a new problem, except for `pvc-usage`, `cert-expiry` and `workload-replicas`. A new object name is a new problem too. **A pod recreated by its Deployment gets a new name, so a downtime on its `pod-crashloop` problem no longer covers it, and it sends mail as usual.** A downtime on the Deployment's `workload-replicas` problem keeps working, but it does not silence the problems of the pods themselves.

## Configuration

All configuration is done through environment variables starting with `SMOKEALARM_`. The Helm chart sets them from its [values.yaml](deploy/helm/smokealarm/values.yaml). Invalid values stop the program at startup with an error message. Durations use Go syntax (`30s`, `5m`, `2h`) plus `d` for days (`30d`).

| Variable | Default | Description |
|---|---|---|
| `SMOKEALARM_ADMIN_USER` | – (required) | Name of the admin user. |
| `SMOKEALARM_ADMIN_PASSWORD_HASH` | – (required) | bcrypt hash of the admin password. Create it with `smokealarm hash-password`. |
| `SMOKEALARM_CHECK_INTERVAL` | `30s` | How often the checks run. |
| `SMOKEALARM_ALERT_AFTER` | `2m` | How long a problem must last before it is reported. `0s` reports at once. |
| `SMOKEALARM_CRITICAL_REPEAT_INTERVAL` | `0s` | How often a critical problem that is still open is reported again, for example `4h`. `0s` means never. Otherwise it must be at least `SMOKEALARM_CHECK_INTERVAL`. Critical problems that have been open for longer are reported on the next run after you turn this on. |
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

For local runs, smokealarm also reads the standard `KUBECONFIG` variable. In a pod it uses the service account.

## The checks

Severity is either critical (act now) or warning (have a look soon). The status page shows which checks are active and why any are skipped.

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

- The cert-manager and Argo CD CRDs are detected once at startup, and only if an enabled check needs them. If a CRD is missing or the service account may not list it, the matching checks are skipped and the status page says why. After installing the CRD or granting the permission, restart smokealarm. While a permission is missing, open problems from those checks stay open, since they may still exist. If the CRD is not installed, they count as gone.
- Disabling a check closes its problems. This applies to `SMOKEALARM_DISABLED_CHECKS` and to the chart's `rbac.*` switches, which disable the checks that need the permission. The open problems are reported as resolved in the next mail, even if the cause is still there. A check that is only skipped for a moment, for example because the kubelet doesn't answer, leaves its problems alone.
- `pvc-usage`, `cert-expiry` and `workload-replicas` can go from warning to critical. It stays the same problem: it isn't reported as resolved, and you get a new mail saying it is now critical.
- `pvc-usage` looks at the filesystem a volume lives on, because that is what the kubelet reports. With k3s `local-path` that is the node's disk, and the requested size is not enforced, so the check only fires when the node disk is nearly full. It is more useful for volumes with their own filesystem (Longhorn, Ceph, block volumes).
- If `pvc-usage` can't read the volume stats (no permission, a node doesn't answer), it is skipped for that run, open problems stay open, and the status page shows the reason.

### Ignoring things

- Put the annotation `smokealarm.gagaschu.github.io/ignore: "true"` on an object or its namespace to silence all its findings. For pods, setting it in the pod template of the Deployment, StatefulSet, Job and so on is enough.
- List namespaces in `SMOKEALARM_IGNORE_NAMESPACES`.
- Switch off a whole check with `SMOKEALARM_DISABLED_CHECKS`.

## Security

smokealarm only needs `get`, `list` and `watch` on a short list of resources, and the `ClusterRole` from the Helm chart grants just that, apart from `get` on `nodes/proxy` (see below). It never reads Secrets. The optional permissions each have a switch: `rbac.nodesProxy` for `pvc-usage`, `rbac.certManager` for cert-manager Certificates and `rbac.argocd` for Argo CD Applications. Turning a switch off disables the checks that need it. The ClusterRole is named `<namespace>-<release>`, so the chart can be installed in several namespaces.

`nodes/proxy` is more than read access. `pvc-usage` gets volume usage from the kubelets, which requires `get` on `nodes/proxy`. The Kubernetes documentation ([RBAC good practices](https://kubernetes.io/docs/concepts/security/rbac-good-practices/#access-to-proxy-subresource-of-nodes)) warns that this reaches the kubelet API, including container logs and command execution in pods, and bypasses audit logging and admission control. In its words, `get` on `nodes/proxy` "is not a read-only permission". smokealarm only calls `stats/summary`, but a stolen service account token would have the same reach. The rule only exists while `pvc-usage` is active. If you don't need the check, set `rbac.nodesProxy=false` or add `pvc-usage` to `disabledChecks`, and the permission goes away.

The container runs as non-root with a read-only root filesystem, all capabilities dropped, no privilege escalation and the default seccomp profile. It can only write to the data volume.

There is one local admin, with the password stored as a bcrypt hash. Sessions are kept in the database, which stores only a hash of the session id. The cookie is `HttpOnly` and `SameSite=Lax`, and `Secure` unless you turn that off. Failed logins are limited to 5 per 15 minutes per client address; behind a proxy, all clients share one limit.

Actions that change state, such as acknowledging a problem or setting a downtime, need a valid session and refuse requests that come from another site or another host of the same domain (Go's `http.CrossOriginProtection`). Your proxy has to pass on the original `Host` header if you use plain HTTP, otherwise these actions fail with 403. Over HTTPS, browsers send a `Sec-Fetch-Site` header and the `Host` does not matter.

Passwords, hashes and session ids are never logged.

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

`make run` sets the user to `admin` and uses plain-text logs and a cookie that works over HTTP. The variables above override all of this. To try it out, use a throw-away cluster or namespace. For example, `kubectl create deployment brokendep --image=does-not-exist/nope` produces a critical finding after `SMOKEALARM_ALERT_AFTER`.

`make help` lists all tasks. The code is meant to be readable without knowing Go; [ARCHITECTURE.md](ARCHITECTURE.md) is the place to start.

## Troubleshooting

- **The pod restarts about two minutes after it starts; the log says `informer caches did not sync within 2m0s: <resources>` and, before that, `forbidden`.** smokealarm can't read the listed resources. The Helm chart grants them, so this happens with `rbac.create=false` and your own RBAC. Compare yours with `deploy/helm/smokealarm/templates/clusterrole.yaml`. Missing cert-manager or Argo CD permissions don't cause this; those checks are skipped, and the status page shows why.
- **Something is broken but nothing is reported.** Problems are only reported after `SMOKEALARM_ALERT_AFTER` (2 minutes by default). Check the overview page and the log for `finding pending` and `finding firing`. Without `SMOKEALARM_SMTP_HOST`, notifications only go to the log.
- **A check shows as skipped on the status page.** The reason is next to it: disabled in the configuration, CRD not installed, or a missing permission.
- **Login works, but I'm logged out again right away.** The cookie is `Secure` and you're using plain HTTP. Use HTTPS, or set `SMOKEALARM_COOKIE_SECURE=false` for local use.
- **The history is gone after a restart.** The database wasn't on a persistent volume (`persistence.enabled=false`, or `SMOKEALARM_DB_PATH` outside the volume).

## License

Apache License 2.0, see [LICENSE](LICENSE).
