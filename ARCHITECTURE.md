# Architecture

This page is for readers who know Kubernetes and operations, but not Go. It explains how smokealarm works, where things live, and how to add a new check. The code is written to be read; every package starts with a comment saying what it is for.

## The idea

Every 30 seconds smokealarm takes a **snapshot** of the cluster objects it keeps in memory, hands it to a list of **checks**, and gets back **findings** ("this pod is crash-looping"). It compares the findings with what it saw last time to find out what is *new* and what is *gone*, stores that in SQLite, and sends **one mail** for everything that changed.

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
                                    internal/notify  ──►  mail (or the log)

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
| `internal/store` | SQLite: findings and login sessions. The schema lives in numbered SQL files (`migrations/`) that are applied at startup. |
| `internal/notify` | Sends notifications. `Notifier` is an interface with two implementations: mail and log. A Telegram or webhook notifier would be one more file. |
| `internal/auth` | Password check, sessions, the login rate limit. The middleware only asks "is there a valid session?", so another login method (OIDC) can be added without touching it. |
| `internal/web` | The pages (overview, history, status, login), as HTML templates and one CSS file, all embedded in the binary. |
| `deploy/helm/smokealarm` | The Helm chart. |

## The life of a finding

A finding has a stable **key** (check + object + a fixed word), so smokealarm can recognize "the same problem" from one run to the next.

```
 first seen ──► pending ──(lasts ≥ ALERT_AFTER)──► firing ──(gone)──► resolved
                  │                                  │                   │
              (gone: dropped,                  (mail: "new problem")  (mail: "resolved")
               nobody is told)
```

- A problem that appears and disappears within `ALERT_AFTER` is never reported.
- Reporting is remembered in the database *after* a successful send, so a failed mail is retried at the next run, even after a restart.
- If a firing problem gets worse (warning → critical), it stays the same problem and is reported again as critical.
- With `CRITICAL_REPEAT_INTERVAL` set, a critical problem that is still firing is reported again once its last report is that old. Warnings are not repeated.
- If a check cannot get its data (for example the kubelet does not answer), it is skipped for that run and its open problems are left alone, because "no data" is not "all fine". They get no reminders either, until the check runs again.
- If a check is switched off (configuration, or an optional CRD or permission is missing at startup), its open problems count as gone and are reported as resolved in the next run. The README says so, because switching a check off can look like a recovery.

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

- add a field to `Snapshot` in `internal/checks/types.go`,
- add an informer for it in `internal/kube/cluster.go` (with its name in `allInformers`, so a cache that never syncs is named in the error) and copy its objects in `internal/kube/snapshot.go`,
- add the `get`, `list`, `watch` permission in `deploy/helm/smokealarm/templates/clusterrole.yaml` and in the expected rules of `deploy/helm/chart_test.go`,
- if the object is a CRD that may not exist, set `Requires` on the check and add the CRD to `optionalResourceFor` in `internal/kube/features.go`. `main` then asks for exactly the CRDs that enabled checks need, skips the check with a reason if the CRD is missing or may not be read, and starts an informer only for CRDs that are in use. `TestEveryCheckRequirementIsKnown` fails if the CRD is not known to `features.go`.

If the permission should be **optional in the Helm chart** (like `nodes/proxy`, cert-manager or Argo CD), the chart has to switch the check off together with the permission, or the check would run without it:

- add an `rbac.<name>` switch to `values.yaml` and the rule behind `{{ if .Values.rbac.<name> }}` in `clusterrole.yaml`,
- add the check names to `smokealarm.disabledChecks` in `deploy/helm/smokealarm/templates/_helpers.tpl`,
- add the switch to `rbacSwitches` in `deploy/helm/chart_test.go`. `TestRBACSwitchesCoverEveryCheckThatNeedsThem` fails for a check with `Requires` (or `pvc-usage`) that no switch covers, and for a switch that disables other checks than it should.

Finish with `make test lint`.
