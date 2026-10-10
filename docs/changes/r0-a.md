# r0-a: Syncthing client TLS pinning and error text

Branch `sim/r0-a`, base `78ef2d8`. Map: `01-client-gate-probe.md` (sections 3.1-3.4, 3.8, hunches 1, 5, 11, 14, 17, 20) and `02-collector.md` (hunch 7, "A CA-signed server is never pinned").

Files owned by this fixer: `internal/stclient/*.go`, `cmd/syncwatch/main.go` (probe `--pin`), `internal/gate/gate.go` (`verify()`).
Hunks outside them, all as small as they go (see item 1): `internal/gate/gate.go` `Options` (+1 field, 1 comment), `internal/collector/collector.go` `newCollector` (+1 line, -1 line), `internal/app/gate.go` `RunGate` (-1 line), `internal/web/views.go` `serverSettings` (2 lines).

Checks before every commit: `gofmt -l .` empty, `go vet ./...` and `go vet -tags integration,compose,loadsim ./test/...` clean, `go build ./...` native and `GOOS=linux` (amd64, arm64), `go test -count=1 ./...` green on Windows. `-race` and the integration/compose/loadsim suites are left to the integrator.

## 1. SECURITY: a CA-signed server is never pinned (map 01 hunch 1 and 17, map 02 hunch 7)

**Root cause.** `stclient.(*Client).verify` returned early when the leaf chained to a trusted CA and never read or set the pin. So a server first seen with a CA-signed certificate had `pin == ""` for ever; when a self-signed certificate later appeared at the same URL (a machine in the path, or a downgrade), the TOFU branch pinned it silently and the API key went to it. No H3, only an Info line. The gate's upstream check (`gate.(*Gate).verify`) had the same shape.

**What changed.**
- `internal/stclient/client.go`: `verify` pins the leaf on first use whether or not it chains to a CA. On a mismatch, if the new leaf chains to a trusted CA for the URL's host name it is accepted, the pin is replaced (`OnPin` is called so the store follows) and an INFO line `certificate renewed` is logged; anything else fails the handshake with `CertChangedError` (H3) as before. `CertInfo.CASigned` is kept and still reports the last presented certificate. `Options.OnPin` is now documented as "called whenever the pin is set or replaced". New `Options.Log *slog.Logger` (nil means `slog.Default()`): the client logs the pin events itself because only it knows which case applies: `trusting self-signed certificate on first use`, `pinned CA-signed certificate`, `certificate renewed` (attributes `server`, `fingerprint`, `expires`, and `old` on renewal).
- `internal/gate/gate.go` `verify`: same rule when Syncthing is not on loopback. New `Options.RootCAs` (nil = system pool) so the CA paths can be tested; production doesn't set it. The gate logs `trusting Syncthing's self-signed certificate on first use`, `pinned Syncthing's CA-signed certificate`, `Syncthing's certificate renewed` (attribute `sha256` in the `AA:BB:…` short form, `old` on renewal). The refusal text is unchanged.
- `internal/collector/collector.go`: passes `Log` to the client and no longer logs "trusting self-signed certificate on first use" from `OnPin` (that text was wrong for a CA-signed pin and for a renewal); `OnPin` only stores the pin.
- `internal/app/gate.go`: the same log line removed from the gate's `OnPin` (it logged `fp[:16]`); the gate logs it in `verify`.
- `internal/web/views.go`: Settings → Servers showed `CA-signed (not pinned)` for a CA-signed server; it now shows the short pin with ` (CA-signed)` appended.
- Tests: `internal/stclient/pin_test.go` (CA-signed then self-signed → `CertChangedError` and no request reaches the server; CA renewal → accepted, pin updated, logged; self-signed → CA-signed upgrade accepted; pinned self-signed unchanged → no callback, no log), `client_test.go` (`TestCASignedCertificateAccepted` now expects the pin to follow the CA-signed certificate; `TestTOFUAndCertChange` checks the client's first-use log line), `internal/gate/pin_test.go` (same flow through the gate off loopback; loopback still trusts anything). The map experiment `TestExpCASignedThenSelfSignedIsTOFU` became `TestCASignedThenSelfSignedRefused` with the expectation inverted.

**User-visible behaviour.**
- A CA-signed server now gets a pin on its first connection after the upgrade (INFO `pinned CA-signed certificate`); Settings shows `AB:CD:…  (CA-signed)` instead of `CA-signed (not pinned)`, and "Forget the pinned certificate" now means something for it.
- A CA-signed server whose certificate turns self-signed (or stops matching the host name / CA) raises H3 with status `cert-changed`, no request and no API key reaches it; before it was trusted silently. The admin accepts it in Settings as for any changed certificate.
- CA renewals (Let's Encrypt and the like, also a new intermediate) are accepted without an alert; one INFO line per renewal.
- A server that goes from a pinned self-signed certificate to a CA-signed one for its host name is accepted the same way (upgrade).
- Through a gate off loopback: Syncthing's CA-signed certificate is pinned in the gate's `syncthing.pin`; a downgrade gives the gate's 502 `certificate changed … delete the gate's syncthing.pin` (still H1 on the syncwatch side, map 01 hunch 6, not in this assignment).
- Unchanged weakness (map 01 hunch 17): a certificate that matches the pin is accepted regardless of expiry or host name, so an expired pinned certificate is still accepted; only `cert_expires` shows it. Trust on first use still applies to whoever answers first after "forget", a URL change with a new key, or a new server.

**README sections to update.** "Security" → the TLS bullet (README.md:250: "CA-signed certificates are verified normally; self-signed ones are trusted on first use and pinned" → every certificate is pinned, a CA-signed renewal is accepted and logged, a downgrade raises H3); "First start" step 2 (README.md:104, "Self-signed Syncthing certificates are trusted on first use and pinned"); "Read-only gate" bullet (README.md:262, "pins Syncthing's certificate on first use when it reaches it at another address": now also CA-signed, renewals followed); the log lines named above if the README ever lists them; `internal/web/templates/settings.html:18` ("Self-signed certificates are trusted on first use and pinned": all certificates); plan.md:35. `docs/api-findings.md` needs nothing.

DECISION: a pin changing to another certificate that chains to a trusted CA for the host is accepted silently (INFO log, pin updated) rather than raising H3. Alternative was H3 on every CA renewal, which with 60-90 day renewals would page the admin for nothing; the CA trust model already covers that case, and it is what the assignment asked for.
DECISION: pin events are logged by the client and the gate (through `Options.Log`, default `slog.Default()`), not by the `OnPin` callers; the collector's and the gate's own log lines were removed instead of reworded. Reason: only `verify` knows whether it was first use, a CA-signed first use or a renewal, and a wrong "self-signed, first use" line on a security-relevant event is worse than a two-line hunk in another file.
DECISION: `internal/web/views.go` was touched (2 lines) because the Settings page would otherwise state that a pinned server is "not pinned". The web owner may reword `(CA-signed)`.

## 2. Pin input normalisation (map 01 hunch 11)

**Root cause.** Pins were compared as exact strings against the lowercase hex that `Fingerprint` produces. A pin typed in upper case or with colons (as `openssl x509 -fingerprint -sha256` prints) never matched; the probe then failed with `TLS certificate changed (pinned B8:3C:…, got B8:3C:…)`, the same short fingerprint twice.

**What changed.**
- `internal/stclient/client.go`: new exported `NormalizePin(s string) (string, error)`: trims, lower-cases, strips a `sha256:` / `SHA256 Fingerprint=` style prefix, removes colons and spaces, and requires exactly 64 hex characters; `""` means no pin. `New` normalises `Options.Pin` and returns `pinned fingerprint: not a SHA-256 fingerprint (64 hex characters)` for a value that could never match. `SetPin` normalises too (a value it can't parse is kept as given, so it never matches, as before).
- `cmd/syncwatch/main.go` `runProbe`: `--pin` goes through `NormalizePin`; a bad value exits with `syncwatch: --pin: not a SHA-256 fingerprint (64 hex characters)` before anything is contacted. Flag help now says upper case and colons are fine.
- Tests: `TestNormalizePin` (forms accepted and rejected, `New` rejecting a bad pin), `TestPinnedSelfSignedUnchanged` (the same pin as lowercase hex, as `AB:CD:…` and as ` sha256:AB:CD:…\n` all match without a callback).
- Settings paths: the only fingerprint the web UI posts is the hidden `fingerprint` field of the accept-cert form (`settings.html:28`), filled from `CertChange.New`, which the client produced; nobody types it. `Manager.AcceptCert` compares it with `CertChange.New` unchanged (owned elsewhere); `SetPin` normalises anyway. The gate's `syncthing.pin` file is machine-written and is not normalised (`gate.New` is not in this assignment).

**User-visible behaviour.** `syncwatch probe --pin AB:CD:…` works; a malformed `--pin` is reported at once. Nothing changes for the dashboard.

**README sections to update.** The probe's flag list, if README documents `--pin`.

DECISION: `New` rejects an unparseable pin instead of running with one that can never match. Stored pins are always canonical (written by `Fingerprint` or by accept), so existing installs are unaffected; the strictness only reaches typed input. `SetPin` stays lenient because it has no error return and its callers pass canonical values.

## 3. cleanErr leaves a leading ': ' (map 01 hunch 5)

**Root cause.** `cleanErr` built `fmt.Errorf("%s: %w", strings.TrimPrefix(ue.Op, "Get"), ue.Err)`; `url.Error.Op` is `"Get"`, so the prefix was always empty and every transport error read `: EOF`, `: connection refused`, `: http: server gave HTTP response to HTTPS client`. The text reached `LastError`, the H1 message and `/api/v1/status`.

**What changed.** `internal/stclient/client.go` `cleanErr` returns `ue.Err` itself (a bare `CertChangedError` when it wraps one, as before). `errors.As` for `CertChangedError` and `errors.Is(err, context.DeadlineExceeded)` keep working (the collector's `shortErr` relies on the latter). Test `TestCleanErr`.

**User-visible behaviour.** Error texts lose the stray `: ` (`EOF`, `dial tcp 127.0.0.1:8384: connect: connection refused`, …). The collector's `shortErr` still shortens the common network phrases, so most dashboard texts are unchanged; the raw form shows for anything it doesn't recognise.

DECISION: no endpoint prefix is added (`/rest/system/status: EOF`); the assignment asked for the plain text, and the endpoint isn't in the message today either.

## 4. Idle connections shorter than the traffic sampling interval (map 01 hunch 14)

**Root cause.** `IdleConnTimeout` 30 s with traffic sampled every 60 s: the sampler's connection was always gone, so each sample dialled and handshook again.

**What changed.** `internal/stclient/client.go`: `IdleConnTimeout` 90 s; `MaxIdleConnsPerHost` stays 4.

**Measured (ad hoc, loopback-only test instances in `the test kit`, tool `idlecheck.go`, outputs `idlecheck-215.txt`, `idlecheck-216.txt`, also copied to `the test kit`):** Syncthing 2.1.5 and 2.1.6 close an idle keep-alive GUI connection after exactly 15 s (Go's `http.Server` with `ReadTimeout` 15 s and no `IdleTimeout`); a second request after 10 s idle reuses the connection. So against Syncthing directly the per-sample handshake remains whatever the client's idle timeout is; the change takes effect where the far side keeps connections longer: the gate (its server has `IdleTimeout` 120 s) and reverse proxies.

**User-visible behaviour.** None visible. Fewer handshakes to a gate.

DECISION: 90 s as asked, with the measurement recorded so nobody expects it to remove the VPN handshake for direct connections. A follow-up that would (not done, outside the assignment): a `tls.ClientSessionCache` on the client's TLS config, so the new connection every 60 s is a resumed handshake (`VerifyConnection` and the pin still run on resumptions); or sampling traffic from the event stream's connection.

## 5. Health sends no API key (map 01 hunch 20 touches it)

**What changed.** `internal/stclient/client.go` `fetchOnce` sets `X-API-Key` on every request, `/rest/noauth/health` included; the `Health` doc says why. `TestOnlyAllowlistedGETs` now requires the key on every path. Syncthing ignores the header on `/rest/noauth/health` (checked on 2.1.5 and 2.1.6 with the `idlecheck.go` tool from item 4: the call answers 200 with the right key, a wrong key and no key, while `/rest/system/status` with a wrong key answers 403).

**User-visible behaviour.** None today: `Health` has no production caller. When fixer b's gate requires the token on every `/rest` path, the client's health call passes.

**Note for fixer b.** `internal/gate/gate_test.go` `TestGateToken` asserts that health needs no token and that no key is forwarded; that test, `ServeHTTP`'s two `pathHealth` exceptions and the README gate bullet are theirs to change.

## 6. Unit tests

All in `internal/stclient/pin_test.go` (helpers: a throwaway CA, a server that swaps its certificate between connections and counts requests that reach it, a locked log buffer) and `internal/gate/pin_test.go`; listed under the items above. Every test uses `httptest` on loopback with random ports; nothing contacts a real network. The old map experiment file is not copied; its scenario lives on as `TestCASignedThenSelfSignedRefused`.

## Not done / out of scope, noticed on the way

- Map 01 hunch 17 (pins ignore expiry and host name once they match): unchanged, see item 1.
- Map 01 hunch 2 (gate re-trusts on an unreadable `syncthing.pin`) and hunch 6 (Syncthing certificate change behind a gate shows as H1): `internal/app/gate.go` and the gate's `ServeHTTP`, not this assignment.
- Map 01 hunch 18 (Windows verifier may go online): not addressed; `RootCAs` is still nil in production.
- `OnPin` is still called with the client's mutex held (map 01, section 2); the store write happens under it as before. Left alone: releasing the lock first would let two concurrent first handshakes race on the pin.
- The probe doesn't pass a logger, so its pin line goes through `slog.Default()` (stderr); `internal/probe/probe.go` isn't in this assignment. One line (`Log: log`) in `probe.Run` would route it with the probe's other lines.
- `internal/gate/gate.go` `New` could normalise `Options.Pin` the way `stclient.New` does; the pin file is machine-written, so it was left to the gate's owner.

## Commits

```
1b986ea perf(stclient): keep idle connections for 90 s
35770e6 fix(stclient): send the API key on /rest/noauth/health too
eb3e353 fix(stclient): drop the stray ': ' from transport error text
80c097d feat(stclient): accept typed fingerprints in any usual form
3d59e44 fix(stclient): pin CA-signed certificates and follow CA renewals
```
plus the commit adding this file.
