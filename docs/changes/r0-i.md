# Changes r0-i: rules, false and missing findings

Branch `sim/r0-i`, base `78ef2d8`. Fixer i. Every item has a unit test that failed before the fix (several are the map's hunch experiments, cleaned up and renamed). `gofmt -l .` is empty, `go vet ./...` and `go test ./...` pass on Windows, `GOOS=linux go build ./...` passes. The integration, compose and loadsim suites were not run (integrator).

Files outside my list that I touched, smallest possible hunks:

- `internal/model/hub.go`: one line in `Hub.Update` (`s.RememberRemoteStates()` after `ensureMaps`), needed by item 2. Nothing else in the model can see every update.
- `internal/store/serverdevice_test.go`: new test file for the store helper of item 4 (named so it can't collide with another fixer's `misc_test.go`).

## 1. Watcher error alone raises an urgent ERROR "Sync stopped" at once (map 03, hunch 9)

- Root cause: `health.go` `folderHealth` put `file watcher error: …` into the same reason list as folder errors and out-of-space, so the H4 that resulted was ERROR, urgent, with MinDuration 0. Syncthing keeps syncing with its periodic scans when the watcher fails; `/api/v1/ha` already didn't count it as an error.
- What changed: `internal/rules/health.go` (the watcher is its own H4 finding with key `watcher:<folderID>`, Severity WARN, MinDuration `watcherGrace` = 1 h, message "File watcher failed for X on S; changes are found by periodic scans only (<error>)", details Folder ID, Path, Watch error), `internal/rules/rules.go` (`add` returns the ID and keeps a preset Severity; the evaluator has a `soft` set of IDs that stay non-urgent even when the check is configured urgent). A watch error that mentions no space left still counts as "out of space" (urgent, at once). Tests: `TestWatcherErrorIsNotUrgent`, `TestFolderHealth`.
- User-visible: a failing watcher is a WARN that opens after an hour and never pings; it stays open next to a "Sync stopped" ERROR for the same folder when both apply. An existing open H4 that was only a watcher error resolves on upgrade (its ID changed) and re-opens as the new WARN an hour later.
- README: "Checks" table row H4; `thresholds.failing_files` comment in "Configuration"; `plan.md` line about H4 timing (not edited by me).
- DECISION: watcher error becomes a non-urgent WARN reason after 1 h (hard-coded `watcherGrace`, no new config knob); folder errors, out of space and failing files > threshold keep H4 as is. Kept as a separate finding ID rather than a reason inside `folder:<id>`, so that a folder error appearing later doesn't flip an old WARN record to urgent and ping without debounce (map 03, hunch 20).

## 2. H6 remote-paused and C3 "doesn't share it back" reset while a device is disconnected (map 03, hunch 10)

- Root cause: Syncthing deletes its per-device folder state when a device disconnects (`remoteFolderStates` in `lib/model`), so `db/completion` and `FolderCompletion` report `remoteState: "unknown"` for every disconnected device; the full check copied that into `Folder.Completion`, the rules saw neither `paused` nor `notSharing`, and the engine dropped the observation. For a PC that is off every night the 3 d / 30 d timers never completed.
- What changed: `internal/model/model.go` (`Server.RemoteStates` map folder → device → last known state, `Server.RemoteState(folder, dev)` returning the live state when it is valid/paused/notSharing and the remembered one otherwise, `Server.RememberRemoteStates()` which records known states and forgets folders and devices that are no longer shared, deep copy in `Server.Clone`), `internal/model/hub.go` (one call after every `Update`), `internal/rules/health.go` (H6 remote paused reads `sv.RemoteState`), `internal/rules/cleanup.go` (C3 notSharing reads `sv.RemoteState`). H5 (device stuck) keeps the live value: it needs the device connected anyway. Tests: `TestRemoteStateRemembered` (model), `TestRemoteStateSurvivesDisconnect` (rules).
- User-visible: H6 "X has paused folder F" and C3 "doesn't share it back" keep counting through disconnects and resolve when the device reports valid again. A device that never reconnects keeps the last state, so such an H6/C3 can stay open next to H7/C2. The web grid (`views.go` `pcCell`) still shows the live state and is not mine.
- README: "Checks" rows H6 and C3 (say that the state is kept from the last connection).
- DECISION: the remembered value is used whenever the live report is unknown or missing, not only while disconnected (covers the short unknown window right after a connect). The memory is in RAM only: after a syncwatch restart it starts again from the next report while connected.

## 3. H8 reference version tie-break picks the lexicographically higher string (map 03, hunch 12)

- Root cause: `health.go` `versions` broke ties with `v > ref` on strings, so "2.1.9" beat "2.1.10".
- What changed: `internal/rules/health.go` (`versionLess` compares `major.minor.patch` part by part as numbers, non-numeric parts as strings, fewer parts counts as older; candidates are visited in sorted order so the result never depends on map order). Tests: `TestVersionTieBreakIsNumeric`.
- User-visible: with one NAS on 2.1.10 and one on 2.1.9, PCs on 2.1.10 no longer get H8; PCs on 2.1.9 do ("the servers run v2.1.10").
- README: nothing (the rule "ties go to the newest" was already the intent).

## 4. A server not loaded since startup is treated as a PC (map 03, hunch 13)

- Root cause: `Snapshot.Devices()` only knows a server by `Server.MyID`, which is learned on the first full check. After a restart while a NAS is unreachable, its device ID (configured on the other NAS) became a "PC": H7/C2/C3/H8-device for the NAS, and the health line counted it.
- What changed: `internal/store/misc.go` (`ServerDeviceID` / `SetServerDeviceID`, settings key `server.device_id.<serverID>`), `internal/collector/collector.go` (3 lines at the end of `fullCheck`: persist `status.MyID`), `internal/app/app.go` (9 lines after `NewHub`: seed the hub with each enabled server's stored ID before the manager's `SetServers`, which keeps existing state), `internal/rules/cleanup.go` (C3's never-connected reason skips polled servers: H1/H2 cover them). Tests: `TestServerDeviceID` (store), `TestHubKeepsSeededDeviceID` (model), `TestKnownServerDeviceIDIsNotAPC` (rules).
- User-visible: after one successful full check per server, restarts no longer produce PC findings or PC counts for an unreachable NAS. The device name shown for it is the server's syncwatch name.
- README: "Security" or "First start" wherever the database contents are listed (one more settings row per server); "Checks" row C3.
- Not done: `settings.DropRemovedServers` (not my file) doesn't delete `server.device_id.<id>` when a server is removed from the configuration; a later server reusing the same ID would be seeded with a stale device ID until its first full check. Harmless, but worth one line there.

## 5. Never-seen devices get H7 + C2 + C3 together, with a wrong H7 text (map 03, hunch 14)

- Root cause: `offlinePCs` (H7), `oldDevices` (C2) and `deadShares` (C3) each handled the "LastSeen is zero" case on their own.
- What changed: `internal/rules/health.go` (H7 skips devices that were never seen anywhere), `internal/rules/cleanup.go` (C2 never-seen message is "X has never connected since syncwatch started watching it, over 90 days ago, but is still configured on …"; C3 skips a device no server has ever seen while C2 is enabled, and skips polled servers for the never-connected reason, see item 4). C3 "never connected to SERVER" still fires for a device another server has seen. Tests: `TestCleanup`.
- User-visible: a device that never connected produces one INFO (C2) after `old_device`, nothing before. The H7 "has not connected since syncwatch started watching it" after 7 days is gone; H7 is only for devices that were online once. With C2 disabled, C3 reports it per folder again and H7 still doesn't.
- README: "Checks" rows H7, C2, C3.
- DECISION: the rules can't know how long the device has been watched (that is the engine's observation, not in the snapshot), so the text says "over <old_device> ago", which is true from the moment the finding opens, instead of an exact "N days ago". DECISION: no H7 for never-seen devices even when C2 is disabled (the task's "only C2"; a device that was never online isn't "offline").

## 6. S1 misses two Syncthing folders with exactly the same path (map 03, hunch 15)

- Root cause: `pathx.Within` is strict, and `overlaps` only used it.
- What changed: `internal/rules/structure.go` (`st.Equal` first; one S1 per pair, key `same:<a>:<b>` in display-name order, message "Syncthing folders A and B have the same path on S; files would be synced twice"). Equality is after `Clean` (so trailing and doubled separators, and `/` vs `\` on Windows servers, don't matter) and case-insensitive on Windows servers. Tests: `TestOverlapSamePath`.
- User-visible: a new urgent ERROR for identical paths. Different spellings of the same directory (QNAP's `/share/X` versus `/share/CACHEDEV1_DATA/X`) are still not seen; that needs the file system.
- README: "Checks" row S1.

## 7. DIR/S3 finding IDs lower-case the path on case-sensitive servers (map 03, hunch 16)

- Root cause: `structure.go` built the keys with `strings.ToLower(path)` for every server.
- What changed: `internal/rules/structure.go` (`pathKey`: cleaned path, lower-cased only when the server's separator is a backslash; used by S3 `nested:` keys and the `DIR` IDs). Tests: `TestDirIDsKeepCaseOnCaseSensitiveServers`.
- User-visible: `LIVE-Dup` and `LIVE-DUP` on a Linux NAS are two findings. On upgrade, every open S3 and S4–S7 finding on a case-sensitive server whose path has an upper-case letter gets a new ID once: it resolves and re-opens (history rows, a "Resolved" message and a new ping for S3 if it was pinged), and a snooze or ignore on the old ID no longer applies. Windows servers are unaffected.
- README: wherever finding IDs are described as stable ("Development" or the "Checks" intro), note the one-time change.
- DECISION: implemented as required rather than only disambiguating on collision; the repo is at its initial commit, so the one-time ID change costs little now and a collision-only scheme would make IDs depend on what else exists.

## 8. X1's subject flips between evaluations (map 03, hunch 17)

- Root cause: `cross.go` iterated `sv.Folders` (a map) and took the subject from the first location.
- What changed: `internal/rules/cross.go` (folders visited with `sortedFolderIDs`, so the subject is the first folder by display name on the first server in configuration order). Tests: `TestX1SubjectStable`.
- User-visible: no more record rewrites and SSE updates on every evaluation when labels differ only in case.
- README: nothing.

## 9. C1 relies on lastFile.at, which only pulled files set (map 03, hunch 18)

- Root cause: Syncthing's `lastFile` is only set for files pulled from other devices (`docs/api-findings.md`), so a folder edited locally on its only server looked inactive.
- What changed: `internal/model/model.go` (`Folder.LastLocalChange time.Time // newest LocalChangeDetected seen by the collector`, last field, same text as fixer s), `internal/rules/cleanup.go` (C1 uses the newest of `LastFileAt` and `LastLocalChange`; the message says "(the last file pulled from another device was on DATE)" or "(the last local change seen by syncwatch was on DATE)", details get "Based on", "Last file" only when a file name is known). Tests: `TestInactiveProjectBasis`.
- User-visible: C1 message changed; folders with local changes seen by the collector stop being reported. Fixer s fills the field from events; until then the behaviour is as before. Local changes made before syncwatch started are still unknown, so C1 can still open at startup for a locally-edited folder whose last pull was long ago, and then resolve at the first local event.
- README: "Checks" row C1 (say what the age is based on).

## 10. UNC and root edge cases in pathx (map 01, hunch 19)

- Root cause: `Parent` returned the root for a root (`Parent("/") == "/"`, `Parent("C:\") == "C:\"`), had no idea of UNC roots (`Parent("\\srv\share") == "\\srv"`), and `Clean` didn't collapse `//`, so `/a//b` and `/a/b` were different paths.
- What changed: `internal/pathx/pathx.go` (`Clean` collapses repeated separators while keeping a UNC `\\` prefix; `isRoot` knows `/`, `\`, `C:\`, `\\srv\share` and a bare `\\srv`; `Parent` returns "" for a root; `Display` leaves a backslash alone on Linux servers, where it is a name character). `Base`, `Within`, `Rel`, `Join`, `Split` unchanged in code, but they go through `Clean`. Tests: `TestRoots`, `TestCleanCollapsesSeparators`.
- User-visible: share detection stops at a UNC root instead of producing a bogus `\\srv` share; S3 subjects on Linux servers keep a backslash in a name. Callers were checked: `shares.go` loops end at "" (the `Parent(p) == p` guard stays, harmless), `collector/structure.go` `exists` treats "" as "keep", `probe.go` still browses a folder's parent `/` when a folder sits directly under the root (that is the probe's choice, not a pathx bug).
- README: nothing.

## 11. Dead `joinMax` in rules.go

- Removed, with the `strings` import it was the only user of. No behaviour change.

## Not a defect / not done

- Map 03 hunch 10 ("may never open") is confirmed from Syncthing's source rather than live: `lib/model` drops `remoteFolderStates[device]` in `Closed()`, so a disconnected device's `remoteState` is `unknown`. No live test was run for it.
- Everything in the assignment is done. Items noticed on the way but outside my files: `settings.DropRemovedServers` and the stale device ID (item 4); `web/views.go` `pcCell` could use `Server.RemoteState` to show "paused"/"not shared" for an offline PC (item 2); README and plan.md still describe the old H4 watcher and H7 never-seen behaviour.
