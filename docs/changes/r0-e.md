# Fix round r0, fixer e: web auth, settings notices and request robustness

Branch `sim/r0-e`, base `78ef2d8`. Map: `05-web.md` (hunches 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 12, 16, 18, 19).
Files changed: `internal/web/auth.go`, `server.go`, `settings.go`, `api.go`, `templates/settings.html`,
`internal/web/*_test.go` (new `settings_test.go`), and two one-line hunks in `internal/app/app.go` (see "Other files touched").
Every item has a unit test in `internal/web` that fails on `78ef2d8` and passes now (items 1, 3, 4, 6, 7 and 9 were also shown
failing by the map's experiments in `05-web-verify_test.go`).

## 1. Login limiter: parallel attempts and the viewer-login reset (map hunches 1 and 2)

- Root cause: `limiter.check` and `limiter.fail` were separate steps with argon2 in between, so wrong passwords sent at the same
  moment all passed the per-address cap (40 parallel guesses: 40 × 401). `limiter.success` deleted the address's whole failure
  list, so a viewer login wiped the failed admin guesses.
- Changed (`auth.go`, `server.go`, `settings.go`): `check` now reserves the attempt (in-flight attempts count against the address
  until `fail` or `success` ends them; at most 5 attempts per address are ever being checked at once). Failures are kept per role:
  a wrong login counts against every password it was checked against (admin, and viewer when viewing is password-protected);
  confirm fields, the password-change form and the setup token count as admin attempts. `success(ip, role)` forgets only that
  role's failures, and an address with nothing against it is dropped from the map (the map no longer grows with every address seen).
- User-visible: a burst of wrong passwords from one address gets at most 5 password checks, the rest 429. After 5 wrong guesses a
  viewer login no longer resets the count, so the address is blocked for 10 minutes as the README promises. An admin who typed a
  wrong confirm password 5 times is blocked as before.
- README: "Access and security" / limiter description — "5 failed attempts per address in 10 minutes" is now true under parallel
  load and with a viewer password; mention that failures count per role.
- Tests: `TestLimiterReservesAttempts`, `TestLimiterCountsRolesSeparately`, `TestParallelLoginsAreLimited`,
  `TestViewerLoginKeepsAdminFailures` (security_test.go).

## 2. Host header check (map hunch 3)

- Root cause: nothing checked `Host`; the Origin check compared with `r.Host`, so a page on a DNS-rebinding name was same-origin
  with the dashboard and, in proxy mode, could create an API token (or read everything with open viewing).
- Changed (`server.go`, `app.go` one line): `ServeHTTP` refuses every request whose `Host` is not an IP address, `localhost`, the
  host of `public_url`, the listen host or a name from `SYNCWATCH_TLS_HOSTS`, with `421 Misdirected Request` and one line:
  "syncwatch doesn't answer to the host name "…": open it by IP address and set public_url under Settings → Notifications, or set
  SYNCWATCH_TLS_HOSTS to the names you use". Ports and a trailing dot are ignored, names compare case-insensitively. Each refusal is
  logged at INFO with the host and the client address. `web.Server.AllowHosts(listen, extra...)` is new; `app.Run` calls it with
  `o.Listen` and `o.TLSHosts`.
- User-visible: deployments reached by a DNS name that is neither `public_url`'s host nor in `SYNCWATCH_TLS_HOSTS` get 421 until one
  of them is set (by IP address the dashboard always works). This includes another container calling the API by compose service name
  (`http://syncwatch:8080/api/v1/ha` from Home Assistant on the same network needs `SYNCWATCH_TLS_HOSTS=syncwatch`) and a reverse
  proxy that forwards the browser's `Host` (the README already requires that; the name must now also be `public_url`'s host or in
  `SYNCWATCH_TLS_HOSTS`). The e2e suites, the Docker HEALTHCHECK and `syncwatch healthcheck` use 127.0.0.1 and are unaffected.
- README: the compose example (`SYNCWATCH_TLS_HOSTS` comment: it is also the list of names the dashboard answers to), "Behind a
  reverse proxy" (the forwarded `Host` must be a known name), "JSON status API" / Home Assistant (calls by service name need the
  name in `SYNCWATCH_TLS_HOSTS`), and the 421 message itself.
- DECISION: every request is checked, bearer-token requests included (the task asked for every request; a token proves nothing
  about the page that sent the request). Loosening this for valid bearer tokens is a possible follow-up if service-name calls turn
  out to be common.
- Tests: `TestUnknownHostNameRefused`.

## 3. Settings notices for every change (map hunches 4 and 5)

- Root cause: each handler announced only a hand-picked subset (server added/moved/key/pin, role, webhook, checks switched off or
  made slower, access mode/viewer). Disabling a server, renaming it, thresholds, quiet hours, reminder, weekly report, timezone,
  `public_url`, structure rules, collector settings, the API token, the admin password and snoozing or ignoring an urgent problem
  were silent, so an alert could be silenced or redirected unseen.
- Changed (`settings.go`, `server.go`): the four describers (`roleChange`, `checkChanges`, `accessChanges`, `serverChanges`) are
  replaced by one `configChanges(old, new)`: servers (added, removed, URL changed, renamed, disabled/enabled, GUI URL, allow_http)
  and checks (enabled, urgent, debounce, both directions) are worded; every other key comes from a flattened YAML view of the
  configuration as `key: old → new` (lists as one value, placement rules by index; `access.mode`, `access.viewer` and the role ID
  keep their old wording). Secrets are outside the configuration (and the differ works on `Redacted()`), so they never appear.
  At most 40 lines per notice ("…and N more changes."). Each save sends one notice: "Server X changed" (plus "API key replaced." /
  "Pinned certificate forgotten: …"), "Notification settings changed", "Checks changed", "Structure rules changed",
  "Collector settings changed", "Access settings changed" (one notice instead of one per line), "Admin password changed",
  "API token added/replaced/revoked", "Configuration imported" (now also lists imported API keys and the token as
  added/replaced), and "Urgent problem snoozed/ignored" with the finding and the until/reason (only for open urgent findings).
  A save that changes nothing sends nothing. Removing a server ID that doesn't exist is refused with "Unknown server." instead of
  announcing a removal.
- User-visible: more notices in Discord (one per save that changed something), with the old and new values. Titles changed for
  server edits (was "Server X: API URL changed" / "API key replaced" / "pinned certificate forgotten" as separate notices; now one
  "Server X changed" with those as lines).
- README: "Settings changes are announced" — list what is announced now (everything, with old → new) and the snooze/ignore notice
  for urgent problems.
- DECISION: snooze/ignore of a non-urgent finding stays quiet (the task asked for urgent ones; the rest would be noise).
- Tests: `TestConfigChanges`, `TestEverySettingsChangeIsAnnounced`, `TestHidingUrgentProblemsIsAnnounced` (settings_test.go);
  the existing notice tests still pass with their wording.

## 4. Import could switch proxy → builtin without an admin password (map hunch 7)

- Root cause: `handleImport` lacked the Access form's guard; a file without an `access` section switched built-in login on, and
  every request then went to `/setup` with a token nobody had seen (it is only logged at a start in built-in mode).
- Changed (`settings.go`): refused before anything is stored: "Not imported: the file switches built-in login on, but no admin
  password is set, so nobody could log in. Set an admin password under Access first." (the password form works in proxy mode).
- README: "Export and import" — note the guard.
- Tests: `TestImportCannotLockEveryoneOut`.

## 5. Bodies of /login and /setup read without limit (map hunch 9)

- Root cause: `page()` had no `MaxBytesReader`, unlike `action()`, and login/setup parse the form (for the CSRF check) before
  anyone is logged in; a 16 MiB upload was read in full before the 403.
- Changed (`server.go`): `page()` limits the body to 64 KiB. `action()` keeps 2 MiB.
- User-visible: none for browsers (the login form is a few hundred bytes).
- Tests: `TestLoginBodyIsLimited`.

## 6. Structure form dropped the default ignore_dirs (map hunch 6)

- Root cause: YAML without `ignore_dirs` decoded to a nil slice, which the handler stored as an empty list (the author wanted
  "omitted = none", but import of the same YAML gets the defaults, and the scan then walks recycle bins and snapshots).
- Changed (`settings.go`): a missing key keeps the current list; an explicit `ignore_dirs: []` clears it. The form already rendered
  the current list (verified by the round-trip test), so no template change was needed.
- README: "Structure rules" — say that leaving `ignore_dirs` out keeps the current list and `[]` clears it.
- Tests: `TestStructureFormKeepsIgnoreDirs`.

## 7. API showed the delivery error text to viewers (map hunch 8)

- Root cause: `notifications.error` went to every authorized caller; the HTML banner already hides it from viewers. The text can
  quote the channel's answer, and redaction of the webhook URL is exact-match only, so it is admin material.
- Changed (`api.go`): `error` is set only when the caller's role is admin (session, or proxy mode); viewers, open viewing and bearer
  tokens get `failing` and `since`.
- User-visible: integrations using the token no longer see the error text in `/api/v1/status`.
- README: "JSON status API" — `notifications.error` is present for admin sessions only.
- DECISION: a bearer token is not treated as admin here (the safer reading of "only admins get the text").
- Tests: `TestAPIDeliveryErrorForAdminsOnly`.

## 8. Notification log time in the process zone (map hunch 10)

- Root cause: `settings.html` printed `datefmt` (configured zone) next to `.SentAt.Format "15:04"` (process zone).
- Changed (`server.go` template func `timefmt`, `settings.html` line 87): one formatter in the configured zone.
- Tests: `TestNotificationLogTimeZone`.

## 9. login?next= for POSTs; /login with a session (map hunch 12)

- Root cause: `deny()` put the request URI into `next` for every method, so a form sent after the session expired led to a GET of
  a POST-only URL (405) after login. `/login` showed the form even with a valid session.
- Changed (`server.go`): `next` is set for GET/HEAD only (a POST goes to plain `/login`, then `/`); `/login` with a session
  that still has a role redirects to `next` (or `/`). A viewer session left from before viewing was switched to `none` resolves
  to no role (`resolve()` keeps the session but drops the role); it gets the login form as before, since sending it on would
  bounce it between the page and `/login` without end (found while checking the resume; fixed in a commit of its own).
- Not changed: with open viewing and no admin password, `/settings` still gives 403 instead of `/setup` (the hunch's second
  part); the nav's "Log in" link reaches `/setup`, and the item asked only for the `next` and session cases.
- Tests: `TestLoginNextOnlyForPages`, `TestLoginPageWithStaleViewerSession`.

## 10. Concurrent settings saves (map hunch 16)

- Root cause: every handler cloned, edited and saved with no version check; secrets were written before the save.
- Changed (`settings.go`, `settings.html`): the settings page carries `rev` (first 48 bits of SHA-256 of the stored configuration
  JSON) as a hidden field in the nine forms that change the configuration (server edit/add/remove, notifications, checks,
  structure, collector, access, import). Each of those handlers checks it first, before any secret or hash is written; a mismatch
  is refused with "Not saved: the settings changed since this page was loaded; reload the page and make the change again."
  The password, API-token, accept-cert and test forms don't change the configuration and carry no revision.
- User-visible: the second of two admins (or tabs) saving different sections after the other's save is told to reload instead of
  silently undoing the first save.
- DECISION: a form without `rev` (scripts, the test harness's `PostForm`, which forwards only `csrf`) is accepted as before.
- README: "Settings" — mention the reload message.
- Tests: `TestStaleSettingsPageRefused`.

## 11. SameSite=Strict broke links from Discord (map hunch 18)

- Root cause: a link clicked in the Discord web app is a cross-site top-level visit; with Strict the session cookie stayed home, the
  user saw the login page and piled up sessions. The fresh CSRF cookie set on that visit also invalidated the forms in other tabs.
- Changed (`auth.go`): session and CSRF cookies (and the logout clearing cookie) are `SameSite=Lax`. Flash cookies stay Strict.
- DECISION: Lax (the task's decision). State changes stay protected by the CSRF token, the Origin check and the new Host check; Lax
  keeps cookies off cross-site POSTs.
- README: "Access and security" cookie description, if it mentions Strict.
- Tests: `TestCookiesAreSameSiteLax`.

## 12. Setup token hidden by the log level (map hunch 19)

- Root cause: the token was logged at WARN only, so `SYNCWATCH_LOG_LEVEL=error` hid it.
- Changed (`server.go`, `app.go` one line): the log line moved into `web.Server.LogSetupToken(stderr)`, which also prints
  `syncwatch: no admin password is set yet: …: <token>` to stderr when the logger doesn't enable WARN. Same conditions as before
  (built-in mode, no admin password).
- README: "First start" — the token is in the log, or on stderr when the log level hides warnings.
- Tests: `TestSetupTokenPrintedWhenLogHidesWarnings`.

## Items not fixed

None of the twelve. Noticed but out of scope (not changed): snooze/ignore accept finding IDs that are not open (map hunch 5), the
check-then-save race inside one request (two saves within milliseconds can both pass the revision check; the hidden field covers
the human-scale case), the `hashSlots` wait ignoring the request context (map hunch 1, second part; the per-address cap of 5
in-flight attempts bounds it per address).

## Other files touched

- `internal/app/app.go`: two lines after `web.New`: `srvWeb.AllowHosts(o.Listen, o.TLSHosts...)` and
  `srvWeb.LogSetupToken(os.Stderr)` (the latter replaces the three-line setup-token `log.Warn`).
- `internal/web/templates/settings.html`: beyond the log time (line 87), the hidden `rev` input on the nine configuration forms
  (lines 35, 48, 55, 69, 96, 130, 139, 156, 195), each appended to the existing `csrf` input line.

## Notes for the integrator

- Behaviour changes that tests elsewhere could notice: the 421 Host check (e2e suites use 127.0.0.1: unaffected), notice titles for
  server edits, one notice per Access save, `notifications.error` absent for token callers, `SameSite=Lax`, `rev` accepted when
  absent. `grep` of `test/integration`, `test/compose`, `test/loadsim` and `test/harness` found no assertion on any of these.
- `go test ./...`, `go vet ./...`, `gofmt -l .` (LF tree) and `GOOS=linux go build ./...` are clean on Windows; `-race` was not run
  here. The new limiter tests use goroutines (`TestParallelLoginsAreLimited`), worth a `-race` run.
- The build-tagged suites compile against the branch: `go vet -tags integration ./test/integration/`, `-tags compose
  ./test/compose/` and `-tags loadsim ./test/loadsim/` are clean (compile only; the suites were not run here).
