# Changes r0-j: test infrastructure safety, CI, release and build hygiene

Branch `sim/r0-j` (base `78ef2d8`), fixer j, 2026-10-10. Nine commits, one per
item; every commit passes `gofmt -l .`, `go vet` (plain and with the suite
tags), `GOOS=linux go build/vet`, `go mod tidy -diff` and `go test ./...` on
Windows with Go 1.27.2. No file outside this fixer's ownership was touched.
Evidence and logs: `the test kit` and `j-summary.md`.

Files changed: `test/compose/docker-compose.yml`, `test/compose/compose_test.go`,
`test/loadsim/docker-compose.yml`, `test/loadsim/loadsim_test.go`,
`test/harness/syncthing.go`, `test/harness/kill_windows.go` (new),
`test/harness/kill_unix.go` (new), `test/harness/syncthing_test.go` (new),
`test/integration/integration_test.go`, `.github/workflows/ci.yml`,
`.github/workflows/release.yml`, `.github/dependabot.yml` (new), `Dockerfile`,
`go.mod`, `.gitattributes` (new), this file.

## 1. The Compose and loadsim stacks contact public Syncthing infrastructure (tests.md O1, safety)

**Root cause.** Both compose files started `syncthing/syncthing:2.1.5` with its
defaults. `compose_test.go` switched discovery, relays, NAT traversal and the
reports off with `PATCH /rest/config/options` only after `WaitReady`, so every
first start of a stack had already set up the global discovery clients, the
relay listener (`relays.syncthing.net`) and NAT detection; `loadsim_test.go`
never switched anything off, so the load NAS ran exposed for the whole run.

**What changed.**
- `test/compose/docker-compose.yml`: the `x-syncthing` anchor (inherited by
  `e2e-akka`, `e2e-sol`, `e2e-mortis`) gets an `entrypoint` that, before the
  first start, runs `syncthing generate --no-port-probing` as `PUID:PGID` when
  there is no `config.xml` yet, then on every start patches
  `globalAnnounceEnabled`, `localAnnounceEnabled`, `relaysEnabled`,
  `natEnabled` and `crashReportingEnabled` to `false`, `urAccepted` to `-1` and
  `autoUpgradeIntervalH` to `0` (the same keys `test/harness/syncthing.go`
  patches natively), verifies each with `grep` and exits 1 if any is still on,
  logs one `syncthing-init:` line and `exec`s the image's own
  `/bin/entrypoint.sh /bin/syncthing` (the image has no `CMD`; `STGUIADDRESS`
  and `STGUIAPIKEY` keep working). `sed` runs as `PUID:PGID` so the file stays
  owned by the Syncthing user.
- `test/loadsim/docker-compose.yml`: the same entrypoint on `load-nas`.
- `test/harness/syncthing.go`: new `Instance.CheckIsolated(ctx)` reads
  `/rest/config/options` and returns an error naming every setting that is
  still on. `compose_test.go` calls it for all three containers instead of the
  PATCH; `loadsim_test.go` calls it after `WaitReady`.
- `test/harness/syncthing_test.go`: `TestGenerateIsolates` starts a real
  Syncthing from the harness's `generate` and checks it the same way (skipped
  without `SYNCTHING_BIN`); passes with 2.1.5 and 2.1.6.

**Verification.** (a) Rehearsal of the script in a `--network none` container:
config generated as 1000:1000, all seven values off, no discovery/relay/NAT
lines, only the 8384 and 22000 listeners. (b) Packet check on a throwaway
stack `sw-fix-j-safety` (a `nicolaka/netshoot` sniffer owning the network
namespace, the fixed Syncthing service joined with `network_mode:
service:sniffer`, no published ports, 60 s of `tcpdump -i any`): 20 packets in
total, 16 off-loopback and all of them kernel ICMPv6 MLD/neighbour
solicitation and ARP self-announcements; no DNS query except the one I sent
to prove the capture works; 0 mentions of `syncthing.net`; the namespace's
sockets were only the 8384/22000 listeners and Docker's embedded DNS; the log
had no `Using discovery mechanism`, `Relay listener` or `Detected NAT` line.
Positive control: the stock image under `--network none` logs 15 such lines
within 10 s, including the lookup of `relays.syncthing.net`. (c) The real
suites (item 10): the compose stack logged 6 `syncthing-init` lines (three
containers, two starts each, so the step is idempotent across `restart` and
`stop`/`start`), loadsim 1; no discovery/relay/NAT line anywhere; all three
configs still off after the run.

**User-visible change.** Tests only. A Syncthing container whose config can't
be put into the isolated state refuses to start instead of running exposed,
and each start logs `syncthing-init: ...`. The compose test no longer sends
the PATCH (one `ConfigSaved` event fewer; the read-only-gate subtest still
sees the ones from `SetName`/`AddDevice`/`AddFolder`).

**DECISION.** `CheckIsolated` treats `urAccepted >= 0` as "on": usage
reporting must be explicitly declined (`-1`), the value the native harness
sets. **DECISION.** When a container is not isolated the test fails
(`t.Fatal`) rather than patching it, because by then the first start has
already happened; the entrypoint's own check is what prevents the start.

**README.** Development: say that the Compose suites run their Syncthing
containers with discovery, relays, NAT traversal and reporting off from the
first start, and that `go test ./...` also runs two harness tests against a
real Syncthing when `SYNCTHING_BIN` is set.

## 2. `harness.Instance.Stop` kills only Syncthing's launcher (tests.md O4)

**Root cause.** Syncthing 2.x runs as a launcher process plus a worker child
that holds the GUI and sync ports, with `--no-restart` too, and not only on
Windows. Experiments: Windows 2.1.6, launcher 14076 and worker 48192 owning
both ports; `Stop-Process` on the launcher (what `Process.Kill` does) left the
worker listening until `taskkill /T /F`. Linux (`syncthing/syncthing:2.1.5`,
`serve --no-restart`): pids 22 and 36; `kill -9 22` left 36 listening on 8384
and 22000. When the API shutdown fails (`Env.Close` after a half-started
instance, a hung instance, a wrong key), the old `Stop` orphaned the worker
and the next `Start` on the same ports failed.

**What changed.** `Stop` waits `stopGrace` (15 s, a package variable so the
test can shorten it) for the API shutdown, then calls `killTree` and waits
for the process; afterwards `waitPortsFree` polls `net.Listen` on the GUI and
sync ports for up to 10 s and returns an error if one is still busy. This
replaces the fixed 500 ms sleep on Windows. `kill_windows.go`: `taskkill /T /F
/PID`. `kill_unix.go`: the children from `pgrep -P` get SIGKILL, then
`Process.Kill`. `TestStopKillsProcessTree` makes the API shutdown fail with a
wrong key and asserts both ports can be bound after `Stop`; on the old code
it failed with `port 57749 still in use after Stop` and left a worker behind.

**User-visible change.** Test harness only. `Stop` can now return an error
("port N still in use after stop"); tests that `Fatal` on `Stop` errors
surface it, `Env.Close` ignores it as before. `ReplaceCert` restarts only
once the ports are free.

**DECISION.** The Unix side looks children up with `pgrep -P` instead of
starting Syncthing in its own process group (`Setpgid`) and killing the
group, as the item suggested: a new group would keep an interactive Ctrl-C on
`go test` or `simenv` from reaching Syncthing (today it does, and Syncthing
shuts down by itself), leaving orphans in exactly the situation the fix is
for. `pgrep` is in procps on ubuntu-latest and `golang:1.27`, and in busybox;
without it the behaviour equals the old one plus the port error.

## 3. Configurable integration base port (`SW_IT_BASEPORT`)

`integration_test.go` reads `SW_IT_BASEPORT` (default 18600; GUI ports from
there, sync ports 1000 above, syncwatch 90 above), then the old undocumented
`SW_IT_PORT`, and rejects a base outside 1024..64531 up front. The package
comment documents it with `SW_IT_DIR`. Verified by the item-10 run with
`SW_IT_BASEPORT=18600`. **DECISION.** `SW_IT_PORT` keeps working as an alias.
**README.** Development: document `SW_IT_BASEPORT` and `SW_IT_DIR`.

## 4. loadsim reuses a stale image; CI's "Logs on failure" sees nothing (tests.md O2, O3)

`loadsim_test.go` runs `docker compose up -d --build syncwatch` (the run in
item 10 built `syncwatch:loadsim` fresh). In `ci.yml` the compose test step
sets `KEEP_COMPOSE=1`, the "Logs on failure" step dumps the whole stack with
`--timestamps --tail 400` (per container) and a new "Stack down" step with
`if: always()` runs `down -v --remove-orphans`. The test's own deferred
`down` still runs when `KEEP_COMPOSE` is unset, so local runs are unchanged.
**README.** None (CI only); the Development section's test commands stay.

## 5. CI downloads Syncthing without a checksum (static.md §4)

The integration job now downloads the tarball under its release name and the
release's `sha256sum.txt.asc`, and runs `grep " <tarball>$" sha256sum.txt.asc
| sha256sum -c -` (with `shell: bash`, so `pipefail` applies) before
unpacking. Rehearsed locally with the real v2.1.5 files: OK; a corrupted copy
fails (`FAILED`, exit 1); an asset missing from the list fails (`no properly
formatted SHA256 checksum lines found`, exit 1). **DECISION.** Checksums
only, as the item allows: the PGP signature on that file is not verified
(that needs the Syncthing release key in CI); since the checksum list comes
from the same release page as the tarball, this guards against truncated or
corrupted downloads and asset mix-ups, not against a tampered release.

## 6. CI and the release binaries use Go 1.27.1 (static.md §2 item 1)

`go.mod` now says `go 1.27.2`; `go.sum` is unchanged (56 lines) and
`go mod tidy -diff` is empty. `ci.yml` (and the release test job) run
`go mod tidy -diff` and `go run golang.org/x/vuln/cmd/govulncheck@v1.8.0
./...`, which passes on 1.27.2 (0 reachable, per the baseline) and would
fail on 1.27.1 (12 reachable standard-library fixes). The Dockerfile's
pinned `golang:1.27-alpine` digest is `1.27.2-alpine3.24`, so the image and
the binaries are built with the same Go. **DECISION.** govulncheck is pinned
by `go run ...@v1.8.0` (no go.mod dependency, verified through the checksum
database); Dependabot does not bump it. GO-2026-5932 (x/crypto/openpgp,
module level) does not fail source mode, as the baseline showed.
**README.** Build it yourself: "Go 1.27 or later" should read "Go 1.27.2 or
later" (older patch releases are refused by the `go` directive unless the
toolchain switch is allowed).

## 7. Release workflow: `latest` on rc tags, no gating, caches and credentials in publishing jobs (static.md §3.9, §4)

`release.yml`: a `test` job (gofmt, vet, govulncheck, `go test -race`) that
`image` and `binaries` need; `type=raw,value=latest,enable=${{
!contains(github.ref_name, '-') }}` so `latest` moves only for final
releases; `prerelease: ${{ contains(github.ref_name, '-') }}` on
`action-gh-release`; `cache: false` on every `setup-go`; `persist-credentials:
false` on every checkout in both workflows. All action pins are unchanged
(the baseline verified every SHA against its tag). `.github/dependabot.yml`:
`github-actions` and `gomod`, weekly. zizmor 1.30.1 (offline, default
persona) on the result: only the informational `superfluous-actions` for
`action-gh-release` remains (upstream: 5 medium, 1 high); actionlint: clean.

**DECISION.** `cache: false` also in the release `test` job, so the whole
tag-triggered workflow is cache-free (zizmor's audit is workflow-wide; the
cache saves little on a tag build). **DECISION.** The gate runs the unit
suite, vet and govulncheck, not the integration and compose suites (they run
on every push to `main`; a tag on a commit that failed them is still caught
by the required unit job, and a 10-minute tag build was judged not worth
it). **DECISION.** `action-gh-release` stays (pinned) rather than switching to
`gh release create`; no action was bumped. Dependabot's `docker` ecosystem
was not added (the Dockerfile digests are updated by hand, see item 8) to
keep the PR noise down; it is a one-stanza addition if wanted.
**plan.md.** §2 steps 3–4: an rc tag is now marked pre-release and does not
move `latest`; the release waits for the test job. **README.** Updating:
`latest` means the latest final release.

## 8. Dockerfile: floating base images, `/data` 0755 (static.md §2 item 3, §5)

`# syntax=docker/dockerfile:1@sha256:4edf897a…`, `golang:1.27-alpine@sha256:f92b6ef8…`
(index, version 1.27.2-alpine3.24) and `gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872…`,
all multi-arch index digests from `docker buildx imagetools inspect`, with a
comment on how to update them. `/data` is copied with `--chmod=0700`. Verified:
the build pulls by digest and succeeds; a fresh named volume populated from
the image is `700 65532:65532`.

**User-visible change.** A fresh named volume for `/data` is now 0700 (was
0755), so other users on the host can't read the database, certificate or
secrets directory; bind mounts keep the host directory's mode. Existing
volumes are not changed. The file modes inside `/data` (the 0644 database
the baseline noted) are the store's business, not this item's.
**DECISION.** `test/fakediscord/Dockerfile` keeps floating tags: a test
helper the suites rebuild with `--build`. **README.** Install step 1:
`mkdir data && sudo chown ...` should add `chmod 700 data`; Updating: the
digest-pinning advice now also describes how the image itself is built.

## 9. No `.gitattributes`: Windows checkouts are CRLF and gofmt lists every file (tests.md O5, static.md §3.3)

`.gitattributes`: `* text=auto eol=lf`, and `*.png`, `*.ico`, `*.db` binary.
The index was already LF for all 114 text files and the 16 binaries are
untouched, so `git add --renormalize .` changes nothing in the repo. Existing
Windows checkouts with `core.autocrlf=true` (such as `the repository root`)
keep their CRLF working files until they are re-checked out: after pulling
this, run `git add --renormalize .` (a no-op here) and then, on a clean tree,
`git rm --cached -r . && git reset --hard`, or clone afresh; `gofmt -l .` is
then clean on Windows and the image built there embeds LF templates. Not
done to `the repository root` in this phase (read-only). **README.**
Development: one line for Windows contributors.

## 10. Suite runs after the changes

All three ran from this worktree at the final tree, concurrently (their
ports and project names don't overlap), on 2026-10-10 13:58–14:03:

| Suite | Command | Result |
|---|---|---|
| Compose (`syncwatch-e2e`) | `KEEP_COMPOSE=1 go test -tags compose -v -count=1 -timeout 30m ./test/compose` | pass, 9/9 subtests, 252 s; 6 `syncthing-init` lines, no discovery/relay/NAT line, configs off; `down -v` rc=0 |
| Load simulation (`syncwatch-loadsim`) | `KEEP_LOADSIM=1 go test -tags loadsim -v -count=1 -timeout 60m ./test/loadsim` | pass, 224 s; baseline 12,901 listings in 1.73 s, hourly 1,291 in 129 ms, slowest status 4 ms, probe estimates exact; `syncwatch:loadsim` built fresh; 1 init line; `down -v` rc=0 |
| Integration, Windows, Syncthing 2.1.6 | `SYNCTHING_BIN=$SYNCTHING_BIN_216 SW_IT_BASEPORT=18600 SW_IT_DIR=the test kit go test -tags integration -v -count=1 -timeout 30m ./test/integration` | pass, 14/14 scenarios, 263 s; no discovery/relay/NAT line in any Syncthing log; no process left |
| Unit (Windows, go1.27.2) | `go test -count=1 ./...` with `SYNCTHING_BIN` set | pass (the two harness tests included, with 2.1.5; also run with 2.1.6) |

Neither stack, volume, network, container nor process from these runs
remains. Logs: `the test kit`, `j-loadsim.*`,
`j-integration-216.*`.

## Not fixed, out of scope

- PGP verification of `sha256sum.txt.asc` (optional per the item).
- zizmor's informational `superfluous-actions` (kept the pinned action).
- `test/fakediscord/Dockerfile` base images are not digest-pinned.
- Dependabot `docker` updates are not configured.
- Release provenance/SBOM/signing, the staticcheck/gosec tool-version
  problems, the dead `joinMax`, the distroless tzdata advisory: other
  static.md leads, not in this assignment.
- The compose test still logs only the last 120 lines of the syncwatch
  service through `t.Log`; the CI step now shows the rest.

## README and plan sections to update (summary)

- README Install (step 1: `chmod 700 data`; `/data` is private in the image),
  Updating (`latest` = final releases), Build it yourself (Go 1.27.2),
  Development (`SW_IT_BASEPORT`, `SW_IT_DIR`, `KEEP_COMPOSE`/`KEEP_LOADSIM`,
  isolated Compose stacks, harness tests need `SYNCTHING_BIN`, Windows LF
  note).
- plan.md status rows "Tests"/"CI" (govulncheck and tidy in CI, release gated
  on tests) and §2 steps 3–4 (rc tags are pre-releases).
