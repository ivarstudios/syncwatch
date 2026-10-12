# pp-app — Pre-release review: the corrections of the area "app"

Branch `sim/pp-app`, worktree `the test kit`, base `review/sim-2026-10` at `7f825f1`. The pre-release
review found 3 defects in `internal/app`, `cmd/syncwatch` and `internal/store`. This branch has a commit for each
finding. The evidence is in `the test kit`.

| Key | Commit | Files |
|---|---|---|
| exposure-2 | `30d8f08` | `internal/app/app.go`, `internal/app/alive_test.go` |
| exposure-4 | `1b589c9` | `internal/app/alive.go`, `internal/app/app.go`, `internal/store/gaps.go`, tests |
| import-2 | `8666b73` | `internal/app/app.go`, `internal/app/app_test.go`, `cmd/syncwatch/main_test.go` |

## 1. exposure-2: a restart loop cut the time when syncwatch did not run

**Defect.** AKKA restarts at 21:00 for a firmware update, and the file of `SYNCWATCH_TLS_CERT` is not at its path. Each
start of the container stops with "cannot read the TLS certificate and key files". Docker (`restart: unless-stopped`)
starts the container again, at most 1 minute after the last start. The administrator corrects the path at 00:05. The
next message gave "syncwatch did not run from 21:00 to 21:04." But syncwatch did not monitor the servers until 00:05.
When the loop began less than 2 minutes after the stop, the message had no such line.

**Cause.** `Run` recorded the gap (`noteGap`) and wrote the heartbeat (`app.alive_at`) before the points where a start
can fail: `SYNCWATCH_ADMIN_PASSWORD`, `engine.New`, the TLS files and `net.Listen`. Each failed start moved the
heartbeat. Thus the start that works found the heartbeat of the last failed start, less than 2 minutes before, and
recorded no gap. Only the first failed start recorded a gap, and that gap ends at the first failed start.

**What changed.** `internal/app/app.go`: `noteGap` and the first heartbeat come after the listener, immediately before
the workers start. A start that fails before this point does not change the heartbeat. A configuration error stops the
start before these steps, as before.

**User-visible.** After a restart loop, the log line `syncwatch did not run` and the next message give the full time.
The time starts at the last heartbeat of the syncwatch that ran and ends at the start that works.

**Tests.** `TestFailedStartKeepsTheHeartbeat` (`internal/app/alive_test.go`) has 3 failed starts and then a start that
works. The failed starts have a TLS file that is missing, an address in use and a short `SYNCWATCH_ADMIN_PASSWORD`. The
test fails on `7f825f1`.

**Live check.**

- Docker, kit slot s27 (`fix-app/exposure-2/kit-s27-pp-app/` and `kit-s27-7f825f1/`): SOL is off and H1 is open. The
  script stops the syncwatch container and replaces `/data/tls-cert.pem` with a directory. Docker (`restart:
  unless-stopped`) starts the container 12 times in 4 minutes. Then the script puts the file back. With this branch, the
  log has `syncwatch did not run from=12:59:54 to=13:04:00`. The resolved message gives "syncwatch did not run from
  12:59 to 13:04." With `7f825f1`, the log has no such line. The resolved message gives "the fault lasted 5 minutes,
  13:05–13:10" without the time when syncwatch did not run.
- Native processes (`fix-app/exposure-2/native-run.txt`): a driver starts each binary again and again, as Docker does.
  The 3 cases of `7f825f1` fail (a TLS file that is missing after 2 minutes 30 seconds or after 5 seconds, an address
  in use). The 3 cases of this branch give the full time. The driver uses only 127.0.0.1:24550-24554, with a fake
  Discord.

## 2. exposure-4: a clock that was behind at the start hid the time when syncwatch did not run

**Defect.** A host without a battery clock (for example a Raspberry Pi with the arm64 image) starts with the time of its
last shutdown. Docker starts syncwatch before NTP sets the clock. The host was off from Friday to Monday. At the start,
`noteGap` compared the last heartbeat (Friday) with the incorrect clock (Friday plus 40 seconds) and recorded no gap. The
first heartbeat wrote the incorrect time. After NTP, the heartbeat wrote the correct time. No message gave the time
from Friday to Monday. The NAS of IVAR have a battery clock.

**Cause.** syncwatch compares wall-clock times only at the start (`internal/app/alive.go`, `noteGap`). No later step
records a gap.

**What changed.**

- `internal/app/alive.go`: `noteGap` returns the last heartbeat. A new `clockCheck` runs in the heartbeat goroutine
  every second, in the first 10 minutes after the start. It compares the clock with the monotonic clock of the process.
  The clock can move forward by more than 2 minutes more than the measured time. Then the start was later than the
  clock showed, and `clockCheck` records the gap to the corrected time of the start. It logs the INFO line
  `syncwatch did not run` with the attribute `clock_behind`, 1 time. The check runs every second because syncwatch
  sends the messages that are due some seconds after the step of the clock.
- `internal/app/app.go`: `Run` gives the last heartbeat and the start to the heartbeat.
- `internal/store/gaps.go`, `AddGap`: a gap from the same heartbeat (`From`) replaces the gap that syncwatch recorded
  before. No message named the new gap. Thus a gap with an end that is too early gets its correct end, and the next
  message names the corrected gap.

**User-visible.** On a host with a clock that is behind at the start, the log has the line `syncwatch did not run ...
clock_behind=71h59m20s` after NTP sets the clock. The next message gives the full time, for example "syncwatch did not
run from 2026-10-09 10:52 to 10:52." (the end is on the day of the message).

**Tests.** `TestGapAfterBootClock` (`internal/app/alive_test.go`) has 8 cases. The first 2 cases are a clock that is
behind and a gap of the start that ends too early. The next 2 cases are a power loss and a quick start, with the clock
before the last heartbeat. The other cases are a correct clock, a small step of the clock, a step after 10 minutes and
the first start.
`TestGapFromTheSameHeartbeatReplaces` (`internal/store/gaps_test.go`) fails on `30d8f08`.

**Live check.** `fix-app/exposure-4/` has the binary with a fake clock (an overlay of package `time`). The clock of the
PC does not change. A fake Discord is on 127.0.0.1:24540-24543. The binary of `7f825f1` records 0 gaps. The binary of
this branch records the gap from Friday to Monday, and the repeat message names it.

**Limits.** The check stops after 10 minutes. A later step of the clock, or a clock that is ahead at the start, does
not change the gap. The monotonic clock of Linux stops while the host sleeps. A host that sleeps in the first 10 minutes
gets a gap with the correct length. But the gap starts at the last heartbeat before the start. A NAS or a server does
not sleep.

## 3. import-2: `syncwatch import-config` blocked each person from the dashboard

**Defect.** syncwatch runs in proxy mode, without an administrator password (the setup behind a reverse proxy or a
VPN). An administrator imports a file without an `access:` section with `syncwatch import-config`. Each file in
`examples/` has no `access:` section. A file without `access:` has `access.mode: builtin`. The command gave "syncwatch
imported 1 server" with exit 0. In less than 10 seconds, each page of the dashboard went to `/setup`. The setup page
asks for the setup token from the log. A start in proxy mode does not log a setup token, and a reload does not log one.
Thus no person had the setup token. The dashboard refuses the same file (`internal/web/settings.go`, `locksEveryoneOut`).

**What changed.** `internal/app/app.go`, `ImportFile`: the stored configuration can be in proxy mode without an
administrator password. Then `ImportFile` refuses a file that sets built-in login to on, before it changes data. The
error gives the 2 ways out:

```
syncwatch: Not imported. The file sets built-in login to on, but no administrator password is set. First set an
administrator password with syncwatch reset-admin-password. Or add access: {mode: proxy} to the file.
```

The exit code is 1. The configuration, the revision, the secrets and the settings notices do not change.

**DECISION pp-app-1:** the check applies only when the stored configuration is in proxy mode. The finding proposed the
check of the dashboard: refuse each file with built-in login when no administrator password is set. The dashboard
shows the import only in proxy mode or with an administrator password, thus the 2 checks are the same there. The
command line also runs at the first start, in built-in mode without an administrator password. That start logged the
setup token, and the token stays valid after the reload. A refusal there blocks the import of an example file, which is
the usual first step.

**DECISION pp-app-2:** no check for the bootstrap file. The finding proposed the same check for the bootstrap file.
syncwatch reads the bootstrap file only when the database has no configuration (`LoadConfig`). The same start then logs
the setup token in built-in mode (`LogSetupToken`). Thus the bootstrap file does not block the dashboard.

**User-visible.** `syncwatch import-config` refuses the file with the message above. The dashboard stays usable.

**Tests.** `TestImportRefusesALockOut` (`internal/app/app_test.go`) checks the refusal and its message. The refused
import does not change the configuration, the revision, the API key of the file or the settings notices. The import
works with `access: {mode: proxy}`, with an administrator password and at the first start.
`TestImportConfigRefusesALockOut` (`cmd/syncwatch/main_test.go`) checks that the command fails with the message and
does not print "imported". The 2 tests fail on `1b589c9`.

**Live check.** `fix-app/import-2/live-7f825f1/` and `live-fix/`: `syncwatch serve` in proxy mode on 127.0.0.1:24545, a
fake Discord on 127.0.0.1:24546. With `7f825f1`, the command imports the file and each page goes to `/setup`. With this
branch, the command stops with exit 1 and the message, and `/`, `/settings` and `/api/v1/ha` stay at 200. The 2 ways
out work: the file with `access: {mode: proxy}`, and `reset-admin-password` and then the file. In kit slot s27 (the
setup of the studio, `fix-app/import-2/kit-s27/`), the import of a file without `access:` works, and the login works.

## Lines for docs/CHANGES-review.md

- `syncwatch import-config` refuses a file that sets built-in login to on, when syncwatch is in proxy mode and no
  administrator password is set. A file without an `access:` section sets built-in login to on. Before, each page of
  the dashboard then asked for a setup token that syncwatch did not log.
- A start that fails (for example a TLS file that is missing or an address in use) does not change the heartbeat. After
  a restart loop of Docker, the time when syncwatch did not run goes to the start that works.
- A host without a battery clock can start syncwatch before NTP sets the clock. syncwatch then corrects the time when it
  did not run, in the first 10 minutes after the start.

## Lines for README.md

- In "Time when syncwatch did not run" (section Notifications), after "at most 30 seconds before the crash.": "A start
  that fails, for example because of a TLS file that is missing, does not count. The clock of the host can be behind at
  the start (a host without a battery clock). Then syncwatch corrects the time after NTP sets the clock."
- In "Import on the command line": "`syncwatch import-config` refuses a file that sets built-in login to on, when
  syncwatch is in proxy mode and no administrator password is set. Add `access: {mode: proxy}` to the file, or set an
  administrator password with `syncwatch reset-admin-password` first."

## Requests to the integrator

- None for the code. The 3 commits change only files of this area.
- The branch `sim/pp-hygiene` removes `docs/changes`. Keep this file in the results folder if that change stays.
