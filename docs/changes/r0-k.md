# Fix round r0, fixer k: dashboard views, counts and forms

Branch `sim/r0-k`, base `78ef2d8`. Map: `05-web.md` (hunches 11, 13, 17 and the view notes in 3.13), `06-config-store-app-cli.md` (hunch 14).
Files changed: `internal/web/views.go`, `api.go`, `ha.go`, `server.go` (snooze handler and the `plural` template function),
`templates/overview.html`, `grid.html`, `problems.html`, `assets/app.js`, `app.css`, `web_test.go`;
`internal/model/finding.go`, `model.go` (the folder state) and the new `internal/model/folder_test.go`.
Other files touched: `internal/web/settings.go` (one line: the checks table in Settings uses the dashboard severity word, item 8).
Every item has a test in `internal/web` or `internal/model` that shows the behaviour; item 1 was also checked in a headless browser
against both versions of the script (results in `the test kit`).

## 1. Live updates wiped half-typed ignore reasons and snooze dates (map hunch 13)

- Root cause: `#problems-body` (like `#overview-body` and `#grid-body`) is swapped with `outerHTML` on every `sse:update`, and the
  engine publishes an update on every visible change, sync percentages and need counts included. During a big sync the whole
  problems list, forms and all, was replaced every few seconds. Reproduced in a headless browser with the script from `78ef2d8`:
  the typed reason was gone from the page after the next update, focus lost.
- Changed (`app.js`, `app.css`): an `htmx:beforeSwap` listener sets `shouldSwap = false` while a form control inside the target has
  focus or an input, select or checkbox inside it holds a change that was not submitted yet. The section gets the class
  `updates-paused` (the summary line shows "· live updates paused while you edit"), the skipped update is remembered and fetched
  (`htmx.trigger(target, "sse:update")`, the same trigger the SSE extension uses) as soon as the inputs are left alone
  (`focusout` and `change`, checked a moment later because focus moves before `focusout` settles). Fold-out state is kept as
  before. The overview and grid sections hold no inputs, so they are never paused.
- User-visible: on the problems page, live updates stop while someone types a reason, picks a date or keeps a button focused, and
  resume (with the missed update applied at once) when the field is left empty or the form is submitted. A field that still holds
  text keeps the section paused until it is cleared or submitted; the note in the summary line says so.
- README: the dashboard / live updates description: add that a section pauses while a form in it is being edited.
- DECISION: guard only. "Do not publish updates the problems page does not show (sync percentages)" needs the engine to tell
  finding changes from snapshot changes (`engine.go` publish path and the SSE handler, both outside this fixer's files; the SSE
  event carries only a version). Possible follow-up for the engine's owner: publish `event: findings` and `event: snapshot`
  separately and let the problems page trigger on the first only.
- DECISION: a focused button counts as "in use" too, so tabbing from the reason field to "Snooze 1 day" does not replace the button
  under the user.
- Tests: `TestLiveUpdateGuardShipped` (pins the asset); the browser scenario above (not a Go test).

## 2. Folder state disagreed between grid, tile, status API and HA (map hunch 11)

- Root cause: four switches (`views.go` tile and grid cell, `api.go`, `model.SyncStatus`/`FolderSync` for HA) each decided a folder's
  state on their own: a watcher error was an error in the grid, idle in the tile and the API, "up to date" in HA; scanning was
  syncing in the tile and the API but scanning in HA.
- Changed (`model.go`): `Folder.FolderSync()` is the one place: paused, else error (`state == "error"`, a folder error text, errors
  or pull errors), else watch error, else scanning (`scanning`, `scan-waiting`, `starting`), else up to date (`idle` with nothing
  needed), else syncing. `SyncWatchError = "watch error"` is a new state; `SyncStatus` ranks it below error and above syncing and
  scanning. `views.go` (tile counts `Scanning` and `WatchErrors`; `serverCell` shows "⚠ watch error" as a warning cell with the
  watcher message as tooltip and "⟳ scanning" for scanning), `api.go` (`folders.scanning`, `folders.watch_error`), `ha.go`
  (`folders_scanning`, `folders_watch_error`, status value `watch error`) all count by it. `grid.html` legend updated.
- User-visible: the tile line reads "N: ✔ idle · ⟳ syncing · ⟳ scanning · ✖ errors · ⚠ watch errors · ⏸ paused"; a watcher error
  is a yellow ⚠ in the grid (was a red ✖) and still counts as a problem in the row; `/api/v1/status` gains `folders.scanning` and
  `folders.watch_error` (idle no longer includes folders with a watcher error, syncing no longer includes scanning);
  `/api/v1/ha` gains `folders_scanning`, `folders_watch_error` and the status `watch error`.
- README: "JSON status API" field list, "Home Assistant" status values and sensor fields, the folder grid legend.
- DECISION: a paused folder is "paused" whatever its error fields say (Syncthing's own GUI does the same; the fields can be stale),
  and a server is "paused" only when every folder is. In the HA status a watcher error outranks syncing and scanning: it is a
  persistent warning and would otherwise disappear during every sync.
- Tests: `model.TestFolderSync`, `model.TestSyncStatusWatchError`, `web.TestFolderStateAgreesEverywhere`.

## 3. "Snooze until date" ended a day early and refused today (map 3.10)

- Root cause: the date was parsed as 00:00 at the start of the chosen day in the configured zone and had to be after now.
- Changed (`server.go` `handleSnooze`): the snooze lasts to the end of the chosen day (00:00 of the next day in the configured
  zone); today is accepted; the message for a past day is "Pick today or a later date.". `problems.html`: the snooze form has an
  optional reason field like the ignore form, and the date field starts at today (`min`, in the configured zone).
- User-visible: "Snooze until 2026-10-10" keeps the problem quiet through 2026-10-10 (flash "Snoozed until 2026-10-11 00:00.");
  a snooze can carry a reason, shown as "snoozed until … (reason)".
- README: the snooze paragraph (until the end of the chosen day; reason optional).
- Tests: `TestSnoozeUntilDate` (Pacific/Auckland, 23:00 local).

## 4. "Resolved this week" showed the capped number and the wrong 30 (map hunch 17)

- Root cause: the loop walked the history newest-opened first and stopped collecting at 30, then sorted the 30 by their formatted
  time; the summary and the heading printed `len` of that list.
- Changed (`views.go`): every resolution of the last 7 days is counted (`ResolvedCount`); the list holds the 30 most recently
  resolved, newest first, and the heading says "Resolved this week (N) · the latest 30" when it is cut.
- User-visible: the real count in "N new and M resolved this week" and in the heading; the list shows the latest resolutions.
- README: nothing (the number was never documented as capped).
- Tests: `TestResolvedThisWeekCountAndOrder` (35 resolutions, opened in the opposite order).

## 5. Count words ("3 error", "1 folders", "(s)")

- Changed: `plural(n, one, many)` in `views.go`, registered as a template function. Used in `overview.html` (errors, watch errors),
  `grid.html` (folders, twice), `problems.html` (errors, warnings, suggestions) and `views.go` (directory listings, failing items,
  items left).
- Still outside this fixer's files, for their owners: `notify/notifier.go:727` "urgent problem(s)", `rules/health.go:110, 250`
  "file(s)", "conflict(s)", `web/settings.go:739` "secret(s)", `cmd/syncwatch/main.go:283` "server(s)", "secret(s)". `plural` is
  unexported in `web`; a shared home (`model` or `rules`) would let them use it.
- Tests: `TestSeverityWordsAndPlurals`.

## 6. Ignored and snoozed findings disappeared without trace

- Changed (`views.go`, `problems.html`, `overview.html`, `app.js`, `app.css`): the problems page ends with a collapsed
  "Snoozed and ignored (N) · still open, hidden above, never ping" list (`#snoozed`) with the snooze or ignore label and an
  Un-snooze / Stop ignoring button for admins; the summary's "N snoozed or ignored" links to it; the overview's "All N open" is
  followed by "· N snoozed or ignored" linking to `/problems#snoozed`. `#f-<id>` links open the fold-outs around a finding in
  that list too. With "Show snoozed and ignored" ticked the rows are inline as before and the list is not shown. The grid shows
  folders, not findings, so nothing changed there.
- User-visible: as above. The problems summary no longer says "hidden" but links to the list.
- README: the snooze/ignore paragraph: where hidden findings can be found and undone.
- DECISION: the list is not narrowed by the severity, check, server and category filters (the old hidden counter was not either),
  so it stays a complete inventory of what is muted.
- Tests: `TestSnoozedAndIgnoredStayVisible`.

## 7. HA and status endpoints report what the dashboard reports (map 06 hunch 14, 05 hunch 17)

- The folder counts and the overall status come from item 2 (one function for all four consumers). `TestAPICountsMatchOverview`
  checks that `/api/v1/status` `counts` (error+warn+info = the overview's open count, urgent, snoozed) and the per-server folder
  counts of both endpoints equal the overview tile's numbers.
- The `moved_24h_gb` spike after downtime is fixed on the store side by fixer d (`RecordTraffic` re-baselines after a gap);
  nothing changed in `ha.go` for it.
- README: "Home Assistant": the new fields and status value (item 2); fixer d's note on gaps.

## 8. Severity names: ERROR/WARN/INFO vs error/warn/info vs errors/warnings/suggestions

- Root cause: the badges used `Severity.String()` (the JSON and log names), the filter showed its URL values, the summary had its
  own words.
- Changed (`model/finding.go`, `views.go`, `problems.html`, `settings.go`): `Severity.Label()` gives the dashboard word (error,
  warning, suggestion); badges, the severity filter (values `error`/`warn`/`info` stay in URLs, labels are the words), the summary
  and the checks table in Settings use it. `ParseSeverity` also accepts "warning" and "suggestion".
- User-visible: badges say "🔴 error", "🟠 warning", "🔵 suggestion"; the filter lists error / warning / suggestion.
- Not changed on purpose: `/api/v1/status` `severity` stays `ERROR|WARN|INFO` (scripts and Home Assistant templates depend on
  it); `notify.HealthLine` ("E errors, W warnings, I suggestions", already these words) belongs to fixer g2.
- DECISION: lower-case full words, as the task said.
- Tests: `TestSeverityWordsAndPlurals`.

## Not done

- Item 1, second half (no SSE update for changes the problems page does not show): see the DECISION under item 1.
