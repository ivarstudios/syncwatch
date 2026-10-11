# r2-repair-2 — Repair round 2 of wave 3: the gap text of W-038, the speed and the time left of W-059

Branch `sim/r2-repair-2`, worktree `the test kit`, base `review/sim-2026-10` at `04c1023`.
The second live check of wave 3 on kit slot s19 found 2 minor defects (`the test kit`, P1
and P2). This branch repairs them. The checks and the live check of the repair are in `the test kit`.

## 1. W-038: the resolved line after a stop of syncwatch (P1)

**Defect.** H4 "LIVE-AURORA-1SOURCE on AKKA" opened at 23:23. syncwatch stopped at 23:38 and started at 23:41. After the
start, syncwatch saw the folder error again, and the finding resolved at 23:42:34. The resolved line was `(the fault started
at 23:23 and was resolved while syncwatch did not run, 23:38–23:41)`. That end is not correct.

**Cause.** `lasted()` used the gap text for each finding that resolved less than 5 minutes after the end of a gap. The engine
set `LastSeen` to the time of the resolution. Thus no value told the 2 cases apart.

**What changed.**

- `internal/engine/engine.go`: at the resolution, `LastSeen` stays the time of the last evaluation that saw the finding open.
  No other code reads `last_seen` of a resolved finding.
- `internal/notify/lines.go`: `lasted()` uses the gap text only when `LastSeen` is not after the end of the gap.

**User-visible.** A fault that syncwatch saw open after its start gets its duration: `(the fault lasted 19 minutes,
23:23–23:42)`. A fault that resolved at the first evaluation after the start keeps the gap text: `(the fault started at
23:23 and was resolved while syncwatch did not run, 23:38–23:41)`.

**Tests.** `TestResolvedFindingKeepsLastSeen` (engine), `TestResolvedLineOfAFaultSeenAfterTheGap` and
`TestResolvedLinesAfterAStop` (notify, with a new engine and a new notifier on the same database). The 3 tests fail on
`04c1023`.

**Note.** An older version sets `LastSeen` to the time of the resolution. A finding that an older version resolved, and that
waits for its resolved message at the update, gets its duration and not the gap text.

## 2. W-059: the speed and the time left of H5 (P2)

**Defects.**

- (a) The Windows PC PC-K2 received the folder (0.46 MB) in some seconds after it connected. Then it stopped with 3 files
  (57 bytes) that it cannot store. For 5 minutes H5 showed `less than 1 minute remaining` and `Speed 0.00075 MB each second`.
- (b) A 20 MB file came to SOL and arrived at 16 KiB each second. When H5 opened, it showed `0 (no data arrived in the last
  10 minutes)` and no time left. But 11 MB arrived before.

**Causes.**

- (a) `AddProgress` kept at most 1 sample each minute. It discarded the completion events of the first minute, with the end
  of the first sync. For 5 minutes more, the speed used the sample from before that sync. Also, a time left for some bytes
  is not an end when the need does not decrease.
- (b) The sample at least 10 minutes old was from before the file (need 0). The need increased, thus the speed found no
  decrease.

**What changed.**

- `internal/model/model.go`, `AddProgress`: the newest sample always has the newest need. A sample less than 1 minute after
  the sample before the newest one replaces the newest one. A sample with the need of the newest one does not replace it.
  Thus the newest sample keeps the time when its need began. A minute has at most 2 samples, and 120 samples are at least
  1 hour.
- `internal/rules/progress.go`, `syncSpeed`: the bytes that arrived are the decreases of the need in the 10 minutes before
  the newest point. A decrease counts at the time of the point that shows it. A need that increases (new files) does not
  hide the decreases. When the folder needed no data at the start of the 10 minutes, the time counts from the first point
  with a need.
- `internal/rules/progress.go`, `progressOf`: no time left when the need did not decrease for longer than the time left.

**DECISION repair-2-1:** the speed adds the decreases and ignores the increases. The report of the live check proposed to
measure from the sample after the last increase. With new files every few minutes (a render job that writes into a Syncthing
folder), that method gives a short measurement or no speed. The sum of the decreases keeps the 10 minutes of the row text.

**DECISION repair-2-2:** no time left when the need did not decrease for longer than the time left. The report of the live
check proposed no time left when the remaining items did not decrease. A sync of 1 large file keeps 1 remaining item until
the end. That rule removes the correct time left of such a sync.

**User-visible.**

- (a) The H5 of PC-K2 has no Time left. Its message is `Device PC-K2 is stuck at 99.9% of folder LIVE-FJORD-4FINALS for more
  than 24 hours (3 remaining items, seen from AKKA)`. The Speed row is `0 (no data arrived in the last 10 minutes)` when the
  files arrived more than 10 minutes before.
- (b) The H5 of the folder has the Speed `0.016 MB each second`. Its message is `Folder LIVE-FJORD-1SOURCE on SOL did not
  finish the sync in 24 hours (syncing, 1 remaining item, 10 of 22 MB received, approximately 12 minutes remaining)`.

**Tests.** `TestStuckPCGetsNoTimeLeft` and `TestStuckSyncCountsDataAfterTheNeedRose` (rules), `TestAddProgress` (model),
`TestProgressSamples` (collector). The 4 tests fail on `b2d55f8`. `TestAddProgress` and `TestProgressSamples` had the old
rule for the samples.

**Note.** `docs/changes/r2-2.md` section 5 and `docs/changes/r2-3.md` describe the rules before this branch. The speed was
the decrease between the newest point and the newest sample at least 10 minutes older, with at most 1 sample each minute.

## 3. Checks

Each commit has these results on the tree of the commit:

- `gofmt -l .` shows no file.
- `go vet ./...` and `go vet -tags integration,compose,loadsim ./...` show no error.
- The builds with `GOOS=windows` and `GOOS=linux` and `CGO_ENABLED=0` show no error.
- All tests of `go test -count=1 ./...` are successful.

The logs are `the test kit`, `gates-p2.log` and `gates-docs.log`.
