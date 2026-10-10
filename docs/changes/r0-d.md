# r0-d: collector core robustness

Branch `sim/r0-d`, base 78ef2d8. Fixer d. Files owned: `internal/collector/collector.go`, `manager.go`, `traffic.go`, `collector_test.go`, `internal/store/traffic.go` (+test). Map: `02-collector.md` (hunches 4, 10, 11, 12, 13, 14, 17, 18, 19, 20) plus item 10 (scan context).

Files outside the owned set that were touched (smallest hunks):

- `internal/collector/events.go` (item 8): the `ConfigSaved` and `Pending*Changed` cases of `apply` only set `c.configDue` / `c.pendingDue`; `refreshPending` returns its error. Nothing else in the file changed.
- `internal/web/web_test.go` (item 7): the three `RecordTraffic` calls pass the new `maxGap` argument (`7*24*time.Hour`, since that test places readings days apart on purpose).

Every item has a unit test in `internal/collector/collector_test.go` (or `internal/store/traffic_test.go`) that was run against the unfixed code where it could compile there; the map's experiments for hunches 4, 11 and 18 were reused in cleaned-up form (`TestPollWait`, `TestCertChangedBacksOffAndWarnsOnce`, `TestLastErrorIsReadable`). The collector package's tests now take about 78 s on this host (40 s before); the new timing-based tests are the cost.

---

## 1. Polled mode spins at full CPU while a structure scan overruns (hunch 4)

**Root cause.** `pollLoop` slept until the earlier of the next poll and the next scan. Once a scan had been running longer than `structure_interval`, that wait was negative; `maybeScan` returned at once because a scan was already running, so the loop ran at full speed until the scan ended (3.7 million allocations in 300 ms in the map's experiment).

**What changed.** `collector.go`: the wait moved into `pollWait`. It is never shorter than 1 s, and a scan that is still running doesn't count towards the next wake-up: the loop waits for the next poll (or a forced full check) instead. Test: `TestPollWait` (the wait values, and the loop's allocation count with an overrunning scan).

**User-visible.** No CPU spin in polled mode. The scan after an overrunning one starts at the next poll (up to `poll_interval` later) rather than the instant the previous one ends.

**README.** Nothing says otherwise; optionally a sentence in "Configuration" next to `structure_interval` ("a scan that is still running when the next is due is retried at the next poll").

DECISION: while a scan runs, the polled loop waits for the next poll rather than for the scan's end (the task allowed either). This keeps `structure.go` untouched (fixer s) and avoids a done-signal between the scan goroutine and the loop.

## 2. A changed certificate is retried every ~10 s forever with a WARN line each time (hunch 11)

**Root cause.** `failed()` returned before counting a `CertChangedError` as a failure, so the backoff stayed at its last value (10 s for a server that was up). Each attempt logged at WARN: about 8,600 lines a day until the certificate was accepted.

**What changed.** `collector.go`: a separate `certFails` counter grows with every certificate failure and feeds the normal backoff formula (`backoffStep` × failures), but with its own cap `certBackoffMax` = 5 min (`backoff()`); ordinary failures keep the 10 s → 60 s backoff. The warning "certificate changed; not connecting until it is accepted" is logged at WARN when the change is first seen (or the fingerprint changes again) and then at most once an hour; the attempts in between log the same line at DEBUG. `succeeded()` resets both. `AcceptCert` and `ResetPin` still wake the collector at once. The backoff constants are package variables so tests can shrink them. Tests: `TestBackoff`, `TestCertChangedBacksOffAndWarnsOnce`.

**User-visible.** After a certificate change the retries go 10, 20, 30 … 300 s and stay at one per 5 min; the log shows one WARN line per hour instead of six per minute. H3 and the Settings box are unchanged. After "accept" the collector reconnects at once as before.

**README.** "Security → TLS" could say that a changed certificate is retried with a growing backoff (up to 5 min) until accepted, and the log reminds hourly.

DECISION: the 5 min cap applies to certificate failures only; the ordinary cap stays at 60 s so outage recovery is still seen within a minute.

## 3. Leftover wake tokens skip the next backoff (hunch 12)

**Root cause.** `Wake()` leaves a token in the buffered channel when `Run` is not waiting. `FullCheckNow` (called from inside the event loop after missed events or a failed config re-read) and `ResetPin` while a server is up leave such a token; the next failure, hours later, then retried at once, and two quick failures marked the server down within seconds (a short restart became a brief H1).

**What changed.** `collector.go` `Run`: the wake channel is drained after `failed()` and before the backoff wait, so only a wake-up that arrives while the collector is actually waiting takes effect. Test: `TestStaleWakeDoesNotSkipBackoff`.

**User-visible.** A failure after an in-session `FullCheckNow` or a pin reset waits the normal backoff before retrying.

**README.** Nothing.

DECISION: "drain before waiting" rather than "stamp the token". The window in which an admin's accept/reset wake-up can be lost is the duration of the failing session itself (one TLS handshake for a certificate failure, milliseconds); if that happens the retry comes after the backoff (up to 5 min after item 2) instead of at once. Considered acceptable; the alternative (time-stamped wakes) was more code for a race that needs µs timing.

## 4. One folder takes the whole server down, or vanishes silently (hunch 13)

**Root cause.** `perFolder` treated only 4xx (except 401/403) as per-folder. A lasting 5xx on one folder's `db/status` or `db/completion` therefore failed every full check, the session, and after `down_after_failures` the whole server (false H1 "can't be reached"). A per-folder 4xx was skipped at DEBUG, leaving the folder with an empty state, which the dashboard and `/api/v1/ha` count as "syncing".

**What changed.** `collector.go` `fullCheck`: a per-folder request that fails with any HTTP status other than 401/403 is that folder's problem (`serverLevel`, `folderErr`): it is logged at WARN and, after the parallel phase (so it can't race with the folder's own `db/status` write), the folder gets `State = "unknown"` when `db/status` didn't answer and `Error = <error text>` when it has no error of its own; completion errors name the device. The check completes and the server stays up. The 404 for a paused folder (`f.Paused`, or a 404 whose body mentions "paused": the folder was paused after the config was read) is still only logged at DEBUG. 401/403, transport errors and timeouts fail the check as before. `folder/errors` failures that aren't server-level are logged at DEBUG and skipped (the folder already counts as failing). `perFolder` itself is unchanged and still used by `refreshConfig` (events.go). Test: `TestPerFolderErrorStaysWithTheFolder`.

**User-visible.** A folder whose `db/status` fails shows state "unknown" with the error (e.g. `/rest/db/status: HTTP 500 …`) on the tile, in the grid and in `/api/v1/status`; it counts as an error folder, so **H4** opens for that folder ("folder error: /rest/db/status: HTTP 500 …") instead of a false **H1** for the server. A completion failure shows the error with the device on the folder (the grid cell of that device stays "unknown"). The error clears at the next full check that succeeds, or at the next `FolderSummary` event for the folder.

**README.** "Checks" (H1 vs H4 wording) and the "How data is collected" paragraph: a per-folder HTTP error marks the folder, not the server; only a bad API key (401/403) or an unreachable server marks the server.

DECISION: the error text goes into `Folder.Error` (so the folder counts as an error and H4 fires, urgent with the 5 min debounce) rather than a new field or a silent "unknown". A lasting per-folder 5xx is a real problem that deserves a finding; transient ones are rare because the client already retries 5xx three times.

## 5. Error texts like ": EOF" (hunch 18)

**Root cause.** `stclient.cleanErr` strips the "Get <url>" operation from transport errors, leaving ": EOF", ": net/http: TLS handshake timeout" or raw DNS errors; `shortErr` only mapped eight substrings and returned the matched substring itself (e.g. "i/o timeout", "No connection could be made").

**What changed.** `collector.go` `shortErr`: a table `netErrs` maps the Linux and Windows wordings of the common network errors to short phrases ("connection refused", "no route to host", "network unreachable", "timed out", "connection reset", "connection closed early", "host name not found", "host name lookup failed"); anything else has a leading ": " trimmed and is cut at 200 bytes. HTTP errors are shown as before. Tests: `TestShortErr`, `TestLastErrorIsReadable`.

**User-visible.** LastError on the tile, in `/api/v1/status` and in H1 messages reads "connection closed early" instead of ": EOF", "timed out" instead of "i/o timeout" or ": net/http: TLS handshake timeout", "connection refused" instead of "No connection could be made". Fixer a's change to the source (stclient) is independent; this is robust either way.

**README.** Nothing (no error texts are documented).

## 6. A ServerKey read error stops monitoring without a trace (hunch 14)

**Root cause.** `Manager.Sync` logged "reading API key" and `continue`d without touching the hub, so the server stayed `unknown` (or kept stale `up` data) forever: no H1, no message. Triggers: a changed `SYNCWATCH_SECRET_KEY`, a lost `secret.key`, a database error during a settings save. The stop loop also treated a read error as "key changed" (key `""`) and stopped a healthy collector.

**What changed.** `manager.go`: on a read error the server is set `down` with `LastError = "API key can't be read (secret key changed?)"` (DownSince stays zero like the other manager-set down states) and `m.keyErr` is set; `Run` now has a ticker (`keyRetry` = 5 min, a variable for tests) that calls `Sync` while `keyErr` is set, so monitoring resumes as soon as the key can be read (the secret key restored, the key entered again). The stop loop only restarts a running collector when the key is readable *and* differs; a key that merely can't be read leaves the running collector alone (it keeps the key it started with). Test: `TestUnreadableAPIKeyMarksServerDownAndRetries` (file-backed store opened under two secret keys).

**User-visible.** H1 "AKKA can't be reached: API key can't be read (secret key changed?)" within a second of the start or save; the tile shows the text; an ERROR log line "reading API key; trying again in 5m0s" per server per retry. Entering the key again in Settings (or restoring the secret key and restarting) clears it.

**README.** "Install" (the sentence about losing `secret.key`) and "Security → Secrets": the dashboard now says so (H1 with this message) instead of silently not polling.

DECISION: the periodic Sync only runs while a key is unreadable (not unconditionally every 5 min), so nothing changes for healthy deployments.

## 7. Traffic totals are skewed after downtime and restarts (hunch 17)

**Root cause.** `RecordTraffic` booked the whole increase since the previous reading at the current time, however old that reading was, so after syncwatch downtime or an unreachable period `moved_24h_gb` included older days. One counter going down made both counters count in full.

**What changed.** `store/traffic.go`: `RecordTraffic` takes a `maxGap`; a reading more than `maxGap` after the previous one records no delta and re-baselines (`TrafficSample.Gap = true`, `Over = 0`); each counter is judged on its own (a counter below its previous value counts in full, the other one as a difference; a changed `startTime` still resets both). `collector/traffic.go` passes `3 × trafficEvery` (3 min) and logs a DEBUG line when a gap was re-baselined. Tests: `store.TestTraffic` (extended), `TestTrafficGapIsNotBooked`.

**User-visible.** `/api/v1/ha` day and week sums no longer jump after syncwatch was down or a server was unreachable for more than 3 minutes; the bytes moved meanwhile are not counted at all (they can't be placed in time). The rate is not updated by a gap reading.

**README.** "Home Assistant" (the paragraph on where the traffic comes from): add that periods without readings (syncwatch down, server unreachable for more than 3 minutes) are not counted.

Not fixed (out of the item's scope, noted for completeness): after a Syncthing restart the hub's `StartTime` lags until the next full check, so bytes moved in the first minute after a restart can be counted twice (once by the counter-went-down rule, once by the startTime rule). Small and bounded to one sample.

## 8. Refreshes inside the event loop are serial, unmerged and partly silent (hunch 20)

**Root cause.** `apply` ran `refreshConfig` for every `ConfigSaved` and `refreshPending` for every `Pending*Changed` event, one by one, while no long-poll was open; `refreshPending` dropped errors silently, so H9 could stay stale until the next full check.

**What changed.** `events.go`: the two cases set `c.configDue` / `c.pendingDue`; `refreshPending` returns its error. `collector.go`: `applyBatch` applies a long-poll's events and then runs each due re-read once; it is used by the event loop and the session's resume path. A failing config re-read is handled as before (WARN + `FullCheckNow`). A failing pending re-read is logged at WARN once per session ("re-reading pending devices and folders"); later failures in the same session are silent, and the next full check reads the lists anyway. Test: `TestEventBatchRefreshesOnce`.

**User-visible.** Five `ConfigSaved` in one batch cost one config re-read; a failing pending re-read shows up in the log.

**README.** Nothing.

## 9. H10 misses conflicts across a Syncthing restart and lags up to full_check_interval (hunches 10 and 19)

**Root cause.** `/metrics` was only read during full checks (every 4 h in events mode). The store already counts the whole value as new when `startTime` changes, but nothing read the counters more often, and a restart that broke no request (between two requests, or during a refresh) was never detected: `fullCheck` didn't compare `startTime`, so `lastEventID` stayed high and Syncthing returned no events until its new IDs passed it (hunch 19's hidden stall).

**What changed.** `collector.go`: `readConflicts` (read, record, update `Server.Conflicts`) is called by every full check and at every session start on the two resume paths that skip the full check (events resume, polled resume); the startTime passed is the one just read, so a reset counter counts in full. `fullCheck` now compares `status.StartTime` with the remembered one: on a change it logs "Syncthing restarted; re-reading everything", remembers the new start and resets `lastEventID` to 0, so the event loop follows the restarted IDs from the beginning (the full check that just ran covers the state; buffered events are replayed on top). Test: `TestConflictsAcrossReconnectsAndRestarts`.

**User-visible.** H10 appears at the next reconnect or full check rather than only at the next full check; after a Syncthing restart the conflicts since the restart are counted (and events are not lost until the 4 h re-check).

**README.** "Checks" H10 / "How data is collected": `/metrics` is read at every full re-check and reconnect.

Not fixed (inherent): conflicts counted by Syncthing between the last reading and a restart are lost with the counter. Reading `/metrics` on a timer (e.g. with every structure scan) would narrow that window; not done, to keep the request pattern as documented.

## 10. Structure scan context tied to the session

**Root cause.** `maybeScan` started the scan with Run's context, so a scan kept listing directories (each request with its own timeout and retries) after the session had failed and the server was marked down.

**What changed.** `collector.go` `Run`: each session gets its own context (`context.WithCancel(ctx)`), cancelled when the session returns; `maybeScan(ctx)` inherits it unchanged, so the scan stops with the session. `structure.go` is untouched. Test: `TestScanStopsWhenSessionEnds` (hung listings are cancelled within 3 s of the session ending).

**User-visible.** No listings continue against a server that is down. Note for fixer s / the integrator: a scan cancelled this way returns `context.Canceled`, which `maybeScan` logs as WARN "structure scan failed err=context canceled" (one line per outage that interrupts a scan). Lowering that to DEBUG when `ctx.Err() != nil` is a one-line change in `structure.go`'s `maybeScan`; not done here to stay out of that file.

**README.** Nothing.

---

## Not done

Nothing from the assignment is left undone. Residual limitations are listed under items 3, 7 and 9.

## Test and build status

`gofmt -l .` clean, `go vet ./...` clean, `GOOS=linux go build ./...` clean, `go test -count=1 ./...` passes on Windows (collector package 78 s). `-race` not available on this host (CGO off); the integrator runs it on Linux.
