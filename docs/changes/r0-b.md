# r0-b: read-only gate hardening

Fixer b. Branch `sim/r0-b`, base 78ef2d8. Map: `01-client-gate-probe.md`, sections 3.7-3.9 and hunches 2, 3, 6, 8, 15.

Files changed: `internal/gate/gate.go` (everything except `verify()`), `internal/gate/gate_test.go`, `internal/app/gate.go`, `internal/app/gate_test.go`, `cmd/syncwatch/main.go` (the gate command only), `cmd/syncwatch/main_test.go`, `Dockerfile` (ENV), `examples/gate-compose.yml`. Outside my files: one line in `test/compose/docker-compose.yml` (item 6).

Commits, oldest first:

```
4954e73 fix(gate): check the token before anything else, health included        (item 2)
0a65163 fix(gate): allow only syncwatch's query parameters, and browse only under folder parents  (item 1)
d0dd67b fix(gate): log refused requests once a minute per client and reason      (item 3)
e4466b0 fix(gate): timeouts toward Syncthing and limits on the gate's own server (item 4)
29626c5 fix(gate): a short, self-contained 502 body for a changed Syncthing certificate (item 7)
ca5cbc2 fix(gate): refuse to start when the Syncthing pin file can't be read     (item 5)
2874a79 fix(gate): own listen address (SYNCWATCH_GATE_LISTEN) and extra browse roots (item 6, and item 1's env variable)
```

Checks: `gofmt -l .` empty, `go vet ./...` and `go vet -tags integration,compose,loadsim ./test/...` clean, `go test ./...` passes on Windows, `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build ./...` ok. The built gate was also run against a loopback Syncthing 2.1.5 on Windows (native binary, discovery/relays/NAT/reporting off, ports 24050/24060) and the browse semantics were checked on 2.1.5 in a `--network none` container; see item 1.

Note on commit ca5cbc2: its message says the 64 KiB header cap is "tested here"; that line and its test actually went in with e4466b0. Not amended (rule).

## 1. Gate passes any query (hunch 3, security)

**Root cause.** `ServeHTTP` copied `r.URL.RawQuery` to the upstream request verbatim (gate.go:170 at 78ef2d8). A token holder could list any directory (`/rest/system/browse?current=/`), pass `filesystem=fake`, and open a Syncthing event subscription per distinct `events=` mask (Syncthing keeps a buffered subscription per mask, with its own goroutine and 1000-event buffer, until it restarts).

**What changed** (`internal/gate/gate.go`): `checkQuery` parses the query with `url.ParseQuery` and allows, per endpoint, only the parameters syncwatch's client sends; the forwarded query is rebuilt from the checked values (`url.Values.Encode()`, so keys are sorted and `,` becomes `%2C`; Syncthing decodes both the same).

| Endpoint | Parameters | Value rules |
|---|---|---|
| `/rest/system/browse` | `current` (required) | must end with a separator; must lie under a root (below) |
| `/rest/events` | `since`, `timeout`, `events` | `since`: integer ≥ 0; `timeout`: integer 0-600; `events`: exactly the collector's mask (`stclient.EventMask`), any order |
| `/rest/db/status` | `folder` | |
| `/rest/db/completion` | `folder`, `device` | |
| `/rest/folder/errors` | `folder`, `page`, `perpage` | `page`, `perpage`: integer ≥ 1 |
| every other endpoint | none | `?` alone is fine |

Anything else, a repeated parameter or a malformed query (`%zz`, `;`) → `400 syncwatch gate: query not allowed: <reason>`. All three event parameters are optional: syncwatch always sends them, the compose e2e test sends `since`+`timeout` only (Syncthing then uses its default subscription).

Browse roots: the directories the token may list are the parents of Syncthing's configured folder paths plus the extra roots from `SYNCWATCH_GATE_BROWSE_ROOTS` / `--browse-roots` (comma-separated; `cmd/syncwatch/main.go`, `app.GateOptions.BrowseRoots`, `gate.Options.BrowseRoots`). The gate reads `/rest/system/status` (for `pathSeparator`) and `/rest/config/folders` with Syncthing's key on the first browse request, again when the list is older than 10 minutes, and once more (at most once a minute) when a request's directory isn't under the list, so a folder that was just added works at once. A failed read is retried after 30 s; while no list has ever been read, browse answers 502 (`Syncthing's folder list can't be read: …`), and an old list stays in use after a later failure. `New()` still doesn't contact Syncthing.

Path handling (`browsePath`, `parseBrowsePath`): the path is split into a root (`/`; on a `\` server also `C:/` and `//` for UNC) and elements; empty and `.` elements are dropped, `..` pops (and stays at an absolute root); when Syncthing's separator is `\`, both `/` and `\` separate and names compare case-insensitively (`strings.EqualFold`); on a `/` server, `\` is an ordinary character. A request is allowed when its cleaned path equals a root or lies inside one. **The cleaned path, rendered with Syncthing's separator, is what Syncthing gets**, so what was checked is what is listed. A folder's parent that is `/` is not a root (syncwatch's `DetectShares` never lists `/` either); a Windows drive root (folder `C:\Sync` → root `C:\`) is, as in `DetectShares`. Relative folder paths with no parent (`Sync`) give no root; `~/Sync` gives the root `~`.

Checked against Syncthing 2.1.5: on Windows (native) both separators work, a trailing `/` or `\` lists the directory, no trailing separator lists names in the parent that start with the last element, `..` climbs out (`…\Shares\..\` lists the parent), `.` is a no-op, names compare without case, `\\?\` prefixes work, `~\` lists the home directory, an empty `current` lists the drive letters, `.\`, `..\` and `D:` resolve relative to Syncthing's working directory, and `filesystem=fake` switches to the fake filesystem. On Linux (container) only `/` separates, `\` is literal, `..` and `//` and `./` collapse, case matters, `/` lists the root. The gate refuses all the escapes (`..` out of a root, `~`, empty, relative, drive-relative, `\\?\`, `filesystem=`) and allows the in-root forms.

**User-visible.** Through a gate, a structure scan behaves as before for shares that are the parents of Syncthing's folders and everything under them. Differences:
- Listing anything else gives `403 syncwatch gate: directory not under a Syncthing folder's parent`; the collector's `shortErr` shows that as the share's error. Shares from `shares.add`, and shares found with `share_type_from_path` that lie more than one level above a folder, need `SYNCWATCH_GATE_BROWSE_ROOTS` on that gate.
- The collector's `exists()` check for a share that lists empty browses the share's parent (`structure.go:290`), which the gate refuses; `exists` treats an error as "present", so a missing share shows as empty instead of "not found on the server (is it mounted…)". Only the diagnostic text is lost, and only behind a gate. The collector's owner may want to special-case a 403 there, or the operator adds the parent to `SYNCWATCH_GATE_BROWSE_ROOTS`.
- A directory symlinked out of a root is still listed (Syncthing follows it); the gate only sees paths.
- `syncwatch probe` through a gate now gets a 400 on its `StartupComplete` long-poll (that mask was never allowlisted anyway).
- An events request with a mask other than the collector's full mask is refused (DECISION below). `stclient.Client.Events` always sends the full mask in the collector and the probe's prime and listen phases.

**README** (Read-only gate section): list the parameter rules and the browse restriction, document `SYNCWATCH_GATE_BROWSE_ROOTS` (and that `shares.add` directories need it), the 400/403 bodies, and that the gate reads Syncthing's folder list every 10 minutes. Shares and structure rules section: note the gate limitation for `share_type_from_path` and `shares.add`.

- DECISION: `events` must be exactly the collector's mask (as a set), not any subset. The task said "subset"; with 16 names a subset rule still lets a token holder create up to 65535 distinct Syncthing subscriptions (each ~70 KB and a goroutine, kept until Syncthing restarts, ≈ 4-5 GB), which is the DoS item 1 is meant to stop. syncwatch's client never sends anything else. Loosening this is a one-line change in `sameMask`.
- DECISION: `current` must end with a separator (`/`, or `\` on a `\` server) → otherwise 400. syncwatch's `Browse` always appends one; Syncthing's other form (prefix match in the parent) has no legitimate use through the gate and would need a second set of rules.
- DECISION: the gate forwards the cleaned path rather than the raw one, so the check and the listing can't disagree (e.g. `/a\..\..\` on Linux, `\\?\` on Windows). For every path syncwatch's client sends the cleaned form is identical to the raw one.
- DECISION: on a `/` server a backslash is not treated as a separator (Syncthing doesn't either); on a `\` server both are. "Normalise both separators" is applied where the host does.
- DECISION: roots are read lazily on the first browse request (with the 10-minute refresh and the 1-minute miss refresh) rather than in a goroutine at start: `gate.New` keeps its "does not contact Syncthing" contract, there is no lifecycle to add, and a gate that starts before Syncthing doesn't log a spurious failure.
- DECISION: env variable `SYNCWATCH_GATE_BROWSE_ROOTS` (flag `--browse-roots`), comma-separated, same cleaning as requests; empty entries are dropped.

## 2. The allowlist can be mapped without a token; health needs none (hunch 15, security)

**Root cause.** The path check ran before the token check (404 vs 403 revealed the allowlist), and `/rest/noauth/health` was passed on without any token.

**What changed.** `ServeHTTP` checks the token first for every request except `GET /healthz`; then the method (405), then the path (404), then the query (400/403). Syncthing's `/rest/noauth/health` needs the gate token now; the gate still sends Syncthing no key for it.

**User-visible.** Without the token everything is `403 wrong or missing token`, including POSTs and unknown paths. `/healthz` (the container healthcheck) is unchanged. syncwatch's client doesn't send the key on `/rest/noauth/health` (`stclient.fetch`); `Client.Health` is only used by tests, and fixer a makes the client send it. `TestClientThroughGate` no longer calls `cl.Health` through the gate; put that assertion back once fixer a's change is merged. The compose e2e test's gate expectations (`test/compose/compose_test.go:233-244`: no token → 403, POST → 405, `/rest/config` → 404, `events?since=0&timeout=1` → 200) still hold under the new order; not run here (rule 3).

**README**: Read-only gate bullet "Only GET requests to exactly the endpoints…": say every request needs the token, health included.

## 3. Unauthenticated clients can fill the log (hunch 15, robustness)

**Root cause.** `deny` logged every refusal at WARN.

**What changed.** `deny(w, r, code, why, detail)` keeps a counter per client address and reason (`why` is one of five fixed strings; `detail`, such as the offending parameter name, is only in the body and the log line). A line is written when a client+reason pair is first seen and then at most once a minute, with `count=` the refusals since the previous line and the first refusal's `detail`. At most 4096 counters are kept; when full, pairs not logged for a minute are dropped, or everything is reset. `Gate.now` is a clock hook for tests.

**User-visible.** Log lines read `gate refused a request why="wrong or missing token" detail="" method=GET path=… ip=… count=37`. Refusals within the minute after a line are not logged individually.

**README**: none needed (optionally mention the per-minute log line under the gate).

## 4. No timeouts toward Syncthing, no header limit on the gate (hunch 15, robustness)

**Root cause.** The upstream `http.Transport` had no dial or response-header timeout; the gate's `http.Server` had no `MaxHeaderBytes` (1 MiB default).

**What changed.** Two upstream clients (`transport()`): `hc` with dial 10 s, TLS handshake 10 s, `ResponseHeaderTimeout` 60 s (also used for the gate's own folder-list reads, with a 60 s context); `hcLong` for `/rest/events` with no header timeout, where the request context gets `timeout`+30 s (`eventsWait`; 60 s when no timeout was given; at most 11 minutes). The gate's server adds `MaxHeaderBytes: 64 << 10` (`ReadHeaderTimeout` 10 s and `IdleTimeout` 120 s were already there). `eventsExtra` is a package variable so the test can shorten it.

**User-visible.** A Syncthing that accepts connections but never answers now gives a 502 after 60 s (events: after the long-poll timeout + 30 s) instead of holding the request as long as the caller waits. Requests with more than 64 KiB of headers get Go's 431.

**README**: none needed.

## 5. An unreadable pin file is treated as "no pin" (hunch 2, security)

**Root cause.** `app/gate.go:62` ignored every `ReadFile` error, so a `syncthing.pin` that exists but can't be read (permissions after a `user:` change, I/O error) made the gate trust whatever answered on first use.

**What changed.** Only `os.ErrNotExist` means first use; any other error returns `reading Syncthing's certificate pin: …` and the gate doesn't start. Test: a directory named `syncthing.pin` (unreadable on both OSes).

**User-visible.** The gate refuses to start, with that message, instead of silently re-trusting.

**README**: Read-only gate: the pin file must stay readable by the gate's user (the `user:` line in the compose example).

## 6. The gate's port depends on how it is started (hunch 8, docs/robustness)

**Root cause.** The gate read `SYNCWATCH_LISTEN`, which the image sets to `:8080` for `serve`, so a gate container listened on 8080 while the README and the example said 8385.

**What changed.** `cmd/syncwatch/main.go`: the gate's flags are parsed in `gateOptions` (testable); `--listen` wins, then `SYNCWATCH_GATE_LISTEN`, then `127.0.0.1:8385`; the gate no longer reads `SYNCWATCH_LISTEN`. `Dockerfile`: `ENV SYNCWATCH_GATE_LISTEN=:8385`. `examples/gate-compose.yml`: `SYNCWATCH_GATE_LISTEN=192.168.2.10:8385`, plus `SYNCWATCH_HEALTHCHECK_URL=https://192.168.2.10:8385/healthz` and a commented `SYNCWATCH_GATE_BROWSE_ROOTS`. `test/compose/docker-compose.yml` (fixer J's file, one line): `SYNCWATCH_GATE_LISTEN: ":8080"` on `e2e-sol-gate`, because that stack addresses the gate on 8080 (`url: https://solgate.e2e.invalid:8080`, port mapping `18704:8080`, and the image healthcheck). Switching the stack to 8385 would be three hunks instead; the integrator may prefer that.

**User-visible.** A gate container now listens on 8385 by default (as documented); an existing deployment that relied on the accidental 8080, or set `SYNCWATCH_LISTEN` for the gate, must set `SYNCWATCH_GATE_LISTEN` instead. **The image's healthcheck** (`syncwatch healthcheck`) derives its URL from `SYNCWATCH_LISTEN`, so a gate container is reported unhealthy unless `SYNCWATCH_HEALTHCHECK_URL` points at the gate (the example does that). `healthcheck()` isn't in my files; the integrator may want to make it gate-aware (e.g. try the gate's URL too when `SYNCWATCH_GATE_LISTEN` is set), which would remove that env line.

**README**: Read-only gate: `SYNCWATCH_GATE_LISTEN` (not `SYNCWATCH_LISTEN`), the healthcheck URL line, the compose example's three LAN-address lines; the environment table if there is one.

- DECISION: `SYNCWATCH_GATE_LISTEN` with the precedence flag > env > `127.0.0.1:8385`; the image sets `:8385`. `SYNCWATCH_LISTEN` is ignored by the gate (no fallback), so the two commands can't collide in the same environment again.
- DECISION: the example compose sets `SYNCWATCH_HEALTHCHECK_URL` explicitly rather than changing `healthcheck()` (outside my files).

## 7. A certificate change behind the gate is a 502 whose hint gets cut (hunch 6, correctness)

**Root cause.** The 502 body was `syncwatch gate: Syncthing can't be reached: ` + up to 200 bytes of the error; syncwatch's client keeps 200 bytes of a body, so the "delete syncthing.pin" hint was cut off.

**What changed.** `upstreamError(err, what)` builds every 502 body for an upstream error: when the error text contains "certificate changed" (the phrase `verify()` uses; `verify()` is fixer a's) the body is the fixed `syncwatch gate: Syncthing's certificate changed; delete syncthing.pin in the gate's data dir and restart the gate` (111 bytes); otherwise `syncwatch gate: <what>: <error>` with the error cut at 90 bytes (`maxErrText`), 145 bytes at most. Used for failed requests and for a failed folder-list read.

**User-visible.** H1's message for a server behind a gate whose Syncthing certificate changed now ends with the full instruction. It is still H1 (an outage), not H3; changing that is beyond the gate.

**README**: Read-only gate: what to do when Syncthing's certificate changes behind a gate (the message now says it).

- Contract for fixer a: if `verify()`'s error text stops containing "certificate changed", `upstreamError` must be adjusted (a typed error would be better; I couldn't add one without editing `verify()`).

## 8. Tests

`internal/gate/gate_test.go` (fake upstream records every request; configurable `pathSeparator`, folder list, 500s and delays):
- `TestGateTokenBeforePath`: without the token everything is 403, nothing reaches Syncthing, `/healthz` stays open (item 2).
- `TestGateQueryAllowlist`: 30 query cases, the number of requests that reached Syncthing equals the number of 200s, the forwarded query is the checked one (item 1).
- `TestGateBrowseRoots`: 29 Linux and 21 Windows cases (allowed, cleaned, forwarded form; escapes refused and never forwarded) (item 1).
- `TestGateBrowseRootsRefresh`: read on first use, not within a minute of a miss, after a minute on a miss, after 10 minutes anyway (item 1).
- `TestGateBrowseWithoutFolderList`: 502 while the list can't be read, one attempt per 30 s, nothing forwarded (item 1).
- `TestGateLogsRefusalsOnceAMinute`: 50 refusals → 1 line; other reasons → own lines with detail; after a minute one line with `count=50` (item 3).
- `TestGateUpstreamTimeouts`: transport settings, `eventsWait`, a slow Syncthing is a 502 within the header timeout, a long-poll within its timeout plus the margin isn't (item 4).
- `TestGate502IsShort`: the exact certificate-change body, also for the folder-list read; a down Syncthing's body ≤ 150 bytes; `cleanErr` cut (item 7).
- Existing tests adjusted: `TestGateToken` (health needs the token), `TestGateRemovesSecrets` (full mask; query compared as values), `TestClientThroughGate` (no `cl.Health`).

`internal/app/gate_test.go`: `TestGateRefusesUnreadablePin` (item 5); `TestRunGate` also sends a 70 KiB header and expects 431 (item 4).

`cmd/syncwatch/main_test.go`: `TestGateListen`: `SYNCWATCH_LISTEN=:8080` is ignored, `SYNCWATCH_GATE_LISTEN` and `--listen` precedence, `SYNCWATCH_GATE_BROWSE_ROOTS` splitting (items 6 and 1).

## Not done / for the integrator

- Hunch 4 (the same `HTTP 403 (check the API key)` for a wrong gate token and a stale Syncthing key at the gate) and hunch 16 (the token logged at WARN on creation) were not in my items; unchanged.
- The gate still trusts any certificate on loopback (3.8, by design).
- `TestClientThroughGate`: re-add `cl.Health(ctx)` through the gate after fixer a's client change.
- `test/compose/docker-compose.yml`: the one-line `SYNCWATCH_GATE_LISTEN: ":8080"`; alternatively move the stack to 8385 (URL, port mapping, healthcheck URL).
- `healthcheck()` and the gate's port (item 6).
- `collector.exists()` behind a gate (item 1).
