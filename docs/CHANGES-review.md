# What the review of October 2026 changed

This is the plain-language list of the changes from the review of October 2026. The review had 3 rounds and a check before the test release. This list tells what you will see and what you must do after the update.

- [What you must do after the update](#what-you-must-do-after-the-update)
- [First round (r0)](#first-round-r0): the corrections of the code review and the security checks, and the text rewrite
- [Second round (r1)](#second-round-r1): the corrections after the tests in a simulated installation
- [Third round (r2)](#third-round-r2): the decisions of the team
- [Before the test release](#before-the-test-release)
- [Integrations: changes to /api/v1 after 78ef2d8](#integrations-changes-to-apiv1-after-78ef2d8)

## What you must do after the update

These items apply to an update from the version `78ef2d8` (before the review).

1. **Gate containers:** set `SYNCWATCH_GATE_LISTEN` (for example `192.168.2.10:8385`) and `SYNCWATCH_HEALTHCHECK_URL` (`https://192.168.2.10:8385/healthz`). The gate ignores `SYNCWATCH_LISTEN` now. A gate without these settings listens on `127.0.0.1:8385` and shows as unhealthy. See [examples/gate-compose.yml](../examples/gate-compose.yml).
2. **Host names:** the dashboard answers only to IP addresses, `localhost`, the host of `public_url`, the listen host and the names in `SYNCWATCH_TLS_HOSTS`. A different name gets the answer 421 with instructions. If you open the dashboard by a DNS name, add that name to `SYNCWATCH_TLS_HOSTS`. Do the same for the container name that Home Assistant uses.
3. **Settings notices:** syncwatch now sends a settings notice for each change in Settings, with the old and new values. Look at the Discord channel after the first changes and make sure that the webhook and the role are correct.
4. **Shares behind a gate:** shares from `shares.add`, and shares that `share_type_from_path` finds more than 1 level above a Syncthing folder, need `SYNCWATCH_GATE_BROWSE_ROOTS` on that gate. Without it, the gate refuses the listing (403). The share then shows the error with the instruction to add the directory to `SYNCWATCH_GATE_BROWSE_ROOTS`.
5. **Configuration values:** syncwatch now refuses values outside the ranges in the README. Examples are durations up to 10 years and thresholds from 10 seconds. Role IDs have 17 to 20 digits, and server IDs have only `a-z`, `0-9` and `-`. A stored configuration with a value outside a range stops the next start with a message. Correct it with `syncwatch import-config`.
6. **Reminder time:** a `reminder_time` in the evening part of the quiet hours is an error now, because syncwatch can never send it. Change it before the update if you have one.
7. **Data directory:** run `chmod 700` on the data directory of a bind mount. The image makes `/data` with mode 0700, and syncwatch sets the database files to mode 0600.
8. **Integrations:** read [Integrations: changes to /api/v1 after 78ef2d8](#integrations-changes-to-apiv1-after-78ef2d8) at the end of this file. Scripts must use the stable values (`check`, `id`, `severity`, `details[].key`, the keys of `counts` and `health_counts`), not the health line or the finding messages.
9. **Findings on Linux servers:** the IDs of S3 to S7 findings keep the case of the path now. After the update, such findings resolve and open again one time (one resolved message and one urgent message for S3). Snoozes and ignores on the old IDs do not apply to the new IDs.
10. **Watcher errors:** an open H4 finding with only a file watcher error resolves at the update. It opens again as a WARNING after 1 hour.
11. **Findings that this version joins:** one cause now gives one finding (see the third round). At the update, the open findings below resolve with a resolved message, but the faults continue in other findings. Tell the persons who read the Discord channel before the update.
    - An S1 finding for a Syncthing folder inside a share root resolves. The S2 finding of the share root stays open and lists the folder.
    - When a full disk of the Syncthing database stopped the folders of a server, the H4 finding of each folder resolves. Then 1 urgent H4 finding `Syncthing database on <server>` opens for the server. Its urgent message and the resolved message come at approximately the same time.
12. **Reverse proxy (r1):** if you use nginx, use `proxy_set_header Host $http_host;`. With `$host`, nginx removes the port, and the login and each save fail with "cross-origin request refused".
13. **Gate and API key (r1):** the gate reads the Syncthing API key only at the start. After a change of the API key in Syncthing, restart the gate. Until then, the gate answers `502`, and H1 shows the instruction.
14. **Gate image (r2):** update the gate on each NAS together with syncwatch, to the same version. The new gate permits the request for the files that a PC still needs (H5). An older gate refuses it: then H5 shows no file names, and the log of syncwatch tells it.
15. **Login over HTTPS (r2):** log in again after the update. Over HTTPS, the cookies have new names, and syncwatch does not read the cookies of the older version. A lobby screen with the viewer password also needs one login.
16. **Reverse proxy with HTTPS (r2):** set `SYNCWATCH_TRUSTED_PROXIES` to the address of the proxy, for example `172.18.0.2`. Without it, syncwatch ignores `X-Forwarded-Proto`. The login works, but the cookies have no `Secure` flag.
17. **Scripts and open access (r2):** with open access, a request to `/api/v1/status` or `/api/v1/ha` without an API token gets `401` now. Make an API token in Settings → API for each script.
18. **Home Assistant (r1 and r2):** compare your configuration with [examples/home-assistant.yaml](../examples/home-assistant.yaml). Change each template and automation that compares the status with `up to date`, `watch error` or `unknown`. The new values are `up_to_date`, `watch_error`, `unreachable`, `certificate_changed` and `not_contacted`. The status `error` now needs an open H4 finding, thus an automation on `error` starts later. Each entity has an `availability` template (r1). Thus a missing server or a refused API token shows as not available.
19. **Imports from a script (r2):** `POST /settings/import` has 2 steps now: a preview, then the import of the previewed file. For an import from the command line, use `syncwatch import-config`.
20. **Health check (r2):** `/healthz` and the healthcheck of the image now fail when notifications fail for more than 15 minutes. An outside check of `/healthz` then reports it. To change the 15 minutes, set `SYNCWATCH_HEALTH_NOTIFY_LIMIT`.

**Good to know before a downgrade.** An older version opens the database of this version. But it does not know the API tokens of this version: make a new API token in it. An older version also refuses to import an export of this version, because of the new keys (for example `thresholds.relay_lost`).

### Recommended after the update

- Set the site of each server in Settings → Servers, and the site of syncwatch in Settings → Notifications. Examples are "IVAR Office" and "Branch office". The messages then name the sites, and the Overview shows the connection between the sites.
- In Settings → Devices, select "Always connects through a relay" for each PC that connects only through a relay. Its H11 findings then resolve, and H12 reports a lost relay connection.
- Give each program its own API token. Give Home Assistant an API token with the scope Home Assistant.
- Add an outside check: set `SYNCWATCH_DEADMAN_URL` for a monitor service, or check `/healthz` from a different computer.
- If necessary, add a second Discord webhook only for settings notices (Settings → Notifications).
- Use `nested_scan_depth: 6` in the structure rules of IVAR Studios, as in [examples/ivar.yaml](../examples/ivar.yaml).

## First round (r0)

### Security

- **Certificates.** syncwatch pins the certificate of each server, also a CA-signed certificate. Before, a CA-signed server had no pinned certificate, and a change to a self-signed certificate was accepted without a message. Now a change to a different certificate opens H3. syncwatch accepts and logs a new CA-signed certificate for the host name, for example a new certificate from the same certificate authority. The same applies to the gate when it reaches Syncthing at an address other than loopback.
- **Gate.** Each request to the gate needs the gate token, the health request included. The gate permits only the query parameters that syncwatch sends. The gate permits a `browse` only under the parents of the Syncthing folders and the directories in `SYNCWATCH_GATE_BROWSE_ROOTS`. The gate has timeouts to Syncthing, limits the request headers to 64 KiB and logs refused requests one time each minute. A gate with an unreadable `syncthing.pin` does not start.
- **Login.** The limit of 5 failed logins in 10 minutes for each address now also applies to parallel tries. It counts the administrator password and the viewer password separately. A viewer login no longer clears the failed administrator tries.
- **Host check.** See item 2 above. This protects the dashboard against a hostile DNS name that changes to a LAN address.
- **Cookies.** The session cookies are `SameSite=Lax` instead of `Strict`. Links from Discord open the dashboard with the current session. The CSRF token, the Origin check and the Host check protect the forms.
- **Import.** An import cannot set built-in login to on while no administrator password is set. Before, that blocked the login of all persons.
- **Settings forms.** Each form has the revision of the configuration. When 2 administrators save at the same time, the second one gets a message to reload the page. Before, the second save replaced the first save without a message.
- **Secrets.** The webhook URL never appears in error texts, in any form. The database files are mode 0600. The maintenance commands (`import-config`, `export-config`, `reset-admin-password`) refuse a data directory without `syncwatch.db` and never make a new secret key.
- **Discord.** syncwatch checks the stored webhook again at each send and does not follow redirects. Names from Syncthing cannot format, link, mention or reorder text in Discord messages, also in titles and in the health line.
- **Release and build.** The release workflow runs the unit tests before it makes the image and the binaries. The `Dockerfile` pins its base images by digest. CI checks the Syncthing download against the checksum list of the release. The test stacks run their Syncthing with discovery, relays, NAT traversal and reports off from the first start.

### Dashboard

- The folder state is the same in the grid, the tiles, the status API and Home Assistant. A file watcher error is a yellow warning (before: a red error in the grid, idle in the tile).
- The Findings page lists the snoozed and ignored findings at the end, with a button to remove the snooze or the ignore. The Overview shows the count.
- A snooze until a date lasts to the end of that day, and today is accepted. A snooze can have a reason.
- The Findings page stops the automatic refresh while you type in a form. It continues when you go to a different part of the page.
- The count words agree with the numbers (1 folder, 2 folders). The resolved list of the week shows the real count and the latest 30 resolutions.
- Settings → Servers shows the fingerprint of a CA-signed server with "(CA-signed)" instead of "not pinned".
- The notification log shows the time in the configured time zone.
- A form that you send after the session expired goes to the login page and then to the dashboard, not to an error page.

### Findings

- After a restart, open findings stay open and show `stale` until syncwatch contacted the server. Before, H1 and H3 resolved at each restart and opened again with a new urgent message.
- Structure findings stay stale until the first scan after a start, and while the listing of their share fails. Before, they resolved and opened again (with messages).
- While one server is unreachable, the cross-server findings H8, X1, X2 and C1 of the other servers continue. Before, one unreachable server stopped all of them. H7 and C2 wait until syncwatch can reach every server of the PC again (r1).
- A removed or disabled server closes its findings without a message. The history gives the reason.
- A higher threshold no longer closes findings that are open.
- A finding ID that is not open, and not resolved in the last 90 days, cannot be snoozed or ignored. Ignores end 90 days after the finding resolved.
- An urgent check that you set to urgent later sends its urgent messages after the debounce, not immediately for all open findings.
- H4: a file watcher error is a separate WARNING finding after 1 hour, never urgent. Folder errors, no free space and failed items keep H4 as ERROR.
- H6 and C3 keep the last known state of a device through a disconnect. Before, a PC that is off each night never reached the 3 day and 30 day thresholds.
- H7 is only for PCs that were online before. A device that never connected gets only C2 after `old_device`, with the text "over 90 days ago".
- H8 compares versions as numbers (2.1.10 is newer than 2.1.9).
- After a restart while a server is unreachable, syncwatch no longer shows that server as a PC.
- S1 also reports 2 Syncthing folders with exactly the same path.
- C1 uses the newest of the last pulled file and the last local change that syncwatch saw (events mode). A folder with local changes is no longer inactive.
- X1 has a stable subject.
- A request error for one folder (for example HTTP 500 on `db/status`) marks that folder with H4, not the server with H1.
- H10 counts conflicts at each full check and each reconnect, also through a restart of Syncthing.

### Notifications

- The overnight summary lists each urgent finding that became due during the quiet hours. This includes a finding that opened before 22:00 and resolved during the night. A late summary marks the findings that became due after the quiet hours.
- The daily reminder skips findings with an urgent message in the last hour. Before, the 08:00 reminder repeated the 07:00 summary.
- Snoozed and ignored findings are not in the overnight summary.
- The resolved message waits until the finding stayed resolved for 5 minutes (or the debounce, if shorter). A server that flaps for 2 hours gives one urgent message and one resolved message, not 12 of each.
- After a start, the weekly report and the reminder wait for the first results of the servers (at most 5 minutes). The health line shows "not contacted" instead of "down" for a server without a result.
- The health line uses the singular for 1 server, 1 PC, 1 error.
- A settings notice that Discord refuses (HTTP 400, 401, 403, 404) is logged one time and discarded. A failed settings notice no longer blocks the urgent messages for a day. (r2 keeps the settings notices about security, see the third round.)
- If syncwatch cannot decrypt the webhook (a changed secret key), the banner and the test button tell it.
- A new webhook or a test message tries again immediately, also while an old send is in progress.
- Discord field errors (HTTP 400) appear in the banner with the field name. The "…and N more" count is correct.
- There is a settings notice for each change of a server: added, removed, renamed, disabled, and a new URL, API key, GUI URL or `allow_http`.
- There is also a settings notice for the checks, the thresholds, the notifications, the structure rules, the collector and the access settings.
- The administrator password, the API token, an import, and a snooze or ignore of an urgent finding also give a settings notice.

### Collector

- Polled mode no longer uses the full CPU while a structure scan runs longer than the interval.
- After a certificate change, syncwatch tries again every 5 minutes and logs one warning each hour. Before, it tried again every 10 seconds.
- An unreadable API key (a changed secret key) marks the server with H1 and the reason, and syncwatch tries again every 5 minutes.
- Error texts are readable: "connection refused", "timed out", "connection closed early" instead of ": EOF".
- Traffic totals do not jump after a downtime: syncwatch does not count a period without readings.
- Some `ConfigSaved` events in one batch need only one read of the configuration.
- A structure scan stops when the connection to the server fails.
- In polled mode, each structure scan also scans the project folders in Syncthing folders. Before, nested folders in Syncthing folders appeared only after the baseline scan (7 days).
- A share whose listing fails keeps its last directory list.
- After a change of the structure rules, S3 findings that no longer apply resolve at the next scan.
- Events and scans use the same definition of a nested project folder, with the depth limit `nested_scan_depth`.
- Missed events start a scan of the affected folders after 30 seconds, and at most one full check every 10 minutes.
- Structure scans in events mode start in less than 10 seconds after they are due.

### Configuration

- Durations have a maximum of 10 years and each value has a range (see the README table). syncwatch refuses a value outside the range and names the key and the range.
- `quiet_hours`, `reminder_time` and `weekly_time` need strict `HH:MM`. `off` works in any case. The 2 ends of `quiet_hours` must be different.
- Server IDs: `a-z`, `0-9` and `-`, 1 to 40 characters, unique without regard to case. The Home Assistant example writes them as `servers['akka']`.
- In the Structure form, a YAML without `ignore_dirs` keeps the current list, and `[]` clears it. An attribute in `share_must_have` must be a named group of `share_type_from_path`. `case_insensitive` applies to all 3 patterns.
- A Discord webhook with spaces or invisible characters around it works. The role ID needs 17 to 20 digits.
- A server URL needs a host name.
- The configuration cannot lose sections through an overflow any more.

### Application and command line

- `syncwatch import-config` while syncwatch runs takes effect in less than 10 seconds. Before, the next save in the dashboard undid it.
- The maintenance commands refuse an incorrect `--data` instead of a new empty installation at that path.
- syncwatch also writes the bootstrap file again behind a symlink and in a single-file bind mount. `null` is not a secret.
- `SYNCWATCH_TLS_SELF_SIGNED` accepts `yes`, `on`, `1`, `true` (and `no`, `off`, `0`, `false`). A different value stops the start. Before, `yes` gave plain HTTP.
- `syncwatch --version` and `syncwatch -h` work.
- `docker stop` with open dashboard tabs takes less than a second (before: 5 seconds or more).
- syncwatch reads its own certificate files again when they change. It makes a new self-signed certificate 30 days before the expiry date, also while it runs.
- `SYNCWATCH_TLS_HOSTS` accepts `[fd00::1]` and `nas:8080`. The dashboard and the gate serve HTTP/2.
- The setup token also goes to stderr when the log level hides warnings.
- The start log shows the data directory as an absolute path.
- syncwatch makes `secret.key` only when the file does not exist. Thus 2 first starts at the same time use the same secret key.

### Tests and build

- The integration test takes its ports from `SW_IT_BASEPORT` (default 18600) and its work directory from `SW_IT_DIR`.
- The test harness stops the complete Syncthing process tree and waits until the ports are free.
- The Compose and load stacks run their Syncthing containers with discovery, relays, NAT traversal and reports off. A container that cannot reach that state does not start.
- CI runs `go mod tidy -diff` and govulncheck. Go 1.27.2 is the minimum. `.gitattributes` keeps LF line endings on Windows.
- `KEEP_COMPOSE=1` and `KEEP_LOADSIM=1` keep a test stack. The CI step shows the logs of a failed stack.
- The release workflow marks a tag with a `-` as a pre-release and moves `latest` only for final releases.

### Documentation

- The README, plan.md, the examples and the comments in the Dockerfile and the workflows follow ASD-STE100 (Simplified Technical English).
- The check titles, the page names and the labels are the names of the product text rewrite. Examples are Findings page, administrator, urgent message, settings notice, full check, structure scan and pinned certificate.
- The severities are ERROR, WARNING and INFO.

## Second round (r1)

### Findings

- When Syncthing stops on a server, syncwatch no longer resolves the folder errors (H4). Before, a stop sent a false "Resolved" message and, after the start, a second urgent message. A real repair now resolves approximately 10 seconds after Syncthing reports it.
- H5 for a device stays open (stale) while the device is off, and counts only the time while the device is connected. Before, H5 resolved at each disconnect, and a PC that is off each night never got H5.
- H5 never shows 100 percent for a device that needs files. A device at 100 percent with remaining items is also stuck.
- H7 and C2 wait while syncwatch cannot reach a server of the PC. Before, a VPN outage opened H7 for the PCs of the other NAS.
- A disabled server stays a server. Before, its device was a PC and got H7 when the server was off.
- The H1 message shows when a server refuses the API key or the gate token, and a detail row tells what to do. For a failure to connect, H1 and H2 name the network between the servers as a possible cause.
- H2 waits 2 minutes after the connection stops, after a start of Syncthing, and when syncwatch can reach both servers again.
- H9 gives the name of a device that another server knows. H7 gives the last contact with the date and the time.
- In events mode, H10 also counts the conflict copies from other devices that reach the server.
- The tag `stale` and the Discord lines show "no current data, last known state". A finding can also be stale while a device is off.

### Notifications

- If the notifications fail, syncwatch tries again every 5 minutes or sooner during a Discord outage. For a refused webhook, it tries again up to every 30 minutes. After the failure, a section lists the urgent findings that opened and resolved during the failure.
- The title of a grouped urgent message names the servers and the checks. The same finding on 2 servers has one line.
- The test message mentions the role.
- The overnight summary does not list a finding again that already had its urgent and resolved messages.
- The weekly report lists and counts only ERROR and WARNING findings that are not snoozed or ignored.
- The health line shows ignored findings, a changed certificate, disabled servers and PCs with an unknown state.
- Settings notices are gray and use the labels of the Settings page. `import-config` and `reset-admin-password` on the command line also send a settings notice.

### Dashboard and Settings

- If the live connection stops, the page shows a banner. When the connection is back, the page loads the new data.
- A server that is not up shows its folders in gray, as the last known state.
- Invisible direction characters in names show as marks, for example `‹U+202E›`.
- A refused save in Checks, Collector or Notifications keeps the values that you typed and shows the error above the Save button.
- Settings has help texts for Urgent, Debounce and the collector settings. The buttons Replace and Revoke API token ask you to make sure first.
- Settings shows a secret that syncwatch cannot decrypt as "cannot decrypt", not as "Set". This applies to the API keys, the webhook and the API token.
- Failed logins at an address no longer block the confirmations of a logged-in administrator. Each session has its own limit for confirmations.

### Gate, collector, log and command line

- The gate answers 502 with an instruction when Syncthing refuses its API key. It does not forward error answers that can contain secrets. Each client address can have at most 4 event long-polls and 64 other requests at the same time.
- A 502 or 429 from the gate on a folder request marks the server (H1), not the folder (H4).
- syncwatch logs refused requests, unknown API tokens and the rate limits at most one time each minute for each client, with a count. The log shows an error at the start when the secret key cannot decrypt the stored secrets.
- `syncwatch probe` exits with 1 when it stops early. If it cannot write the report file, it writes the report to standard output.

## Third round (r2)

The team agreed each change of the third round. After the live check, a repair corrected 2 small faults (see [Repairs after the live check](#repairs-after-the-live-check)).

### Sites and connections

- Each server can have a site: the office where the server is, for example "IVAR Office" or "Branch office". syncwatch also has a site. Type them in Settings → Servers and Settings → Notifications.
- With the sites, H1 names the 2 sites: "Server SOL (Branch office) is unreachable from IVAR Office: timeout". The first possible cause is then the VPN between the 2 sites.
- H2 names the VPN between the 2 sites in "What to check".
- The Overview has a new section Connections below the server tiles. It shows if the sites are connected, also when both server tiles are green.
- With 2 or more sites, the Overview and the Folders page have the list Site. Select a site to show only its servers and findings. The browser keeps the selected site.
- Each server tile shows its site.

### Findings

- One cause gives one finding. A share root that is a Syncthing folder gives 1 S2 finding, which lists the Syncthing folders inside it. These folders get no S1.
- A full disk of the Syncthing database gives 1 H4 finding for the server, with the list of the stopped folders. Before, each folder had its own urgent H4.
- When Syncthing cannot store a file because the disk of the folder is full, H4 opens immediately. Before, H4 waited for `failing_files` (1 hour).
- New check H12 "Relay connection lost" (WARNING). A device that always connects through a relay gets no H11. H12 opens when that device has no connection for more than 30 minutes.
- H12 also opens when a server has no connection to its relay for more than 30 minutes.
- New check H13 "Certificate expires soon" (INFO). The certificate of a server expires in less than 30 days, or it expired. syncwatch continues to read the server.
- H5 for a PC names the files that the PC still needs, at most 20. An example is a file name that Windows cannot store.
- H5 shows the data received, the speed in the last 10 minutes and the time left. A PC that receives no data gets no time left.
- C1 also opens for a project folder that no person changed after the copy. It then uses the time when syncwatch first saw the folder. syncwatch keeps that time in its database.
- The C1 message gives the date and the time of the last change in the time zone of syncwatch.
- syncwatch makes a baseline scan every day when the last baseline scan took less than 10 seconds. Thus it finds a project folder that a person copies into another project folder in less than 1 day.

### Notifications

- Settings notices about security wait until a channel accepts them, also through a restart of syncwatch. Examples are a new API key, a removed pinned certificate and a new password.
- An optional second Discord webhook gets only the settings notices. They then go to their own channel, without the role mention.
- syncwatch can send an urgent message again while an ERROR finding of a server stays open: every hour, 2 hours or 4 hours. This repeat is off by default.
- Urgent findings that fall due in the same 10 seconds share one urgent message.
- A resolved message tells what is correct again and how long the fault lasted. An example is "Servers AKKA and SOL are connected again (the fault lasted 7 minutes, 21:10–21:17)".
- After a stop of syncwatch, the next message about findings gives the time when syncwatch did not run.
- Urgent messages, repeats and the daily reminder show the first advice of a finding under its line. Thus a person at the other site can read it also when the VPN is down.
- The views of 1 PC from 2 servers are 1 line in each message: "(also seen from SOL)".
- While notifications fail, the banner gives the time of the next try.
- syncwatch sends a settings notice when it pins a certificate at the first contact, or when it pins a new CA-signed certificate.
- Settings notices name each setting by its label on the Settings page. A settings notice about a check that is not urgent does not name an urgent message.

### Dashboard and Settings

- An import first shows a preview and does not change the configuration. The preview lists the sections that change, the servers that the import removes and the types of secrets in the file.
- An import that removes servers needs a checkbox and the administrator password.
- The new Settings section Devices lists the PCs. Select "Always connects through a relay" for a PC that connects only through a relay.
- Settings → Notifications has the site of syncwatch, the second webhook with its test button, and "Repeat urgent messages".
- Settings → API has up to 5 API tokens. Each API token has a name, a scope and the time of its last use.
- Viewers see only the first part of each device ID, and no links "Open in Syncthing".
- A duration that is not valid, for example "3 days", gets a message with the correct format.
- The new lobby page `/lobby` is for a screen in the office. It shows the sites, the servers and the urgent and ERROR findings, without links and forms.

### API and Home Assistant

- The JSON API needs an API token or a login, also with open access.
- An API token with the scope Home Assistant can read only `/api/v1/ha`. That answer has no device IDs, addresses or folder paths.
- Each time in the API is UTC, without fractions of a second. The status values of Home Assistant have no spaces. New values tell why syncwatch has no current data about a server.
- `/api/v1/ha` has the counts of the findings, the state of the notifications, the sites and the connections.
- The status `error` in Home Assistant needs an open H4 finding. Syncthing repairs many failed items itself, and H4 waits for that.
- `/api/v1/status` gives the end and the reason of a snooze.
- The details are in [Integrations: changes to /api/v1 after 78ef2d8](#integrations-changes-to-apiv1-after-78ef2d8), items 18 to 26.

### Health check and security

- `/healthz` answers 503 when notifications fail for more than 15 minutes, or when syncwatch did not evaluate the checks for more than 5 minutes. Docker then shows the container as unhealthy.
- With `SYNCWATCH_DEADMAN_URL`, syncwatch calls an outside monitor service while it is healthy. When the calls stop, that service sends its own alarm.
- syncwatch accepts `X-Forwarded-Proto` only from the proxies in `SYNCWATCH_TRUSTED_PROXIES`.
- Over HTTPS, the cookies have the names `__Host-sw_session` and `__Host-sw_csrf`. The CSRF token belongs to the session and changes at each login.
- syncwatch refuses a form with `Origin: null`, and a form from an `http://` page when the dashboard uses HTTPS.
- `import-config` names the servers that the file removes. Its settings notice and the settings notice of `reset-admin-password` are settings notices about security.

### Gate and collector

- The gate permits `/rest/db/remoteneed`, for the file names of H5. Update the gate on each NAS.
- Each minute, syncwatch reads the state of the relay listeners of each server, with 1 more small request.
- syncwatch keeps the first-seen time and the last local change of each folder in its database.

### Repairs after the live check

- A resolved message after a restart of syncwatch gives the correct end of the fault.
- The speed of H5 is steady. A PC that does not receive data gets no time left.

## Before the test release

### Release and build

- The `linux/arm64` image contains the program for arm64. Before, the `Dockerfile` put the amd64 program into the arm64 image, and that image cannot start on an ARM NAS. The `linux/amd64` image and a plain `docker build` do not change.
- Before the release workflow makes the image available, it checks that the image of each platform contains the program for that platform.
- The image of the release workflow has provenance, a record of the build. The image index then has 1 more entry for each platform, with the platform `unknown/unknown`. The test release shows how the container interfaces of NAS products show these entries.

### Documentation

- The README example for Home Assistant on the same Docker network is `https://syncwatch:8080` with `verify_ssl: false`. The example `http://syncwatch:8080` gets the answer HTTP 400 from the image, because the image serves HTTPS.
- The README and this list tell which open findings resolve one time after the update (item 11 of [What you must do after the update](#what-you-must-do-after-the-update)).

## Integrations: changes to /api/v1 after 78ef2d8

This list is for scripts, Home Assistant and other programs that read `GET /api/v1/status` or `GET /api/v1/ha`. `78ef2d8` is the version before the review. Use the stable values: `check`, `id`, `severity`, `details[].key`, the keys of `counts` and `health_counts`, and the status values. The texts for persons (the health line, `findings[].message`) can change in each version. The README has the [field reference](../README.md#field-reference) of both answers.

**Unchanged.** `findings[].severity` is `ERROR`, `WARN` or `INFO` (upper case). The keys of `counts` are `error`, `warn`, `info`, `snoozed` and `urgent`. r1 adds the key `ignored`. The detail values do not change. The time fields are UTC, and r2 changes their form (item 20).

Changes of the first round (r0) and of the text rewrite:

1. **Host check.** The API answers only to known host names, also for requests with an API token. A request with a different `Host` header gets `421`. Add the name to `SYNCWATCH_TLS_HOSTS`, or set `public_url`.
2. **`notifications.error`** is present only for a request with the administrator role: an administrator session, or each request in proxy mode. A request with an API token or a viewer session does not get it.
3. **`servers[].folders`** has the new counts `scanning` and `watch_error`. The counts use the folder state of the dashboard: `paused`, `error`, `watch_error`, `scanning`, `idle` (up to date), and `syncing` for all other folders. Before, a folder that scanned counted in `syncing`. A folder with only a file watcher error counted in `idle`.
4. **`/api/v1/ha`** has the new counts `folders_scanning` and `folders_watch_error`. The field `status` has the new value `watch error` (r2 changes it to `watch_error`, item 21).
5. **The health line** has new words:

| 78ef2d8 | Now |
|---|---|
| `2 servers OK` | `2 servers up` |
| `1/2 servers OK (SOL down)` | `1 of 2 servers up (SOL down)` |
| `1/2 servers OK (SOL down)`, before the first contact | `1 of 2 servers up (SOL not contacted)` |
| `6 PCs, 0 offline > 3 minutes` | `6 PCs, 0 offline more than 3 minutes` |
| `0 errors, 2 warnings, 0 suggestions` | `ERROR 0 · WARNING 2 · INFO 0` |
| `1 servers OK`, `1 PCs` | `1 server up`, `1 PC` |

6. **`findings[].message`** has new texts. For example, the H4 text `Sync stopped for …` is now `Folder … cannot sync: …`. Use `check`, `id` and `details[].key`, not the message.
7. **`findings[].details[].key`.** The text rewrite changed 7 keys. r1 restores the keys of 78ef2d8: `Watch error`, `Failing items`, `Items left`, `Bytes left`, `Type`, `URL` and `Down since`. The dashboard shows these keys with the labels of the text rewrite (for example `Remaining items`). r0 added the keys `Based on` (C1) and `Folder` (X1).
8. **Finding IDs.** On a case-sensitive server, the IDs of S3 to S7 findings keep the case of the path. This changes the IDs one time at the update. A file watcher error is a separate H4 finding with its own ID: `WARN` after 1 hour, never urgent.

Changes of the second round (r1):

9. **`health_counts`** (new) has the numbers of the health line: `servers` (disabled servers included), `servers_up`, `servers_disabled`, `pcs`, `pcs_online`, `pcs_offline` and `pcs_unknown`. A PC is offline only when each server that knows the PC is up. Else the PC is unknown.
10. **Disabled servers.** `servers[]` also lists the disabled servers, with `status` `disabled`. `devices[]` marks their device IDs as servers (`server: true`), not as PCs. Before, a disabled server was not in `servers[]` and counted in the PCs.
11. **`servers[].folders.stale`** is `true` when the server is not up (unreachable, not contacted or with a changed certificate). The folder counts are then the last known state.
12. **`findings[].ignored`** and **`counts.ignored`** (new). `findings[].snoozed` and `counts.snoozed` do not change: they include the ignored findings.
13. **`servers[].uptime_s`** is `null` when syncwatch does not know the uptime. Before, the key was not in the answer.
14. **Authorization.** syncwatch accepts the word `Bearer` in any case (for example `bearer`).
15. **Error answers** are JSON (`{"error": "…"}`) with `Content-Type: application/json`, also the `421` of the Host check for `/api/v1/*`. Before, the content type was `text/plain`.
16. **The health line** has new texts. Ignored findings have their own count (`1 ignored`). A server with a changed certificate shows as `SOL certificate changed` instead of `SOL down`, and a disabled server as `SOL disabled`. The line also counts the PCs whose state is not known.
17. **`findings[].details`** of H1 and H2 have new rows with advice, with new keys. The other keys do not change.

Changes of the third round (r2):

18. **Access.** Without an API token, only a logged-in session (administrator or viewer) or proxy mode reads the API. With open access, a request without an API token gets `401 {"error":"token required"}` on both addresses.
19. **API tokens.** There are up to 5 named API tokens with the scope `ha` (only `/api/v1/ha`) or `full` (both addresses). An `ha` API token gets `403 {"error":"the API token permits only /api/v1/ha"}` from `/api/v1/status`, with `WWW-Authenticate: Bearer realm="syncwatch", error="insufficient_scope", scope="full"`. The API token of the older version is now the API token "API token" with the scope `full`.
20. **Times.** Each time is UTC without fractions of a second, for example `2026-10-11T15:29:17Z`. Before, `servers[].last_contact` and `devices[].last_seen` had the local offset and fractions of a second. A time that syncwatch does not know is `null`, and the key is always in the answer. Before, the keys `last_contact`, `cert_expires`, `last_seen` and `notifications.since` were not in the answer when the time was not known.
21. **Status values of Home Assistant.** `up to date` is now `up_to_date`, and `watch error` is now `watch_error`. `unknown` is now `unreachable` (the server is down), `certificate_changed` (a changed certificate) or `not_contacted` (no full check after the start). These values do not change: `syncing`, `scanning`, `error`, `paused`, `disabled`.
22. **Status `error` of Home Assistant.** The status is `error` only while an H4 finding with the severity ERROR is open for the server, snoozed or not. For failed items without H4, the status is `syncing`. `folders_error` does not change.
23. **New in /api/v1/status:** `servers[].site`, `notifications.next_try`, `findings[].snooze_until`, `findings[].snooze_reason`, `links[]` and `site_links[]`. `servers[]` and `devices[]` are `[]` when they are empty (before: `null`).
24. **New in /api/v1/ha:** at the top `findings_open`, `findings_urgent`, `findings_error`, `findings_warn`, `findings_info`, `notifications_failing`, `notifications_failing_since`, `links` and `site_links`. For each server: `site`, `last_contact`, `findings_open` and `findings_urgent`.
25. **`findings[].since`** is `null` for C1 and H13. The age of these findings counts from `opened_at`. The C1 message gives the time of the last change. Before, `since` of C1 was the time of the last change.
26. **Findings in the answer.** `findings[].check` has the new values `H12` and `H13`. A share root that is a Syncthing folder gives 1 S2 finding with a row `Syncthing folder inside` for each folder in it. These folders get no S1. A full disk of the Syncthing database gives 1 H4 finding for the server (subject "Syncthing database on SOL") and no H4 for each folder. A device that always connects through a relay gets no H11. The new detail keys are in the README (field reference). The keys of 78ef2d8 do not change.
