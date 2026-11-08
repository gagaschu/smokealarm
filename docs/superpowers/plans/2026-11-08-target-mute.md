# Switching notification targets off and on – Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A notification target can be switched off and on in the web UI; while it is off it sends nothing, switching it on works like the end of a downtime for that target, and the overview names every target that is off.

**Architecture:** A new table `target_mutes` holds one row per target that is off. The web handlers only write that table (switch off, ask to switch on). The engine skips muted targets in `notify` and, in a new step `finishMutes` between `saveState` and `notify`, finishes a requested switch-on in one store transaction: it clears the firing markers of this target for the firing problems meant for it, and marks its unreported resolved problems as reported without a message. The pages read `target_mutes` directly, so a click shows at once.

**Tech Stack:** Go, `database/sql` with `modernc.org/sqlite`, `net/http`, `html/template`. No new dependency.

**Spec:** `SPEC.md` (Notifications: "Since phase 31, a notification target can be switched off…"; Web UI: Overview and Status), `PLAN.md` (phase 31, decision "Muting a target").

## Global Constraints

- Readable, plain Go: no generics, no reflection, no `init()`, no globals except constants; functions < 50 lines; every error wrapped with context (`fmt.Errorf("…: %w", err)`); comments explain the why.
- Time from the injectable `now`; stored times are Unix seconds.
- Code, comments, logs, UI and docs in English. Times on pages in `SMOKEALARM_TIME_ZONE` (`s.zone()`, format `timeFormat`).
- Secrets never in logs, errors or pages: name a target by `Notifier.Description()` in logs and pages, by `Notifier.Name()` only in the database and the URL.
- State-changing routes go through `action(...)` (session + `http.CrossOriginProtection`).
- No change to message formats, webhook JSON, chart values or environment variables.
- Branch `claude/pensive-shannon-7tk0na`; commit at every task; `go build ./...`, `go vet ./...`, `go test ./...`, `golangci-lint run` green at the end.
- CHANGELOG: one line under `## [Unreleased]` → `### Added`.

## Review Focus

1. Clicking "Switch off" again while the target is "being switched on" → it stays off and the request is cancelled (store test in Task 1, web test in Task 3).
2. A Telegram target name with a colon and a minus (`telegram:-100123`) in the URL → the routes find it (web test in Task 3).
3. A problem that goes away in the very run that finishes the switch-on → it ended while the target was off, so no "resolved" (engine test in Task 2).
4. A muted target removed from the config and added again later → it starts switched on (store test in Task 1).
5. Every target off and nothing broken → "All good" is still shown, and the note names every target (web test in Task 3).

---

### Task 1: Store – table and queries

**Files:**
- Create: `internal/store/migrations/012_target_mutes.sql`
- Create: `internal/store/mutes.go`
- Create: `internal/store/mutes_test.go`
- Modify: `internal/store/deliveries.go` (`forgetTarget`: also delete the row in `target_mutes`)

**Interfaces:**
- Produces:
  ```go
  // TargetMute is a notification target that is switched off.
  type TargetMute struct {
      Target            string     // Notifier.Name
      MutedAt           time.Time
      UnmuteRequestedAt *time.Time // nil while it stays off; set when the web UI asked to switch it on
  }
  func (s *Store) MuteTarget(ctx context.Context, target string, at time.Time) (changed bool, err error)
  func (s *Store) RequestUnmute(ctx context.Context, target string, at time.Time) (changed bool, err error)
  func (s *Store) TargetMutes(ctx context.Context) (map[string]TargetMute, error) // keyed by Target
  func (s *Store) FinishUnmute(ctx context.Context, target string, firingIDs []int64, at time.Time) (finished bool, err error)
  ```

Semantics:
- `MuteTarget`: inserts `(target, at, NULL)`; if the row exists with a pending request, clears `unmute_requested_at` and keeps `muted_at` (changed = true); if it is already off without a request, nothing changes (changed = false).
- `RequestUnmute`: sets `unmute_requested_at = at` where the row exists and the column is NULL; changed = a row was updated.
- `FinishUnmute`, one transaction: `DELETE FROM target_mutes WHERE target = ? AND unmute_requested_at IS NOT NULL`; 0 rows → return `false, nil` (switched off again meanwhile, nothing else changes). Otherwise `UPDATE deliveries SET firing_notified_at = NULL WHERE target = ? AND finding_id = ?` for each id in `firingIDs` (`start_heard_at` stays), then mark every `resolved` finding whose delivery row for this target is missing or has `resolved_notified_at IS NULL`: upsert `deliveries.resolved_notified_at = at` and `findings.resolved_notified_at = COALESCE(resolved_notified_at, at)`. Return `true, nil`.

- [ ] **Step 1: Write the migration** `012_target_mutes.sql` with a comment why the table exists:
  ```sql
  CREATE TABLE target_mutes (
      target              TEXT PRIMARY KEY,
      muted_at            INTEGER NOT NULL,
      unmute_requested_at INTEGER
  );
  ```
- [ ] **Step 2: Write the failing tests** in `mutes_test.go`:
  - `TestMuteTargetAndRequestUnmute`: `MuteTarget("mail", t0)` → changed; again → not changed; `TargetMutes()["mail"].MutedAt == t0`, `UnmuteRequestedAt == nil`; `RequestUnmute("mail", t1)` → changed, `*UnmuteRequestedAt == t1`; `RequestUnmute("slack", t1)` → not changed.
  - `TestMuteAgainCancelsTheUnmuteRequest` (Review Focus 1): mute t0, request t1, mute t2 → changed; `MutedAt == t0`, `UnmuteRequestedAt == nil`.
  - `TestFinishUnmuteWithoutRequestDoesNothing`: muted, no request → `finished == false`, row still there, markers unchanged.
  - `TestFinishUnmuteClearsFiringAndSilencesResolved`: one firing finding marked for "mail" and "slack" (use `firingRecord`, `mustMarkFiring`), one resolved finding marked firing for "mail" without resolved marker; request + `FinishUnmute("mail", []int64{firingID}, t2)` → `finished`; `unnotifiedKeys(t, s, "mail")` lists the firing key only; `unnotifiedKeys(t, s, "slack")` still lists the resolved key; the row is gone.
  - `TestRemovedTargetForgetsItsMute` (Review Focus 4): `SyncTargets([mail, slack])`, mute slack, `SyncTargets([mail])`, `SyncTargets([mail, slack])` → `TargetMutes()` is empty.
- [ ] **Step 3: Run** `go test ./internal/store/ -run 'Mute|Unmute'` → FAIL (undefined).
- [ ] **Step 4: Implement** `mutes.go` (package doc not needed, file comment on the why) and the extra `DELETE` in `forgetTarget`.
- [ ] **Step 5: Run** `go test ./internal/store/` → PASS.
- [ ] **Step 6: Commit** "Store: target_mutes, switch notification targets off and on".

### Task 2: Engine – skip muted targets, finish switching on

**Files:**
- Create: `internal/engine/mutes.go`
- Create: `internal/engine/mutes_test.go`
- Modify: `internal/engine/notifications.go` (`notify`)
- Modify: `internal/engine/engine.go` (`RunOnce`)

**Interfaces:**
- Consumes: Task 1 (`TargetMutes`, `FinishUnmute`); existing `routedTo`, `failedIDs`, `e.targets`, `e.mu`.
- Produces: `func (e *Engine) finishMutes(ctx context.Context, unknown []string, now time.Time) error` (unexported).

Behavior:
- `notify` loads `e.Store.TargetMutes(ctx)` once (error → `fmt.Errorf("load muted targets: %w", err)`) and `continue`s for every target with a row, also while a switch-on is requested.
- `finishMutes` runs in `RunOnce` after `saveState` and before `notify` (after `saveState`, so a problem that ended in this run is already resolved and stays silent). It returns nil at once if `len(unknown) > 0`. For each row with `UnmuteRequestedAt != nil` whose target is in `e.Notifiers`: ids = open records with `State == StateFiring`, `AcknowledgedAt == nil` and `e.routedTo(target, rec)`; call `FinishUnmute`; if finished: `delete(e.failedIDs, name)`, clear `LastError`/`LastErrorAt` of `e.targets[name]` under `e.mu`, log Info "notification target switched on" with `"target", n.Description()`. One failing target does not stop the others (`errors.Join`).

- [ ] **Step 1: Write the failing tests** in `mutes_test.go` (use `twoTargets`, `finding`, `wantNotifications`; a helper `h.mute(name)` / `h.requestUnmute(name)` calling the store with `h.now`):
  - `TestMutedTargetSendsNothingWhileOthersDo`: mute "second"; a fires, reminder interval 1h, run 2m, run 1h, a gone, run 5m (RESOLVE_AFTER default of the harness) → first target `new:a`, `reminders:a`, `resolved:a`; second has no notifications.
  - `TestSwitchingOnReportsFiringProblemsAsNew`: a fires and is heard by both; mute second; run 1h; request; run 0 → second gets `new:a/reminders:/resolved:`; next reminder to second only after `CriticalRepeatInterval` from that message.
  - `TestProblemThatEndedWhileMutedStaysSilent`: a heard by both; mute second; a gone and resolved; request; run → second gets nothing.
  - `TestProblemEndingInTheRunOfSwitchingOnStaysSilent` (Review Focus 3): a heard by both; mute; request; set `h.findings = nil` with `RESOLVE_AFTER` 0 so a resolves in this run; run → second nothing, first `resolved:a`.
  - `TestAcknowledgedProblemStaysSilentAfterSwitchingOn`: a heard, acknowledged, mute, request, run → nothing; a gone → second `resolved:a`.
  - `TestDowntimeWinsOverSwitchingOn`: a in a downtime, mute, request, run → nothing; downtime ends → second `new:a`.
  - `TestSwitchingOnWaitsForUnknownChecks`: mute, request, `h.skipped` makes the check state unknown, run → row still there (`TargetMutes` has it with a request), nothing sent; clear `h.skipped`, run → finished.
  - `TestSwitchingOnForgetsTheLastError`: second fails once (`fail = true`), then mute, request, run → `Status().Targets[1].LastError == ""`.
  - `TestMuteSurvivesARestart`: mute, `h.restart()`, a fires → second nothing.
- [ ] **Step 2: Run** `go test ./internal/engine/ -run 'Mute|SwitchingOn'` → FAIL.
- [ ] **Step 3: Implement** `mutes.go`, the skip in `notify`, the call in `RunOnce` with a comment why it sits after `saveState`.
- [ ] **Step 4: Run** `go test ./internal/engine/` → PASS (all old tests too).
- [ ] **Step 5: Commit** "Engine: muted targets send nothing; switching on works like the end of a downtime".

### Task 3: Web – buttons on the status page, note on the overview

**Files:**
- Create: `internal/web/mutes.go` (handlers and the view helper)
- Create: `internal/web/mutes_test.go`
- Modify: `internal/web/server.go` (two routes)
- Modify: `internal/web/pages.go` (`page.MutedTargets`, `targetView` fields, `overview`, `status`, `targetViews`)
- Modify: `internal/web/templates/status.html`, `internal/web/templates/overview.html`
- Modify: `internal/web/static/style.css` only if the existing `hint` / `pill` / `small` button classes are not enough
- Modify: `internal/web/screenshots_test.go` (one target muted in the status and overview screenshots), `docs/screenshots/*`

**Interfaces:**
- Consumes: Task 1 (`MuteTarget`, `RequestUnmute`, `TargetMutes`); `engine.Status.Targets` (`Name`, `Description`).
- Produces:
  ```go
  type mutedTargetView struct {
      Description string
      Since       string // muted_at in s.zone(), timeFormat
      Unmuting    bool   // switch-on requested, not finished yet
  }
  // page: MutedTargets []mutedTargetView (overview)
  // targetView: MuteURL, UnmuteURL string ("/targets/" + url.PathEscape(Name) + "/mute" or "/unmute"), MutedSince string, Unmuting bool
  func (s *Server) mutedTargets(ctx context.Context, targets []engine.TargetStatus) ([]mutedTargetView, error)
  ```

Routes: `POST /targets/{name}/mute` → `s.muteTarget`, `POST /targets/{name}/unmute` → `s.unmuteTarget`, both via `action(...)`. The name must be one of `s.Status().Targets` and not `"log"`, else 404. Log Info "notification target switched off" / "switching notification target on requested" with `"target", description`. Redirect 303 to `/status`. The form actions come from `targetView.MuteURL` / `UnmuteURL`, escaped in Go, so the template stays simple.

Copy (fixed):
- Status page, State column: `<span class="pill">off</span>` with `.below` "since <time>", or "being switched on" while `Unmuting`. Button: "Switch off" / "Switch on" (`class="small"`); while unmuting the button is "Switch off" (cancels).
- Overview, at the top (above the API note), `div.hint` with `icon-info`: "**Notifications are off** for <descriptions, comma-separated> (since <time>)." per target, plus "being switched on" where it applies, and a link "Status" to `/status`. Shown in every state, also next to "All good".

- [ ] **Step 1: Write the failing tests** in `mutes_test.go` (use `newTestEnv`, `login`, `do`; set `e.status.Targets` to `mail` and `telegram:-100123`):
  - `TestMuteAndUnmuteATarget`: POST `/targets/mail/mute` → 303 to `/status`; `TargetMutes()` has mail; POST `/targets/mail/unmute` → request set.
  - `TestMuteRouteFindsTelegramChats` (Review Focus 2): POST `/targets/` + `url.PathEscape("telegram:-100123")` + `/mute` → 303, row for `telegram:-100123`.
  - `TestMuteUnknownTargetIs404`: `/targets/nope/mute` and `/targets/log/mute` → 404.
  - `TestMuteNeedsSessionAndSameOrigin`: without cookie → redirect to login, no row; cross-site `Sec-Fetch-Site: cross-site` → 403.
  - `TestMuteAgainWhileSwitchingOn` (Review Focus 1): mute, unmute, mute → row without request; status page shows "off", not "being switched on".
  - `TestStatusPageShowsMutedTargets`: body contains "off", the time in the zone, "Switch on"; a not muted target shows "Switch off".
  - `TestOverviewNamesMutedTargets`: one muted → "Notifications are off" and the description; none → the note is absent.
  - `TestOverviewAllGoodWithEveryTargetOff` (Review Focus 5): both muted, `LastRun` set, no records → "All good" and both descriptions.
- [ ] **Step 2: Run** `go test ./internal/web/ -run 'Mute|Muted'` → FAIL.
- [ ] **Step 3: Implement** routes, handlers, view helper, templates. `TestTemplatesHaveNoStyleAttributes` must stay green.
- [ ] **Step 4: Run** `go test ./internal/web/` → PASS; regenerate screenshots the way `screenshots_test.go` documents and look at them.
- [ ] **Step 5: Commit** "Web: switch notification targets off and on; the overview names the targets that are off".

### Task 4: Docs, changelog, final check

**Files:**
- Modify: `docs/notifications.md` (new section "Switching a target off", the rules from SPEC in user words, with the hint that a Telegram chat is one target)
- Modify: `docs/how-it-works.md` only if it lists the run steps (add the switch-on step)
- Modify: `ARCHITECTURE.md` only if it lists the run steps
- Modify: `CHANGELOG.md`: under `### Added`: "Notification targets can be switched off and on on the status page. While a target is off it sends nothing; when it is switched on it reports what is still firing as new. The overview names every target that is off."
- Modify: `PLAN.md` phase 31 entry: replace "`TargetStatus` gets `MutedAt` and `Unmuting`" by "the pages read `target_mutes` directly, so a click shows at once".

- [ ] **Step 1: Write the docs and the changelog line.** `go test ./cmd/smokealarm/` (docs link test) → PASS.
- [ ] **Step 2: Run** `go build ./... && go vet ./... && go test ./... && golangci-lint run` → all green.
- [ ] **Step 3: Commit** "Docs: switching notification targets off; CHANGELOG" and push `git push -u origin claude/pensive-shannon-7tk0na`.
- [ ] **Step 4: Open the PR** against main (summary, how to try it locally: `make run`, status page → "Switch off", overview note), then stop.
