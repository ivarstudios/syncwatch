# pp-auth: fixes of the area "auth" (pre-release review)

Branch `sim/pp-auth`, made from `review/sim-2026-10` at `7f825f1`. There are 6 commits, 1 for each finding:

| Finding | Commit | Files |
|---|---|---|
| tokens-2 | `956cfd4` | `internal/store/tokens.go`, `internal/web/api.go`, `internal/web/tokens.go`, tests |
| sessions-2 | `689b6e2` | `internal/web/auth.go`, tests |
| sessions-3 | `c42344b` | `internal/web/auth.go`, tests |
| sessions-4 | `32e1556` | `internal/web/server.go`, tests |
| data-2 | `d73b910` | `internal/store/tokens.go`, `internal/store/store.go` (not a file of this area, see the requests), tests |
| tokens-1 | `b02b959` | `internal/store/tokens.go`, tests |

The new tests of the web package are in `internal/web/pp_auth_test.go`. The new tests of the store are in
`internal/store/tokens_test.go`. Each new test fails on `7f825f1` (outputs in `the test kit`).

## tokens-2: a replace during a request gives the new value a "Last used" time

- **The fault.** A request with an API token read the row by the value (`api.go`), and then recorded the use by
  the ID (`TouchAPIToken(id)`). A Replace keeps the ID and sets a new hash. When the Replace came between the 2 database
  calls, the use of the old value went to the new value. The card then showed "Last used" for a value that no program
  sent. The administrator uses this time to see if Home Assistant has the new value.
- **What changed.** `store.TouchAPIToken(token, at)` takes the value and updates the row with its hash
  (`WHERE hash=?`). `apiAuthorized` gives the trimmed value to `touchAPIToken(t, value)`. After a replace or a revoke,
  the old value changes no row. The store then returns `ErrNoToken`, which `touchAPIToken` already ignores.
- **What the user sees.** "Last used" is only the time of a request with the current value.
- **Test.** `TestAPITokenUseDuringReplace` (web): the clock of the server does the Replace when `touchAPIToken` asks for
  the time, thus exactly between the read and the record. On `7f825f1` the new value gets the time. The store test
  `TestAPITokens` also checks a touch of the old value after a replace.
- **Live (kit s21, Docker).** 4 clients send `GET /api/v1/ha` with a value while the administrator replaces it.
  `7f825f1` (image `rv-tokens`): 4 of 500 replaces gave the new value a time. This build: 0 of 500
  (`live-02-tokens-2-race-pp-auth.txt`, `live-03-tokens-2-race-7f825f1.txt`).

## sessions-2: the global slow-down does not slow parallel guesses from many addresses

- **The fault.** After 30 failures from all addresses in 10 minutes, each try waited up to 5 seconds. But
  the tries from different addresses waited at the same time. Each address kept its 5 failures. A device on the LAN with
  250 IPv4 addresses thus got 1000 checked guesses in approximately 10 seconds, also while the slow-down was active.
- **What changed.** `limiter.limit()` gives the number of failures and parallel tries for each address. The number is 5,
  or 1 while the failures in the window and the tries in progress from all addresses reach 30. The tries in progress
  count, because a burst after a quiet window has no failures at this time. `check` and `retryAfter` use the same limit, thus the
  message of a refused confirmation gives the correct wait. The limiter keeps the count of tries in progress
  (`inflight`). It sets the count to 0 when it removes the addresses (more than 10000).
- **What the user sees.** While the slow-down is active, each address has 1 try in 10 minutes. An address without a
  failure, for example the address of the administrator, can still log in. A password confirmation counts for each
  session, with the same rule. During an attack, 1 incorrect password in a confirmation blocks the confirmations of that
  session for 10 minutes. A new login gives a new session.
- **Test.** `TestLimiterSlowsDownGuessesFromManyAddresses` checks the limiter: 250 addresses, a burst after a quiet
  window, `retryAfter`, and the reset at 10000 addresses. `TestLoginSlowDownWithParallelGuessesFromManyAddresses` sends
  3 parallel incorrect passwords from each of 40 addresses through the login form. syncwatch checks 40 and refuses 80.
  Then the administrator logs in at a different address. The mutation tests of each part are in `sessions-2-mutations.txt`.
- **Live (native binary, HTTPS on 127.0.0.1:24420, source addresses 127.0.1.1 to 127.0.1.250).** After 40 failures
  from 40 addresses, 250 addresses send 4 incorrect passwords each at the same moment. `7f825f1`: 1000 checked (401), 0
  refused. This build: 250 checked, 750 refused (429). The administrator at a new address logs in (303) on both
  (`live-05-sessions-2-native-7f825f1.txt`, `live-06-sessions-2-native-fix.txt`).
- **Not changed.** 1 guess for each address in 10 minutes is still 250 guesses for 250 addresses. A global queue of
  password checks is the stronger option of the finding, but the administrator then waits in the queue during an
  attack. IPv6 addresses still count one by one (known item, `plan.md:153`).

## sessions-3: a long session_ttl for the lobby screen also gives long administrator sessions

- **The fault.** `startSession` used `access.session_ttl` for each role. The README tells to set `365d` for a
  lobby screen. An administrator closed the browser on a shared PC without "Log out". Then the browser kept an
  administrator session of 365 days, with a persistent cookie and no idle time limit.
- **What changed.** `startSession` gives an administrator session at most `maxAdminSession` (30 days, the default of
  `session_ttl`). A viewer session keeps `session_ttl`. `resolve` refuses an administrator session that is older than
  30 days, also a session that an older version made with a longer `session_ttl`.
- **What the user sees.** The administrator logs in again after 30 days at most. The lobby screen with the viewer
  password stays logged in for `session_ttl`.
- **Test.** `TestAdminSessionLasts30DaysAtMost`: with `session_ttl: 365d`, the viewer cookie has `Max-Age=31536000`, the
  administrator cookie `Max-Age=2592000`. On day 31 the administrator session and a session of an older version get the
  login page. On day 364 the lobby screen still works.
- **Live (kit s21).** An import sets `session_ttl` from 30d to 365d ("Access (session_ttl): 30d → 365d"). The lobby
  screen gets `Max-Age=31536000`. The administrator on the shared PC gets `Max-Age=2592000`
  (`live-01-sessions-3-4.txt`).

## sessions-4: a browser with a viewer session cannot log in as the administrator

- **The fault.** `deny` answered 403 "This page needs the administrator password." to a viewer, and
  `handleLoginPage` sent each session with a role back to `next`. Thus the browser of a lobby screen never got the login
  form. The only way was "Log out", which also logged out the lobby screen and lost the page.
- **What changed.** `deny` sends a viewer on a page (GET or HEAD) to `/login?next=…`, as a person without a login.
  The error message is "This page needs the administrator password." A form (POST) of a viewer session keeps the 403.
  `handleLoginPage` sends only an administrator session to `next`. A viewer session gets the form.
- **What the user sees.** An administrator page in a browser with a viewer session shows the login form with the
  reason. After the login with the administrator password, the browser shows the page. With open access and no
  administrator password, an administrator page now sends the browser to the first start (`/setup`). This also corrects
  the known item W-090.
- **Test.** `TestViewerSessionGetsTheAdministratorLogin` checks the redirect and its message, and the form with
  `next`. It also checks the viewer password on that form, the administrator login, and a POST of a viewer session (403).
  The last part is open access without an administrator password (`/setup`).
- **Live (kit s21).** The lobby browser: `GET /settings` gives 303 to `/login?next=/settings`. The login page shows "✖
  This page needs the administrator password." with the form. The administrator password then gives `/settings` (200).
  A POST of a viewer session gets 403 (`live-01-sessions-3-4.txt`).

## data-2: after a downgrade and a new update, the API token of the older version is refused

- **The fault.** The move of the API token of an older version (the secret `api.token`) ran only while
  `api_tokens` had no row. After an update, a downgrade, and a new API token in the older version (as
  `docs/CHANGES-review.md` tells), the next update left that value a secret. The API refused it (401 for Home
  Assistant). The value from before the downgrade, which the older version refused, worked again with the scope Full.
  The button "Revoke old API token" then deleted the value in use.
- **What changed.** `migrateAPIToken` runs at each open, also when `api_tokens` has rows. The new
  `moveOldAPIToken` (`tokens.go`) has the rule of an imported `api.token`:
  - a value that an API token has already: syncwatch only deletes the secret;
  - else the value replaces the value of the API token "API token" (the name without regard to case). Its scope stays,
    "Made" is the time of the secret, and "Last used" is empty;
  - else, with fewer than 5 API tokens, the value becomes the API token "API token" with the scope Full;
  - else the value stays a secret, and Settings → API shows it as refused.
  `store.go` only calls `moveOldAPIToken` instead of its own insert.
- **What the user sees.** After a downgrade, the API token that the administrator makes in the older version works
  after the next update. The value from before the downgrade gets 401.
- **Test.** `TestOldAPITokenAfterDowngrade` does an update, a downgrade and an update again. It also checks the scope
  that stays, a value that an API token has, and 5 API tokens. The end of `TestOldAPITokenMigrated` checked the old rule ("a secret api.token stays a
  secret") and now checks the new rule.
- **Live (kit s21, the real image of r1 `review-r1-75d23db` and this build on one volume).** r1 refuses the kit's API
  token V1 and makes V2. After the update, V2 gets 200 on `/api/v1/status` and `/api/v1/ha`, and V1 gets 401. The card
  shows "API token", Full, made at the time of V2, and no line about an older version
  (`live-04-downgrade-data-2-tokens-1.txt`, steps 0 to 2).
- **Decision changed.** `docs/changes/r2-5.md` had the decision "a decryptable `api.token` that the move did not take
  … is not accepted". This build moves it at the next open. It stays refused only with 5 API tokens and none named "API
  token" (see tokens-1).

## tokens-1: a refused API token of an older version becomes a Full API token after a restart

- **The fault.** The move ran at each open while `api_tokens` had no row. Settings showed a value of an older
  version as refused ("syncwatch does not accept it"). The administrator revoked each API token. At the next restart, the
  value became the API token "API token" with the scope Full. There was no settings notice and no log line.
- **What changed.** With the data-2 correction, syncwatch moves such a value at the update. Thus syncwatch accepts
  it, and a revoke ends it. A
  value stays refused only with 5 API tokens and none named "API token". For that case, `moveOldAPIToken` keeps the time
  of the secret in the setting `api.token_refused`. A later open does not move that secret, also with fewer API tokens.
  "Revoke old API token" deletes the secret. A new value of an older version has a different time, and syncwatch moves
  it at the next open.
- **What the user sees.** A value that Settings shows as refused stays refused until the administrator revokes it.
- **Test.** `TestRefusedOldAPITokenStaysRefused` checks the scenario of the finding, which fails on `7f825f1`. It also
  checks 5 API tokens with a revoke and a restart, which fails on `d73b910` (the data-2 commit).
- **Live (kit s21).** Steps 3 and 4 of `live-04-downgrade-data-2-tokens-1.txt`: after the revoke of each API token and
  `docker restart`, V2 and V1 get 401. With 5 API tokens, the value V3 of r1 gets 401, and the card shows "The database
  has an API token of an older version. syncwatch does not accept it. Revoke it." The administrator revokes 1 API token,
  and syncwatch starts again. Then the administrator revokes all API tokens, and syncwatch starts again. V3 still gets
  401, and the card still shows the line.
  "Revoke old API token" removes the line.
- **The test of the finding.** The reviewer's test `TestRVVTokens1R_RefusedOldTokenBecomesFullAfterRestart` stops at
  step 3 on this branch, because it checks that the update refuses the value of the older version. That is the old
  rule of data-2. With step 3 as a log line, steps 4 and 5 give 401 (`tokens-1-reviewer-test-step3-as-log.txt`).

## Lines for docs/CHANGES-review.md

At the end of "Good to know before a downgrade" (line 35):

> At the next update, the API token that you made in the older version is the API token "API token" of this version.
> The value from before the downgrade then gets `401`.

New items for this stage:

> - **Login limit:** after 30 failures from all addresses in 10 minutes, each address can have only 1 failure in 10
>   minutes. Before, 250 addresses of a LAN made 1000 guesses in 10 minutes. An address without a failure can still log
>   in.
> - **Administrator sessions:** an administrator session ends after 30 days at most, also with a longer
>   `access.session_ttl`. A viewer session keeps `access.session_ttl`, for example 365 days for a lobby screen.
> - **Viewer sessions:** an administrator page in a browser with a viewer session shows the login form. Before, it
>   showed the error page "Not permitted".
> - **API tokens:** the time "Last used" in Settings → API is only the time of a request with the current value.
>   Before, a request with the old value during a replace sometimes gave the new value a time.

## Lines for README.md

- Security → **Login limit** (line 516). Replace the sentence "Failures from many addresses only make logins slower,
  thus no person can block the administrator from a different address." with these sentences:
  > After 30 failures from all addresses in 10 minutes, each address can have only 1 failure in 10 minutes. Many
  > addresses can thus make only 1 guess each in 10 minutes. An address without a failure, for example the address of
  > the administrator, can still log in.
- Security → **Shared addresses** (line 517), after "These are the confirmations … the API keys.":
  > After 30 failures from all addresses, each session can also have only 1 failure in 10 minutes.
- Lobby page, after step 2 (line 498):
  > An administrator session ends after 30 days at most, also with a longer `access.session_ttl`. Thus use the viewer
  > password on the screen.
- Settings table (line 311), the row of `access.session_ttl`: `30d (an administrator session: 30 days at most)`.
- JSON API (line 587), after "It continues to work.":
  > After a downgrade, make a new API token in the older version. At the next update, this API token replaces the value
  > of the API token "API token", and the value from before the downgrade gets `401`. With 5 API tokens and none with
  > the name "API token", syncwatch does not accept the API token of the older version. Settings → API then shows it.
  > Revoke it there. Then add an API token for the program.

stecheck on these lines: 21 of 21 compliant (100 %).

## Requests to the integrator

1. `internal/store/store.go` is not a file of this area. Commit `d73b910` changes only `migrateAPIToken`. It removes the
   guard "return when `api_tokens` has a row". The insert is a call of `moveOldAPIToken` (`tokens.go`). syncwatch deletes
   the secret only when `moved` is true. The comment gives the new rule.
2. `internal/notify` `TestUndecryptableNoticesWebhook` fails on `7f825f1` and on this branch: the test depends on the
   day. `sim/pp-notify` corrects it (`480b6f4`, notify-4). The tests of all other packages are correct.
3. `internal/web/server.go` also changes on `sim/pp-notify` (line 750) and `sim/pp-settings` (line 586). This branch
   changes `deny` (line 284) and `handleLoginPage` (line 466). The hunks do not overlap.
4. The move of the API token of an older version at the update writes no log line: the store has no logger. The card
   shows the API token "API token" with "Made" at the time of the older version. If a log line is necessary, `app` can
   compare `APITokenByName("API token")` before and after `store.Open`.

## Checks before each commit

`gofmt -l .` empty; `go vet ./...`; `go vet -tags integration,compose,loadsim ./test/...`; `GOOS=windows` and
`GOOS=linux CGO_ENABLED=0 go build ./...`; `go test -count=1 ./...` (the tests of all packages are correct, except
the notify test of request 2). Outputs: `precommit-<finding>.txt`.

## Evidence

`the test kit`: the tests that fail on `7f825f1` (`*-test-7f825f1.txt`), the tests on the corrected code,
the pre-commit checks, the live runs `live-01` to `live-06`. Scripts: `the test kit`.
