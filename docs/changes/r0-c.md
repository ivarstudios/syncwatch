# Changes r0-c: finding lifecycle in the engine

Branch `sim/r0-c`, base `78ef2d8`. Fixer c. Map: `03-rules-engine-model.md` (engine sections, hunches 1, 2, 4, 5, 6, 7, 8, 11, 20) and `02-collector.md` hunches 1, 2 and 15.

Files changed: `internal/engine/engine.go`, `internal/engine/lifecycle_test.go` (new), `internal/store/findings.go`, `internal/model/hub.go` (ready flag only), `internal/notify/notifier.go` (the one debounce line), `internal/notify/urgent_since_test.go` (new test file, outside the owned list: it reuses the notify test rig to check the debounce end to end, and touches no existing notify file).

Every item has a unit test that fails on `78ef2d8` and passes now. `gofmt -l .`, `go vet ./...`, `go test ./...` (Windows) and `GOOS=linux go build ./...` are clean. The integration, compose and loadsim suites were not run (rule 3).

Tests: the map's reproductions in `03-hunch-tests/engine/` were rewritten as `internal/engine/lifecycle_test.go` (one test per item, cleaned up and renamed), plus `TestDebounceCountsFromBecomingUrgent` in `internal/notify/urgent_since_test.go`. Not taken over: `TestHunchNeverSeenH7AndC2` (rules, not engine; hunch 14 belongs to the rules fixer).

---

## 1. Restart resolves open H1/H3 (map 03 hunch 1, map 02 hunch 1)

**Root cause.** After a start every server is `unknown` until its first session (`hub.go:37-39`). `frozen()` exempted H1/H3 from freezing unconditionally (`engine.go:153-156`), and the rules don't produce H1/H3 for an unknown server, so the first evaluation resolved them. The notifier then sent "Resolved" for pinged ones, and a new urgent ping followed once the collector found the server down again.

**What changed.** `staleServers`/`frozen` were replaced by a per-evaluation `freezer` (`engine.go`). H1/H3 are frozen while their server's status is `unknown`; every other finding of an unknown server is frozen as before (unknown counts as stale). Observations follow their open record's freeze rule (`keepObservation`).

**User-visible.** After a restart, an open H1/H3 stays open and shows the `stale` tag (dashboard, problems page, API `stale: true`) until the server is contacted. Found down again: the tag goes away, same finding, no new ping, no "Resolved". Found up: it resolves as before.

**README.** "Checks" paragraph on frozen findings (line 226): say that findings also freeze while a server hasn't been contacted since a start, H1/H3 included.

## 2. S3–S7 resolve after a restart, and under a failed share listing (map 03 hunches 2 and 7, map 02 hunches 2 and 5)

**Root cause.** After the first full check the server is `up` and `Loaded`, but `sv.Structure` stays nil until the first (baseline) scan finishes, so S3 and the DIR findings (S4–S7) aren't produced and resolved. A share whose listing failed gets `sh.Error` and no `Dirs`, so its S4–S7 findings resolved and re-opened after the next scan (history churn, and pings if S4/S5 are made urgent).

**What changed.** The freezer marks a server `unscanned` while it is up but `Structure == nil || Structure.ScannedAt.IsZero()`; S3–S7 findings of such a server are frozen. For a scanned server, S3–S7 findings whose `Path` detail lies inside a share with `Error != ""` are frozen (path comparison with the server's `PathSep` style). Note: a `Structure` created by a nested-folder event before the first scan has no `ScannedAt`, so an S3 reported by an event during the first minutes after a start opens when that first scan finishes (the scan keeps event entries). The freeze under a failed share keys on the `Path` detail that `rules/structure.go` writes for S3 and DIR findings; if that detail is renamed, the share rule silently stops applying (fails open).

**User-visible.** After a restart, open S3/S4–S7 findings show `stale` until the first scan finishes, then carry on or resolve on the scan result. A transient browse error on one share no longer resolves and re-opens that share's findings; they show `stale` until the share lists again.

**README.** "Checks" paragraph on frozen findings: structure findings also wait for the first scan after a start and for a share whose listing failed.

## 3. Startup race: evaluation before the server set is published (map 03 hunch 5)

**Root cause.** `eng.Run` and `mgr.Run` start as goroutines (`app.go:219-220`). An evaluation on the empty hub sees no servers, nothing is stale, and every open finding resolves.

**What changed.** `model.Hub` has a `ready` flag set by the first `SetServers`, with `Hub.Ready()`. `Engine.Evaluate` returns early until then. The `SetServers` call signals `Changed`, so an evaluation follows within a second.

**User-visible.** None in normal operation (the race was unlikely). No "Resolved" storm on a slow start.

**README.** Nothing.

## 4. Raising a threshold resolves open findings (map 03 hunch 6)

**Root cause.** `MinDuration` was re-checked on every evaluation (`engine.go:208-210`): an H6 open for 4 days dropped out of "current" when `thresholds.paused` went from 3d to 10d, resolved, and re-opened 6 days later.

**What changed.** The `MinDuration` check applies only when there is no open record for the finding. An open finding stays open while its condition holds. A new occurrence waits for the full new threshold.

**User-visible.** Raising a threshold no longer closes open findings (no "Resolved", no later re-open, no new history entry). Lowering a threshold opens matching observations at the next evaluation, as before.

**README.** "Configuration" (`thresholds:` block, line 140) or "Checks": one sentence that thresholds decide when a finding opens, and raising one doesn't close findings that are already open.

## 5. "Resolved notified" mark lands on a re-opened record (map 03 hunch 4)

**Root cause.** The notifier reads `PendingResolved`, sends, then calls `MarkResolvedNotified`. If the finding re-opened in between, the new open record inherited the old `PingedAt` (flap protection, `engine.go:229-233`) and `mark()` set `ResolvedNotifiedAt` on it: Discord said "Resolved", the problem was open again, no new ping came, and no "Resolved" followed when it really ended.

**What changed.** `mark()` with `resolved=true` no longer touches an open record. The "resolved" message was about the earlier occurrence, which is done with, so the inherited `PingedAt` is cleared on the open record instead; the new occurrence is announced again after its debounce and reported resolved on its own. A resolved row (the finding didn't re-open, or re-opened and resolved again) is marked as before. There is one row per finding ID, so the resolved generation no longer exists to carry the mark; clearing the inherited ping is the equivalent.

**User-visible.** During a flap that overlaps a slow "Resolved" send, the re-opened problem pings again after its debounce and gets its own "Resolved" later, instead of going silent.

**README.** Nothing ("Resolved: for every finding that was pinged" stays true).

## 6. Removed or disabled server: "Resolved" messages (map 02 hunch 15)

**Root cause.** The collector manager publishes only enabled servers (`manager.go:68-77`). A removed or disabled server leaves the hub; its findings weren't produced and weren't frozen (nothing stale), so they resolved like fixed problems and "Resolved" went out for pinged ones.

**What changed.** The freezer knows which servers are present. An open record whose server is absent (or, for cross-server findings, all of whose related servers are absent; for H2 either server) is closed quietly: `ResolvedAt` set, `ResolvedNotifiedAt` set so the notifier doesn't report it, history entry closed with the note ` (server removed or disabled)` appended to its message (`store.HistoryResolveNote`), log line `finding closed reason="server removed or disabled"`. Observations of absent servers are dropped. Cross-server findings that still have a present server carry on with the remaining `Related` (rewritten in place). Absent servers count as stale for the cross-server freeze rule, so `H7` for a PC on a removed NAS and a down NAS stays frozen rather than resolving.

**DECISION:** resolve silently (no "Resolved" message); the history and the weekly "resolved this week" list show the entry with the note. A settings notice for a disabled server is not added here (web/settings is not mine; the notice for removed servers already exists).

**User-visible.** Disabling or removing a server closes its findings without Discord messages; the history/weekly list says why. Re-enabling it opens them fresh (new history entry, new ping after the debounce).

**README.** "Notifications" → "Resolved": except findings closed because their server was removed or disabled. "Checks" paragraph: what happens to a disabled server's findings.

## 7. One stale server blinds every cross-server check (map 03 hunch 8)

**Root cause.** H7, C2, H8-device, X1, X2 and C1 use `Related` = every server that sees the device/folder, and `frozen()` froze on ANY stale related server. With two NAS, one unreachable NAS blocked every PC finding on the other.

**What changed.** Cross-server findings (`Server == ""`) are frozen only when EVERY related server is stale (or absent). Findings with a `Server` keep the old rule (server or any related stale → frozen), which keeps H2 right. Observations without an open record use the same all-stale rule.

**User-visible.** While one NAS is down, H7/C2/H8/X1/X2/C1 open and resolve on what the other NAS reports. A PC that was connected only to the down NAS looks offline to the monitor and can get H7 after `pc_offline` (its last-seen time is the newest any server knows, including the down NAS's last data), which is what the data says.

**README.** "Checks" paragraph on frozen findings: cross-server findings freeze only when none of their servers can be reached.

## 8. Snooze/ignore accept any ID, ignores never expire (map 03 hunch 11, weak spot 1)

**Root cause.** `setSnooze` stored any string as a finding ID (`engine.go:437-463`); IDs are deterministic, so H1/H3 of a server could be pre-ignored invisibly. `PruneSnoozes` only removed expired snoozes (`store/misc.go:89`).

**What changed.** `Snooze`/`Ignore` accept only an ID that is open, or was resolved within the last 90 days (findings table or history: `store.FindingSeenSince`); otherwise they return `No open or recent problem has this ID.`, which the web handlers show as the flash message. `Prune` (at start and every 6 h) also calls `store.PruneIgnores`, which drops ignores of findings that are neither open nor resolved within 90 days (`resolvedMemory`, the same constant as `PruneResolved`). `Unsnooze` is unchanged. `PruneIgnores` lives in `store/findings.go` rather than `misc.go` to stay inside the owned files.

**DECISION:** the window for "in history" is 90 days, the same as the resolved-record memory, rather than the 400-day history retention: an ignore of an older finding would be pruned at the next `Prune` anyway, and an ID with no record in 90 days is exactly the pre-emptive case. Existing ignores of IDs that never had a finding are pruned at the first start with this change.

**User-visible.** Snoozing or ignoring an unknown ID fails with a message. An ignore disappears 90 days after its finding resolved (a re-open after that pings again). Ignores on open findings are untouched.

**README.** "Notifications" bullet on snoozed/ignored findings: only open or recently resolved problems can be snoozed or ignored; an ignore lasts until 90 days after the problem resolved.

## 9. Switching a check to urgent pings old findings at once (map 03 hunch 20)

**Root cause.** The notifier's debounce counted from `OpenedAt` (`notifier.go:253`). A check switched to urgent, or a DIR finding gaining an urgent reason, made findings open for days due immediately, en masse.

**What changed.** `store.Record` has `UrgentSince`, set when a record opens urgent or turns urgent on a content update, cleared when it stops being urgent. It is persisted inside the record's existing `data` JSON column (`urgent_since`, Unix ms), so there is no schema change and old rows read back as zero. `Record.UrgentFrom()` returns the later of `OpenedAt` and `UrgentSince`; `dueRecords` counts the debounce from it (the one-line change in `notifier.go`).

**DECISION:** switching urgency off clears `UrgentSince`, so switching it on again starts a fresh debounce (less noisy than pinging at once).

**User-visible.** Making a check urgent pings its open findings after the check's debounce, not immediately. Findings that open urgent behave as before.

**README.** "Notifications" → "Urgent ping": "open for its debounce time" → counted from when the finding became urgent.

---

## Not fixed / out of scope

Nothing from the assignment was left out. Related hunches noticed but not touched because they belong to other fixers: map 03 hunch 13 (a server that hasn't loaded since startup is treated as a PC; `model.Devices`), hunch 14 (never-seen device gets H7, C2 and C3 together; rules), hunch 19 (snooze date semantics and the missing reason field; web).
