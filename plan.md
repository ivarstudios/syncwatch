# Plan: syncwatch to v1.0

The [README](README.md) documents what syncwatch does and how (install, configuration, checks, notifications, security). This plan covers what is left before v1.0, the known gaps, and the decisions behind the design. Update it as work lands.

## 1. Where things stand (2026-10-08)

| | State |
|---|---|
| Built | The read-only client (one typed method per allowlisted `GET`); the collector (event stream, periodic full re-checks, polled mode, structure scan, traffic counters); the checks H1–H11, X1–X2, S1–S7, C1–C3; the finding engine; the Discord notifier (urgent pings, resolved messages, daily reminder, quiet hours, weekly report, settings notices, back-off with a banner); the SQLite store; the dashboard (overview, folder grid, problems) and settings; the JSON status API and the Home Assistant endpoint; the CLI (`serve`, `probe`, `gate`, `reset-admin-password`, `import-config`, `export-config`, `healthcheck`); the read-only gate; the Docker image; CI and release workflows. |
| Reviews | Two security reviews, all findings fixed or documented (reverse-proxy limits, monitoring the monitor's own host). |
| Tests | Unit tests (with the race detector in CI); integration against real Syncthing 2.1.5 processes (three servers, two PCs, fake Discord); a Docker Compose suite (the image, Syncthing containers, a gate, fake Discord); a structure-scan load simulation (Docker, run by hand). |
| CI | Unit, integration and Compose suites on every push to `main` and on pull requests. |
| Release | None yet: no tag, so no published image or binaries. The README's status note says so. |
| Real servers | Not yet run against real NAS hardware (§2, step 2). |

## 2. Road to v1.0

| # | Step |
|---|---|
| 1 | A review of the security-sensitive parts: key handling (`internal/settings`, `internal/collector/manager.go`, `internal/web/settings.go`), auth (`internal/web/auth.go`), the bootstrap scrubber (`internal/app/bootstrap.go`), the notifier and the gate. |
| 2 | Run `syncwatch probe` on real servers, with `--project-pattern`: browse timing on the largest share, long-polls across a VPN, the cost of a full re-check (watch NAS CPU and RAM; if it's noticeable, polled mode), the structure scan's listings, and counts. Add what applies generally to `docs/api-findings.md`; review a probe report before committing it, and keep addresses and share paths out. |
| 3 | A test release (such as `v0.9.0-rc1`) with provenance on, to see how NAS container UIs (QNAP Container Station) show the image; keep provenance if it looks fine. |
| 4 | Tag `v1.0.0`: the release workflow publishes the image and binaries. Check that the GHCR package is public, and replace the README's status note. |
| 5 | A week on a real deployment: no false urgent pings, the Monday report arrives, and a stopped server gives one H1 ping and its "resolved" message. |

**v1.0 is done** when step 5 passes. After that: image signing, and anything from §5.

## 3. Decisions

### Design

- Read-only by construction: the Syncthing client can only send allowlisted `GET`s, a unit test fails otherwise, and `ConfigSaved` payloads are dropped. Writing to Syncthing never happens inside syncwatch.
- Go single binary with htmx and server-sent events; SQLite; English only; MIT.
- Events plus periodic full re-checks, with a polled fallback for servers where re-checks are expensive. After a dropped connection the event stream resumes; a Syncthing restart or missed events trigger a full re-check.
- Trust on first use for self-signed Syncthing certificates, pinned by SHA-256; a changed certificate raises H3 until an admin accepts it.
- A settings page plus YAML import and export; a bootstrap YAML is imported once and its secrets are removed from the file.
- Discord is the only notification channel (shoutrrr was dropped: a large dependency tree, and it could point anywhere).
- Docker image on GHCR (`linux/amd64`, `linux/arm64`), HTTPS with a self-signed certificate by default in the image; the plain binary serves HTTP on loopback unless told otherwise.
- The read-only gate (`syncwatch gate`): one container per server holds the Syncthing API key; syncwatch only gets a token that reaches the allowlisted `GET`s, and secrets are stripped from answers.
- Syncthing 2.x only.

### Security

- An API key is only ever sent to the URL it was entered for: changing a server's URL needs the key again, also by import, and a removed server's key is deleted. Plain `http://` server URLs need `allow_http`. Server URLs can't carry credentials, queries or fragments.
- Accepting a changed certificate or forgetting a pin needs the admin password or the server's API key.
- Changes that weaken access (proxy mode, more open viewing, a new viewer password) need the admin password, also by import.
- Security-relevant settings changes are announced at once, also during quiet hours, with the client address, to the channels configured before the change; notices that can't be sent are retried for a day.
- The Discord webhook must be a Discord webhook (`SYNCWATCH_ALLOW_ANY_WEBHOOK=true` for test setups). Names from Syncthing are escaped in Discord messages.
- `SYNCWATCH_SECRET_KEY` takes only a random base64-encoded 32-byte key.
- Failed logins from many addresses slow logins down instead of locking everyone out; password checks run at most two at a time.
- No trusted-proxy support (`X-Forwarded-For`): the README documents the limits behind a reverse proxy instead.
- Monitoring the monitor's own host: documented ("Where to run it"), no heartbeat feature.
- The release workflow pins actions to commit SHAs with permissions per job; the docs pin the image by digest and advise against auto-updaters.

### Behaviour

- H4 waits `thresholds.failing_files` (default 1 h) when failing files are its only reason; folder errors, watcher errors and a full disk open it at once.
- Failing notifications back off (1, 2, 4 … then every 30 minutes) and show a banner on every page; nothing counts as sent before a channel exists, and a channel that already got a message isn't sent it again.
- The self-signed certificate follows `public_url`, a specific listen address and `SYNCWATCH_TLS_HOSTS`.

### Process

- Work is merged directly to `main`; CI runs on every push and on pull requests.
- Examples, tests and docs use placeholder addresses (192.168.1.10, 192.168.2.10, 192.168.3.10, `*.invalid`, 127.0.0.1); nothing in the repo describes a real deployment's network.

## 4. Known gaps in v1

- PCs are visible only through the servers: no PC folder errors, disk space or pending offers on a PC.
- `browse` lists directories only; file-level information comes from events and the conflict counter.
- Nested project folders deeper than `nested_scan_depth` in folders not in Syncthing aren't found.
- No disk-space or hardware monitoring, no graphs.
- No watchdog for syncwatch itself.
- Without the gate, syncwatch holds full-access Syncthing API keys.
- `syncwatch import-config` on the command line needs no admin password, because it already needs shell access.
- The "notifications failing" banner refreshes every 60 seconds, not live.

### Revisit if needed

- An outside healthcheck or a small watchdog instance, if an unnoticed outage of the monitor's host turns out to matter.
- Trusted-proxy support, if people run syncwatch behind reverse proxies.
- Login limiter details: IPv6 addresses aren't grouped by prefix, and the address table is cleared at 10,000 entries.

## 5. Later ideas (not in v1)

- **Registering new project folders** as paused, unshared Syncthing folders with `.stignore` in place, so a migration from another sync tool becomes Resume, compare, share per folder. It needs Syncthing's full key, so it would be a separate tool next to the gate, never part of syncwatch. The API behaviour it relies on was tested on 2.1.5 (`docs/api-findings.md`, "Folders added paused through the API").
- Folder-structure tree view · trend graphs / Prometheus `/metrics` · optional PC agent · disk space via SNMP · HTML email report · heartbeat watchdog · fix hints per problem type · settings-drift checks · a Windows service · translations · Syncthing 1.x.
