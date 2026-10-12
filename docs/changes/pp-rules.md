# pp-rules: rules, engine and model (pre-publication review)

Branch `sim/pp-rules`, worktree `the test kit`, base `review/sim-2026-10` at `7f825f1`. This stage
corrected the 4 findings of the area "rules" that the verifiers reproduced: text-4, data-4, data-5 and data-1. Each finding
has 1 commit, and this file has 1 commit.

| commit | finding | files |
|---|---|---|
| `d3ee581` | text-4: hidden characters in "Needed files" and "Failed items" | `internal/rules/progress.go`, `internal/rules/health.go` |
| `30aa37d` | data-4: the H1 row "What AKKA sees" from old polled data | `internal/rules/health.go` |
| `cc49c00` | data-5: H12 for a paused relay device, and absence findings at a start | `internal/rules/health.go`, `internal/engine/engine.go`, `internal/web/r2_int_test.go` (see "Requests to the integrator") |
| `4d2fd57` | data-1: S1 for a folder inside a share root while its S2 is hidden or not urgent | `internal/rules/structure.go`, `internal/rules/rules.go`, `internal/model/finding.go`, `internal/engine/engine.go` |

The new tests are in `internal/rules/pp_rules_test.go` and `internal/engine/pp_engine_test.go`. Each test fails on
`7f825f1` and is successful with the change. The outputs are in `the test kit`
(`*-test-before-fix.txt`, `*-test-after-fix.txt`).

The checks before each commit are in `precommit-*.txt`: `gofmt -l .` (empty), `go vet ./...`,
`go vet -tags integration,compose,loadsim ./test/...`, `GOOS=windows` and `GOOS=linux CGO_ENABLED=0 go build ./...`,
`go test -count=1 ./...`. All are successful, with 1 exception: `TestUndecryptableNoticesWebhook` in `internal/notify`.
That test also fails on `7f825f1` without a change (`the test kit`). It is not in the
area "rules". The integrator runs the integration, compose and loadsim suites.

Live checks: kit slot s25 with 2 builds, `sw-kit/syncwatch:pp-rules-base` (`7f825f1`, before) and
`sw-kit/syncwatch:pp-rules` (`4d2fd57`, after). Also a native run with real Syncthing 2.1.5 on 127.0.0.1:24502-24506. The
evidence is in `the test kit` and `data-4-native\`. The slot is removed.

---

## text-4: "Needed files" and "Failed items" hide white space and control characters

**What went wrong.** H5 lists the names of the files that a PC still needs (row `Needed files`). H4 lists the failed items
as "path: error". The rules wrote the names as Syncthing reports them. HTML joins white space, and it removes white space
at the start and the end of a line. Thus "delivery/from-mac/brief " (a space at the end) showed as
"delivery/from-mac/brief". "LIVE-Y\r\nline2.txt" showed as "LIVE-Y line2.txt", and "tab\there.txt" showed as
"tab here.txt". Windows frequently refuses a name because of these characters. The administrator did not see them.

**What changed.** `quoteName` (`progress.go:212`) writes a name as a quoted Go string (`strconv.Quote`) in 2 cases. The
name has white space at the start or at the end, or it has a control character. `neededFiles` (`progress.go:192`) and the
H4 lines (`health.go:328`) use it. Other names do not change. The web package marks direction characters itself
(`showControls`). In a quoted name, `strconv.Quote` writes them as `\u202e`.

**User-visible change.** The row `Needed files` of H5 and the failed items of H4 show such a name in quotes:
`"delivery/from-mac/brief "`, `"delivery/LIVE-Y\r\nline2.txt"`. The same applies to `details` and `paths` in
`/api/v1/status`. Discord does not show these rows.

**Test.** `TestFileNamesShowHiddenCharacters`, `TestQuoteName` (`internal/rules/pp_rules_test.go`).

**Live check.** I ran the test of the verifier (`rvv-text-4-r\zz_rvv_text4_test.go.txt`) on the corrected tree, with
headless Edge 155, the real `app.css` and the CSP. The result is successful. Each name that Windows refuses shows different
from its look-alike. An example is `"delivery/from-mac/brief "` next to `delivery/from-mac/brief`
(`text-4-edge\go-test-output.txt`, `edge-findings.png`).

**Line for docs/CHANGES-review.md** (Findings):

- Some file names have a space at the start or the end, or a control character. H5 "Needed files" and the failed items of H4 show these names in quotes, for example `"brief "`. Before, the dashboard did not show these characters.

**Line for README.md** (field reference, after the table row of H5):

- A file name with a space at the start or the end, or with a control character, shows in quotes. An example is `"brief "`.

---

## data-4: in polled mode, H1 shows that Syncthing on SOL runs while SOL is off

**What went wrong.** The H1 row `What AKKA sees` uses the connections of another server (AKKA) to the server that syncwatch
cannot reach (SOL). In polled mode, syncwatch reads these connections only at a full check (every 15 minutes).
`peerViews` checked only that the last contact with SOL was 10 minutes (`deadLinkTime`) old. It did not check the age of
the data of AKKA. An example: 11:50 last full check of SOL, 11:55 full check of AKKA (connected to SOL), 11:56 SOL is off,
12:05 H1 opens. Until the next full check of AKKA, the row showed "AKKA is connected to SOL. Thus Syncthing on SOL runs,
and the network path from syncwatch to SOL is down."

**What changed.** `health.go:156` adds a condition for a polled server. `peerViews` uses its connections only when its last
full check came 10 minutes (`deadLinkTime`) or more after the first failed request to SOL. The collector sets `DownSince`
to the time of that request. syncwatch sends no request between 2 full checks. Thus SOL can be off from any time before
the first failed request. Syncthing closes a connection without a network path in approximately 5 minutes. Thus a
connection that AKKA reports 10 minutes after that time shows that Syncthing on SOL runs. Events mode does not change
(live connections).

The finding proposed `LastFullCheck - LastContact >= deadLinkTime`. That condition is not sufficient. An example: the check
of AKKA is at 12:01 (11 minutes after the last contact), and SOL is off at 12:03. Then the row is still incorrect (case
"data of AKKA from 12:01" in the test).

**User-visible change.** In polled mode, the row `What AKKA sees` shows later. It shows after the first full check of AKKA
that comes 10 minutes or more after the first failed request to SOL. With `poll_interval` 15 minutes, that is 10 to 25
minutes after the first failure. Before, the row sometimes showed immediately, from data that was older than the failure.

**Test.** `TestPolledPeerViewNeedsNewerData` (`internal/rules/pp_rules_test.go`) has 7 cases. 2 cases are controls: polled
data 20 minutes after the failure, and events mode. 4 cases fail on `7f825f1`.

**Live check.** I ran the native test of the verifier (`harness_zz_rvv_data4_run_test.go.txt`, ports changed to
24502-24506). It used polled mode with the default timings and the binary of `30aa37d`. The result is successful. SOL was
off at 12:53:02, H1 opened at 13:03:22, and the next full check of AKKA came at 13:07:14. The log has "SUMMARY: the row
"What AKKA sees" never appeared" (`data-4-native\go-test-output.txt`). On `7f825f1` the verifier saw the row for 3 minutes
45 seconds.

**Line for docs/CHANGES-review.md** (Findings):

- In polled mode, the H1 row "What AKKA sees" uses only data from 10 minutes or more after the first failed request to SOL. Before, the row sometimes showed "Syncthing on SOL runs" while SOL was off.

**Line for README.md** (field reference, H1 row). Add after `What <server> sees (the connection of another server to the
unreachable server)`:

- In polled mode, only from a full check of the other server 10 minutes or more after the first failed request.

---

## data-5: H12 for a paused relay device, and absence findings for some seconds after each start

**What went wrong.**

1. An administrator can pause a device with `devices[].relay: true` on each server. Then the device has no connection.
   After `relay_lost`, H12 opened with the causes "The relay is off or unreachable, or the device is off." H2 skips a
   paused pair for this reason, and H6 reports the pause after `thresholds.paused`.
2. H7, C2 and H12 of a device are the absence checks. They waited only for the servers that have the device in their
   device list (`SeenBy`). A server has no device list before its first full check after a start. In one case, AKKA
   finished first, and a device had a connection only to SOL. Then the device was absent for some seconds, and an H7 or
   H12 opened and resolved. Each start or update gave a history entry, a WARNING in the counts and lines in the weekly
   report. While SOL was down from the start, the finding stayed open and not stale.

**What changed.**

1. `relayDevices` (`health.go:625`) skips a device when each server with the device in its configuration pauses it
   (`pausedOnAll`, `health.go:807`).
2. `newFreezer` (`engine.go:181`) records a server that did not finish a full check after the start. The engine then keeps
   the absence findings as they are (`engine.go:307`). An open finding stays open and becomes stale, and no new finding
   opens. The same rule applied before to a server in `SeenBy` that is down.

The finding proposed to wait only for a server that is up and did not finish a full check. This change also waits for a
server that is down from the start. Its device list is also not known (case C of the verifier: H12 open for 1 hour next to
H1). A server that is disabled in syncwatch is not in the snapshot and does not stop the checks.

**User-visible change.**

1. A device with "Always connects through a relay" that each server pauses gets no H12. H6 reports the pause.
2. After a start of syncwatch, H7, C2 and H12 for a device wait until each server finished its first full check. While
   syncwatch cannot reach a server from the start, no new H7, C2 or H12 for a device opens. The open findings are stale.

**Test.** `TestRelayDevicePausedOnEveryServer` (`internal/rules/pp_rules_test.go`), `TestPausedRelayDeviceGetsNoH12`,
`TestAbsenceWaitsForEveryServerAtStart` (`internal/engine/pp_engine_test.go`).

**Live check** (s25, after an import of `devices: [{id: <LAPTOP-DANI>, relay: true}]` and `relay_lost: 2m`):

| step | before (`7f825f1`) | after (`4d2fd57`) |
|---|---|---|
| pause LAPTOP-DANI on AKKA and SOL, 3 minutes | H12 "Device LAPTOP-DANI has no connection for more than 2 minutes (last seen 2026-10-12 12:56). It connects only through a relay", `Possible causes` "The relay is off or unreachable, or the device is off. …"; H6 on AKKA and SOL (`24-…`) | no H12; H6 on AKKA and SOL (`54-…`) |
| `wan s25 down --only-sync --reject` (WS-CARL and EDIT-02 connected only to SOL), 3.5 minutes, `wan s25 set --latency 400ms`, restart of syncwatch | 10 seconds with AKKA up and SOL not loaded: H7 "PC EDIT-02 is offline for 3 minutes" and H7 WS-CARL, not stale; 2 history entries 11:03:42-11:03:53 UTC (`33-…`, `34-…`) | 2 starts (the change of build, then a restart), each with 10 seconds of AKKA up and SOL not loaded: no H7, no new history entry (`40-…`, `44-…`, `45-…`) |

H7 "PC LAPTOP-DANI is offline for 3 minutes" opened in both builds while LAPTOP-DANI was paused. The kit has `pc_offline`
3 minutes. That H7 is not part of this finding (see "Not changed").

**Lines for docs/CHANGES-review.md** (Findings):

- A device with "Always connects through a relay" that each server pauses gets no H12. H6 reports the pause.
- After a start of syncwatch, H7, C2 and H12 for a device wait until each server finished its first full check. Before, a device that was connected only to the slower server got an H7 or H12 for some seconds at each start.

**Lines for README.md:**

- Check table, H12, add: "A device that each server pauses gets H6, not H12."
- "Stale findings", add a new item below the item about H12 for a device: "H7, C2 and H12 for a device, after a start until each server finished its first full check. Before that, syncwatch does not know the devices of the server."

---

## data-1: an ignored, snoozed or not-urgent S2 hides a Syncthing folder with a double sync (W-046)

**What went wrong.** With W-046, S2 (share root is a Syncthing folder) lists the Syncthing folders inside the share root,
and these folders get no S1. That is correct only while a person sees S2 with the urgency of S1:

- S2 ignored or snoozed: a folder inside had no finding that a person sees. A new folder inside changed only the count
  of the hidden S2. syncwatch did not announce a snoozed S2 again.
- S2 not urgent, S1 urgent: no urgent message for a folder inside.
- An update from `78ef2d8` with S2 ignored: the open S1 resolved with an incorrect "Resolved" message.

DECISION W-046-1 keeps S1 when S2 is disabled, because "else no finding would report them". The same reason applies to a
hidden S2.

**What changed.**

- `rules.Input.Hidden` (`rules.go:24`) has the IDs of the findings that are snoozed or ignored now. `Engine.Evaluate` fills
  it from the active snoozes (`engine.go:404`).
- `s2Covers` (`structure.go:55`) decides if S2 lists the folders inside instead of their S1. S2 does this only while S2 is
  not snoozed or ignored, and is urgent when S1 is urgent. Else each folder inside gets its S1 (`structure.go:93`). The S1
  has the ID from before W-046. S2 keeps its rows.
- `model.Finding.Covers` (`finding.go:105`, not stored): the S2 finding names the S1 findings that it reports
  (`structure.go:153`). The engine closes such an open S1 without a resolved message (`engine.go:537`), as for a server
  that an administrator removed. The history entry gets the note "(listed in S2 {share} on {server})". The log has
  "finding closed" with `reason="listed in S2 …"`.

The finding proposed to add "S1" to the checks of S2 for the urgency. With that change, a new folder inside a share root
gets no message, because syncwatch announced the S2 before. The test of the verifier shows it: the mode "S2 not urgent"
fails with that change (`rvv-data-1-r\trial-fix-B.txt`) and is successful with this change.

**User-visible change.**

- S2 can be ignored or snoozed, or not urgent while S1 is urgent. Then each Syncthing folder inside the share root has an
  S1 finding and an urgent message, as before W-046.
- When S2 lists the folders again, their S1 findings close without a resolved message. Examples: the snooze ends, an
  administrator removes the ignore, or S2 is urgent again.
- An update with an ignored S2 keeps the open S1. syncwatch sends no incorrect "Resolved" message.
- No change: a person adds a folder inside a share root while you see S2, and S2 is urgent and announced. Then the message
  of the S2 changes ("It contains 2 other Syncthing folders"), but syncwatch sends no new urgent message (W-046).

**Test.** `TestShareRootHiddenGivesS1` (`internal/engine/pp_engine_test.go`) has 3 modes: ignore, snooze for 7 days, S2
not urgent. Each mode adds PROJ-B inside the share root, and then S2 lists the folders again. The test fails on `7f825f1`
("no S1"). With only the rules part of the change, it fails with "a resolved message is due for 2 findings"
(`data-1-test-rules-only.txt`).

I also ran the test of the verifier on this change (`notify_zz_rvv_data1_test.go.txt`, ports 24510-24512). It uses the real
notifier and a fake webhook.

- Scenario B: the 4 modes are successful (`data-1-notify-B-after-fix.txt`). With ignore and with snooze, Discord gets
  "<@&4242> 🚨 Syncthing folder inside another: PROJ-B inside AKKA-LIVE-4FINALS on AKKA". When the snooze ends, Discord
  gets no resolved message, and the daily reminder lists S2.
- Scenario A, the update: step 1 on `78ef2d8`, step 2 on this change, with the same database. The result is successful,
  with "Discord 'resolved' messages for it: 0" (`data-1-notify-A-update-78ef2d8-to-fix.txt`).

**Live check** (s25, `envctl inject s25 s2 --existing`, then S2 ignored in the dashboard):

| step | before (`7f825f1`) | after (`4d2fd57`) |
|---|---|---|
| S2 ignored | 0 findings that name LIVE-FJORD-4FINALS and are not snoozed (`15-…`) | S1 "Syncthing folder LIVE-FJORD-4FINALS is inside Syncthing folder AKKA-LIVE-4FINALS on AKKA. Syncthing syncs the files 2 times", urgent; Discord #7 "🚨 Syncthing folder inside another: LIVE-FJORD-4FINALS inside AKKA-LIVE-4FINALS on AKKA" (`40-…`, `41-…`) |
| ignore removed (13:07:29) | — | S1 closed at 13:07:48, history "… (listed in S2 AKKA-LIVE-4FINALS on AKKA)", `resolved_notified_at` = `resolved_at`; no Discord message for it after #7; S2 shown (`43-…`, `61-discord-all.txt`) |

**Lines for docs/CHANGES-review.md** (Findings, below the line about W-046):

- While S2 is ignored or snoozed, or is not urgent while S1 is urgent, each Syncthing folder inside the share root gets its S1 again. Thus you always see a finding for a folder whose files Syncthing syncs 2 times, with the urgency of S1.
- When S2 lists these folders again, their S1 findings close without a resolved message.

**Lines for README.md** (check table, S2). Replace "They get no S1." with:

- They get no S1 while you see S2 with the urgency of S1. When you ignore or snooze S2, each folder inside gets its S1.

Also in `docs/CHANGES-review.md`, item 26 of "Integrations" ("These folders get no S1."): add "while S2 is not snoozed or
ignored".

---

## Requests to the integrator

1. `internal/web/r2_int_test.go` (`TestSettingsKeysReachTheChecks`, commit `cc49c00`) has 1 new line before the relay part.
   It gives SOL the status up and loaded. Before, SOL did not finish a full check in that test, and now H12 for LAPTOP-DANI
   waits for SOL (data-5). The test is about the settings keys, not about the start.
2. Run the integration suite after the merge. The change of data-5 moves the time when the absence checks open after a
   start. The change of data-1 changes S1 for a share root.

## Not changed

- H7 for a device that each server pauses: in the kit (`pc_offline` 3 minutes), H7 opened next to H6 in both builds. With
  the default values, `paused` (3 days) comes before `pc_offline` (7 days). This is not part of data-5.
- S2 sends no new message for a new folder inside the share root while you see S2 and syncwatch announced it (W-046,
  see data-1).

## Language

New text for the user: the note "(listed in S2 {check subject})" in the history, the log attribute
`reason="listed in S2 …"` of "finding closed", and the lines above. The checker result is in
`the test kit` and `ste-report.md`: 13 of 13 entries have no counted violation.
This file has 88 percent (`ste-doc-report.md`). The other entries are the heading "What went wrong", a quote of DECISION
W-046-1 and false positives of the checker.
