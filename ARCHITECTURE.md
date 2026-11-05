# Architecture

This page is for readers who know Kubernetes and operations, but not Go. It explains how smokealarm works, where things live, and how to add a new check. The code is written to be read; every package starts with a comment saying what it is for.

## The idea

Every 30 seconds smokealarm takes a **snapshot** of the cluster objects it keeps in memory, hands it to a list of **checks**, and gets back **findings** ("this pod is crash-looping"). It compares the findings with what it saw last time to find out what is *new* and what is *gone*, stores that in SQLite, and sends **one notification** for everything that changed to every configured target: mail, ntfy, Telegram, Slack, Teams or a webhook.

A check is a plain function: *snapshot in, findings out*. It never talks to the API and keeps no state. That is why checks are easy to read and easy to test with hand-made objects.

## Data flow

```
  Kubernetes API
       │  watch (kept up to date in memory by "informers")
       ▼
  internal/kube ──── snapshot ────►  internal/checks     (pure functions)
                                          │ findings
                                          ▼
                                    internal/engine  ◄──►  internal/store (SQLite)
                                          │ changes
                                          ▼
                                    internal/notify  ──►  mail, webhook, ntfy, Telegram,
                                                          Slack, Teams (or the log)

  Browser ──► internal/web ──► internal/auth (login, sessions)
                    └─ reads the current state from internal/store and internal/engine
```

`cmd/smokealarm/main.go` creates all the parts and connects them. There is no hidden wiring: what a part needs is passed to it as a field when `main` creates it.

## Packages

| Package | What it does |
|---|---|
| `cmd/smokealarm` | `main()`: loads the configuration, creates everything, starts the web server and the main loop. Also the `hash-password` subcommand. |
| `internal/config` | Reads and checks the `SMOKEALARM_*` environment variables. A wrong value stops the start with a clear message. |
| `internal/kube` | Talks to the Kubernetes API: clients, informers (a local, always-current copy of the objects we watch), detection of optional CRDs (installed, and may the service account read them?), kubelet volume stats, and the snapshot. |
| `internal/checks` | **The checks**, one file per area (`pods.go`, `workloads.go`, `nodes.go`, `jobs.go`, `storage.go`, `certs.go`, `argocd.go`), the finding and snapshot types, and the ignore rules. |
| `internal/engine` | The main loop. Runs the checks, decides what is new/firing/resolved, sends notifications, cleans up old data. |
| `internal/store` | SQLite: findings, downtimes and login sessions. The schema lives in numbered SQL files (`migrations/`) that are applied at startup. |
| `internal/notify` | Sends notifications. `Notifier` is an interface with one implementation per target, each in its own file: `mail.go`, `webhook.go`, `ntfy.go`, `telegram.go`, `slack.go`, `teams.go`, and `log.go` when nothing is configured. `message.go` has the texts (mail subject and body, the short chat text), `http.go` the shared HTTP request that keeps secrets out of errors. |
| `internal/auth` | Password check, sessions, the login rate limit. The middleware only asks "is there a valid session?", so another login method (OIDC) can be added without touching it. |
| `internal/web` | The pages (overview, history, downtimes, status, login) and the actions (acknowledge and remove that, downtimes), as HTML templates, one CSS file, small SVG icons and a favicon, all embedded in the binary. |
| `deploy/helm/smokealarm` | The Helm chart. |

## The life of a finding

A finding has a stable **key** (check + object + a fixed word), so smokealarm can recognize "the same problem" from one run to the next.

```
 first seen ──► pending ──(lasts ≥ ALERT_AFTER)──► firing ──(gone ≥ RESOLVE_AFTER)──► resolved
                  │                                  │                   │
              (gone: dropped,                  (mail: "new problem")  (mail: "resolved")
               nobody is told)
```

- A problem that appears and disappears within `ALERT_AFTER` is never reported.
- Reporting is remembered in the database *after* a successful send, so a failed mail is retried at the next run, even after a restart. This is remembered **per notification target** (table `deliveries`), so if one target is down, only that one is retried and the others get nothing twice. See [Where notifications go](#where-notifications-go).
- If a firing problem gets worse (warning → critical), it stays the same problem and is reported again as critical, once that has lasted `ALERT_AFTER` (`findings.escalating_since`). So a problem that flips between the two does not send a message per flip.
- A firing problem that is no longer found waits for `RESOLVE_AFTER` (`findings.gone_since`) before it is resolved, so a crash loop, which has no finding while the container runs, is one alarm and not a pair of messages per restart. If it comes back in that time, it just goes on.
- With `CRITICAL_REPEAT_INTERVAL` set, a critical problem that is still firing is reported again once its last report is that old. Warnings are not repeated.
- A firing critical problem can be acknowledged in the web UI. From then on only its end is reported: no reminders, no mail if it goes from warning back to critical, and a first mail that has not gone out yet is dropped. The mark belongs to this one alarm, so a problem that returns after "resolved" is new again. If the mark is removed, the problem behaves like any other, so an overdue reminder goes out in the next run.
- A downtime silences one key until a fixed time, whether the problem is there or not. While it runs, nothing is sent; a problem that goes away is marked as reported without a mail. At the start of each run the engine finishes the downtimes that have ended. If the problem is still firing and not acknowledged, its "reported" marker is cleared, so the normal path reports it as new. If a check's state is unknown, this waits until the check runs again.
- A node that is down holds back the problems it causes. After the checks and the annotations, `checks.ApplyNodeCauses` sets `CausedBy` on problems of pods on a down node and of Deployments, StatefulSets and DaemonSets whose missing replicas their pods on down nodes explain (by their selector), but only while `node-not-ready` runs. The engine keeps such a problem **pending** as long as it is a consequence (`findings.caused_by`), so all the usual rules for pending problems apply: nothing is sent, and if it goes away it is dropped. When it no longer is one, the `ALERT_AFTER` clock starts again (`findings.pending_since`; `first_seen` stays the real start). Notifications list a down node first and carry the problems it holds back as `Alert.Affected`, filtered with the same routing as everything else.
- If a check cannot get its data (for example the kubelet does not answer), it is skipped for that run and its open problems are left alone, because "no data" is not "all fine". They get no reminders either, until the check runs again.
- The informer caches stay "synced" after their first fill, so `internal/kube/apihealth.go` watches whether they are still updated: the watch error handler of every informer, and a request to `/version` in every run (client-go retries "connection refused" without calling the handler, and when the server answers again the watch errors of the outage are forgotten). After 3 minutes it sets `Snapshot.APIProblem`, and the check `api-connection` reports it. If nothing can be read, the engine treats all other checks as skipped.
- If a check is switched off (configuration, or an optional CRD or permission is missing at startup), its open problems count as gone and are reported as resolved after `RESOLVE_AFTER`. The README says so, because switching a check off can look like a recovery.

## Where notifications go

`main` creates one notifier per configured target (`cmd/smokealarm/notifiers.go`); Telegram gets one per chat. At the end of each run, the engine goes through them one by one and, for each, does what it did for mail alone before:

```
 for every target:
   what has this target not been told yet?   store.Unnotified(target)
   + which reminders are due for it?          store.CriticalDueForReminder(target, ...)
   minus problems in a downtime
   minus problems not meant for it            routedTo: MinSeverity, targets annotation
   ──► one combined notification ──► target.Send
   if it worked: remember it for this target  store.MarkFiringNotified / MarkResolvedNotified
```

- Each target has its own markers, so its own retries and its own reminder clock. One that fails does not stop the others.
- The columns `firing_notified_at` and `resolved_notified_at` in `findings` are the summary: "reported to at least one target". The web UI and the escalation rule use them. Clearing a marker (escalation, end of a downtime) clears it for every target the problem is meant for now, so they hear about it again. A target that heard the start but is no longer meant for it (the annotation moved, or it is critical-only and the problem is a warning now) keeps its marker, so it still hears the end.
- Routing: after the checks, `checks.ApplyAnnotations` sets the severity and the targets of each finding from the `smokealarm.gagaschu.github.io/severity` and `/targets` annotations of its object or namespace, so the checks themselves know nothing about it. The targets are stored with the problem (`findings.targets`). `routedTo` in the engine filters new problems and reminders per target; a problem not meant for a target gets no marker there, so the target hears it as new once that changes. The end goes to every target that heard the start, by its marker. A targets annotation that names no configured target counts as none, so a typo cannot silence an alarm. `Engine.Audience` applies the same rule for the web UI, which says "Notifies only: …" or "Notifies nobody" when not every target hears about a problem.
- At startup, `store.SyncTargets` registers the configured targets. A new one starts from the current state (it copies the summary markers, so it gets no history), and one that is gone is forgotten.
- Secrets (tokens, webhook URLs, the ntfy topic) stay inside the notifier. `Name()` is a stable id for the database (`slack`, `telegram:-100123`), and `Description()` is the safe label for the log and the status page. Errors from `net/http` contain the request URL, so `http.go` takes it out before wrapping.

## How do I add a new notification target?

Example: a target `gotify` that posts to a Gotify server.

1. **Write `internal/notify/gotify.go`** with a struct holding its settings and the shared `*http.Client`, and three methods:
   - `Name()`: a fixed id, `"gotify"`. It is stored in the database, so never change it later.
   - `Description()`: what the status page shows, without secrets, e.g. `"gotify (" + hostOf(g.Server) + ")"`.
   - `Send(ctx, n)`: build the text with `Subject(n)` and `ChatBody(n, limit)` (limit = what the service accepts), then call `post(ctx, g.Client, request{...})`. Put every secret into `request.secrets`, so it is taken out of error messages; the URL is added automatically.
2. **Test it** in `internal/notify/targets_test.go`: an `httptest` server checks method, path, headers and body (a golden file in `testdata/`), and `newEcho` plus `wantNoSecret` prove that the token never shows up in an error.
3. **Configuration** in `internal/config/notify.go`: read the variables (`SMOKEALARM_GOTIFY_URL`, `SMOKEALARM_GOTIFY_TOKEN`), validate them in `validateNotify` (never print a secret value), and add table cases to `notify_test.go`.
4. **Wire it** in `newNotifiers` in `cmd/smokealarm/notifiers.go`. The engine needs no change. For routing, add a `SMOKEALARM_GOTIFY_MIN_SEVERITY` line to `minSeverityVariables` in `internal/config/routing.go`; the name `gotify` then also works in the targets annotation.
5. **Helm chart**: plain settings in `values.yaml` and `deployment.yaml`, secrets in `smokealarm.notificationSecrets` in `_helpers.tpl` (they go into the Secret), and the new variables in `TestEverySettingBecomesAnEnvironmentVariable` and `TestNotificationSecretsAreOnlyInTheSecret` in `deploy/helm/chart_test.go`.
6. **Document it**: a section and the variables in `README.md` (a test fails if a variable is missing), and a line in `CHANGELOG.md`.

## Go for ops people

You need very little Go to read this code:

- A **struct** is a record with named fields, like a row. A **function value** (`Run func(...)`) is a function stored in a field.
- **`err != nil`**: Go has no exceptions. Functions return an error as a second value and the caller checks it. Here errors are always wrapped with context (`fmt.Errorf("load config: %w", err)`) and never swallowed.
- **`ctx`** (`context.Context`) carries a cancellation signal, used to stop work on shutdown.
- **`:=`** declares and assigns a variable. **`[]Finding`** is a list of findings; **`map[string]string`** is a dictionary.
- **Tests** live next to the code in `*_test.go` files. They are *table-driven*: a list of cases (input, expected result) and one loop. `go test ./...` runs them all.
- There are no generics, no reflection and no global variables, on purpose.

## How do I add a new check?

Example: report pods that were **evicted** by the kubelet (status reason `Evicted`). The data we need (pods) is already in the snapshot, so only steps 1 to 4 apply.

**1. Write the check** in `internal/checks/pods.go` (or a new file for a new area). Give it a doc comment that says *what* is checked, *why* it is a problem and *which severity* it has:

```go
const checkPodEvicted = "pod-evicted"

// PodEvicted reports pods that the kubelet evicted, usually because the node
// ran out of memory or disk. The workload lost a replica, so this is a warning.
func PodEvicted(snap *Snapshot, opts Options) []Finding {
	var findings []Finding
	for _, pod := range snap.Pods {
		if pod.Status.Reason != "Evicted" || isIgnored(snap, opts, pod.ObjectMeta) {
			continue
		}
		msg := pod.Status.Message
		findings = append(findings, newFinding(checkPodEvicted, Warning, podRef(pod), "Evicted", msg))
	}
	return findings
}
```

Two rules matter: skip ignored objects with `isIgnored` (this is what makes the ignore annotation work for your check), and keep the *reason* (`"Evicted"`) fixed, because it is part of the finding key. Put changing details like counters into the *message*. If the problem can get worse over time (like a filling volume), use `newEscalatingFinding` with a fixed key part instead, so a worse state is the same problem.

**2. Register it** in `All()` in `internal/checks/types.go`:

```go
{Name: checkPodEvicted, Run: PodEvicted},
```

That is all the engine needs. The check now runs, can be disabled with `SMOKEALARM_DISABLED_CHECKS=pod-evicted`, and shows up on the status page.

**3. Test it** in `internal/checks/pods_test.go`: a table with cases for "evicted", "not evicted", and "ignored by annotation", built from hand-made `corev1.Pod` objects. Look at the existing tests in that file and copy the pattern.

**4. Document it**: add a line to the checks table in `README.md` and to `CHANGELOG.md`. A test fails if a check is missing from the README, so this cannot be forgotten.

If your check needs **data that is not in the snapshot yet** (a new kind of object), there are a few more steps:

- add a field to `Snapshot` in `internal/checks/types.go`, and the kind as a `Kind…` constant there,
- add the objects to `objectAnnotations` in `internal/checks/annotations.go` and to `TestApplyAnnotationsCoversEveryKind`, so the routing annotations on the object work (otherwise only those on its namespace would count),
- add an informer for it in `internal/kube/cluster.go` (with its name in `allInformers`, so a cache that never syncs is named in the error) and copy its objects in `internal/kube/snapshot.go`,
- add the `get`, `list`, `watch` permission in `deploy/helm/smokealarm/templates/clusterrole.yaml` and in the expected rules of `deploy/helm/chart_test.go`,
- if the object is a CRD that may not exist, set `Requires` on the check and add the CRD to `optionalResourceFor` in `internal/kube/features.go`. `main` then asks for exactly the CRDs that enabled checks need, skips the check with a reason if the CRD is missing or may not be read, and starts an informer only for CRDs that are in use. `TestEveryCheckRequirementIsKnown` fails if the CRD is not known to `features.go`.

If the permission should be **optional in the Helm chart** (like `nodes/proxy`, cert-manager or Argo CD), the chart has to switch the check off together with the permission, or the check would run without it:

- add an `rbac.<name>` switch to `values.yaml` and the rule behind `{{ if .Values.rbac.<name> }}` in `clusterrole.yaml`,
- add the check names to `smokealarm.disabledChecks` in `deploy/helm/smokealarm/templates/_helpers.tpl`,
- add the switch to `rbacSwitches` in `deploy/helm/chart_test.go`. `TestRBACSwitchesCoverEveryCheckThatNeedsThem` fails for a check with `Requires` (or `pvc-usage`) that no switch covers, and for a switch that disables other checks than it should.

Finish with `make test lint`.
