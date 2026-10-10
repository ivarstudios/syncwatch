# r0-s — Structure scan and disk events

Branch `sim/r0-s` (base 78ef2d8). Files owned: `internal/collector/structure.go`, `internal/collector/events.go`, their tests (new files `structure_test.go`, `events_test.go`; `collector_test.go` untouched), `internal/model/model.go` (the agreed `Folder.LastLocalChange` field only).

Hunks outside the owned files, all in `internal/collector/collector.go` (kept to the minimum; the integrator may need to merge them with fixer c's work):

| Where | Lines | Why |
|---|---|---|
| `Collector` struct | `scanTimerOn bool` and `gaps gapState` fields, after `scanning` | items 7 and 5 need per-collector state |
| `fullCheck`, hub update | 5 lines before `s.Folders = newFolders`: carry `LastLocalChange` over from the old folder | item 6: only events know the value and the full re-check rebuilds every folder |
| `eventLoop`, gap branch | the two lines inside `if … evs[0].ID > c.lastEventID+1` become `c.missedEvents(ctx, c.lastEventID, evs[0].ID, evs)` | item 5 |

Commits (`git log --oneline main..HEAD`):

```
3fc8a00 fix(collector): missed events re-scan the affected folders; gap-caused full re-checks at most every 10 min
90eda7b fix(collector): start structure scans on time in events mode
bf4d6bf fix(collector): polled mode walks synced project folders on every scan
84385a1 fix(collector): one definition of a nested project folder for scans, events and kept entries
62ffc97 fix(collector): keep a share's directory list when its listing fails
e0edb91 fix(collector): track the newest LocalChangeDetected time per folder
```

Every new test fails on the base commit and passes with its fix (checked by swapping the pre-fix files in once). `gofmt -l .` empty, `go vet ./...` clean, `go test ./...` passes natively (collector package 53 s), `GOOS=linux go build ./...` passes.

---

## 1. Polled mode never looks inside synced project folders between baselines

Map: 02 hunch 3, "Polled mode only looks inside synced project folders on baseline scans".

**Root cause.** `scan` skips a top-level project directory that is, or lies inside, a Syncthing folder unless the scan is a baseline (`structure.go`, `synced && !baseline`), on the assumption that disk events cover those directories. Polled mode has no event stream, so a nested project folder created inside a synced folder was only found by the next baseline (7 days) or a restart. The README says polled mode re-reads everything every 15 minutes.

**What changed.** `structure.go`: the skip now also requires events mode (`cfg.Collector.Mode != config.ModePolled`); the `scan` doc comment says so. Test `TestPolledScanWalksSyncedFolders` (`structure_test.go`): a regular scan lists the three synced project folders of the fixture in polled mode and none of them in events mode.

**User-visible change.** In polled mode S3 for nested folders inside synced project folders appears after the next structure scan (`structure_interval`, default 1 h) instead of after the next baseline. Every scan in polled mode costs what a baseline costs: in the loadsim layout 12,901 listings (1.5 s native, 18–30 s in the emulated Docker VM) per `structure_interval` instead of 1,291. The overview tile shows the listings per scan.

**DECISION.** Polled mode walks synced project folders on every regular scan (same depth as the baseline); the cost is accepted because polled mode otherwise misses nested folders for up to a week.

**README.** "How data is collected" (polled mode paragraph), the `collector:` example comments (`baseline_interval: … (events cover them in between)` is now events mode only), `docs/api-findings.md` scan-load table (add the polled-mode row: regular scan = baseline listings).

## 2. Scan errors silently resolve findings

Map: 02 hunch 5, "A scan during an outage or a slow listing silently drops findings".

**Root cause.** A share whose `browse` failed (or came back empty with the share missing from its parent's listing) got `Share.Error` and no `Dirs`. The rules build S4–S7 from `Dirs`, so every directory finding of that share resolved (with "Resolved" notices for pinged ones) and re-opened after the next successful scan.

**What changed.** `structure.go`: before listing, `scan` takes the previous `Structure.Shares` from the hub (cloned); a share whose listing fails keeps its previous `Dirs` and `ScannedAt` (so it is visibly stale on the tile: "scanned <old time>" together with the error) via `stale()`. A working listing replaces them again. Fixer c freezes findings of shares with errors on the engine side. Test `TestScanKeepsDirsWhenListingFails`.

**User-visible change.** During an outage or a failing listing the directory findings of that share stay as they were, and the tile shows the share error. New shares that fail on their first listing still have no directories.

**DECISION.** The "not found on the server (is it mounted…)" case keeps the previous list too: an unmounted volume is the main way it happens and its findings should freeze, not resolve. A share that never listed successfully has nothing to keep.

**README.** The structure-scan paragraph could mention that a share with a listing error keeps its last directory list.

## 3. Stale nested entries after rule changes

Map: 02 hunch 8, "Old nested entries survive rule changes".

**Root cause.** Entries the scan did not find again (all event-found entries inside synced folders, in events mode) were kept as long as the directory existed, without re-checking the current project pattern, `ignore_dirs`, `nested_scan_depth` or the share list. Tightening the rules left S3 findings open until the directory went away or syncwatch restarted.

**What changed.** `structure.go`: kept entries must still have a share (`rules.ShareOf` over the current shares, so `shares.exclude` applies) and satisfy `nestedIn` (item 4) under the current rules; their `Parent` and `Share` are refreshed. Test `TestScanReappliesRulesToKeptEntries`: an entry added by an event survives an unchanged scan and is dropped after each of: `ignore_dirs` matching the middle directory, a depth that no longer reaches it, a pattern it doesn't match, its share excluded.

**User-visible change.** After a structure-rule or share change, S3 findings that no longer qualify resolve at the next structure scan (≤ `structure_interval`) instead of lingering. Entries the rules still cover are unchanged.

**README.** The note "Structure rules saved. The next structure scan uses them." is now fully true for nested folders too; the limitations paragraph can drop any caveat about stale nested findings.

## 4. The event path and the scan disagree on what a nested project folder is

Map: 02 hunch 9.

**Root cause.** `diskChange` accepted any matching directory at least two levels below a share: it did not require the top-level directory to be a project folder, did not apply `ignore_dirs` to the directories in between, and had no depth limit. The scan (`descend`) did all three.

**What changed.** One predicate `nestedIn(st, cs, share, path) (top, ok)` in `structure.go`: the path is inside the share, 2 to `nested_scan_depth`+1 segments below it, the first and last segments match the project pattern, and no segment is ignored. `descend` emits by it (it keeps the ignored-subtree pruning as a walk limit), `diskChange` decides by it (a cheap name pre-check stays before `DetectShares`), and item 3's kept-entry check uses it. Tests `TestEventNestedUsesScanRules` (ignored middle directory, depth) and `TestEventNestedNeedsProjectTop` (a share whose top-level directory isn't a project folder, via `shares.add`/`exclude`).

**User-visible change.** Events no longer report (a) a matching directory under a non-project top-level directory as "nested inside project folder <non-project>", (b) directories behind an ignored directory (e.g. inside `.stversions`), (c) directories deeper than `nested_scan_depth` levels below the top-level project folder. (c) is a reduction: before, events found nested folders at any depth inside synced folders. The scan never did, and item 3 would drop deeper event entries at the next scan anyway, so the two paths now agree.

**DECISION.** The depth limit applies to events too (one definition, as assigned). Users who want deeper detection raise `nested_scan_depth`; the probe's listing estimate shows the cost.

**README.** Limitations: "Inside synced folders, nested folders older than the last baseline scan and deeper than 3 levels aren't found" → nested folders deeper than `nested_scan_depth` levels below the top-level project folder aren't found by scans or events; the event description in "How data is collected".

## 5. Per-file disk events overflow Syncthing's event buffer and chain full re-checks

Map: 02 hunch 6.

**Measurements** (native Syncthing 2.1.5 in `the test kit`, GUI 127.0.0.1:24200, one folder `LIVE-BIG-1SOURCE` under a share `AKKA-LIVE-1SOURCE`, fsWatcher delay 1 s; syncwatch from this branch's base or head, events mode, `SYNCWATCH_LOG_LEVEL=debug`; logs in `the test kit`):

| # | Link | Files dropped (one `mv` of a staged tree, 50 subdirectories) | What else | "missed events" | Full re-checks | Notes |
|---|---|---|---|---|---|---|
| 1 | native, direct | 5,000 | nothing | 0 | 0 extra | Syncthing indexed them in < 25 s and emitted 5,090 events on the shared subscription (IDs read from `/rest/events`); the loop never fell 1,000 events behind. |
| 2 | TCP delay proxy, 50 ms one-way (≈100 ms RTT, a VPN) | 20,000 | syncwatch (base) restarted 4 s after the drop, so its startup full re-check (1.17 s over the proxy) overlapped the scan | 6 | 8 (1 at start, 1 at restart, 6 gap-caused) | A gap after every re-check, each ≈1,000 events (`from=8105 to=9107`, `from=10106 to=11108`, …), chained for the 20 s the scan emitted events. Subscription reached ID 25,173. |
| 3 | as 2, with this branch | 20,000 | as 2 | 4 | 3 (1 at start, 1 at restart, 1 gap-caused) + 1 deferred (10 min later) | The 3 later gaps logged "a full re-check ran recently, deferring the next". One re-scan of the affected folder ran 30 s after the last gap: 154 listings, 16 s over the proxy, 0 failed. Subscription reached ID 45,255. |

So gaps do not happen on a fast link, but on a slow link any full re-check (startup, reconnect, the 4-hourly one, a `ConfigSaved` that fails to refresh) during a big sync starts a chain: the re-check holds the event stream for seconds, which is what makes the next gap.

**What changed.** `events.go`: `missedEvents` (called from `eventLoop`'s gap branch) notes the Syncthing folders in the batch and (1) arms a re-scan of them `gapScanAfter` (30 s) after the last gap, at the latest `gapScanMax` (10 min) after the first pending one; (2) requests a full re-check at once only when none was caused by a gap in the last `gapFullEvery` (10 min); otherwise one deferred re-check is scheduled for when that time has passed (at most one pending). `rescanAfterGap` runs one re-scan at a time. `structure.go`: `rescanFolders` walks the top-level project folder that holds each affected Syncthing folder (as a baseline does, same `descend`, same depth) and replaces the nested entries under those top-level folders, keeping `FoundAt`/`Source` of re-found ones and removing nothing when any listing of the walk failed; the throttled lister is factored into `browser`, shared with `scan`. Log lines: "missed events; running a full re-check" (unchanged), "missed events; a full re-check ran recently, deferring the next", "project folders re-scanned after missed events" (Info, with folders/listings/failed/took). Test `TestMissedEventsRescansFoldersAndLimitsFullChecks`.

**User-visible change.** During a big sync over a slow link the server is no longer re-read every second; at most one gap-caused full re-check per 10 minutes (plus one deferred). Hub state that a lost event would have changed (a folder going to error, a device disconnecting) can therefore be up to ~10 minutes stale after a gap instead of ~1 s + a re-check; disk events lost in the gap are covered by the re-scan, so S3 for a nested folder created during the sync appears ≤ 30 s after the gaps stop (≤ 10 min into a very long one) instead of never (before, a lost `LocalChangeDetected` of a new directory was never recovered until a baseline or restart).

**DECISION.** As assigned: a gap re-scans the affected folders instead of (only) re-checking everything, and gap-caused full re-checks are limited to one per 10 minutes. Refinements chosen: the first gap still re-checks at once (one outage-sized state resync is cheap and the old behaviour), a deferred re-check catches up after the 10 minutes so state staleness is bounded, the re-scan is debounced so a storm of gaps costs one walk, and a walk with a failed listing never removes entries (no false S3 resolution).

**README.** "How data is collected": say what happens on missed events; `docs/api-findings.md` 3b consequence; the limitations paragraph (state after a gap can be up to 10 min old).

## 6. C1 uses lastFile.at, which only changes for pulled files

Map: 03 hunch 18, "C1 can report active folders as inactive".

**Root cause.** `stats/folder` `lastFile.at` is set by Syncthing only for files pulled from other devices (`docs/api-findings.md` 7). A folder edited locally on its server (over SMB) never moves it, so C1 reports the folder inactive after `inactive_project`.

**What changed.** `model.go`: `Folder.LastLocalChange time.Time // newest LocalChangeDetected seen by the collector` (last field; fixer i adds the identical line). `events.go`: on `LocalChangeDetected` (files and directories; `RemoteChangeDetected` doesn't count, that is what `lastFile.at` covers) the folder's `LastLocalChange` becomes the event time if newer. `collector.go` `fullCheck`: the value is carried over when the folders are rebuilt. `refreshConfig` keeps known folders, so nothing to do there. Test `TestLocalChangeTimeTracked` (events_test.go): set from a local event, not from a remote one, never goes backwards, survives a full re-check. The C1 rule change (max of `LastFileAt` and `LastLocalChange`) is fixer i's.

**User-visible change.** None from this branch alone; with fixer i's rule change, a folder with local edits is no longer reported inactive. Note the value lives only in memory and only in events mode: after a restart C1 again sees only `lastFile.at` until the next local change event; polled mode never sees local changes (C1 keeps its old behaviour there). Event times are the NAS clock, like `lastFile.at`.

**README.** C1 description ("last change" now means pulled or local change, events mode) and the C1 row in the checks table; `docs/api-findings.md` 7 consequence.

## 7. Structure scans in events mode run only after a long-poll returns

Map: 02 §3.5 step 3 / §3.8 timing (no numbered hunch): `maybeScan` runs once per loop round, so a scan falling due during the long-poll started up to `event_timeout` (50 s) late.

**What changed.** `structure.go`: `maybeScan` starts, once per collector and only in events mode, a `scanTimer` goroutine on Run's context that checks every `scanTimerEvery` (10 s; a package var like `trafficEvery`, tests shorten it) and calls `maybeScan` while the server is `up` and loaded; `lastScan`, `lastBase` and `scanning` are now all handled under `scanMu` because two goroutines call `maybeScan`. `collector.go`: the `scanTimerOn` field. Polled mode is unchanged: `pollLoop` already wakes for the next scan and reads `lastScan` only from its own goroutine. Test `TestScanTimerStartsDueScans`: a due scan starts without the loop, not while the server is down, and again when it is back.

**User-visible change.** Scans start within 10 s of falling due instead of up to 50 s late; otherwise none. One more goroutine and a hub read every 10 s per server.

**DECISION.** A 10 s tick instead of waking the long-poll: it needs no change to the event loop and no cancelled requests, and `structure_interval` is at least 1 minute.

**README.** None required ("hourly structure scan" stays true).

---

## Not done / not a defect

- Nothing in the assignment was found to be a non-defect. All seven items are fixed above.
- Not touched (other owners): the polled-mode busy loop while a scan overruns (02 hunch 4, `pollLoop` in `collector.go`); the scan running on Run's context during an outage (02 hunch 5, second part) — item 2 and the `up` check in `scanTimer` make it harmless for findings; the mixing of NAS event times with syncwatch's clock in the scan merge (02 §3.8) is unchanged, and `rescanFolders` follows the same rule (`FoundAt.After(now)` keeps event entries added during the walk).
- Measurement caveat: the delay proxy adds 50 ms per TCP chunk per direction, so it is a plausible VPN, not a measured one. The real deployment's RTT and NAS scan speed decide how often gaps happen; the fix bounds the cost either way.

## Tests added

`internal/collector/structure_test.go`: `TestScanKeepsDirsWhenListingFails`, `TestScanReappliesRulesToKeptEntries`, `TestPolledScanWalksSyncedFolders`, `TestScanTimerStartsDueScans`, `TestMissedEventsRescansFoldersAndLimitsFullChecks` (white box: `startCollector` builds a collector on the fixture server without the manager, `browseLog` records and fakes `browse` answers).
`internal/collector/events_test.go`: `TestLocalChangeTimeTracked`, `TestEventNestedUsesScanRules`, `TestEventNestedNeedsProjectTop` (through the manager and the fake Syncthing, like the existing tests).
