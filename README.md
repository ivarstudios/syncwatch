# syncwatch

[![CI](https://github.com/ivarstudios/syncwatch/actions/workflows/ci.yml/badge.svg)](https://github.com/ivarstudios/syncwatch/actions/workflows/ci.yml)

syncwatch is a small **read-only** monitor for [Syncthing](https://syncthing.net). IVAR Studios made it for installations with some servers (NAS hardware, on at all times) and many PCs. syncwatch gives you:

- a **dashboard** with server tiles, a grid of folders and devices and a Findings page. The dashboard is usable on a phone and refreshes automatically.
- a **weekly report** in Discord.
- **urgent messages** in Discord for large faults. Examples: a server unreachable, servers not connected, a folder that stopped the sync, a nested project folder.

syncwatch **never changes Syncthing**. It reads the REST API of each server with a typed client. The client can only send `GET` requests to an allowlist of endpoints. Thus syncwatch needs no SMB mounts, no file system access and no software on the PCs.

![Overview](docs/screenshots/overview.png)

**Status:** pre-release. There is no tagged version. The image and the binaries named below appear with the first release. Until then, [build syncwatch yourself](#build-it-yourself). syncwatch supports Syncthing 2.x only.

## Contents

- [Install](#install) · [First start](#first-start) · [Configuration](#configuration) · [Checks](#checks) · [Notifications](#notifications)
- [Security](#security) · [JSON status API](#json-status-api) · [Home Assistant](#home-assistant) · [Server probe](#server-probe) · [Development](#development) · [Limitations](#known-limitations)

## Install

The Docker image `ghcr.io/ivarstudios/syncwatch` is available for `linux/amd64` and `linux/arm64`.

```yaml
services:
  syncwatch:
    image: ghcr.io/ivarstudios/syncwatch:1.0.0   # also use the digest, see Updates
    container_name: syncwatch
    restart: unless-stopped
    user: "65532:65532"          # the user of the image. It must own ./data
    read_only: true
    tmpfs: [/tmp]
    cap_drop: [ALL]
    security_opt: ["no-new-privileges:true"]
    environment:
      - TZ=Europe/Stockholm
      - SYNCWATCH_TLS_HOSTS=192.168.1.10   # the address that you open: for the certificate and the Host check
    ports:
      - "192.168.1.10:8080:8080"  # LAN address only
    volumes:
      - ./data:/data
```

Caution: keep `data` private. `secret.key` in it decrypts the stored API keys.

```sh
mkdir data && sudo chown 65532:65532 data && chmod 700 data   # the same user as the user: line
docker compose up -d
docker logs syncwatch                       # shows the setup token
```

The image runs as the non-root user 65532 with a read-only root file system. A named volume gets the owner 65532 from the image. A bind mount works with any user ID that owns the directory: use that ID in the `user:` line. If the user cannot write to `/data`, syncwatch stops at the start with "permission denied" in the log.

The image writes only to `/data`: an SQLite database, `secret.key` and, with self-signed HTTPS, its certificate. The image makes `/data` with mode 0700. A bind mount keeps the mode of the host directory. syncwatch sets the database files to mode 0600 at each start. Make a backup of `/data` (see [Backup and restore](#backup-and-restore)).

If you lose `secret.key`, or change `SYNCWATCH_SECRET_KEY`, syncwatch cannot decrypt the stored API keys and the webhook. The dashboard then shows an H1 finding for each server with the reason that the API key is not readable. The notification banner reports that the webhook is not readable. Settings shows the webhook and the API token as set, but not readable. Type the API keys, the webhook and the API token again, or restore the old secret key.

The log reports the secret key at the start. If syncwatch makes a new `secret.key` while the database contains secrets, the log shows an error: the old secret key is missing or replaced. If `SYNCWATCH_SECRET_KEY` is set and `/data/secret.key` also exists, the log shows a warning, because syncwatch uses only the variable.

The binary `syncwatch` also runs without Docker, also on Windows: `syncwatch serve --data ./data`. See the [releases](https://github.com/ivarstudios/syncwatch/releases). The binary listens on `127.0.0.1:8080`. To listen on a LAN address, give `--listen 192.168.1.10:8080` or set `SYNCWATCH_LISTEN`. The image listens on `:8080` in the container. Thus the address in `ports:` decides who can reach the dashboard. Always give that address: a `-p 8080:8080` without an address opens the port on each interface of the host.

The data directory is `--data` or `SYNCWATCH_DATA`. The default is `/data` if that directory exists, or `./data` in the current directory. The start log shows the resolved path.

Caution: do not connect syncwatch to the internet. Use the LAN, a VPN or a reverse proxy with its own authentication.

### Build it yourself

```sh
git clone https://github.com/ivarstudios/syncwatch && cd syncwatch
docker build -t syncwatch:dev .                 # the image: write image: syncwatch:dev above
go build -o syncwatch ./cmd/syncwatch           # or the binary (Go 1.27.2 or later)
```

The image builds for `linux/amd64` and `linux/arm64` (`docker buildx build --platform …`). The binary cross-compiles with `GOOS`, `GOARCH` and `CGO_ENABLED=0`.

### Where to run syncwatch

syncwatch cannot report a failure of its own host. If that host is a monitored server and it loses power or stops, syncwatch does not send the H1 urgent message. Do one of these:

- Run syncwatch on a host that it does not monitor. Examples: a different NAS, or a small computer that is on at all times. The host must reach the API URL of each server.
- If syncwatch runs on a monitored server, add an outside check of `https://<host>:8080/healthz`. The outside check (Uptime Kuma, healthchecks.io, Home Assistant, the router) must send its own notification.

Without one of these, a missed Monday report is the only sign that syncwatch or its host stopped.

### HTTPS

The image serves HTTPS by default. Thus the password and the session cookie never cross the LAN unencrypted. syncwatch makes a self-signed certificate in `/data` and logs the first 8 bytes of its SHA-256 fingerprint (for example `B6:CF:ED:46:0C:D4:10:AA`). Each browser asks one time to accept it. Compare the start of the fingerprint in the browser with the log. The certificate includes `localhost`, the host of `public_url`, a specific listen address and the names in `SYNCWATCH_TLS_HOSTS` (comma-separated). Set `SYNCWATCH_TLS_HOSTS` to the address that you open, because the container only sees `:8080`.

Entries in the form `[fd00::1]` or `nas:8080` are accepted: syncwatch removes the brackets and the port. syncwatch does not accept wildcards. An entry that is not an IP address or a DNS name gives a warning in the log, and syncwatch ignores it. When these names change, the next start makes a new certificate and the browsers ask again. syncwatch also makes a new self-signed certificate while it runs, when fewer than 30 days of validity remain. The browsers then ask again.

- Your own certificate: `SYNCWATCH_TLS_CERT` and `SYNCWATCH_TLS_KEY`. They have priority over the self-signed certificate. syncwatch reads the 2 files again when they change (it looks at the files at most one time each minute). Thus a new certificate from certbot needs no restart.
- Plain HTTP: `SYNCWATCH_TLS_SELF_SIGNED=false`. The permitted values are `true`, `false`, `1`, `0`, `yes`, `no`, `on` and `off` (`t` and `f` also work). A different value stops the start with an error that names the variable.
- The binary serves plain HTTP if you set none of these. It listens on loopback by default.

The dashboard serves HTTP/2. Cookies have the `Secure` flag over HTTPS. `syncwatch healthcheck` gets the address and the protocol from `SYNCWATCH_LISTEN`, `SYNCWATCH_TLS_SELF_SIGNED` and `SYNCWATCH_TLS_CERT`. The image sets the first 2. For the binary, give the healthcheck the same variables as `serve`, or set `SYNCWATCH_HEALTHCHECK_URL`. Without them, the healthcheck sends HTTP to an HTTPS server and reports "HTTP 400".

**Host check.** syncwatch answers only to known host names: IP addresses, `localhost`, the host of `public_url`, the listen host and the names in `SYNCWATCH_TLS_HOSTS`. A request with a different `Host` header gets the answer `421 Misdirected Request` and a message that shows what to set. syncwatch ignores the port and a final dot and compares the names without regard to case. The check applies to each request, also to requests with an API token. syncwatch logs the refused requests with the host name and the client address. It writes at most one line each minute for each client address, with a count. The log shows at most 100 characters of the host name.

### Updates

syncwatch keeps API keys that can change all data in your Syncthing. Thus update syncwatch manually, not automatically. Use the digest of the image. Then a tag that moves cannot change what runs:

```sh
docker buildx imagetools inspect ghcr.io/ivarstudios/syncwatch:1.0.1   # shows "Digest: sha256:…"
```

```yaml
    image: ghcr.io/ivarstudios/syncwatch:1.0.1@sha256:<digest>
```

1. Read the release notes.
2. Change the `image:` line.
3. Run `docker compose up -d`.

Keep auto-updaters such as Watchtower off for this container. The tag `latest` points to the latest final release. A pre-release tag (with a `-`, for example `v1.2.0-rc1`) gets its own tag only. The `Dockerfile` also pins its base images by digest.

### Backup and restore

`/data` has the database (`syncwatch.db`, `syncwatch.db-wal` and `syncwatch.db-shm`), `secret.key` and the TLS files. Make a backup of all of `/data`. If you use `SYNCWATCH_SECRET_KEY`, make a backup of the database and keep a copy of the secret key in a different location. Without the secret key, syncwatch cannot decrypt the API keys, the webhook and the API token of the backup.

Caution: a backup with `secret.key` decrypts the stored API keys. Keep it private, the same as `/data`.

To make a backup:

1. Stop the container: `docker compose stop syncwatch`.
2. Copy all of the data directory: `cp -a data data-backup`.
3. Start the container: `docker compose start syncwatch`.

If you cannot stop the container, copy `syncwatch.db`, `syncwatch.db-wal` and `syncwatch.db-shm` together, or use `sqlite3 data/syncwatch.db ".backup data-backup/syncwatch.db"` on the host. A copy of `syncwatch.db` alone from a container that runs does not have the latest changes, because they are in `syncwatch.db-wal`.

To restore a backup:

1. Stop the container.
2. Replace the contents of the data directory with the backup.
3. Give the files to the user of the `user:` line: `chown -R 65532:65532 data && chmod 700 data`. syncwatch sets the database files to mode 0600 at the start.
4. Start the container.

A restored backup has the findings and the notifications of its time. syncwatch then reports the changes after the time of the backup again. For example, it sends a resolved message for a finding that resolved after the backup.

## First start

1. Open `https://<host>:8080` (`http://` if [HTTPS](#https) is off). Accept the self-signed certificate one time.
2. The dashboard shows the **First start** page while no administrator password is set. Type the **setup token** from the log (`docker logs syncwatch`). Then type a new administrator password. The setup token shows that you can read the log of the host. Thus the first visitor on the LAN cannot take the administrator account. If the log level hides warnings, syncwatch also prints the setup token on stderr. Or set `SYNCWATCH_ADMIN_PASSWORD` for the first start.
3. Open **Settings → Servers**. Add each server that syncwatch must poll. Type a name, the API URL (for example `https://192.168.1.10:8384`) and its API key. The API key is in Syncthing under Actions → Settings → API Key. syncwatch pins the certificate of the server on first use, self-signed or CA-signed. syncwatch sends an API key only to the URL that you typed it for. When you change the URL of a server, type its API key again.
4. Open **Settings → Notifications**. Paste a Discord webhook URL (Channel → Integrations → Webhooks) and the numeric ID of the role to mention in urgent messages. Select **Send test message** to check it. The test message mentions the role, the same as an urgent message. If your phone shows no notification, check the role ID and make sure that you have the role.
5. If necessary, paste structure rules under **Settings → Structure** (see [examples/ivar.yaml](examples/ivar.yaml)).

The host of syncwatch must reach the API URL of each server. The PCs do not need to be reachable: syncwatch sees them through the servers.

Each server has an ID that syncwatch makes from the name (`AKKA` gives `akka`). An explicit `id` in the YAML must have 1 to 40 characters from `a-z`, `0-9` and `-`. IDs and names are unique without regard to case, and a name must not be the ID of a different server. A generated ID that exists already gets a number (`server-2`).

### Bootstrap file

Instead of the settings forms, you can put a YAML file at `/data/syncwatch.yaml` before the first start. Or point `SYNCWATCH_CONFIG` to the file. syncwatch imports the bootstrap file one time, when the database has no configuration. The import includes `api_key`, `discord_webhook` and `api.token`. syncwatch encrypts these secrets in the database and removes them from the file. To remove them, syncwatch writes the file again: the comments stay, but the format of the file can change.

syncwatch also rewrites a file behind a symlink and a single-file bind mount. If syncwatch cannot rewrite the file (a read-only mount), the log reports it at each start until you delete the file. A value of `null` or `~` is not a secret.

A bootstrap file with an error stops the start of syncwatch. Examples are an unknown key, a value outside its range, or a webhook that is not a Discord URL. The log (`docker logs syncwatch`) names the line or the key. Correct the file or remove it. Until then, a container with `restart: unless-stopped` starts and stops again and again.

Later changes: use the dashboard, or **Settings → Export and import**, or `syncwatch import-config FILE`. A syncwatch that runs applies the imported file in less than 10 seconds and sends a settings notice with the changed sections. Delete the file after the import if it had secrets. Exports never contain secrets.

The maintenance commands (`import-config`, `export-config`, `reset-admin-password`) need the data directory of the installation and its secret key. The data directory is `--data`, `SYNCWATCH_DATA`, or `/data` in the image. A path without `syncwatch.db` is an error: the commands make no new data directory, no database and no secret key.

## Configuration

Each setting is available on the Settings page. The YAML format (export and import) is this. Each key is optional, except the servers:

```yaml
public_url: https://192.168.1.10:8080  # dashboard links in Discord messages
timezone: Europe/Stockholm
servers:
  - name: AKKA
    url: https://192.168.1.10:8384
    gui_url: https://akka.lan:8384       # optional: the target of the "Open in Syncthing" links
    api_key: …                           # import only, never exported, valid for this url only
    # allow_http: true                   # necessary for a plain http:// url (sends the API key unencrypted)
collector:
  mode: events              # events (real time) or polled (less load: a full check every poll_interval)
  event_timeout: 50s        # long-poll timeout. Keep it below the idle timeout of the VPN
  full_check_interval: 4h   # full check in events mode (also on start and reconnect)
  poll_interval: 15m        # polled mode
  structure_interval: 1h    # structure scan of the top level of each share
  baseline_interval: 7d     # baseline scan of each project folder (in events mode, events report the Syncthing folders between scans)
  concurrency: 3            # parallel requests of each activity (full check, structure scan)
  request_timeout: 30s
  down_after_failures: 2
checks:                     # custom values for each check
  H7: { urgent: true, debounce: 1h }
  S7: { enabled: false }
thresholds:                 # a threshold decides when a finding opens
  failing_files: 1h         # H4 when failed items are the only reason
  stuck_sync: 24h           # H5
  paused: 3d                # H6
  pc_offline: 7d            # H7
  conflict_window: 7d       # H10
  inactive_project: 180d    # C1
  old_device: 90d           # C2
  dead_share: 30d           # C3
notifications:
  discord_webhook: …        # import only
  discord_role_id: "123456789012345678"   # 17 to 20 digits
  quiet_hours: "22:00-07:00"   # or "off". The 2 ends must be different
  reminder_time: "08:00"       # or "off". Must be outside the evening part of the quiet hours
  weekly_day: monday           # or "off"
  weekly_time: "08:00"
  weekly_top: 10
access:
  mode: builtin             # builtin, or proxy (built-in login off, see Security)
  viewer: none              # none (administrator only), password, or open (view without login)
  session_ttl: 30d
structure: …                # structure rules, see below
shares:
  add:     { SIRIUS: [/share/CACHEDEV2_DATA/SIRIUS-LIVE-5COLLECT] }  # by server name or ID
  exclude: {}
```

**Durations.** A duration is a number with a unit: `s`, `m`, `h`, `d` or `w` (Go forms such as `1h30m` also work). The maximum is 10 years (`3650d`). The value `0` is the default. Each value has a range. syncwatch refuses a value outside the range on import and in the settings forms, with the key and the range.

| Key | Range | Default |
|---|---|---|
| `collector.event_timeout` | 5s to 10m | 50s |
| `collector.full_check_interval` | 1m to 7d | 4h |
| `collector.poll_interval` | 1m to 24h | 15m |
| `collector.structure_interval` | 1m to 7d | 1h |
| `collector.baseline_interval` | 1h to 90d | 7d |
| `collector.request_timeout` | 1s to 10m | 30s |
| `collector.concurrency` | 1 to 16 | 3 |
| `collector.down_after_failures` | 1 to 20 | 2 |
| `checks.<ID>.debounce` | 0 to 30d | 5m (H2: 15m) |
| `thresholds.*` | 10s to 3650d | see the example |
| `structure.nested_scan_depth` | 1 to 10 | 3 |
| `notifications.weekly_top` | 1 to 50 | 10 |
| `access.session_ttl` | 1h to 365d | 30d |

**Parallel requests.** `collector.concurrency` limits each activity separately: the full check and the structure scan. The 2 can run at the same time, together with the event long-poll and the traffic sample of each minute. Thus up to 2 times `concurrency` plus 2 requests can reach one server at the same time.

**Clock values.** `quiet_hours`, `reminder_time` and `weekly_time` use `HH:MM` (24-hour clock, 1 or 2 digits for the hour, 2 digits for the minutes). The value `off` in any case sets the feature to off. An empty value is the default. The 2 ends of `quiet_hours` must be different. A `reminder_time` in the evening part of the quiet hours is an error, because syncwatch can never send that reminder. The same applies to a time more than 12 hours after the end of the quiet hours.

**Thresholds.** A threshold decides when a finding opens. A higher threshold does not close findings that are already open.

### Shares and structure rules

A **share** is a directory whose subdirectories are project folders. syncwatch finds the shares automatically. A share is the parent directory of each Syncthing folder path, plus `shares.add`, minus `shares.exclude`. With `share_type_from_path`, the share is the nearest parent directory whose name matches.

The general structure checks (S1, S2, S6, S7) need no rules. The checks for names and placement (S3, S4, S5, X2) use the structure rules:

```yaml
structure:
  project_pattern: '^(ME-)?LIVE-[A-Za-z0-9]'     # a top-level directory that is a project folder (S3, S4)
  case_insensitive: false
  share_type_from_path: '-(?P<me>ME-)?LIVE-(?P<type>[1-5][A-Z]+)$'  # named groups become share attributes
  type_tokens: [1SOURCE, 2PROJECTFILES, 3INTERMEDIATE, 4FINALS, 5COLLECT]
  placement:
    - when_name_matches: '^ME-LIVE-'
      share_must_have: { me: true }               # true or false: the named group matched or not
    - when_name_matches: '^LIVE-'
      share_must_have: { me: false }
  ignore_dirs: ['.*', '@*', '#recycle']           # glob patterns, case-insensitive
  nested_scan_depth: 3
```

- syncwatch compares `share_type_from_path` with the directory name of each share. The named groups are the attributes of the share. syncwatch compares the group `type` with the type tokens.
- syncwatch splits a directory name on `-` and `_` and compares each segment with the type tokens, without regard to case. A name with a token that is different from the `type` of the share gets an S5 finding. A name with 2 different tokens also gets S5.
- Without a `project_pattern`, each top-level directory is a project folder, and S3 and S4 are off.
- An attribute in `share_must_have` must be a named group of `share_type_from_path`. A different attribute name is an error. A placement rule without `share_type_from_path` is also an error.
- `case_insensitive: true` applies to `project_pattern`, `when_name_matches` and `share_type_from_path`.
- In the Structure form, a YAML without the `ignore_dirs` key keeps the current list. An explicit `ignore_dirs: []` clears the list. To get the defaults back, paste them: `['.*', '@*', '#recycle', '#snapshot', '$RECYCLE.BIN', 'System Volume Information', 'lost+found']`. In an import, an omitted key gives the defaults.
- Behind a [read-only gate](#read-only-gate), the gate permits listings only under the parents of the Syncthing folders. Shares from `shares.add`, and shares that `share_type_from_path` finds more than 1 level above a folder, need `SYNCWATCH_GATE_BROWSE_ROOTS` on that gate.

[examples/ivar.yaml](examples/ivar.yaml) has the complete structure rules of IVAR Studios, the first installation.

## Checks

The severity is ERROR, WARNING or INFO. An *urgent* check sends an urgent message to Discord for each new finding. You can change the urgent flag, the debounce and the enabled state of each check under Settings → Checks.

| ID | Check | Description | Severity | Urgent |
|---|---|---|---|---|
| H1 | Server unreachable | syncwatch cannot reach the API of the server, or the server refuses the API key or the gate token (HTTP 401 or 403). The message names the cause, and a detail row tells what to do. | ERROR | yes (5 minutes) |
| H2 | Servers not connected | 2 polled servers have no connection to each other. After a start of syncwatch or of a server, H2 waits 2 minutes for the connection. | ERROR | yes (15 minutes) |
| H3 | Server certificate changed | The server sent a different TLS certificate. H3 replaces H1. Accept the certificate in Settings. | ERROR | yes |
| H4 | Folder sync stopped | A folder error, no free space, a request error for the folder, or failed items for more than 1 hour. A file watcher error is a separate WARNING finding after 1 hour. | ERROR | yes |
| H5 | Stuck sync | A folder is not idle, or has remaining items, for 24 hours. Or a device is stuck below 100 percent for 24 hours of connected time. The finding of a device stays open (stale) while the device is off. | WARNING | |
| H6 | Paused for a long time | A folder or a device is paused for more than 3 days, on a server or on a PC. | WARNING | |
| H7 | PC offline | A PC that was online before is offline for more than 7 days. | WARNING | |
| H8 | Version mismatch | The servers run different versions, or a connected PC runs a different version. | WARNING | |
| H9 | Pending device or folder | A device or a folder offer waits for acceptance. | WARNING | |
| H10 | New sync conflicts | New sync conflicts in the last 7 days: conflict copies that the server makes, and in events mode also the conflict copies from other devices. | WARNING | |
| H11 | Connected through a relay | A device is connected through a relay. | WARNING | |
| X1 | Same label, different folder IDs | 2 servers have a folder with the same label but different folder IDs. | WARNING | |
| X2 | Folder in different share types | The same folder ID is in shares of different types. | WARNING | |
| S1 | Syncthing folder inside another | A Syncthing folder is inside a different Syncthing folder, or 2 Syncthing folders have the same path. | ERROR | yes |
| S2 | Share root is a Syncthing folder | The directory of a share is a Syncthing folder. | ERROR | yes |
| S3 | Nested project folder | A project folder is inside a different project folder. | ERROR | yes |
| S4 | Incorrect name | A top-level directory does not match the project pattern. | WARNING | |
| S5 | Incorrect share | The type token or a placement rule does not match the share. | ERROR | |
| S6 | Duplicate name | The same name is in 2 shares on one server. | WARNING | |
| S7 | Not in Syncthing | A project folder is not in a Syncthing folder. | INFO | |
| C1 | Inactive project folder | No file change for 6 months. The age is the newest of the last pulled file and the last local change that syncwatch saw (events mode). | INFO | |
| C2 | Inactive device | A device is offline for 90 days, or never connected, but is still configured. | INFO | |
| C3 | Folder not used by device | A folder is shared with a device that never connected to the server, or that does not share it back, for 30 days. | INFO | |

Structure findings have one row for each directory with all its reasons (for example `S5+S6`). Finding IDs are stable, with one exception. On a case-sensitive server, the IDs of S3 to S7 findings keep the case of the path from this version on. Such findings resolve and open again one time after the update.

**Stale findings.** While syncwatch cannot reach a server, the findings of the server become **stale**. They stay open, show the tag `stale` and send no new urgent messages. Findings are also stale in these cases:

- after a start, until syncwatch contacted the server (H1 and H3 included);
- structure findings, until the first structure scan after a start;
- findings in a share whose listing failed (the share keeps its last directory list);
- H7 and C2, while syncwatch cannot reach one of their servers. Only a server that is up can show that a PC is offline. No new H7 or C2 opens in that time.
- the other cross-server findings (H8, X1, X2, C1), only when syncwatch can reach none of their servers.

When you disable or remove a server, syncwatch closes its findings without Discord messages. The history shows the reason. A disabled server stays a server: its device is not a PC and gets no H7, C2 or C3. The health line and `/api/v1/status` show it as disabled.

H5, H6 and C3 keep the last known state of a device through a disconnect, until the device reports again. H5 counts only the time while the device is connected. A device that never connected gets only C2 after `old_device`: no H7 and no C3.

**How syncwatch collects the data** (events mode):

- One long-poll event stream for each server: state changes, completion, errors, connections and new directories.
- A full check on start, after a reconnect and every `full_check_interval` (default 4 hours): `db/status` and `db/completion` for each folder. Syncthing documents `db/status` as slow, thus syncwatch calls it only then.
- `/metrics` for the conflict counters, at each full check and each reconnect. These count the conflict copies that the server makes. The event stream also reports the conflict copies that other devices send to the server. syncwatch counts each such copy one time. In polled mode, syncwatch sees only the copies that the server makes.
- A structure scan of the top level of each share every `structure_interval` (default 1 hour). The scan goes 3 levels deep into project folders that are not in a Syncthing folder. A baseline scan of each project folder every `baseline_interval` (default 7 days). Between baseline scans, the events report the changes in the Syncthing folders.
- Missed events: syncwatch scans the affected folders again 30 seconds after the last gap. It does a full check immediately for the first gap, then at most one full check every 10 minutes.
- An HTTP error for one folder marks that folder (state `unknown`, with the error), not the server. Only an unreachable server or a refused API key (401, 403) marks the server.

In polled mode there is no event stream. syncwatch reads all data every `poll_interval` (default 15 minutes). Each structure scan in polled mode also scans the project folders in Syncthing folders (the same work as a baseline scan). A structure scan that still runs when the next scan is due continues, and the next scan starts at a later poll.

## Notifications

syncwatch sends all notifications to Discord: a webhook, embeds colored by severity, a dashboard link on each message. The webhook must be a Discord webhook URL (`https://discord.com/api/webhooks/…`). syncwatch checks the URL when you save it and again at each send, and it does not follow redirects. syncwatch removes spaces and invisible characters around a pasted URL. The role ID must have 17 to 20 digits (Discord → Server Settings → Roles → Copy Role ID).

- **Urgent message**: when an urgent finding stayed open for its debounce time. The debounce counts from the time the finding became urgent. Findings that open in the same cycle go into one message. The title of that message names the servers and the checks, for example `🚨 2 new urgent findings on AKKA, SOL: S3`. Findings with the same check and subject on 2 servers have one line. The message mentions the configured role. A finding that resolves and opens again in less than 5 minutes is the same incident: no new urgent message.
- **Resolved message**: for each finding that had an urgent message, after it stayed resolved for 5 minutes (or the debounce of the check, if shorter). No mention. Exception: findings of a removed or disabled server.
- **Daily reminder** (08:00): urgent findings that are still open. It skips findings with an urgent message in the last hour. No mention.
- **Quiet hours** (22:00–07:00): syncwatch sends no notifications. At 07:00 one overnight summary lists 3 groups. The groups are: new and still open findings, resolved findings, and findings that opened and resolved during the night. The summary includes each urgent finding that became due during the quiet hours. A summary that syncwatch sends late has a section for the findings that became due after the end of the quiet hours.
- **Weekly report** (Monday 08:00): a health line (`3 servers up · 14 PCs, 2 offline more than 7 days · ERROR 2 · WARNING 5 · INFO 3 · 3 snoozed`), new and resolved findings of the week, the most important open findings and a dashboard link. After a start, syncwatch waits for the first results of the servers (at most 5 minutes) before it sends a missed weekly report or reminder. The health line marks a server without a result as not contacted. It marks a server with a changed certificate as certificate changed, and a disabled server as disabled. The health line shows a PC as offline only when each server that knows the PC is up. Else the PC is unknown. Ignored findings have their own count after the snoozed findings. The list of new findings marks snoozed and ignored findings. The count of new findings is the count of the lines, and a long list ends with "…and N more".
- **Settings notice**: one settings notice for each save that changed a setting, with the old and new values (at most 40 lines). Other causes: a new or replaced API key, an accepted or removed pinned certificate, a changed administrator password, an import. This includes `syncwatch import-config` and `syncwatch reset-admin-password` on the command line. Also an API token that is added, replaced or revoked, and a snooze or ignore of an urgent finding. Each settings notice includes the client address. syncwatch sends it immediately, also during quiet hours, to the webhook that was configured before the change. Thus a replaced or removed webhook also gets the settings notice. If the send fails, syncwatch tries again with the next notifications for up to 1 day and adds the time of the change. If Discord refuses a settings notice (HTTP 400, 401, 403, 404), syncwatch logs one warning and discards it. A kept settings notice never delays an urgent message. No mention.
- **Snoozed and ignored findings** never send urgent messages and are not in the overnight summary. You can snooze a finding for 1 day, 1 week or until the end of a day that you select, with an optional reason. You can snooze or ignore only an open finding, or a finding that resolved in the last 90 days. An ignore ends 90 days after the finding resolved. The Findings page lists the snoozed and ignored findings at the end, with a button to remove the snooze or the ignore.
- **No webhook**: without a webhook, syncwatch marks no notification as sent. When you add the webhook, the due urgent findings go into one message.
- **A webhook that fails**: if Discord has an outage, syncwatch tries again after 1, 2 and 4 minutes, then every 5 minutes. An outage is HTTP 5xx or 429, a timeout or a network error. If Discord refuses the webhook (HTTP 400, 401, 403 or 404), syncwatch tries again after 1, 2, 4 … minutes, up to every 30 minutes. An example is a webhook that a person deleted in Discord. Each page shows a banner until a message arrives. `/api/v1/status` reports it under `notifications`. A test message or a new webhook starts a new try immediately, also while an old send is in progress. If syncwatch cannot decrypt the webhook (a changed secret key), the banner and the test button report it. Save the webhook again to correct it.
- **Findings during a failure**: the first message after a failure also lists the urgent findings of the failure time. These are the findings that became due for an urgent message and resolved while the notifications failed. syncwatch lists them without a mention. Its section is "Opened and resolved while notifications failed".

Names from Syncthing cannot format, link, mention or reorder text in Discord messages: syncwatch escapes titles, summaries and lines.

## Security

- **Read-only by construction.** The Syncthing client has one typed method for each endpoint of the allowlist and no general request method. A unit test fails if the client can make a different request. syncwatch never calls `/rest/config` (it has the GUI password hash and the API key). It reads only `/rest/config/folders` and `/rest/config/devices`, which do not have them. The `ConfigSaved` event of Syncthing has the same data, thus the client removes the payload of that event before a different part reads it.
- **API keys have full access.** Syncthing has no read-only API keys. Without a gate, the read-only guarantee comes out of the code of syncwatch, not out of Syncthing. Thus keep the host of syncwatch and its `/data` confidential. With the [read-only gate](#read-only-gate) on each server, syncwatch keeps only gate tokens, which cannot change Syncthing.
- **Keys stay with their server.** syncwatch stores each API key together with the URL that you typed it for, and the collector sends it to no other address. When you change the URL of a server, in the settings or by an import, syncwatch needs the API key again. When you remove a server, syncwatch deletes its API key. To accept a changed certificate or to remove a pinned certificate, you need the administrator password or the API key of the server. An administrator session alone is not sufficient. syncwatch sends a settings notice for these changes and logs the client address. It also logs each refused password or API key of these actions, with the client address.
- **Secrets.** syncwatch encrypts the API keys, the webhook URL and the API token with AES-256-GCM. syncwatch makes the secret key on first start in `/data/secret.key`. Or you set `SYNCWATCH_SECRET_KEY`: a base64-encoded random key of 32 bytes (`openssl rand -base64 32`), not a passphrase. Secrets are write-only in the dashboard and never in the log, the exports, the JSON API or Discord. With the secret key in `/data`, the encryption protects a copy of the database alone, for example a backup without `secret.key`. It does not protect a copy of the complete `/data`. To protect that also, set `SYNCWATCH_SECRET_KEY`, for example from an `env_file` that only root can read. Then store `secret.key` outside `/data`.
- **TLS.** syncwatch checks the certificate during the handshake, thus it sends no API key before the check. syncwatch pins the certificate of each server on first use, self-signed or CA-signed. A new certificate that a trusted CA issued for the host name is accepted: syncwatch replaces the pinned certificate and logs one line. A different certificate blocks the connection and opens H3 with the old and the new fingerprint, until an administrator accepts it. Syncthing replaces its own GUI certificate approximately every 820 days, thus H3 happens sometimes. syncwatch tries the connection again with a longer interval each time, up to one try every 5 minutes. It repeats the warning in the log each hour. The pinned certificate also finds a server name that resolves to a different computer. Use IP addresses or fully qualified names in server URLs. For a name that does not resolve, the container asks the DNS 2 times for each try. The second request adds the domain of the container to the name. A name with a final dot (`akka.lan.`) prevents the second request. A plain `http://` URL sends the API key unencrypted, thus it needs `allow_http` (a checkbox in the settings).
- **Login.** An administrator password (argon2id) and an optional viewer password or open view. The session cookies are HttpOnly with `SameSite=Lax`. Each form has a CSRF token, and the pages have a strict Content-Security-Policy (no inline scripts). You need the administrator password to set login to off, to open the view or to set a new viewer password. This also applies to an import. An administrator session alone is not sufficient.
- **Login limit.** syncwatch keeps the count of failed logins for each client address. After 5 failures at an address in 10 minutes, each login from that address must wait 10 minutes, for the administrator and for viewers. The limit also applies to parallel tries. Failures from many addresses only make logins slower, thus no person can block the administrator from a different address. Password checks run at most 2 at the same time, thus a burst cannot use all memory.
- **Shared addresses.** The limit is for each address as syncwatch sees it. Behind a reverse proxy, Docker Desktop, a userland proxy or a VPN with NAT, all users have the same address (see [below](#behind-a-reverse-proxy)). The password confirmations of a logged-in administrator have a separate count for each session: 5 failures in 10 minutes. These are the confirmations for a certificate, the access settings, the passwords and the API keys. Failed logins from the same address do not block them.
- **Settings forms.** Each form has the revision of the configuration. If a different administrator saved after you opened the page, syncwatch refuses your save and asks you to reload the page. A section of the dashboard stops the automatic refresh while you type in a form in it.
- **Network.** One listen port (loopback only for the binary if you give no address), optional HTTPS, no UPnP. syncwatch connects only to the configured API URLs and to Discord.
- **Proxy mode** (`access.mode: proxy`) sets built-in login to off: each person who can reach syncwatch is an administrator. Use it only behind a reverse proxy or a VPN that authenticates users. Configure the proxy as described in [Behind a reverse proxy](#behind-a-reverse-proxy).
- **Lost administrator password.** Run `docker exec -i syncwatch syncwatch reset-admin-password`. The command reads the new password from stdin. It needs the data directory and the secret key of the installation. syncwatch logs out all sessions and sends a settings notice. If syncwatch does not run, it sends the settings notice at the next start.

### Read-only gate

A Syncthing API key can change all data. Thus a person who controls syncwatch, or a copy of its `/data`, controls each monitored Syncthing. The gate removes that: `syncwatch gate` runs next to the Syncthing of each server (one small container for each server, the same image). The gate keeps the Syncthing API key, and syncwatch gets a gate token instead.

- Each request to the Syncthing API needs the gate token, also the health request of Syncthing (`/rest/noauth/health`). Without the token, the gate answers `403`. Only the health answer of the gate itself (`/healthz`, for the container healthcheck) needs no gate token. The gate accepts only `GET` requests to exactly the endpoints that the client of syncwatch uses. The gate refuses all other requests before they reach Syncthing, and the Syncthing API key is not usable at the gate.
- The gate permits only the query parameters that syncwatch sends, with value rules. `browse` needs a `current` that ends with a separator. `events` needs the event mask of the collector. Numbers must be in range. A different query gets `400` and the text `syncwatch gate: query not permitted: …` with the reason.
- The gate permits a `browse` only under the parents of the Syncthing folders, plus the directories in `SYNCWATCH_GATE_BROWSE_ROOTS` (`--browse-roots`, comma-separated). A different directory gets `403`. The gate reads the folder list from Syncthing on the first browse and again after 10 minutes. When a request is outside the list, the gate reads it again, at most one time each minute. Thus a new folder works immediately.
- The gate removes secrets from the answers. These are the payload of `ConfigSaved` events (the GUI API key and the password hash) and the passwords for untrusted devices in the folder configuration. The gate does not forward an answer that it cannot check. If Syncthing answers such a request with an error (not 2xx), the gate sends only the status code and a short text, for example `syncwatch gate: Syncthing answered HTTP 500`.
- The gate always serves HTTPS (self-signed if you give it no certificate), with the same reload and HTTP/2 as the dashboard. syncwatch pins the gate certificate the same as a server certificate. The gate accepts Syncthing on `127.0.0.1`. At a different address, the gate pins the Syncthing certificate on first use, self-signed or CA-signed. It accepts a new CA-signed certificate for the host name.
- The API key comes out of the Syncthing `config.xml` (mounted read-only, `SYNCWATCH_GATE_SYNCTHING_CONFIG`), a key file, or `SYNCWATCH_GATE_SYNCTHING_KEY`. The gate makes the gate token on first start, logs it one time and keeps it in `/data/gate-token` (or set `SYNCWATCH_GATE_TOKEN`).
- The gate reads the Syncthing API key only at the start. After you change the API key in Syncthing, restart the gate. Until then, Syncthing refuses the requests of the gate. The gate then answers `502`, logs a warning at most one time each minute, and syncwatch shows H1 for the server.
- The gate listens on `SYNCWATCH_GATE_LISTEN` (`--listen`). The default is `127.0.0.1:8385`, and the image sets `:8385`. The gate ignores `SYNCWATCH_LISTEN`. The healthcheck of the image uses `SYNCWATCH_LISTEN`, thus set `SYNCWATCH_HEALTHCHECK_URL` for a gate container (the example does that).
- The pinned Syncthing fingerprint is in `/data/syncthing.pin`. The gate user needs read access to the file, or the gate does not start. When the Syncthing certificate changes behind a gate, syncwatch shows H1 for the server. The H1 message gives the instruction: delete `syncthing.pin` in the data directory of the gate and restart the gate.
- Each client address can have at most 4 event long-polls (`/rest/events`) and 64 other requests in progress at the same time. The gate refuses more requests with `429`. syncwatch sends at most 2 times `concurrency` plus 2 requests at the same time to one server.
- The gate logs refused requests one time each minute for each client address and reason, with a count.

Installation: [examples/gate-compose.yml](examples/gate-compose.yml) on each server. Then type the gate URL (`https://<nas>:8385`) and the gate token in syncwatch instead of the API URL and the API key. Also set **GUI URL for links** (`gui_url`) to the Syncthing GUI (`https://<nas>:8384`). Without it, the "Open in Syncthing" links of the dashboard open the gate, and the gate answers `403`. The Syncthing GUI stays as it is for the administrators.

### Behind a reverse proxy

syncwatch is for direct access on the LAN, and it does not read `X-Forwarded-For`. Behind a reverse proxy it sees each user as the address of the proxy. Docker Desktop, userland proxies and a VPN with NAT also give all users one address. Thus:

- all users share one login limit: 5 incorrect passwords from one person block the logins of all persons, the administrator included, for 10 minutes;
- the log and the settings notices show the address of the proxy instead of the address of the user.

Use proxy mode with the authentication of the proxy there, or accept the shared limit.

The proxy must forward the original `Host` header of the browser, with the port. syncwatch refuses a form when the `Origin` of the browser does not match the host and the port that received it. With an incorrect `Host`, the login and each save fail with "cross-origin request refused". The forwarded host name must also be a known name: the host of `public_url` or a name in `SYNCWATCH_TLS_HOSTS` (see the [Host check](#https)). The proxy must also send `X-Forwarded-Proto`, then the cookies get the `Secure` flag over HTTPS. Caddy and Traefik do both by default. nginx needs:

```nginx
location / {
    proxy_pass http://127.0.0.1:8080;
    proxy_set_header Host $http_host;          # the Host header of the browser, with the port ($host removes the port)
    proxy_set_header X-Forwarded-Proto $scheme;
}
```

The automatic refresh uses server-sent events. syncwatch sends `X-Accel-Buffering: no`, thus nginx does not buffer them. If the connection stops, the page shows a banner with the time of the last data and connects again.

### Security reports

syncwatch keeps the keys to your Syncthing servers. Report a vulnerability in private: **Security → Report a vulnerability** on this repository, not a public issue.

## JSON status API

Send `GET /api/v1/status` with the header `Authorization: Bearer <token>` (the word `Bearer` in any case). Make the API token under Settings → API. A logged-in browser session also works. The answer has the health line, the counts, the notification state, the servers, the devices and the open findings. The API is read-only and never includes secrets.

Scripts must use the stable values of this table, not the texts. The health line and `findings[].message` are texts for persons, and a new version can change them. The changes after the version 78ef2d8 are in [docs/CHANGES-review.md](docs/CHANGES-review.md#integrations-changes-to-apiv1-after-78ef2d8).

| Field | Values |
|---|---|
| `findings[].severity` | `ERROR`, `WARN` or `INFO` (upper case, unchanged after 78ef2d8). The dashboard shows `WARN` as WARNING. |
| `findings[].check`, `findings[].id` | the check ID (for example `H4`) and the stable ID of the finding |
| `findings[].details[].key` | the name of a detail row, for example `Watch error` or `Items left`. The dashboard shows some keys with a different label. |
| `findings[].snoozed`, `findings[].ignored` | `snoozed` is `true` for a snoozed or an ignored finding. `ignored` is `true` only for an ignored finding. |
| `counts` | the keys `error`, `warn`, `info` and `urgent` for the findings that are not snoozed or ignored. `snoozed` counts the snoozed and the ignored findings, `ignored` only the ignored findings. |
| `health_counts` | `servers`, `servers_up`, `servers_disabled`, `pcs`, `pcs_offline`, `pcs_unknown` |
| `servers[].status` | `up`, `down`, `cert-changed`, `unknown` (not contacted after the start) or `disabled` |
| `servers[].folders` | the counts `total`, `idle`, `syncing`, `scanning`, `error`, `watch_error` and `paused`. `stale` is `true` when the server is not up: the counts are then the last known state. |
| `servers[].uptime_s` | seconds, or `null` when syncwatch does not know the uptime |
| `devices[].server` | `true` for a server, also for a disabled server. `false` for a PC. |
| `notifications` | `failing` and `since`. `error` is present only for the administrator role. |

syncwatch logs a request with an unknown API token, at most one line each minute for each client address. The time fields are UTC (RFC 3339). An error answer is JSON (`{"error": "…"}`) with `Content-Type: application/json`.

```sh
curl -k -H "Authorization: Bearer $TOKEN" https://192.168.1.10:8080/api/v1/status   # -k: the self-signed certificate
```

A request by a container name needs that name in `SYNCWATCH_TLS_HOSTS` (see the [Host check](#https)). Example: `http://syncwatch:8080` from Home Assistant on the same Docker network.

## Home Assistant

`GET /api/v1/ha` (the same API token) gives flat values for each server, for the RESTful sensors of Home Assistant. The key is the server ID:

| Field | Value |
|---|---|
| `running` | the server answers (`true` or `false`) |
| `status` | the most severe state of its folders: `error`, `watch error`, `syncing`, `scanning`, `up to date`. `paused` when each folder is paused. `unknown` while syncwatch cannot reach the server. `disabled` for a server that is set to off in syncwatch (each configured server is in the answer). |
| `folders`, `folders_syncing`, `folders_scanning`, `folders_error`, `folders_watch_error`, `folders_paused` | folder counts |
| `moved_24h_gb`, `received_24h_gb`, `sent_24h_gb` | data moved in the last 24 hours |
| `moved_7d_gb`, `received_7d_gb`, `sent_7d_gb` | data moved in the last 7 days |
| `download_mb_s`, `upload_mb_s` | the current transfer rate (the last minute) |

The traffic comes out of the counters of Syncthing (`/rest/system/connections`). syncwatch reads them one time each minute for each server and keeps them for 8 days. Thus the sums start when syncwatch started to monitor the server. A period without readings (syncwatch down, or a server unreachable for more than 3 minutes) is not included. The status follows the Syncthing events in real time.

[examples/home-assistant.yaml](examples/home-assistant.yaml) is a complete `rest:` block for `configuration.yaml`. It makes one request each minute. For each server it has a binary sensor for `running` and 5 sensors. The sensors are the status, the data moved in 24 hours and in 7 days, and the download and upload speed. Write the server IDs as `servers['akka']`: an ID can have a hyphen, and `servers.nas-1` is a subtraction in a template. Home Assistant only gets the read-only API token of syncwatch, never a Syncthing API key. Thus the example also works with the [read-only gate](#read-only-gate).

- The example has the servers AKKA and SOL. The sensors of SIRIUS are a comment. Use the sensors only for your own servers.
- Each entity has an availability template. When the answer has no data for the server, the state is `unavailable`, not `off`. An example is the answer HTTP 401 after a change of the API token.
- `running` is also `false` in the first seconds after a start of syncwatch, until syncwatch contacted the server. Give an automation on `running` a delay, for example `for: "00:05:00"`.
- A second request reads the count of urgent findings from `/api/v1/status` (`counts.urgent`). Use this sensor for automations. The status of a server can show `syncing` or `error` for a short time without a finding.

Caution: with `verify_ssl: false`, Home Assistant does not check the certificate of syncwatch. It sends the API token to each server that answers at the address of the resource. If possible, give syncwatch your own certificate (`SYNCWATCH_TLS_CERT`) and use `verify_ssl: true`.

To replace the API token:

1. In syncwatch, open Settings → API and select **Replace API token**. syncwatch refuses the old API token immediately.
2. Type the new API token in `secrets.yaml` of Home Assistant (`syncwatch_auth: "Bearer swt_…"`).
3. In Home Assistant, open Developer tools → YAML. Select the reload of the RESTful entities. Home Assistant needs no restart.

## Server probe

Before you depend on syncwatch, examine how the API of your servers behaves. The probe is also read-only. It takes the API key from the environment and does not store it:

```sh
mkdir -p docs && sudo chown 65532 docs          # the image runs as user 65532
docker run --rm -e SYNCWATCH_PROBE_API_KEY=… -v "$PWD/docs:/docs" ghcr.io/ivarstudios/syncwatch:1.0.0 \
  probe --name SIRIUS --url https://192.168.3.10:8384 --out /docs/api-findings.md --event-wait 5m \
  --project-pattern '^(ME-)?LIVE-[A-Za-z0-9]'
```

If the probe cannot write the `--out` file, it writes the report to the standard output and stops with an error. If the connection fails, the probe writes what it has and exits with code 1.

The probe checks these items: the browse answers (hidden directories, spaces, å/ä/ö, brackets), the browse time, a long-poll over your network path, the event mask. Also `remoteState` in `FolderCompletion`, the duration of a full check, `/metrics`, `lastFile.at`, TLS and the counts. Monitor the CPU of the NAS during the full check. If the load is large, use the polled mode.

With `--project-pattern` (the `project_pattern` of your rules), the probe also scans the shares the same as the structure scan. It estimates the directory listings of a structure scan and a baseline scan. `--scan-budget` (default 20000) limits the listings before the probe estimates the rest. `--record DIR` also saves the responses (without secrets) as test fixtures. `--pin` gives the known fingerprint of the certificate, also in upper case or with colons. The results of the development runs are in [docs/api-findings.md](docs/api-findings.md).

## Development

```sh
go test ./...                                   # unit tests: client allowlist, rules, notifier (fake clock), store
SYNCTHING_BIN=/path/to/syncthing go test -tags integration -v ./test/integration
go test -tags compose -v ./test/compose         # the Docker image and Syncthing containers (needs Docker)
go test -tags loadsim -v ./test/loadsim         # structure scan load on a large simulated share (needs Docker)
go run ./test/simenv -syncthing /path/to/syncthing -demo   # a simulated installation for the dashboard
```

- In a slow environment (an emulated Docker VM, or `-race` there), `SYNCWATCH_TEST_SLOW=20` multiplies the timeouts of the collector tests by 20.
- With `SYNCTHING_BIN` set, `go test ./...` also runs 2 harness tests against a real Syncthing.
- The integration test uses the loopback ports from `SW_IT_BASEPORT` (default 18600) and keeps its work directory in `SW_IT_DIR`.
- The Compose and load suites start their Syncthing containers with discovery, relays, NAT traversal and reports off from the first start. They also build the image again (`--build`). `KEEP_COMPOSE=1` and `KEEP_LOADSIM=1` keep a stack after the test.
- The fake Discord of the tests (`test/harness`, `test/fakediscord`) answers 204. To test failures, send `POST /mode` with `{"mode": "fail404:3"}`, or set `MODE` at the start. The modes are `ok`, `fail500`, `fail400` (a field error), `fail404` (Unknown Webhook) and `ratelimit` (HTTP 429 with `retry_after`), the same as in the simulation kit. `:N` limits a mode to the next N messages.
- `SYNCWATCH_ALLOW_ANY_WEBHOOK=true` permits a webhook on a different host for test installations. When you remove the variable, a saved webhook on a different host stops at the next send.
- Windows: the repository keeps LF line endings (`.gitattributes`). After a pull of that file, run `git add --renormalize .` and then, on a clean tree, `git rm --cached -r . && git reset --hard`, or clone again.
- CI runs `gofmt`, `go vet`, `go mod tidy -diff`, govulncheck and the unit tests with the race detector. This happens on each push and each pull request, then the integration and Compose suites run. The release workflow runs the unit tests before it makes the image and the binaries. CI also checks the Syncthing download against the checksum list of the release.

The integration test starts real Syncthing processes (3 servers, 2 PCs). It configures the scenarios through the APIs of the test servers, thus syncwatch stays read-only. It checks the findings, the dashboard, the JSON API and the Discord payloads. The scenarios are: nested project folders (found by events and by the scan), a Syncthing folder inside another, a share root as a Syncthing folder. Also a paused folder and a folder that stops the sync (a missing folder marker). Also a PC that goes offline, a server that stops and a replaced certificate.

Layout: `cmd/syncwatch` (CLI) · `internal/stclient` (the client with the allowlist) · `internal/collector` (events, full checks, structure scan) · `internal/rules` (pure checks) · `internal/engine` (finding lifecycle). Also `internal/notify` (Discord, schedule) · `internal/store` (SQLite) · `internal/web` (dashboard, settings, API) · `internal/gate` (read-only gate). The icon (the IVAR hexagon with an eye) and how to build its PNGs again are in [docs/icon](docs/icon/icon-design.md).

`syncwatch --version` prints the version. `syncwatch -h` prints the usage. What remains before v1.0, the known gaps and the decisions behind the design: [plan.md](plan.md). The changes of the review of October 2026: [docs/CHANGES-review.md](docs/CHANGES-review.md).

## Known limitations

- syncwatch sees the PCs only through the servers: no PC folder errors, no PC disk space, no offers that wait on a PC.
- `browse` lists directories only. File-level information comes out of the events and the conflict counter.
- syncwatch does not find nested project folders deeper than `nested_scan_depth` levels below the top-level project folder, by scan or by event.
- After missed events, the state of a server can be up to 10 minutes old.
- No disk-space or hardware monitor, no graphs, no metrics history.
- No watchdog for syncwatch itself. If its host stops, syncwatch sends no messages, and if that host is a monitored server, syncwatch never sends its H1 urgent message. Run syncwatch on a different host or add an outside check on `/healthz` ([Where to run syncwatch](#where-to-run-syncwatch)). If you do neither, the missed Monday report is the signal.
- Syncthing 2.x only.

## License

MIT. See [LICENSE](LICENSE). The dashboard includes [htmx](https://htmx.org) (0BSD) and its [SSE extension](https://github.com/bigskysoftware/htmx-extensions).

IVAR Studios made syncwatch to monitor its own Syncthing servers.
