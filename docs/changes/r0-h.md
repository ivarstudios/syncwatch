# Changes r0-h: app wiring, CLI, store files, TLS serving

Branch `sim/r0-h`, base 78ef2d8. Map: `06-config-store-app-cli.md`. Files touched:
`cmd/syncwatch/main.go`, `cmd/syncwatch/main_test.go`, `internal/app/app.go`,
`internal/app/bootstrap.go`, `internal/app/tls.go`, `internal/app/app_test.go`,
`internal/store/store.go`, `internal/store/secrets.go`, `internal/store/misc.go`
(config revision only), `internal/store/store_test.go`. No file outside the
assignment was edited. README.md and plan.md were not touched; the sections
that need rewriting are listed per item.

Verification beyond `go test ./...` on Windows: the `internal/app`,
`internal/store` and `cmd/syncwatch` tests were also run on Linux
(golang:1.27 container, `--network none`, host module cache), where the
Unix-only tests (file modes, symlink scrub) run for real; several items were
checked end to end with the built binary (notes per item).

## 1. Maintenance commands create a fresh instance at a wrong `--data`

Map hunch 3 (verified): "Maintenance CLI commands create a new, empty instance
at a wrong path and report success."

Root cause: `reset-admin-password`, `import-config` and `export-config` called
`app.OpenStore`, which does `MkdirAll`, creates `secret.key` and an empty
`syncwatch.db` wherever it is pointed. With the wrong `--data` (or the plain
binary's `./data` default in another working directory) the commands
succeeded against a brand-new instance.

What changed (`internal/app/app.go`, `cmd/syncwatch/main.go`): new
`app.OpenExistingStore`, which creates nothing. It fails with
`no syncwatch data in <absolute path>` when `syncwatch.db` is missing, and
with `no secret.key in <path> and SYNCWATCH_SECRET_KEY is not set; use the key
the server runs with` when neither a key file nor the env key exists. The three
commands use it. `reset-admin-password` opens the store *before* prompting, so
a wrong path is reported before a password is typed.

User-visible: exit 1 with the message above instead of a silent new instance;
no directory, key or database is created by the maintenance commands.

DECISION: the maintenance commands also refuse to invent a `secret.key`
(before, a missing key file was silently created). Otherwise `import-config`
run on the host without the server's `SYNCWATCH_SECRET_KEY` would encrypt the
imported secrets under a key the server doesn't have (silent monitoring
outage, map hunch 2). The user must pass `--secret-key`/`SYNCWATCH_SECRET_KEY`
or run the command where the key file is.

README: "Lost the admin password?" and the `import-config` paragraph should
say the commands need the instance's data directory (`--data`/`SYNCWATCH_DATA`,
`/data` in the image) and its secret key, and that a wrong path is an error.

Test: `cmd/syncwatch/main_test.go` `TestMaintenanceCommandsNeedExistingData`.

## 2. `import-config` while serving is undone by the next UI save

Map hunch 4: "CLI import-config against a running server."

Root cause: the serve process never re-read the stored configuration, so the
import was invisible until a restart, and the next UI save wrote the old
in-memory configuration back (after `DropRemovedServers` had already deleted
the removed servers' keys and pins).

What changed:
- `internal/store/misc.go`: `ConfigRevision` / `BumpConfigRevision` on the
  settings table (key `config_revision`, a counter).
- `internal/app/app.go`: every configuration save goes through `saveConfig`
  (settings.Save + bump): bootstrap import, defaults, `ImportFile` and the web
  `SaveConfig` hook. A `configWatcher` worker checks the revision every 10 s
  (`configCheckInterval`); a revision it didn't apply itself is loaded from
  the database and installed with `holder.Set`, which runs `manager.Sync` and
  `engine.Trigger` exactly like a UI save, then `notifier.ChannelsChanged`
  (the import may have brought a new webhook, as the UI import assumes).
- `cmd/syncwatch/main.go`: the import message says "A running syncwatch
  applies it within 10 seconds; otherwise it is used on the next start."

User-visible: `docker exec syncwatch syncwatch import-config /data/new.yaml`
takes effect within 10 s; log line `configuration reloaded: it was changed by
another process (syncwatch import-config) revision=N servers=M`. A UI save
afterwards keeps the import. Checked end to end with the binary (serve +
import-config from a second process: reload logged after 10 s).

DECISION: polling (10 s) instead of file watching or signals; the UI's own
saves never trigger a reload (the watcher tracks the revision it applied); a
revision that can't be applied (corrupt/invalid stored config) is logged as
ERROR once per revision, the running configuration is kept.

README: the bootstrap/import paragraph ("Later changes happen in the UI, or
with ... `syncwatch import-config FILE`") can drop any "restart" wording and
mention the 10 s pickup.

Tests: `store_test.go` `TestConfigRevision`; `app_test.go`
`TestConfigReloadedAfterExternalImport` (two Store handles on one file, as two
processes).

## 3. Bootstrap scrub: symlinks, single-file bind mounts, `null` secrets

Map hunches 5 and 6 (6 verified).

Root cause: `scrubBootstrap` renamed a temp file over the given path. A
symlinked bootstrap file was replaced by a regular file while the target kept
the plaintext (and the log claimed success); a single-file bind mount can't be
renamed over (EBUSY), so the WARN repeated on every start. `walkSecrets`
treated `api_key: null`/`~` as a secret: nothing was imported or scrubbed, and
"still contains secrets" was logged on every start.

What changed (`internal/app/bootstrap.go`, `internal/app/app.go`):
`filepath.EvalSymlinks` first, so the target is rewritten and the link kept;
if the rename fails the file is rewritten in place (`os.WriteFile`, same
mode); the result is re-parsed and an error returned if secrets remain; the
WARN now reads "could not remove the imported secrets from the bootstrap file:
it still holds them in plaintext; delete it or remove them by hand". Plain
scalars with YAML tag `!!null` no longer count as secrets (a quoted `"null"`
still does: it is a string). `renameFile` is a package variable for the test.

User-visible: no false warning for `null`/`~`; symlinked files and bind-mounted
single files are scrubbed. Checked end to end: a Windows-host file bind-mounted
as `/data/syncwatch.yaml` into a `--network none` container was scrubbed in
place (host copy verified).

README: the sentence "If the file can't be rewritten (a read-only mount), the
log says so on every start until you delete it" stays true; may add that
symlinks and single-file mounts are handled.

Tests: `TestBootstrapNullIsNotASecret`, `TestBootstrapScrubFollowsSymlink`
(skips where symlinks are unavailable; ran on Linux and on this host),
`TestBootstrapScrubInPlaceWhenRenameFails`.

## 4. `SYNCWATCH_TLS_SELF_SIGNED=yes` silently serves plain HTTP

Map hunch 7.

Root cause: `envBool` used `strconv.ParseBool` and dropped the error, so
`yes`/`on` meant false.

What changed (`cmd/syncwatch/main.go`): `envBool` returns `(bool, error)`;
accepts `true/false`, `1/0`, `yes/no`, `on/off` (and `t/f`, which ParseBool
accepted) case-insensitively, trimmed; unset/empty is false; anything else is
`SYNCWATCH_TLS_SELF_SIGNED must be true or false, not "maybe"`. `serve` and
`main` (for `SYNCWATCH_ALLOW_ANY_WEBHOOK`) exit 1 with that message.
`healthcheck` ignores a bad value (serve wouldn't have started).

User-visible: `yes`/`on` now serve HTTPS; a typo is a startup error naming the
variable instead of plain HTTP with a green healthcheck.

DECISION: `t`/`f` stay accepted for compatibility with values that worked
before.

README: the HTTPS section ("Plain HTTP: `SYNCWATCH_TLS_SELF_SIGNED=false`")
should list the accepted values.

Test: `TestEnvBool`; `TestHealthURL` gained the `yes` case.

## 5. `--version`, `-h`, `--help` routed to serve

Map hunch 20 (dead cases, verified).

Root cause: any first argument starting with `-` selected `serve`, so the
`--version`/`-h`/`--help` cases in `main` were unreachable; `--version` hit
`flag.ExitOnError` (exit 2), `-h` printed only serve's flags.

What changed (`cmd/syncwatch/main.go`): routing extracted into `command()`:
`-version`/`--version` -> `version`, `-h`/`-help`/`--help` -> `help`; other
flags still mean serve; `syncwatch serve -h` still prints serve's flags.

User-visible: `syncwatch --version` prints `syncwatch <ver>` and exits 0;
`syncwatch -h` prints the full usage and exits 0. Checked with the binary.

README: wherever the CLI is listed, `--version` can be mentioned.

Test: `TestCommandRouting`.

## 6. `secret.key` without O_EXCL; database files 0644

Map hunch 17.

Root cause: `LoadKey` wrote the key with `os.WriteFile`, so two first starts
at once could overwrite each other's key; SQLite creates `syncwatch.db` (and
so `-wal`/`-shm`) 0644 although it holds the argon2 hashes, session CSRF
tokens and the encrypted secrets.

What changed:
- `internal/store/secrets.go`: `createKeyFile` opens with
  `O_WRONLY|O_CREATE|O_EXCL`, mode 0600. A process that loses the race reads
  the winner's key (retrying for up to 1 s while the winner finishes writing).
- `internal/store/store.go`: after opening, on non-Windows, `chmod 0600` on
  the database, `-wal` and `-shm` (best effort, errors ignored). SQLite gives
  later WAL/SHM files the database file's mode. The data directory itself is
  never chmod'ed.

User-visible: `ls -l /data` shows `-rw-------` for the three database files
(existing installations too, from the next start). Confirmed on Linux: against
the upstream `store.go` the test reports `-rw-r--r--` for all three files.

DECISION: the chmod runs on every open, not only on creation, so existing
deployments are tightened too; a file owned by another user (chmod fails) is
left as is.

README: the Docker/data-directory section can say the database files are
owner-only (and that `/data` keeps the host's directory mode).

Tests: `TestKeyFileCreatedOnce` (direct O_EXCL check plus 8 concurrent first
starts), `TestDatabaseFilesOwnerOnly` (skipped on Windows; ran on Linux).

## 7. Shutdown waits for open SSE tabs; store closed under running workers

Map hunch 19.

Root cause: nothing ended the `/events` handlers, so `hs.Shutdown` used its
whole 5 s whenever a dashboard tab was open (then up to 15 s for workers;
Docker SIGKILLs at 10 s). On an `hs.Serve` error `Run` returned at once and
the deferred `st.Close()` ran while collector, engine and notifier still used
the store.

What changed (`internal/app/app.go`): the HTTP server gets a `BaseContext`
that is cancelled first on shutdown, so every request context (the SSE
handlers select on it) ends and `Shutdown` returns at once. Workers run under
a `sync.WaitGroup`; on every exit path the context is cancelled and the
workers are waited for (10 s at most, then a WARN) before the store closes;
a Serve error is logged (`the web server stopped`) and returned after that.
`Options.OnListen` (for tests) reports the bound address.

User-visible: `docker stop` with tabs open takes well under a second instead
of 5 s+. Measured in the test: 0.02 s with the fix, 5.0 s without.

DECISION: the worker wait is 10 s in total (was 5 s per worker, 15 s worst
case) to stay inside Docker's 10 s stop window; in-flight requests at
shutdown see a cancelled context (same as a client disconnect).

README: nothing required; the "Shutdown" row of any ops notes can drop the
"about 5 s" caveat.

Test: `TestShutdownClosesEventStreams` (uses `access.viewer: open` so the
stream needs no login). The Serve-error path has no unit test: it can't be
triggered without reaching into the listener; verified by reading.

## 8. TLS: certificate fixed at start, useless SANs, no HTTP/2

Map hunch 15 (the TLS_HOSTS regeneration was verified).

Root cause: `tls.Config.Certificates` was loaded once; the self-signed
certificate was only renewed on start; `SYNCWATCH_TLS_HOSTS` entries such as
`[fd00::1]` or `nas:8080` became DNS SANs that `VerifyHostname` can never
match, so a new certificate was made on every start; no `NextProtos`, so
HTTP/1.1 only (about six connections per host in browsers, one per open tab's
event stream).

What changed (`internal/app/tls.go`):
- `certSource` serves `GetCertificate` (no static `Certificates`, so it is
  consulted for IP-address connections without SNI too). Own certificate
  files are re-read when either file's mtime changed, checked at most once a
  minute at handshake time; a pair that doesn't load (certbot writes the two
  files one after the other) keeps the previous certificate and is retried at
  the next change. Log: `reloaded the TLS certificate ... sha256=`.
- Self-signed: a daily check (at most once a day, on a handshake) runs
  `ensureSelfSigned` with the names computed at start; with fewer than 30 days
  left a new certificate is written and served. Log: `renewed the self-signed
  certificate; browsers ask to trust it again`.
- `tlsHosts` normalises extra entries (`hostName`): brackets and a numeric
  port are stripped; entries that are neither an IP address nor a DNS name
  (letters, digits, `-`, `_`, dots; no wildcards) are skipped with a WARN
  `ignoring a SYNCWATCH_TLS_HOSTS entry ...`.
- `NextProtos: h2, http/1.1`. The gate (`RunGate`, `ServeTLS`) shares
  `tlsConfig` and so gets the reload, the renewal and h2 as well.

User-visible: certbot-style renewals need no restart; a long-running
container never serves an expired self-signed certificate; `[::1]:8080` in
`SYNCWATCH_TLS_HOSTS` works and the certificate is kept across restarts;
browsers use HTTP/2 (one connection for all tabs). Checked end to end in a
`--network none` container with curl: `/healthz`, the overview page and a
streaming `/events` all over HTTP/2.

DECISION (h2): enabled; Go's bundled HTTP/2 server supports flushing, so SSE
works unchanged (verified with curl). DECISION (renewal): the running renewal
uses the names from start; a `public_url` host changed in the UI is still only
covered after a restart (as documented), and a renewal means browsers ask to
trust the certificate again (unavoidable with self-signed). The expiry check
is lazy (on a handshake, at most daily): a server nobody connects to renews at
the next handshake, which is when it matters. DECISION (TLS_HOSTS): invalid
entries are skipped with a warning rather than failing startup.

README: the HTTPS paragraph should say own certificate files are reloaded on
change (checked once a minute), the self-signed certificate is renewed while
running, `SYNCWATCH_TLS_HOSTS` accepts `[v6]`/`host:port` forms (ports are
ignored, wildcards not), and the dashboard speaks HTTP/2.

Tests: `TestSelfSignedCertificate` (adapted to `GetCertificate`, checks h2),
`TestTLSHostsNormalised`, `TestCertificateFilesReloaded`,
`TestSelfSignedRenewedWhileRunning`, `TestHTTP2Served`.

## 9. Default data dir `./data` relative to the working directory

Map 3.9 "Data directory".

What changed (`internal/app/app.go`): `Run` resolves `DataDir` with
`filepath.Abs` first, so `syncwatch started ... data=` and every derived path
in later log lines are absolute; `OpenExistingStore` reports the absolute
path too (item 1). The default itself is unchanged.

User-visible: `data=D:\...\data` / `data=/home/x/data` in the start log.

README: the data-directory section (undocumented `SYNCWATCH_DATA`/`--data`,
per the map) should state the default: `/data` if it exists, otherwise
`./data` relative to the working directory, shown resolved at start.

Test: covered by the CLI test's absolute-path assertion; the serve log line
was checked with the binary.

## Not fixed / out of scope

- Serve-error path (item 7): code changed and reviewed, no unit test (see
  above).
- `usage()` text still lists `syncwatch version` only; docs phase.
- The notifier is woken with `ChannelsChanged` on every external reload even
  when no webhook changed (resets the delivery back-off once); harmless.
