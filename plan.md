# Plan: syncwatch to v1.0

The [README](README.md) describes what syncwatch does and how: install, configuration, checks, notifications, security. This plan has what remains before v1.0, the known gaps and the decisions behind the design. Update it when work lands.

## 1. Current state (2026-10-12)

| Part | State |
|---|---|
| Built | The read-only client (one typed method for each `GET` of the allowlist). The collector (event stream, periodic full checks, polled mode, structure scan, baseline scan, traffic counters). The collector also reads the relay listeners and the files that a PC still needs. The checks H1 to H13, X1 to X2, S1 to S7, C1 to C3. The finding engine. The Discord notifier: urgent messages, repeats, resolved messages, daily reminder, quiet hours, weekly report, settings notices and a banner while a send fails. An optional second webhook gets only the settings notices. The SQLite store. The dashboard (Overview with the connections between the sites, Folders, Findings, the lobby page) and Settings (with the import preview and the devices). The JSON status API with named API tokens, and the Home Assistant endpoint. The health address `/healthz` and the optional dead-man URL. The CLI (`serve`, `probe`, `gate`, `reset-admin-password`, `import-config`, `export-config`, `healthcheck`, `--version`). The read-only gate. The Docker image. The CI and release workflows. |
| Reviews | 2 security reviews and the review of October 2026. The review had 3 rounds: 13 branches (r0), 8 branches (r1), and a foundation, 8 branches and a repair branch (r2). A text rewrite brought the product text and the documentation to the STE style guide. The review is complete and merged: the branch `review/sim-2026-10` has all rounds, and the test release takes it into `main`. See [docs/CHANGES-review.md](docs/CHANGES-review.md). |
| Decisions | The team agreed the recommendations of the decision cards of the review report (W-044 to W-062). The third round (r2) implemented them. W-056 (help for the person who keeps the project folders in order) stays open: the team still discusses it (section 2). |
| Tests | Unit tests (with the race detector in CI). An integration suite against real Syncthing 2.1.5 and 2.1.6 processes (3 servers, 2 PCs, a fake Discord, 14 scenarios). A Docker Compose suite (the image, Syncthing containers, a gate, a fake Discord). A structure scan load simulation (Docker, started by hand). The Compose and load stacks run their Syncthing with discovery, relays, NAT traversal and reports off. Each round of the review ended with a live check in a simulation of 2 sites with a VPN, a relay and a Windows PC. |
| CI | `gofmt`, `go vet`, `go mod tidy -diff`, govulncheck and the unit suite on each push to `main` and on each pull request, then the integration and Compose suites. CI checks the Syncthing download against the checksum list of the release. |
| Release | No release until now: no tag, thus no image and no binaries on GitHub. The status note in the README reports it. The next step is the test release `v0.9.0-rc1` (section 2, step 1). |
| Real servers | Not run against real NAS hardware until now (section 2, step 2). |

## 2. Road to v1.0

The review of October 2026 did the first step of the plan before: a review of the security-relevant parts. These are the API keys, the authentication, the bootstrap file, the notifier and the gate.

| # | Step |
|---|---|
| 1 | A test release `v0.9.0-rc1` from the merged review, with provenance on. Push the tag only after the team agrees. Look at how the container user interfaces of NAS products (QNAP Container Station) show the image. Keep provenance if the result is satisfactory. An rc tag is a pre-release on GitHub and does not move `latest`. |
| 2 | The checks on the real NAS hardware. Install the test release and its gate on each NAS, the gate together with syncwatch. Run `syncwatch probe` on each server, with `--project-pattern`. Measure the browse time on the largest share and a long-poll through the VPN. Measure the load of a full check (monitor the CPU and the RAM of the NAS). Measure the duration of a baseline scan: less than 10 seconds gives the daily baseline scan. If the load is large, use the polled mode. |
| 3 | Set the sites of the servers and of syncwatch, and the devices that always connect through a relay. Add an outside check (`/healthz` or the dead-man URL). Add what applies in general to `docs/api-findings.md`. Review a probe report before you commit it. Keep addresses and share paths out of it. |
| 4 | One week on the real installation with the test release. Success is: no false urgent messages, the Monday report arrives, and a stopped server gives one H1 urgent message and its resolved message. |
| 5 | Tag `v1.0.0`: the release workflow waits for the test job, then makes the image and the binaries. Make sure that the GHCR package is public. Replace the status note in the README. |

**v1.0 is complete** when step 5 is done after a successful step 4. Then: a signature for the image, and items from section 5.

### Open for the team

- **W-056: help for the person who keeps the project folders in order.** The team still discusses it. The decision card has these options:
  - A button "Scan now", and the time of the last scan on each structure finding.
  - Accepted names for helper folders, for example names that start with `_`.
  - A page for each share.
  - S7 on in the example settings.

  syncwatch only reports. The registration of new project folders as Syncthing folders is not a part of syncwatch (section 5).

### Open items from the review of October 2026

Items that the review found and did not do. The source is in parentheses.

- The healthcheck of the image derives its URL from `SYNCWATCH_LISTEN`. A gate container needs `SYNCWATCH_HEALTHCHECK_URL` until `healthcheck` knows the gate port ([docs/changes/r0-b.md](docs/changes/r0-b.md)).
- Behind a gate, the check that a share exists gets 403 for the parent of the share. A missing share then shows as empty, not as "not found on the server". The administrator can add the parent to `SYNCWATCH_GATE_BROWSE_ROOTS` ([docs/changes/r0-b.md](docs/changes/r0-b.md)).
- The Findings page refreshes on each snapshot change, also for changes that it does not show. Separate SSE events for findings and snapshots will stop that ([docs/changes/r0-k.md](docs/changes/r0-k.md)).
- With open view and no administrator password, `/settings` answers 403 instead of a redirect to `/setup` ([docs/changes/r0-e.md](docs/changes/r0-e.md)).
- The revision check of the settings forms accepts 2 saves in the same millisecond ([docs/changes/r0-e.md](docs/changes/r0-e.md)).
- A pinned certificate that matches is accepted after its expiry date and for a different host name. H13 now reports the expiry ([docs/changes/r0-a.md](docs/changes/r0-a.md), [docs/changes/r2-2.md](docs/changes/r2-2.md)).
- The collector package needs approximately 150 seconds in `go test ./...`. `SYNCWATCH_TEST_SLOW` exists for slow hosts ([docs/changes/r0-d.md](docs/changes/r0-d.md), [docs/changes/r2-3.md](docs/changes/r2-3.md)).
- The Compose test stack addresses the gate on port 8080 through one environment line. A move to 8385 needs 3 changes in `test/compose/docker-compose.yml` ([docs/changes/r0-j.md](docs/changes/r0-j.md)).
- The notification form refuses `OFF`. `off` works, and an import accepts both ([docs/changes/r0-g.md](docs/changes/r0-g.md)).
- `settings.DropRemovedServers` does not delete the stored device ID of a removed server ([docs/changes/r0-i.md](docs/changes/r0-i.md)).
- The lower limit of the thresholds is 10 seconds, not 1 minute, because the test suites use 20 and 40 seconds ([docs/changes/r0-g.md](docs/changes/r0-g.md)).
- H10 does not name the conflict copies and does not resolve when you delete them. A read of `/metrics` at each structure scan changes the request pattern of the collector (r1).
- syncwatch reads no `X-Forwarded-For`. Behind a reverse proxy, the login limit and the log use the address of the proxy. `SYNCWATCH_TRUSTED_PROXIES` applies only to `X-Forwarded-Proto` (r1, r2).

### Open items from the third round (r2)

The live check of the third round found these small items. They need no correction before the test release.

- `syncwatch import-config` resets the state of the notifications. The banner then shows a later start of a failure, and the limit of `/healthz` for failed notifications starts again. A reset only for a changed webhook will correct it.
- After a Discord outage, the kept settings notices wait for the next try of the webhook for settings notices, up to 30 minutes. "Send test settings notice" sends them immediately.
- After a restart of syncwatch, the time of 1 hour starts again before a new settings notice about security also goes to the first webhook.
- H13 rounds the days down: "in 9 days" when 9 days and 23 hours remain. The date is correct.
- The Overview tile shows "Certificate valid until …" also for an expired certificate. H13 shows "expired on …".
- The lobby page shows "syncwatch tries again automatically." without the time of the next try. The other pages show the time.
- H1 for "connection refused" (for example a stopped gate) names the VPN first in Possible causes. After 10 minutes, the row "What AKKA sees" gives the real cause.
- Settings → Servers refuses a site with more than 40 characters with a message at the top of the page, not at the form.
- H5 shows "did not finish the sync" also for a folder that waits for a scan (W-076).
- A baseline scan that stops because the server is lost comes again only after the interval ([docs/changes/r2-3.md](docs/changes/r2-3.md)).
- The database keeps the first-seen time of a folder that the server no longer has. When the folder ID is on the server again, it gets its old first-seen time ([docs/changes/r2-3.md](docs/changes/r2-3.md)).
- A C1 finding that was open before the update keeps its old start until it resolves ([docs/changes/r2-2.md](docs/changes/r2-2.md)).
- The review report lists more items of low priority for later (W-074 to W-102). Examples are the layout of the Findings filters on a phone and the texts of the structure findings. Other examples are the gate token in the log of the gate, secrets in the container environment and the release supply chain.

## 3. Decisions

### Design

- Read-only by construction: the Syncthing client can only send the `GET` requests of the allowlist, and a unit test fails if not. The client discards `ConfigSaved` payloads. syncwatch never writes to Syncthing.
- One Go binary with htmx and server-sent events. SQLite. English only. MIT.
- Events plus periodic full checks, with a polled mode for servers where a full check is slow. After a lost connection, the event stream resumes. A Syncthing restart or missed events start a full check, at most one full check every 10 minutes for missed events.
- syncwatch pins each server certificate on first use (trust on first use), self-signed or CA-signed, by SHA-256. A changed certificate opens H3 until an administrator accepts it. A new CA-signed certificate for the host name is accepted without H3. syncwatch sends a settings notice for each certificate that it pins without an administrator.
- A Settings page plus YAML import and export. An import in the dashboard first shows a preview. syncwatch imports a bootstrap file one time and removes its secrets from the file. A syncwatch that runs applies `import-config` in less than 10 seconds.
- Discord is the only notification target (shoutrrr was removed: a large dependency tree, and it can send to any address). An optional second Discord webhook gets only the settings notices.
- A Docker image on GHCR (`linux/amd64`, `linux/arm64`). The image serves HTTPS with a self-signed certificate by default. The binary serves HTTP on loopback if you give no other setting.
- The read-only gate (`syncwatch gate`): one container for each server keeps the Syncthing API key. syncwatch only gets a gate token that reaches the `GET` requests of the allowlist, and the gate removes secrets from the answers. The gate has its own listen address (`SYNCWATCH_GATE_LISTEN`, 8385). It checks the query of each request and permits a browse only under the parents of the Syncthing folders.
- Sites are optional names of the locations of the servers. syncwatch shows the connection between 2 sites from the server pairs of H2 (W-057).
- The lobby page is read-only and uses the viewer access, not an API token. Thus no API token goes into a browser (W-052).
- Syncthing 2.x only.

### Security

- syncwatch sends an API key only to the URL that you typed it for. A changed server URL needs the API key again, also by import. syncwatch deletes the API key of a removed server. A plain `http://` server URL needs `allow_http`. A server URL cannot have user names, passwords, queries or fragments.
- To accept a changed certificate or to remove a pinned certificate, you need the administrator password or the API key of the server.
- Changes that reduce the access control (proxy mode, a more open view, a new viewer password) need the administrator password, also by import. An import cannot set built-in login to on without an administrator password. An import that removes servers needs a checkbox and the administrator password. The command line does not ask again, because a person who can run it can also change the database (W-055).
- syncwatch announces each settings change with a settings notice, immediately, also during quiet hours, with the client address and the old and new values. This includes `import-config` and `reset-admin-password` on the command line. It sends the settings notice to the webhook that was configured before the change. syncwatch keeps a settings notice about security until a channel accepts it, also through a restart. It discards such a settings notice only when Discord refuses the message itself (W-051). syncwatch tries other settings notices again for 1 day. A kept settings notice never delays an urgent message.
- The Discord webhook must be a Discord webhook (`SYNCWATCH_ALLOW_ANY_WEBHOOK=true` for test installations). syncwatch checks it again at each send and does not follow redirects. syncwatch escapes names from Syncthing in titles, summaries and lines of Discord messages.
- `SYNCWATCH_SECRET_KEY` takes only a random base64-encoded key of 32 bytes. The database files have mode 0600, and the image makes `/data` with mode 0700. syncwatch keeps only the SHA-256 hash of each API token.
- Failed logins from many addresses make logins slower instead of a block for all persons. Password checks run at most 2 at the same time. The limit of 5 failed logins in 10 minutes applies to each address, also for parallel tries. 5 failures at one address block the logins of all roles from that address. The password confirmations of a logged-in administrator have a separate count for each session.
- The session cookies are `SameSite=Lax`, thus links from Discord open the dashboard with the current session. Over HTTPS, the cookies have the `__Host-` names. The CSRF token belongs to the session and changes at each login. syncwatch refuses `Origin: null` and an `http://` Origin when the dashboard uses HTTPS (W-027). The CSRF token, the Origin check and the Host check protect the forms.
- syncwatch answers only to known host names (IP addresses, `localhost`, the host of `public_url`, the listen host, `SYNCWATCH_TLS_HOSTS`). Other names get 421.
- The JSON API needs an API token or a login, also with open access. Each API token has a scope: Home Assistant (only `/api/v1/ha`) or Full. There are at most 5 API tokens (W-048, W-053). Viewers see only short device IDs and no links to Syncthing.
- syncwatch accepts `X-Forwarded-Proto` only from the addresses in `SYNCWATCH_TRUSTED_PROXIES` (W-034). No support for a proxy with `X-Forwarded-For`. The README documents the limits behind a reverse proxy instead.
- syncwatch does not monitor its own host. `/healthz` reports failed notifications and stopped checks, and the optional dead-man URL lets an outside monitor service find a stopped syncwatch. After a stop, the next message gives the time when syncwatch did not run (W-044). There is no daily "syncwatch runs" message, because the channel must stay quiet.
- The release workflow pins the actions to commit SHAs with permissions for each job. It runs the unit suite before it makes the release, and it uses no build cache. The `Dockerfile` pins its base images by digest. The documentation recommends the image digest and no auto-updaters.

### Behavior

- H4 waits `thresholds.failing_files` (default 1 hour) when failed items are its only reason. A folder error and a full disk open it immediately. H4 also opens immediately when Syncthing refuses a file because the disk of the folder is full. A file watcher error is a separate WARNING finding after 1 hour and never urgent (W-060). On some NAS storage, a file watcher can fail without an error: the README tells it.
- One cause gives one finding (W-046). A share root that is a Syncthing folder gives 1 S2 that lists the folders inside it. A full disk of the Syncthing database gives 1 H4 for the server. The views of 1 PC from 2 servers stay 2 findings and share 1 line in the messages.
- A device that always connects through a relay gets no H11. H12 opens when it has no connection for `thresholds.relay_lost`, or when a server loses its relay (W-047). H7 keeps the same limit for all devices.
- H5 keeps the time rule of 24 hours. It shows the data received, the speed in the last 10 minutes and the time left (W-059). For a PC, it also shows the files that the PC still needs (W-058). A sync that does not move gets no time left.
- H13 gives a hint when the certificate of a server expires in less than 30 days or expired (W-061). C1 uses the time when syncwatch first saw a folder when it knows no change (W-021).
- syncwatch makes a baseline scan every day when the last one took less than 10 seconds, else after `baseline_interval` (W-045). The example of IVAR Studios looks 6 levels down. The scans and the events use the same level rule.
- Each Windows profile with its own Syncthing is a separate PC (W-062).
- A device that never connected gets only C2 after `old_device`. H5, H6 and C3 keep the last known device state through a disconnect. H5 counts only the time while the device is connected.
- Findings of an unreachable server are stale, not resolved. This also applies after a start until syncwatch contacted the server, and to structure findings until the first scan. H7, C2 and the H12 of a device wait while one of their servers is unreachable. Only a server that is up can show that a PC is offline. The other cross-server findings are stale only when none of their servers is reachable. syncwatch closes the findings of a removed or disabled server without a message. A disabled server stays a server, not a PC.
- A higher threshold does not close open findings. The debounce of an urgent message counts from the time the finding became urgent. Urgent findings that fall due in the same 10 seconds share one urgent message (RT-3).
- syncwatch sends the resolved message after the finding stayed resolved for 5 minutes (or the debounce, if shorter). A finding that opens again in that time is the same incident. The resolved message tells what is correct again and how long the fault lasted (W-038).
- The repeat of an urgent message is an option and off by default. It repeats only an urgent ERROR finding of one server, and never in the quiet hours (W-050).
- Urgent messages, repeats and the daily reminder show the first advice of a finding. Thus a message is complete also when the dashboard does not open (W-057).
- Failed notifications wait longer between tries and show a banner with the time of the next try on each page. A Discord outage (HTTP 5xx or 429, a timeout or a network error) gives tries after 1, 2 and 4 minutes, then every 5 minutes. A refused webhook (HTTP 400, 401, 403 or 404) gives tries after 1, 2, 4 … minutes, up to every 30 minutes. The first message after a failure lists the urgent findings that opened and resolved during the failure. Without a webhook, syncwatch marks no notification as sent. A webhook that got a message does not get it again.
- The status values of `/api/v1/ha` have no spaces and do not change with the texts. The status is `error` only while an H4 finding with the severity ERROR is open. Each time in the API is UTC without fractions of a second (W-049, W-054).
- The self-signed certificate follows `public_url`, a specific listen address and `SYNCWATCH_TLS_HOSTS`. syncwatch makes a new one 30 days before the expiry date, also while it runs, and reads own certificate files again when they change. The dashboard serves HTTP/2.
- In polled mode, each structure scan also scans the project folders in Syncthing folders. The depth limit `nested_scan_depth` applies to scans and to events.

### Process

- Work goes directly to `main`. CI runs on each push and on each pull request. The review of October 2026 used the branch `review/sim-2026-10`.
- Examples, tests and documentation use placeholder addresses (192.168.1.10, 192.168.2.10, 192.168.3.10, `*.invalid`, 127.0.0.1). No file in the repository describes the network of a real installation.
- The repository keeps LF line endings (`.gitattributes`), also on Windows.
- The text that users read follows the STE style guide of the review (ASD-STE100, approximately 80 percent). Each item has one name: finding, administrator, urgent message, settings notice, full check, structure scan, server, host, pinned certificate, site, lobby page.

## 4. Known gaps in v1

- syncwatch sees the PCs only through the servers: no PC folder errors, no disk space, no pending offers on a PC.
- `browse` lists directories only. File-level information comes out of the events, the conflict counter and the files that a PC still needs.
- syncwatch does not find nested project folders deeper than `nested_scan_depth` below the top-level project folder.
- A file watcher that fails without an error gives no finding. The daily baseline scan finds a nested project folder in less than 1 day.
- A PC with a separate Syncthing for each Windows user shows as more than 1 PC.
- After missed events, the state of a server can be up to 10 minutes old.
- No disk-space or hardware monitor, no graphs.
- syncwatch cannot report its own stop: it needs an outside check of `/healthz` or the dead-man URL.
- Without the gate, syncwatch keeps Syncthing API keys with full access.
- `syncwatch import-config` on the command line needs no administrator password, because it already needs shell access and the secret key.
- The banner for failed notifications refreshes every 60 seconds, not in real time.
- Different spellings of the same directory (QNAP `/share/X` and `/share/CACHEDEV1_DATA/X`) are 2 paths for S1.

### Items to examine if necessary

- Support for a proxy with `X-Forwarded-For`, if people run syncwatch behind reverse proxies.
- Details of the login limiter: the limiter does not group IPv6 addresses by prefix.
- A TLS session cache in the client, if the handshake each minute for the traffic sample is a load on a VPN.

## 5. Later ideas (not in v1)

- **Registration of new project folders** as paused, unshared Syncthing folders with `.stignore` present. A migration from a different sync tool then becomes: resume, compare, share for each folder. It needs the full Syncthing API key, thus it is a separate tool next to the gate, never a part of syncwatch. The author tested the API behavior that it depends on with 2.1.5 (`docs/api-findings.md`, "Folders added paused through the API").
- A page for each share with its top-level directories and findings (W-056).
- A preview of the weekly report and of the daily reminder (W-050).
- A list of all folders and the traffic for each device in the JSON API (W-052).
- A warning for file names that Windows cannot store, if people on Mac or Linux make such names frequently (W-058).
- H5 only after 24 hours without new data, if a PC that uses a relay gets incorrect H5 findings (W-059).
- A separate H7 limit for each device, if the team asks for it (W-047).
- A computer name that groups the Windows profiles of 1 PC (W-062).
- A tree view of the folder structure.
- Trend graphs or a Prometheus `/metrics` endpoint.
- An optional PC agent.
- Disk space through SNMP.
- An HTML email report.
- Repair hints for each finding type.
- Settings drift checks.
- A Windows service.
- Translations.
- Syncthing 1.x.
