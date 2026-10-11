# Plan: syncwatch to v1.0

The [README](README.md) describes what syncwatch does and how: install, configuration, checks, notifications, security. This plan has what remains before v1.0, the known gaps and the decisions behind the design. Update it when work lands.

## 1. Current state (2026-10-10)

| Part | State |
|---|---|
| Built | The read-only client (one typed method for each `GET` of the allowlist). The collector (event stream, periodic full checks, polled mode, structure scan, traffic counters). The checks H1 to H11, X1 to X2, S1 to S7, C1 to C3. The finding engine. The Discord notifier (urgent messages, resolved messages, daily reminder, quiet hours, weekly report, settings notices, a banner while a send fails). The SQLite store. The dashboard (Overview, Folders, Findings) and Settings. The JSON status API and the Home Assistant endpoint. The CLI (`serve`, `probe`, `gate`, `reset-admin-password`, `import-config`, `export-config`, `healthcheck`, `--version`). The read-only gate. The Docker image. The CI and release workflows. |
| Reviews | 2 security reviews and the review of October 2026. The review had 13 branches of corrections in the first round (r0) and 8 in the second round (r1), see [docs/CHANGES-review.md](docs/CHANGES-review.md). The team corrected or documented all findings, or added them to the open items below. |
| Tests | Unit tests (with the race detector in CI). An integration suite against real Syncthing 2.1.5 processes (3 servers, 2 PCs, a fake Discord). A Docker Compose suite (the image, Syncthing containers, a gate, a fake Discord). A structure scan load simulation (Docker, started by hand). The Compose and load stacks run their Syncthing with discovery, relays, NAT traversal and reports off. |
| CI | `gofmt`, `go vet`, `go mod tidy -diff`, govulncheck and the unit suite on each push to `main` and on each pull request, then the integration and Compose suites. CI checks the Syncthing download against the checksum list of the release. |
| Release | No release until now: no tag, thus no image and no binaries on GitHub. The status note in the README reports it. The release workflow runs the unit suite before it makes the image and the binaries. A pre-release tag (with a `-`) does not move `latest`. |
| Real servers | Not run against real NAS hardware until now (section 2, step 2). |

## 2. Road to v1.0

| # | Step |
|---|---|
| 1 | A review of the security-relevant parts. These are: the API key code (`internal/settings`, `internal/collector/manager.go`, `internal/web/settings.go`), authentication (`internal/web/auth.go`), the secret removal from the bootstrap file (`internal/app/bootstrap.go`), the notifier and the gate. |
| 2 | Run `syncwatch probe` on real servers, with `--project-pattern`. Measure the browse time on the largest share and a long-poll through the VPN. Measure the load of a full check (monitor the CPU and the RAM of the NAS). Measure the listings of the structure scan, and the counts. If the load is large, use the polled mode. Add what applies in general to `docs/api-findings.md`. Review a probe report before you commit it. Keep addresses and share paths out of it. |
| 3 | A test release (for example `v0.9.0-rc1`) with provenance on. Look at how the container user interfaces of NAS products (QNAP Container Station) show the image. Keep provenance if the result is satisfactory. An rc tag is a pre-release on GitHub and does not move `latest`. |
| 4 | Tag `v1.0.0`: the release workflow waits for the test job, then makes the image and the binaries. Make sure that the GHCR package is public. Replace the status note in the README. |
| 5 | One week on a real installation. Success is: no false urgent messages, the Monday report arrives, and a stopped server gives one H1 urgent message and its resolved message. |

**v1.0 is complete** when step 5 is successful. Then: a signature for the image, and items from section 5.

### Open items from the review of October 2026

Items that the review found and did not do. The source is in parentheses.

- The healthcheck of the image derives its URL from `SYNCWATCH_LISTEN`. A gate container needs `SYNCWATCH_HEALTHCHECK_URL` until `healthcheck` knows the gate port ([docs/changes/r0-b.md](docs/changes/r0-b.md)).
- Behind a gate, the check that a share exists gets 403 for the parent of the share. A missing share then shows as empty, not as "not found on the server". The administrator can add the parent to `SYNCWATCH_GATE_BROWSE_ROOTS` ([docs/changes/r0-b.md](docs/changes/r0-b.md)).
- The Findings page refreshes on each snapshot change, also for changes that it does not show. Separate SSE events for findings and snapshots will stop that ([docs/changes/r0-k.md](docs/changes/r0-k.md)).
- Count words with `(s)` remain in `notify`, `rules/health.go`, `settings.go` and `main.go` ([docs/changes/r0-k.md](docs/changes/r0-k.md)). The STE rewrite of the product text replaces them.
- With open view and no administrator password, `/settings` answers 403 instead of a redirect to `/setup` ([docs/changes/r0-e.md](docs/changes/r0-e.md)).
- The revision check of the settings forms accepts 2 saves in the same millisecond ([docs/changes/r0-e.md](docs/changes/r0-e.md)).
- A pinned certificate that matches is accepted after its expiry date and for a different host name ([docs/changes/r0-a.md](docs/changes/r0-a.md)).
- The collector package needs 90 seconds in `go test ./...`. `SYNCWATCH_TEST_SLOW` exists for slow hosts ([docs/changes/r0-d.md](docs/changes/r0-d.md), [docs/changes/r0-s.md](docs/changes/r0-s.md)).
- The Compose test stack addresses the gate on port 8080 through one environment line. A move to 8385 needs 3 changes in `test/compose/docker-compose.yml` ([docs/changes/r0-j.md](docs/changes/r0-j.md)).
- The notification form refuses `OFF`. `off` works ([docs/changes/r0-g.md](docs/changes/r0-g.md)).
- `settings.DropRemovedServers` does not delete the stored device ID of a removed server ([docs/changes/r0-i.md](docs/changes/r0-i.md)).
- The lower limit of the thresholds is 10 seconds, not 1 minute, because the test suites use 20 and 40 seconds ([docs/changes/r0-g.md](docs/changes/r0-g.md)).
- The product text does not follow the STE style guide until the product groups of the rewrite go into the code ([docs/changes/ste-docs.md](docs/changes/ste-docs.md)).

### Open items from the second round (r1)

- `/api/v1/ha` gives `running: false` also for a server that syncwatch did not contact after a start. `null` for that case is a change of the API contract and needs a decision.
- H10 does not name the conflict copies and does not resolve when you delete them. A read of `/metrics` at each structure scan changes the request pattern of the collector.
- The urgent message does not wait for findings that become due some seconds later. A wait time for more findings changes the rule "Findings that open in the same cycle go into one message".
- The notification banner does not show the time of the next try.
- C1 keeps the time of the last local change only in memory.
- The CSRF protection does not refuse `Origin: null`. syncwatch has no setting that names a reverse proxy for `X-Forwarded-For`.
- H5 does not list the files that a PC cannot store. The list needs `/rest/db/remoteneed` in the allowlist of the client and the gate.
- The screenshots in `docs/screenshots/` show the dashboard before r1.

## 3. Decisions

### Design

- Read-only by construction: the Syncthing client can only send the `GET` requests of the allowlist, and a unit test fails if not. The client discards `ConfigSaved` payloads. syncwatch never writes to Syncthing.
- One Go binary with htmx and server-sent events. SQLite. English only. MIT.
- Events plus periodic full checks, with a polled mode for servers where a full check is slow. After a lost connection, the event stream resumes. A Syncthing restart or missed events start a full check, at most one full check every 10 minutes for missed events.
- syncwatch pins each server certificate on first use (trust on first use), self-signed or CA-signed, by SHA-256. A changed certificate opens H3 until an administrator accepts it. A new CA-signed certificate for the host name is accepted without H3.
- A Settings page plus YAML import and export. syncwatch imports a bootstrap file one time and removes its secrets from the file. A syncwatch that runs applies `import-config` in less than 10 seconds.
- Discord is the only notification target (shoutrrr was removed: a large dependency tree, and it can send to any address).
- A Docker image on GHCR (`linux/amd64`, `linux/arm64`). The image serves HTTPS with a self-signed certificate by default. The binary serves HTTP on loopback if you give no other setting.
- The read-only gate (`syncwatch gate`): one container for each server keeps the Syncthing API key. syncwatch only gets a gate token that reaches the `GET` requests of the allowlist, and the gate removes secrets from the answers. The gate has its own listen address (`SYNCWATCH_GATE_LISTEN`, 8385). It checks the query of each request and permits a browse only under the parents of the Syncthing folders.
- Syncthing 2.x only.

### Security

- syncwatch sends an API key only to the URL that you typed it for. A changed server URL needs the API key again, also by import. syncwatch deletes the API key of a removed server. A plain `http://` server URL needs `allow_http`. A server URL cannot have user names, passwords, queries or fragments.
- To accept a changed certificate or to remove a pinned certificate, you need the administrator password or the API key of the server.
- Changes that reduce the access control (proxy mode, a more open view, a new viewer password) need the administrator password, also by import. An import cannot set built-in login to on without an administrator password.
- syncwatch announces each settings change with a settings notice, immediately, also during quiet hours, with the client address and the old and new values. This includes `import-config` and `reset-admin-password` on the command line. It sends the settings notice to the webhook that was configured before the change. syncwatch tries a failed settings notice again for 1 day. syncwatch discards a settings notice that Discord refuses, after one warning. A kept settings notice never delays an urgent message.
- The Discord webhook must be a Discord webhook (`SYNCWATCH_ALLOW_ANY_WEBHOOK=true` for test installations). syncwatch checks it again at each send and does not follow redirects. syncwatch escapes names from Syncthing in titles, summaries and lines of Discord messages.
- `SYNCWATCH_SECRET_KEY` takes only a random base64-encoded key of 32 bytes. The database files have mode 0600, and the image makes `/data` with mode 0700.
- Failed logins from many addresses make logins slower instead of a block for all persons. Password checks run at most 2 at the same time. The limit of 5 failed logins in 10 minutes applies to each address, also for parallel tries. 5 failures at one address block the logins of all roles from that address. The password confirmations of a logged-in administrator have a separate count for each session.
- The session cookies are `SameSite=Lax`, thus links from Discord open the dashboard with the current session. The CSRF token, the Origin check and the Host check protect the forms.
- syncwatch answers only to known host names (IP addresses, `localhost`, the host of `public_url`, the listen host, `SYNCWATCH_TLS_HOSTS`). Other names get 421.
- No support for a proxy with `X-Forwarded-For`. The README documents the limits behind a reverse proxy instead.
- syncwatch does not monitor its own host. The README documents it ("Where to run syncwatch"). There is no heartbeat feature.
- The release workflow pins the actions to commit SHAs with permissions for each job. It runs the unit suite before it makes the release, and it uses no build cache. The `Dockerfile` pins its base images by digest. The documentation recommends the image digest and no auto-updaters.

### Behavior

- H4 waits `thresholds.failing_files` (default 1 hour) when failed items are its only reason. A folder error and a full disk open it immediately. A file watcher error is a separate WARNING finding after 1 hour and never urgent.
- A device that never connected gets only C2 after `old_device`. H5, H6 and C3 keep the last known device state through a disconnect. H5 counts only the time while the device is connected.
- Findings of an unreachable server are stale, not resolved. This also applies after a start until syncwatch contacted the server, and to structure findings until the first scan. H7 and C2 wait while one of their servers is unreachable, because only a server that is up can show that a PC is offline. The other cross-server findings are stale only when none of their servers is reachable. syncwatch closes the findings of a removed or disabled server without a message. A disabled server stays a server, not a PC.
- A higher threshold does not close open findings. The debounce of an urgent message counts from the time the finding became urgent.
- syncwatch sends the resolved message after the finding stayed resolved for 5 minutes (or the debounce, if shorter). A finding that opens again in that time is the same incident.
- Failed notifications wait longer between tries and show a banner on each page. A Discord outage (HTTP 5xx or 429, a timeout or a network error) gives tries after 1, 2 and 4 minutes, then every 5 minutes. A refused webhook (HTTP 400, 401, 403 or 404) gives tries after 1, 2, 4 … minutes, up to every 30 minutes. The first message after a failure lists the urgent findings that opened and resolved during the failure. Without a webhook, syncwatch marks no notification as sent. A webhook that got a message does not get it again.
- The self-signed certificate follows `public_url`, a specific listen address and `SYNCWATCH_TLS_HOSTS`. syncwatch makes a new one 30 days before the expiry date, also while it runs, and reads own certificate files again when they change. The dashboard serves HTTP/2.
- In polled mode, each structure scan also scans the project folders in Syncthing folders. The depth limit `nested_scan_depth` applies to scans and to events.

### Process

- Work goes directly to `main`. CI runs on each push and on each pull request.
- Examples, tests and documentation use placeholder addresses (192.168.1.10, 192.168.2.10, 192.168.3.10, `*.invalid`, 127.0.0.1). No file in the repository describes the network of a real installation.
- The repository keeps LF line endings (`.gitattributes`), also on Windows.
- The text that users read follows the STE style guide of the review (ASD-STE100, approximately 80 percent). Each item has one name: finding, administrator, urgent message, settings notice, full check, structure scan, server, host, pinned certificate.

## 4. Known gaps in v1

- syncwatch sees the PCs only through the servers: no PC folder errors, no disk space, no pending offers on a PC.
- `browse` lists directories only. File-level information comes out of the events and the conflict counter.
- syncwatch does not find nested project folders deeper than `nested_scan_depth` below the top-level project folder.
- After missed events, the state of a server can be up to 10 minutes old.
- No disk-space or hardware monitor, no graphs.
- No watchdog for syncwatch itself.
- Without the gate, syncwatch keeps Syncthing API keys with full access.
- `syncwatch import-config` on the command line needs no administrator password, because it already needs shell access and the secret key.
- The banner for failed notifications refreshes every 60 seconds, not in real time.
- Different spellings of the same directory (QNAP `/share/X` and `/share/CACHEDEV1_DATA/X`) are 2 paths for S1.

### Items to examine if necessary

- An outside healthcheck or a small watchdog, if an unnoticed outage of the host of syncwatch becomes important.
- Support for a proxy with `X-Forwarded-For`, if people run syncwatch behind reverse proxies.
- Details of the login limiter: the limiter does not group IPv6 addresses by prefix.
- A TLS session cache in the client, if the handshake each minute for the traffic sample is a load on a VPN.

## 5. Later ideas (not in v1)

- **Registration of new project folders** as paused, unshared Syncthing folders with `.stignore` present. A migration from a different sync tool then becomes: resume, compare, share for each folder. It needs the full Syncthing API key, thus it is a separate tool next to the gate, never a part of syncwatch. The author tested the API behavior that it depends on with 2.1.5 (`docs/api-findings.md`, "Folders added paused through the API").
- A tree view of the folder structure.
- Trend graphs or a Prometheus `/metrics` endpoint.
- An optional PC agent.
- Disk space through SNMP.
- An HTML email report.
- A heartbeat watchdog.
- Repair hints for each finding type.
- Settings drift checks.
- A Windows service.
- Translations.
- Syncthing 1.x.
