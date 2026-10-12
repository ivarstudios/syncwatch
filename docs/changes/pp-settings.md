# pp-settings — Settings, import and access (pre-publication review)

Branch `sim/pp-settings`, worktree `the test kit`, base `review/sim-2026-10` at `7f825f1`.
Findings: sessions-1, import-3, import-4, text-5. All 4 are fixed. Each finding has its own commit.

Commits (`git log --oneline review/sim-2026-10..HEAD`, before this file):

```
f764251 fix: the import preview and the notification log show direction controls (text-5)
58fbed3 fix: one site typed in two spellings is one site (import-4)
76eae1b fix: check the API token of an imported file and show its scope (import-3)
b91b1f0 fix: the viewer password must differ from the administrator password (sessions-1)
```

Files changed. Owned by this area: `internal/web/settings.go`, `internal/web/importpreview.go`,
`internal/config/config.go`, new tests `internal/config/pp_config_test.go` and `internal/web/pp_settings_test.go`, the
test values in `internal/settings/token_test.go` and one expected line in `internal/web/r2_settings_test.go`. Not owned
(3 small changes for sessions-1, see "Requests to the integrator"): `internal/web/server.go` (`handleSetup`, 4 lines),
`cmd/syncwatch/main.go` (`resetPassword`, 3 lines), `internal/app/app.go` (`SYNCWATCH_ADMIN_PASSWORD` check, 3 lines), each
with a new test file `pp_settings_test.go` in its package.

Checks before each commit: `gofmt -l .` empty, `go vet ./...`, `go vet -tags integration,compose,loadsim ./test/...`,
`GOOS=windows go build ./...`, `GOOS=linux CGO_ENABLED=0 go build ./...`: pass. `go test -count=1 ./...` (Windows): every
package passes except **one test of the notify area**, `internal/notify` `TestUndecryptableNoticesWebhook`. It also fails
on the base `7f825f1` without these changes (see "Requests to the integrator" 4). The integration, compose and loadsim
suites were not run (not this stage). Logs: `the test kit`.

Each new test fails on the code before its fix: `sessions-1-before-*.txt`, `import-3-before.txt`, `import-4-before.txt`,
`text-5-before.txt` in `the test kit`. The probe tests of the reviewers pass with the fixes
(`*-verifier-tests-after-fix.txt`).

Live check: kit slot s22 with the image of `f764251` (`kit-pp-settings-f764251`), HTTPS as in the image, the fake Discord
of the slot. Script: `the test kit` (standard library only),
`paste_import.js` and `shot_log.js` (Playwright). Evidence: `live-*.txt`, `shots\` in the results folder. The import-3
paths of the bootstrap file and of `import-config` ran with the native binary on 127.0.0.1:24440-24441
(`live-import-3-native.txt`). The slot is removed again (`envctl down s22`).

New product strings: 8, stecheck 100 % (`ste-mapping.md`, `ste-report.md`). The proposed README and changelog lines
below: 10, stecheck 100 % (`ste-docs-mapping.md`, `ste-docs-report.md`).

## 1. sessions-1: the viewer password can be the administrator password

**What went wrong.** The login checks the administrator password first (`server.go` `handleLogin`). Settings → Access
stored any new viewer password, and "Change administrator password" stored any new administrator password. With the same
text for both, each login with the viewer password got an administrator session: the staff and the lobby screen could
open Settings, snooze findings and make API tokens. `reset-admin-password`, the first start and
`SYNCWATCH_ADMIN_PASSWORD` had the same gap.

**What changed.**

- `handleAccessSave`: a new viewer password now always needs the administrator password in "Administrator password", also
  when it has no effect now (viewer access "administrator only"). After that confirmation, the field `confirm` is the
  administrator password, thus a plain comparison finds the same password without a second argon2 check. The order
  matters: a session that does not know the administrator password (an unlocked browser, a copied cookie) gets the same
  answer for each guess. Thus the form is no test for guesses of the administrator password. The confirmation has the
  rate limit of confirmations (5 failures in 10 minutes for each session).
- `handlePasswordSave` and `handleSetup` refuse an administrator password that is the viewer password
  (`sameAsViewer`: 1 argon2 check, only when a viewer password is set).
- `resetPassword` (CLI) and `Run` (`SYNCWATCH_ADMIN_PASSWORD`) refuse it too.

**What the user sees.** New messages:

- "The viewer password must be different from the administrator password." (Settings → Access)
- "The administrator password must be different from the viewer password." (Change administrator password, First start)
- `syncwatch: the administrator password must be different from the viewer password` (`reset-admin-password`, exit 1)
- `syncwatch: SYNCWATCH_ADMIN_PASSWORD must be different from the viewer password` (start)

A new viewer password with viewer access "administrator only" now needs the administrator password. Before, the form saved
it without the administrator password. A refused change sends no settings notice.

**Limit.** An installation that has the same 2 passwords now keeps them until a person changes one. syncwatch stores only
argon2 hashes with different salts, thus it cannot find this case in the stored passwords. See the changelog line
"What you must do after the update" below and "Requests to the integrator" 1.

**Tests.** `internal/web/pp_settings_test.go`: `TestViewerAndAdministratorPasswordsDiffer` (both forms, each person with a
browser of its own, the role of the stored session), `TestViewerPasswordIsNoAdministratorPasswordOracle`,
`TestSetupRefusesTheViewerPassword`; `cmd/syncwatch/pp_settings_test.go` `TestResetPasswordRefusesTheViewerPassword`;
`internal/app/pp_settings_test.go` `TestAdminPasswordFromEnvironmentIsNotTheViewerPassword`.

**Live (s22, `live-sessions-1.txt`).** The administrator password as the viewer password: refused. Viewer password
"studio lobby 2026": saved. The lobby screen logs in with it: 303, then `GET /settings` 403. The viewer password as the
administrator password: refused. The staff log in with the viewer password: 303, `GET /settings` 403. A new viewer
password without the administrator password: the same answer for a wrong guess and for the administrator password.
`docker exec -i sw-s22-syncwatch syncwatch reset-admin-password` with the viewer password: exit 1 with the message. Fake
Discord: only the 2 notices of the saved changes, none for a refused change.

## 2. import-3: an imported api.token becomes a full-scope API token without a check

**What went wrong.** `config.Validate` checked each field except `api.token`. A bootstrap file, `import-config` or a
dashboard import with `api: {token: x}` made "x" the value of the API token "API token" with the scope Full. The API has
no rate limit for API tokens (about 12,600 guesses each second in the review), thus a short value was open to a fast
guess. The preview and the notice did not give the scope.

**What changed.**

- `config.Validate` refuses an `api.token` with less than 10 characters (`MinTokenLength`, the minimum of the
  passwords), or with white space, control characters or format characters. The message names `api.token` and points to
  Settings → API. `ApplyDefaults` removes white space at the ends (a paste with a line break). All 3 paths use
  `ParseYAML`, thus all 3 refuse the value before syncwatch stores something. The dashboard import refuses it at step 1.
- Why 10 and not 20: a random value of 10 characters from a-z and 0-9 needs thousands of years at the measured rate. The
  check is for placeholders, cut pastes and example values, which are short. API tokens from the dashboard have 36
  characters. A longer minimum is a 1-line change of `MinTokenLength` if the team prefers it.
- The preview gives the scope: `the API token "API token" (Full: /api/v1/status and /api/v1/ha)`, or the scope that the
  existing API token "API token" keeps. The settings notice adds the existing sentence of the scope:
  `The API token "API token" is replaced. It can read /api/v1/status and /api/v1/ha.`

**What the user sees.** `Import failed. API token (api.token): the value must have at least 10 characters and no spaces
or control characters. Make the API token in Settings → API.` The same text stops the first start and `import-config`.
The preview and the notice give the scope.

**Tests.** `internal/config/pp_config_test.go` `TestImportedAPITokenIsChecked` (9 refused values, 5 accepted values,
with the trim); `internal/web/pp_settings_test.go` `TestImportedAPITokenIsCheckedAndShowsItsScope` (step 1 refuses "x",
nothing is stored; the preview and the notice for the scope Full and for a kept scope Home Assistant). Test values of
`internal/settings/token_test.go` that were shorter than 10 characters are longer now. The expected preview line of
`TestImportPreviewChangesNothing` has the scope now.

**Live (s22, `live-import-3.txt`; native, `live-import-3-native.txt`).** Step 1 with `x`, `changeme`, `swt_12345` and
`"my token value"`: refused with the message; `Bearer x` gets 401. A good value: the preview says
`the API token "API token" (Full: /api/v1/status and /api/v1/ha)`, the notice says `… is replaced. It can read
/api/v1/status and /api/v1/ha.`, the new value gets 200 and the old value 401. The kit's API token was imported again at
the end. Native: a bootstrap file with `api: {token: x}` stops `serve` with the message (exit 1) and keeps the file;
`import-config` with `changeme` exits 1; a value with spaces at the ends is accepted.

## 3. import-4: one site typed in two spellings becomes two sites

**What went wrong.** The rules and the dashboard compare the sites exactly (plan C11). `siteOK` accepted the no-break
space U+00A0, U+2028, U+3000 and U+3164, and `ApplyDefaults` only removed white space at the ends. "IVAR Office" and
"IVAR" + U+00A0 + "Office" look the same but were 2 sites: H1 for AKKA said "unreachable from IVAR Office" with the cause
"The VPN between IVAR Office and IVAR Office is down", and the Overview and the lobby showed 2 sites with a link. A case
variant ("IVAR office") did the same.

**What changed** (all in `internal/config/config.go`, plus 3 calls in `settings.go`).

- `CleanSite` makes each run of white space one space and removes white space at the ends. The Servers form, the
  Notifications form and `ApplyDefaults` use it. Thus a no-break space or a line separator from a paste is a space.
- One site has one spelling. `ApplyDefaults` gives a site that is the same as an earlier site without regard to case the
  spelling of the earlier site (first the site of syncwatch, then the servers in their order). The 2 forms give the typed
  spelling to each site that is the same without regard to case (`Config.SpellSite`). Thus the rules still compare
  exactly (plan C11 stays true), but a site has only one spelling. The notice of the form names each site that changes:
  for example "The site of the server AKKA changed from IVAR Office to IVAR office." and "Site where syncwatch runs:
  IVAR Office → IVAR office". A new server also lists the sites of the other servers that change.
- `siteOK` also refuses the other default-ignorable characters (for example U+3164) and white space other than the space
  (a value that did not go through `CleanSite`).
- Not done: NFC normalization. Go has it only in `golang.org/x/text`, a new dependency. A composed and a decomposed "é"
  are still 2 sites.

**What the user sees.** One site where there were 2. H1 for a server at the site of syncwatch says "Server AKKA is
unreachable: …" with the general causes. A case variant changes the spelling of the site for all servers of the site and
for syncwatch, and the settings notice says so. No new strings.

**Tests.** `internal/config/pp_config_test.go` `TestOneSiteHasOneSpelling` (U+00A0, 2 spaces, U+2028, U+3000 and a case
variant become one site; U+3164, U+200B, BEL and U+202E are refused; `CleanSite`; `SpellSite`);
`internal/web/pp_settings_test.go` `TestOneSiteTypedInTwoSpellings` (the forms, H1 of the engine, the datalist, the
notices of a case variant and of a new server).

**Live (s22, `live-import-4.txt`, `live-import-4-h1.txt`).** AKKA "IVAR Office", SOL "Branch office", syncwatch "IVAR" +
U+00A0 + "Office": the export says `site: IVAR Office` with a space; the datalist has 2 names; the lobby has 2 groups; the
Connections row is "AKKA ↔ SOL". AKKA as "IVAR office": the site of syncwatch is "IVAR office" too, with one notice
that names both changes. `envctl stop s22 AKKA`: H1 after 40 s, "Server AKKA is unreachable: no route to host" with the
general causes, on the Findings page and in the urgent message. (The test client first sent `weekly_day` empty, thus
Discord #9 and #11 of the slot came from the client, not from syncwatch; see the note in `live-import-4.txt`.)

## 4. text-5: the import preview and the notification log show direction controls raw

**What went wrong.** The other pages show U+202E and the other direction controls as `‹U+202E›` (`showControls`). The
import preview printed the lines of `changesOf` as they are: a PC named U+202E "gnp.exe" U+202C showed the rest of the
line in reverse. The table "Recent notifications" printed the stored titles raw: the title of an urgent message has the
folder label or the device name from Syncthing.

**What changed.**

- `buildImportPreview` applies `showAll` to the lines with names (Changes, Removed, KeyAgain, Secrets) and `showControls`
  to the label of the checkbox `delete_servers`. Server names have no check for these characters (a file can bring
  them), thus the server lines get it too. A refused step 2 shows its reason the same way. The preview is only for the
  page: the settings notice of the import is made separately and the notifier removes the controls for Discord.
- `settingsData` applies `showControls` to the title and the error of each row of the notification log.

**What the user sees.** `‹U+202E›gnp.exe‹U+202C›` in the preview and `LIVE-‹U+202E›ECRUOS1` in "Recent notifications",
as on the Devices card and the Findings page. No new strings.

**Tests.** `internal/web/pp_settings_test.go` `TestImportPreviewShowsDirectionControls` (a device name and server names in
all 5 lists and in the reason of a refused step 2) and `TestNotificationLogShowsDirectionControls` (title and error).

**Live (s22, `live-text-5.txt`, `live-text-5-browser.txt`, `live-text-5-log.txt`, `shots\`).** LAPTOP-DANI renamed on
AKKA to U+202E "gnp.exe" U+202C: the Devices card and the preview line both show `‹U+202E›gnp.exe‹U+202C›`, also in
Chromium after a paste into the import card (`shots\text5-import-preview-card.png`). The folder live-fjord-1source with
the label "LIVE-" U+202E "ECRUOS1" and `inject folder-error`: the urgent message after 41 s; the row of "Recent
notifications" says `🚨 Folder sync stopped: LIVE-‹U+202E›ECRUOS1 on AKKA` (`shots\text5-recent-notifications.png`). The
names and the label were set back, and `heal s22 all` ran.

## Lines for docs/CHANGES-review.md

A new section for the pre-publication review, for example "## Pre-publication review (pp)":

- **Settings:** The viewer password must be different from the administrator password. Before, a person who logged in
  with the viewer password got an administrator session when the 2 passwords were the same.
- **Settings:** A new viewer password needs the administrator password, also when the viewer access is "administrator
  only".
- **Settings:** An imported `api.token` needs at least 10 characters and no spaces or control characters. The import
  preview and the settings notice give the scope of the API token "API token".
- **Sites:** One site has one name. syncwatch makes each white space in a site name a space, and names that differ only
  in capital letters are one site. Before, "IVAR Office" with a no-break space was a second site, and H1 named the VPN
  between IVAR Office and IVAR Office.
- **Dashboard:** The import preview and the table "Recent notifications" show a character that changes the direction of
  text as ‹U+202E›, as the other pages do.

Under "What you must do after the update":

- If the viewer password and the administrator password are the same, change one of them in Settings → Access. syncwatch
  cannot find this case in the stored passwords.

## Lines for README.md

- Security, **Login** (line 513), after "You need the administrator password … to set a new viewer password.": "The viewer
  password must be different from the administrator password. syncwatch refuses the same password in Settings → Access,
  on the first start, in `reset-admin-password` and in `SYNCWATCH_ADMIN_PASSWORD`."
- **Import in the dashboard** (line 228), a new bullet after the `api.token` bullet: "The value of `api.token` must have
  at least 10 characters and no spaces or control characters. The preview and the settings notice give the scope of the
  API token "API token"."
- Bootstrap file (line 212), after "… syncwatch keeps only its SHA-256 hash.": "A shorter `api.token`, or a value with
  spaces, stops the first start with a message."
- Sites (line 327), replace "A site name has at most 40 characters and no control characters. syncwatch compares the
  names exactly, with the capital letters." with: "A site name has at most 40 characters and no control characters or
  invisible characters. syncwatch makes each group of spaces one space, also a no-break space from a paste. Names that
  differ only in capital letters are one site. syncwatch uses the name that you type in Settings for all servers of that
  site and for syncwatch. In a file, the first form of the name applies: first the site of syncwatch, then the sites of
  the servers in their order."
- Line 492 (`/lobby?site=`) stays: the parameter still needs the name as the configuration has it.

## Requests to the integrator

1. **Files of other areas changed for sessions-1** (keep them, they are the same check at each place where an
   administrator password is set): `internal/web/server.go` `handleSetup` (+4 lines, calls `sameAsViewer` of
   `settings.go`), `cmd/syncwatch/main.go` `resetPassword` (+3 lines), `internal/app/app.go` `Run` (+3 lines). Optional,
   not done: `handleLogin` (`server.go:517-524`) could log a warning when a password matches both hashes. That is the
   only place that can find an existing pair of equal passwords.
2. **Rate limit for API tokens** (`internal/web/api.go`, the auth area): an unknown API token gets 401 without a limit
   (12,600 tries each second in the review). import-3 removes the short imported values, but the missing limit stays the
   amplifier for each weak value.
3. **Sites:** no change is necessary in `internal/rules` or `internal/model`. The configuration now keeps one spelling for
   each site, thus the exact comparison there is correct.
4. **Failing test of the notify area:** `internal/notify` `TestUndecryptableNoticesWebhook` fails on `7f825f1` too. Its
   5 `Tick` calls start at `time.Now()` with the default weekly report (Monday 08:00). On a Monday after 08:00 (the day of
   this run), the weekly report goes to the channel of the findings and the test sees 1 message. Set `weekly_day: off`
   in the test configuration, or use a fixed time.

## Notes

- Observation, not changed: the Servers card of Settings shows the server name raw in its heading
  (`settings.html:15`, `<summary><strong>{{.Name}}</strong>`). A server name with U+202E there reverses the rest of the
  heading. The fix needs a separate display field, because the same row fills the input "Name". The administrator types
  the server names, thus the risk is low.
- The test client of the live check found that Settings has 2 text areas with the name `yaml` (Structure rules and the
  import card). A browser sends the correct one. Scripts must select `#import textarea[name=yaml]`.
