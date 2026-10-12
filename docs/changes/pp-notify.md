# pp-notify — Pre-release review: the findings of the area "notify"

Branch `sim/pp-notify`, worktree `the test kit`, base `review/sim-2026-10` at `7f825f1`.
The pre-release review confirmed 11 findings in the notifier (`internal/notify`). This branch corrects all of them, with
one commit for each finding or group of findings with the same cause. Each test fails on `7f825f1` (or without the
change) and passes on this branch. The evidence (test output before and after each change, the live checks on kit slot
s24 with the image built from `e15a08f`) is in `the test kit`.

| Key | Commit | Short title |
|---|---|---|
| notify-4 | `480b6f4` | A test of the notifier failed on each Monday and stopped CI and the release job |
| import-1, text-1, notify-1 | `9087441` | Settings notices sent names from Syncthing to Discord without escaping |
| text-3 | `70aea80` | 2 PCs with the same name shared 1 line in the messages |
| notify-2, exposure-3 | `2ac5c8d`, `dc59192` | "Notifications fail" did not end when no message was due |
| exposure-1 | `f5f2afa` | A restart forgot that notifications fail |
| notify-3 | `0ab9ac4` | An older kept notice held up the fallback of a security notice for a day |
| notify-5 | `bbccfc4` | Repeats mentioned the role once for each finding, not once in each interval |
| notify-6 | `f11242a` | A security notice was lost when syncwatch stopped during its first send |
| notify-7 | `e15a08f` | In the night of the clock change, "from 02:30 to 02:30" |

## 1. notify-4: TestUndecryptableNoticesWebhook depended on the day

**What went wrong.** The test ticked 40 minutes from the real clock with the default schedule. From Monday 08:00 to
Tuesday 08:00 (the zone of the host; UTC on the GitHub runners) the weekly report went to the channel of the findings, and
the test failed. The test job of `ci.yml` and `release.yml` runs before the image job, thus a tag on such a day gets no
image.

**What changed.** `internal/notify/r2_notices_test.go` only: the test runs at 4 fixed times of the week (Monday 09:30,
Tuesday 07:50, Thursday 12:00, Sunday 23:00) with the weekly report and the daily reminder off.

**User-visible.** None. CI and the release job no longer fail on Mondays.

**Test.** The test itself. With the old configuration, the Monday and Tuesday runs fail (`notify-4-before-fix.txt`).

**CHANGES-review.md** (Tests and build): "A test of the notifier no longer fails on Mondays. It stopped the test job of CI
and of the release."

## 2. import-1, text-1, notify-1: settings notices sent names from Syncthing without escaping

**What went wrong.** `Discord.Payload` escaped the title of a settings notice but wrote its summary raw. The summaries
contain names that syncwatch does not control. A PC chooses its own Syncthing device name, and the server takes it when an
administrator accepts the PC. With the name `ANNA-PC\nThe administrator password is changed. [Log in again](https://evil.invalid/login) **urgent**`,
the notices "Devices changed" (Settings → Devices) and "Configuration imported" showed a forged second line and a working
masked link in a syncwatch notice. The notice of a snoozed or ignored urgent finding did the same with a folder label or an
error text of Syncthing. The README says that syncwatch escapes titles, summaries and lines.

**What changed.**

- `internal/notify/discord.go`: `Payload` escapes each line of a `KindNotice` summary with `escapeMD`. The line breaks
  between the lines stay, because they are syncwatch's own. The stored notice and the log line keep the raw text.
- `internal/notify/message.go`: `OneLine` (exported) changes line breaks and other control characters of an outside text
  to spaces. `escapeMD` uses it.
- `internal/web/settings.go` (1 line, see the requests to the integrator): `deviceChanges` puts the device name through
  `notify.OneLine`.
- `internal/web/server.go` (1 line): `announceHidden` puts the finding line and the detail line through `notify.OneLine`.

**User-visible.** A name from Syncthing in a settings notice is plain text on its line: no new line, no link, no bold, no
mention, no reversed text. A URL in a settings notice (for example in "certificate pinned") is no longer a link: a
zero-width space follows `https:`, as in the lines of findings.

**Tests.** `TestHostileNamesAreNeutralised` has 2 settings notices. `TestDeviceNameInSettingsNoticesIsEscaped` and
`TestFindingTextInSnoozeNoticeIsEscaped` (`internal/web`) go through the real handlers, notifier and Discord sender
(`escape-*-before-fix.txt`, `escape-*-after-fix.txt`). `internal/app/alive_test.go`: the expected text of the pin notice
has the zero-width space.

**Live check** (s24, `live\a-escaped-devices-notice.txt`): the name was set on AKKA, then "Always connects through a relay"
was selected for the PC. The fake Discord got 1 line: `The device ANNA-PC The administrator password is changed. \[Log in
again\]\(https:​//evil.invalid/login\) \*\*urgent\*\* \(NR2YH7L\) always connects through a relay. H11 is off for it.`

**CHANGES-review.md** (Notifications): "syncwatch escapes the names from Syncthing in settings notices for Discord. A
device name cannot add a line, a link or bold text to a settings notice."

**README.md:** line 482 is correct now. No change.

## 3. text-3: 2 PCs with the same name shared 1 line

**What went wrong.** Syncthing does not make device names unique. When the office and site B each had a PC named EDIT-01,
the merge rule of the lines saw the same subject on 2 servers and wrote 1 line: `Device EDIT-01 is stuck at 40% … (seen from
AKKA) _(also seen from SOL)_`. The state and the details link of the second PC were lost in the urgent, repeat, reminder,
resolved, overnight and weekly messages. The same happened for H6, C3, H11 and for 2 unknown devices with the same name (H9).

**What changed.** `internal/notify/lines.go`: `mergeItem` has the device ID of the details ("Device ID", or "Offered by"
for a folder offer), and the merge key contains it. The views of 1 PC from both servers stay 1 line.

**User-visible.** 2 devices with the same name have 2 lines, each with its state and its link.

**Limit.** An occurrence in the history has no device ID (`store.HistoryEntry`). The section "New this week" of the weekly
report, and the lines of findings that opened and resolved without a message, keep the old rule.

**Tests.** `TestTwoPCsWithTheSameNameHaveTwoLines` (H5, H9 and H11 in each list; the control: 1 PC seen from both
servers is 1 line) and `TestTwoUnknownDevicesWithTheSameNameInTheUrgentMessage` (through the rules and the engine).

**CHANGES-review.md** (Notifications): "2 devices with the same name have a line each in the messages."

## 4. notify-2 and exposure-3: "notifications fail" did not end when no message was due

**What went wrong.** Only a successful send ended the state "notifications fail". When nothing sent again, the state
stayed. Examples: a test message that failed in a short Discord outage; an urgent message whose finding was snoozed or
resolved before the next try; a failure just before the quiet hours (nothing is sent in them). After
`SYNCWATCH_HEALTH_NOTIFY_LIMIT` (15 minutes), `/healthz` answered 503, the container was unhealthy and the dead-man URL was
not called, until the next message. In a quiet week that was the weekly report, up to 7 days later. The banner said
"syncwatch tries again automatically", but nothing tried.

**What changed.**

- `internal/notify/discord.go`: `Discord.Probe` asks for the webhook with `GET` on the webhook URL. Discord answers with the
  webhook (HTTP 200), and with 404 when the webhook is deleted. The check posts nothing in the channel, follows no
  redirect, and discards the answer (it contains the token). `brokenChannel.Probe` fails with the reason, as its `Send`.
- `internal/notify/notifier.go`: at the end of a tick that sent nothing to the channel of the findings, while no try is
  due, `probe` checks a failing channel, also in the quiet hours. A success ends the failure. The catch-up of what the
  channel missed stays. A failure backs off as the sends do (1, 2, 4, 5 minutes; up to 30 minutes when Discord refuses the
  webhook), and the banner shows the time of the next check. The back-off of the messages does not change. The banner keeps
  the error of the last message (`dc59192`): a test setup whose fake Discord takes only posts answers a check with another
  error.

**User-visible.** When Discord works again, the banner, `notifications_failing` of `/api/v1/ha` and the reason in
`/healthz` end within some minutes, also when no message is due and in the quiet hours. The log has `Discord webhook
answers again`. While Discord fails, the log has `cannot reach the Discord webhook` after each check. syncwatch now sends a
GET to the webhook URL while notifications fail.

**Tests.** `TestFailedTestMessageEndsWhenDiscordWorksAgain` (a day of ticks, no 503 at any moment, no post),
`TestFailureBeforeQuietHoursEndsInThem` (the overnight summary still announces the finding at 07:00),
`TestFailureEndsAfterTheFindingIsSnoozed`, `TestCheckOfADeletedWebhookKeepsTheFailure` (404: 6 checks in 1 hour, the
banner gives the next check), `TestDiscordProbe` (GET, no post, redirect, 404, 503, no token in the error), and
`TestHealthzAfterAFailedTestMessage` (`internal/web`: the real handler, notifier and Discord sender, then `/healthz` after
the limit). Without the check, the first 4 and the web test fail (`notify-2-exposure-3-before-fix.txt`).

**Live check** (s24, `live\b-failed-test-message-clears.txt`): the fake Discord answered the test message with HTTP 500 at
13:25:48. The banner said "Notifications fail from 2026-10-12 13:25: Discord: HTTP 500". At 13:25:49 the log said `Discord
webhook answers again`, and at 13:25:58 the banner and `notifications_failing` were gone. The fake Discord got no other post.

**CHANGES-review.md** (Notifications): "While notifications fail, syncwatch checks the webhook without a message. When
Discord answers, the failure ends, also when no message is due. Before, a failed test message kept `/healthz` at 503 until
the next message."

**README.md:**

- Line 99, replace "syncwatch finds a notification failure only when it tries to send a notification." with "syncwatch
  finds a notification failure when it tries to send a notification. While notifications fail, syncwatch also checks the
  webhook without a message (a GET of the webhook URL), also in the quiet hours. When Discord answers, the failure ends."
- Line 478, replace "Each page shows a banner until a message arrives." with "Each page shows a banner until a message
  arrives or a check of the webhook works."

## 5. exposure-1: a restart forgot that notifications fail

**What went wrong.** The state "notifications fail" was in memory only. After a restart, `/healthz` answered `ok` and the
dead-man URL was called again until the first send failed and the limit passed again: 15 minutes for each restart, also
while Discord refused every message. With an automatic restart of unhealthy containers, the pauses of the dead-man calls
were shorter than the grace time of the monitor service, thus the monitor never reported the failure.

**What changed.** `internal/notify/notifier.go`: the start and the latest error of a failure are stored in the setting
`notify.failing` (JSON). A send that works, a check that works and a new webhook (`ChannelsChanged`) delete it. `New`
continues a stored failure: the state, and the start of the catch-up of what the channel missed. The first tick then checks
the webhook (section 4).

**User-visible.** After a restart during a failure, the banner, `/api/v1/ha` and `/healthz` continue with the time when the
failure started. When that is longer ago than the limit, `/healthz` answers 503 at once, and the dead-man URL is not
called. The log has `notifications fail from before the start`. After a restart, the section "Opened and resolved while
the notifications failed" is sent as after a failure without a restart.

**Test.** `TestRestartKeepsTheFailure` (a restart on the same database while Discord refuses the messages; then a send that
works; then a new webhook). Without the change, the state after the restart is empty (`exposure-1-before-fix.txt`).

**Live check** (s24, `live\c-restart-keeps-failure.txt` and `live\c2-restart-after-the-limit.txt`): with the fake Discord
at 404, the test message failed at 13:27:30. After `envctl sw s24 syncwatch restart` the banner still said "Notifications
fail from 2026-10-12 13:27", and `notifications_failing_since` was 11:27:30Z. When the fake Discord answered again, the
failure ended at the next check. In C2 the failure started at 13:29:53, and `/healthz` answered 503 from 13:45. After the
restart at 13:45:55, `/healthz` answered 503 at once with "notifications fail from 2026-10-12 13:29", and the image
healthcheck of the container stayed "unhealthy". When the fake Discord answered again, the next check (13:52:57) ended the
failure.

**CHANGES-review.md** (Notifications): "A restart keeps the state "notifications fail". `/healthz` and the dead-man URL
report a failure that started before the restart."

**README.md:** line 479, replace "syncwatch keeps the start of the failure only in memory. After a restart during a failure,
syncwatch does not send the section." with "syncwatch stores the start of the failure. After a restart during a failure,
the banner, `/healthz` and the section continue from that start."

## 6. notify-3: an older kept notice held up the fallback of a security notice

**What went wrong.** `retryOwnNotices` stopped at the first notice that the webhook for settings notices did not take.
When that was an older notice that is not about security (kept for a day after HTTP 5xx, a redirect, or a webhook that
syncwatch cannot decrypt), a security notice behind it never got the fallback to the channel of the findings after 1
hour. It arrived when the older notice expired, approximately 24 hours later. The README promises the fallback after 1
hour.

**What changed.** `internal/notify/notices.go`: after the first failure of the webhook in a pass, the webhook gets nothing
more in that pass. Each security notice that waits for more than 1 hour still goes to the channel of the findings. When
that channel fails too, the pass stops.

**User-visible.** The fallback of a security notice comes after 1 hour, also behind an older notice.

**Test.** `TestFallbackIsNotBlockedByAnOlderNotice` (HTTP 503, a redirect, and a webhook that syncwatch cannot decrypt):
the security notice of 10:05 arrives at 11:01 or 11:05, not the next day (`notify-3-before-fix.txt`, `notify-3-after-fix.txt`).

**CHANGES-review.md** (Notifications): "A settings notice about security goes to the first webhook after 1 hour of
failures of the webhook for settings notices. An older settings notice that waits does not stop it."

## 7. notify-5: repeats mentioned the role once for each finding

**What went wrong.** Each finding had its own repeat clock. With "repeat every hour", SOL down from 09:00 and SIRIUS from
09:21 gave 2 repeats with the role mention in each hour (10:05 and 10:26, 11:05 and 11:26 …). N urgent ERROR findings that
opened at different times gave N mentions in each interval.

**What changed.** `internal/notify/notifier.go`, `repeatDue`: when one finding is due for a repeat, the findings announced
more than half an interval before go in the same repeat message. From then on they share one clock. A finding announced
less than half an interval before keeps its own clock.

**User-visible.** The role gets 1 repeat in each interval, with all the findings that are still open:
"🚨 2 urgent findings are still open on SOL, SIRIUS: H1". The first repeat of a finding can come earlier than 1 interval
after its urgent message, but not earlier than half an interval.

**Tests.** `TestRepeatMentionsTheRoleOnceEachInterval` (1 repeat in each hour from 10:00 to 12:59, for both servers) and
`TestRepeatTakesOnlyFindingsAnnouncedHalfAnIntervalBefore` (`notify-5-before-fix.txt`). `TestRepeatSelection` and
`TestUrgentMessageIsRepeated` pass unchanged.

**CHANGES-review.md** (Notifications): "A repeat of urgent messages names all the findings that are still open. Thus the
role gets 1 mention in each interval, not 1 for each finding."

**README.md:** line 462, after "It sends a repeat that falls due in the quiet hours when they end." add "When a repeat falls
due, syncwatch also repeats each urgent finding that it announced more than half the interval before. Thus the role gets
1 mention in each interval."

## 8. notify-6: a security notice was lost when syncwatch stopped during its first send

**What went wrong.** A settings notice went into the stored list only after its first send failed. When syncwatch stopped
during that send (Discord does not answer: 20 seconds for each try, up to a minute), the notice was in no list, and no
channel ever got it. The command line (`reset-admin-password`, `import-config`) deleted its own copy right after the call.
This broke W-051: a security notice is kept until a channel accepts it, also through a restart.

**What changed.** `internal/notify/notices.go`, `notifier.go`: a security notice goes into the stored list before the
first send, marked as "in the first send" (in memory only). The retries skip it until that send ends, thus there is no
second copy. A success, a message that Discord refuses, and "no channel" delete it. A failure lets the retries send it.
After a restart, the retries send it. A notice in its first send does not count in "settings notices wait".

**User-visible.** A security notice arrives after a restart that stopped its first send. Its text then has the line
"Changed at … syncwatch kept this settings notice because the first send failed." The command line needs no change: when
the call returns, the notice is stored, and only then does `sendCLINotices` delete its copy.

**Tests.** `TestSecurityNoticeSurvivesAStopDuringItsFirstSend` (`Notice` and `NoticeLater` with a Discord that does not
answer, a tick during the send, then a start on the same database) and `TestSecurityNoticeSentAtOnceIsNotKept`
(`notify-6-before-fix.txt`).

**Live check** (s24, `live\d-stop-during-first-send.txt`): the fake Discord held the next request for 40 seconds. An API
token was added at 13:53:08 (the security notice "API token added"), and the syncwatch container was restarted at 13:53:11,
during the first send. At 13:53:12 the new process loaded 1 kept notice and sent it ("Changed at 2026-10-12 13:53.
syncwatch kept this settings notice because the first send failed."). The first send had no answer.

**Note.** When Discord posts the first copy before the stop, the channel gets the notice 2 times. The fake Discord of the
kit did this: it recorded the first request after its wait, although syncwatch was gone. A duplicate is better than a lost
security notice.

**CHANGES-review.md** (Notifications): "syncwatch stores a settings notice about security before its first send. A
restart during the send does not lose it."

## 9. notify-7: in the night of the clock change, "from 02:30 to 02:30"

**What went wrong.** The messages wrote clock times without the UTC offset. In the night of 2026-10-25 (Europe/Stockholm,
03:00 CEST is 02:00 CET) a time of 1 hour can start and end at the same clock time: "syncwatch did not run from 02:30 to
02:30." read as no gap, and a fault of 60 minutes read "02:15–02:15". In the spring, a gap of 1 hour read as 2 hours.

**What changed.** `internal/notify/notifier.go` (`zoned`, the gap line, `MissedMessage` now uses `span`) and
`internal/notify/lines.go` (`span`, `blipLines`): when the 2 times of a range have different UTC offsets, each clock time
gets its zone.

**User-visible.** Only in the night of a clock change: "syncwatch did not run from 02:30 CEST to 02:30 CET.", "(the fault
lasted 60 minutes, 02:15 CEST–02:15 CET)", the blip lines and the title of the catch-up section. On other days nothing
changes.

**Test.** `TestTimesInTheNightOfTheClockChange` (gap in the autumn and in the spring, resolved line, blip line, catch-up
section, a normal day) (`notify-7-before-fix.txt`). The new forms pass `stecheck` (`ste\report.md`).

**CHANGES-review.md** (Notifications): "In the night of a clock change, the times in the messages name the zone, for
example "from 02:30 CEST to 02:30 CET"."

## Requests to the integrator

- `internal/web/settings.go`, `deviceChanges`: `name := notify.OneLine(deviceName(id)) // the PC names itself` (1 line,
  commit `9087441`).
- `internal/web/server.go`, `announceHidden`: the summary is `notify.OneLine(f.Check+" "+f.Subject+": "+f.Message) + "\n" +
  notify.OneLine(detail)` (1 line, commit `9087441`).
- `internal/app/alive_test.go`, `TestRunWiring`: the expected text of the pin notice has `https:\u200b//` (commit
  `9087441`).
- `internal/web/pp_notify_test.go`: new tests in `internal/web` (`TestDeviceNameInSettingsNoticesIsEscaped`,
  `TestFindingTextInSnoozeNoticeIsEscaped`, `TestHealthzAfterAFailedTestMessage`).
- Not changed, for information: the fake Discord of the test harness (`test/harness/discord.go`) has no `GET
  /api/webhooks/{id}/{token}` and answers 405. While notifications fail in an integration run, the log then has `cannot
  reach the Discord webhook err="Discord: HTTP 405"`, and the failure ends only with the next message, as before. The kit's
  fake Discord answers the GET like Discord. A route `GET /api/webhooks/{id}/{token}` that answers 200 (404 in the mode
  `fail404`) would make the harness the same as Discord.
- `README.md` and `docs/CHANGES-review.md`: the lines above. They pass `stecheck`
  (`the test kit`).
