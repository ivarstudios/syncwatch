# What the review of October 2026 changed

This is the plain-language list of the changes from the review (13 branches, `sim/r0-a` to `sim/r0-s`, merged in `review/sim-2026-10`). It says what you will see and what you must do after the update. The technical reports are in [docs/changes/](changes/) (one file for each branch, `r0-*.md`).

## What you must do after the update

1. **Gate containers:** set `SYNCWATCH_GATE_LISTEN` (for example `192.168.2.10:8385`) and `SYNCWATCH_HEALTHCHECK_URL` (`https://192.168.2.10:8385/healthz`). The gate ignores `SYNCWATCH_LISTEN` now. A gate without these settings listens on `127.0.0.1:8385` and shows as unhealthy. See [examples/gate-compose.yml](../examples/gate-compose.yml).
2. **Host names:** the dashboard answers only to IP addresses, `localhost`, the host of `public_url`, the listen host and the names in `SYNCWATCH_TLS_HOSTS`. If you open the dashboard by a DNS name, or Home Assistant calls it by a container name, put that name in `SYNCWATCH_TLS_HOSTS` or set `public_url`. A different name gets the answer 421 with instructions.
3. **Settings notices:** syncwatch now sends a settings notice for each change in Settings, with the old and new values. Look at the Discord channel after the first changes and make sure that the webhook and the role are correct.
4. **Shares behind a gate:** shares from `shares.add`, and shares that `share_type_from_path` finds more than 1 level above a Syncthing folder, need `SYNCWATCH_GATE_BROWSE_ROOTS` on that gate. Without it, the gate refuses the listing (403) and the share shows an error.
5. **Configuration values:** syncwatch now refuses values outside the ranges in the README (durations up to 10 years, thresholds from 10 seconds, role IDs with 17 to 20 digits, server IDs from `a-z`, `0-9` and `-`). A stored configuration with a value outside a range stops the next start with a message. Correct it with `syncwatch import-config`.
6. **Reminder time:** a `reminder_time` in the evening part of the quiet hours is an error now, because syncwatch could never send it. Change it before the update if you have one.
7. **Data directory:** run `chmod 700` on the data directory of a bind mount. The image makes `/data` with mode 0700, and syncwatch sets the database files to mode 0600.
8. **Integrations:** scripts that read the health line of `/api/v1/status` see "not contacted" for servers without a result. `/api/v1/status` and `/api/v1/ha` have new folder counts (`scanning`, `watch_error`) and the status value `watch error`. The field `notifications.error` is present only for administrator sessions, not for API tokens.
9. **Findings on Linux servers:** the IDs of S3 to S7 findings keep the case of the path now. After the update, such findings resolve and open again one time (one resolved message and one urgent message for S3). Snoozes and ignores on the old IDs do not apply to the new IDs.
10. **Watcher errors:** an open H4 finding that was only a file watcher error resolves at the update and opens again as a WARNING after 1 hour.

## Security

- **Certificates.** syncwatch pins the certificate of each server, also a CA-signed certificate. Before, a CA-signed server had no pinned certificate, and a change to a self-signed certificate was accepted without a message. Now a change to a different certificate opens H3. A new CA-signed certificate for the host name (a renewal from Let's Encrypt, for example) is accepted and logged. The same applies to the gate when it reaches Syncthing at an address other than loopback.
- **Gate.** Each request to the gate needs the gate token, the health request included. The gate permits only the query parameters that syncwatch sends. A `browse` is permitted only under the parents of the Syncthing folders and the directories in `SYNCWATCH_GATE_BROWSE_ROOTS`. The gate has timeouts to Syncthing, limits the request headers to 64 KiB and logs refused requests one time each minute. A gate with an unreadable `syncthing.pin` does not start.
- **Login.** The limit of 5 failed logins in 10 minutes for each address now holds for parallel tries and counts the administrator and the viewer password separately. A viewer login no longer clears the failed administrator tries.
- **Host check.** See item 2 above. This stops DNS rebinding attacks on the dashboard.
- **Cookies.** The session cookies are `SameSite=Lax` instead of `Strict`. Links from Discord open the dashboard with the current session. The CSRF token, the Origin check and the Host check protect the forms.
- **Import.** An import cannot set built-in login to on while no administrator password is set. Before, that locked out all persons.
- **Settings forms.** Each form has the revision of the configuration. The second of 2 administrators who save at the same time gets a message to reload the page, instead of a silent loss of the first save.
- **Secrets.** The webhook URL never appears in error texts, in any spelling. The database files are mode 0600. The maintenance commands (`import-config`, `export-config`, `reset-admin-password`) refuse a data directory without `syncwatch.db` and never make a new secret key.
- **Discord.** syncwatch checks the stored webhook again at each send and does not follow redirects. Names from Syncthing cannot format, link, mention or reorder text in Discord messages, also in titles and in the health line.
- **Release and build.** The release workflow runs the unit tests before it makes the image and the binaries. The `Dockerfile` pins its base images by digest. CI checks the Syncthing download against the checksum list of the release. The test stacks run their Syncthing with discovery, relays, NAT traversal and reports off from the first start.

## Dashboard

- The folder state is the same in the grid, the tiles, the status API and Home Assistant. A file watcher error is a yellow warning (before: a red error in the grid, idle in the tile).
- The Findings page lists the snoozed and ignored findings at the end, with a button to remove the snooze or the ignore. The Overview shows the count.
- A snooze until a date lasts to the end of that day, and today is accepted. A snooze can have a reason.
- The Findings page stops the automatic refresh while you type in a form, and continues when you leave the form.
- The count words agree with the numbers (1 folder, 2 folders). The resolved list of the week shows the real count and the latest 30 resolutions.
- Settings → Servers shows the fingerprint of a CA-signed server with "(CA-signed)" instead of "not pinned".
- The notification log shows the time in the configured time zone.
- A form that you send after the session expired goes to the login page and then to the dashboard, not to an error page.

## Findings

- After a restart, open findings stay open and show `stale` until syncwatch contacted the server. Before, H1 and H3 resolved at each restart and opened again with a new urgent message.
- Structure findings stay stale until the first scan after a start, and while the listing of their share fails. Before, they resolved and opened again (with messages).
- While one server is unreachable, the cross-server findings (H7, C2, H8, X1, X2, C1) of the other servers continue. Before, one unreachable server stopped all of them.
- A removed or disabled server closes its findings without a message. The history says why.
- A higher threshold no longer closes findings that are open.
- A finding ID that is not open, and not resolved in the last 90 days, cannot be snoozed or ignored. Ignores end 90 days after the finding resolved.
- An urgent check that you set to urgent later sends its urgent messages after the debounce, not immediately for all open findings.
- H4: a file watcher error is a separate WARNING finding after 1 hour, never urgent. Folder errors, no free space and failed items keep H4 as ERROR.
- H6 and C3 keep the last known state of a device through a disconnect. Before, a PC that is off each night never reached the 3 day and 30 day thresholds.
- H7 is only for PCs that were online before. A device that never connected gets only C2 after `old_device`, with the text "over 90 days ago".
- H8 compares versions as numbers (2.1.10 is newer than 2.1.9).
- After a restart while a server is unreachable, that server no longer counts as a PC.
- S1 also reports 2 Syncthing folders with exactly the same path.
- C1 uses the newest of the last pulled file and the last local change that syncwatch saw (events mode). A folder with local changes is no longer inactive.
- X1 has a stable subject.
- A request error for one folder (for example HTTP 500 on `db/status`) marks that folder with H4, not the server with H1.
- H10 counts conflicts at each full check and each reconnect, also across a Syncthing restart.

## Notifications

- The overnight summary lists each urgent finding that became due during the quiet hours, also a finding that opened before 22:00 and resolved during the night. A late summary marks the findings that became due after the quiet hours.
- The daily reminder skips findings with an urgent message in the last hour. Before, the 08:00 reminder repeated the 07:00 summary.
- Snoozed and ignored findings are not in the overnight summary.
- The resolved message waits until the finding stayed resolved for 5 minutes (or the debounce, if shorter). A server that flaps for 2 hours gives one urgent message and one resolved message, not 12 of each.
- After a start, the weekly report and the reminder wait for the first results of the servers (at most 5 minutes). The health line says "not contacted" instead of "down" for a server without a result.
- The health line uses the singular for 1 server, 1 PC, 1 error.
- A settings notice that Discord refuses (HTTP 400, 401, 403, 404) is logged one time and discarded. A failed settings notice no longer blocks the urgent messages for a day.
- If syncwatch cannot decrypt the webhook (a changed secret key), the banner and the test button say so.
- A new webhook or a test message retries immediately, also while an old send is in progress.
- Discord field errors (HTTP 400) appear in the banner with the field name. The "…and N more" count is correct.
- Settings notices exist for each change: servers (added, removed, renamed, disabled, URL, API key, GUI URL, `allow_http`), checks, thresholds, notifications, structure rules, collector, access, administrator password, API token, import, and a snooze or ignore of an urgent finding.

## Collector

- Polled mode no longer uses the full CPU while a structure scan runs longer than the interval.
- After a certificate change, the retries slow down to one every 5 minutes, with one warning in the log each hour (before: one every 10 seconds).
- An unreadable API key (a changed secret key) marks the server with H1 and the reason, and syncwatch tries again every 5 minutes.
- Error texts are readable: "connection refused", "timed out", "connection closed early" instead of ": EOF".
- Traffic totals do not jump after a downtime: a period without readings is not counted.
- Several `ConfigSaved` events in one batch cost one configuration read.
- A structure scan stops when the connection to the server fails.
- In polled mode, each structure scan also scans the project folders in Syncthing folders. Before, nested folders in Syncthing folders appeared only after the baseline scan (7 days).
- A share whose listing fails keeps its last directory list.
- After a change of the structure rules, S3 findings that no longer apply resolve at the next scan.
- Events and scans use the same definition of a nested project folder, with the depth limit `nested_scan_depth`.
- Missed events start a scan of the affected folders after 30 seconds, and at most one full check every 10 minutes.
- Structure scans in events mode start in less than 10 seconds after they are due.

## Configuration

- Durations have a maximum of 10 years and each value has a range (see the README table). A value outside the range is refused with the key and the range.
- `quiet_hours`, `reminder_time` and `weekly_time` need strict `HH:MM`. `off` works in any case. The 2 ends of `quiet_hours` must be different.
- Server IDs: `a-z`, `0-9` and `-`, 1 to 40 characters, unique without regard to case. The Home Assistant example writes them as `servers['akka']`.
- In the Structure form, a YAML without `ignore_dirs` keeps the current list, and `[]` clears it. An attribute in `share_must_have` must be a named group of `share_type_from_path`. `case_insensitive` applies to all 3 patterns.
- A Discord webhook with spaces or invisible characters around it works. The role ID needs 17 to 20 digits.
- A server URL needs a host name.
- The configuration cannot lose sections through an overflow any more.

## Application and command line

- `syncwatch import-config` while syncwatch runs takes effect in less than 10 seconds. Before, the next save in the dashboard undid it.
- The maintenance commands refuse a wrong `--data` instead of a new empty installation at that path.
- The bootstrap file is rewritten behind a symlink and in a single-file bind mount. `null` is not a secret.
- `SYNCWATCH_TLS_SELF_SIGNED` accepts `yes`, `on`, `1`, `true` (and `no`, `off`, `0`, `false`). A different value stops the start. Before, `yes` meant plain HTTP.
- `syncwatch --version` and `syncwatch -h` work.
- `docker stop` with open dashboard tabs takes less than a second (before: 5 seconds or more).
- Own certificate files are reloaded when they change. The self-signed certificate is made again 30 days before its expiry date, also while syncwatch runs. `SYNCWATCH_TLS_HOSTS` accepts `[fd00::1]` and `nas:8080`. The dashboard and the gate serve HTTP/2.
- The setup token also goes to stderr when the log level hides warnings.
- The start log shows the data directory as an absolute path.
- `secret.key` is made with an exclusive create, thus 2 first starts at the same time share one key.

## Tests and build

- The integration test takes its ports from `SW_IT_BASEPORT` (default 18600) and its work directory from `SW_IT_DIR`.
- The test harness stops the complete Syncthing process tree and waits until the ports are free.
- The Compose and load stacks run their Syncthing containers with discovery, relays, NAT traversal and reports off. A container that cannot reach that state does not start.
- CI runs `go mod tidy -diff` and govulncheck. Go 1.27.2 is the minimum. `.gitattributes` keeps LF line endings on Windows.
- `KEEP_COMPOSE=1` and `KEEP_LOADSIM=1` keep a test stack. The CI step shows the logs of a failed stack.
- The release workflow marks a tag with a `-` as a pre-release and moves `latest` only for final releases.

## Documentation

- The README, plan.md, the examples and the comments in the Dockerfile and the workflows follow the STE style guide of the review. The check titles, the page names and the labels are the names of the product text rewrite: Findings page, administrator, urgent message, settings notice, full check, structure scan, pinned certificate, ERROR, WARNING, INFO.
