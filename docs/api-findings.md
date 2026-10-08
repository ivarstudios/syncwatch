# Syncthing API findings

Written by `syncwatch probe` plus notes from development against real Syncthing **2.1.5** instances (three "servers" and two "PCs" running on one Windows host). To check your own servers, run `syncwatch probe` against each of them (see the README); it appends a section like the ones under [Probe runs](#probe-runs).

## Summary

| # | Question | Answer (2.1.5) | Consequence in syncwatch |
|---|---|---|---|
| 1 | Does `browse` list every top-level directory, including hidden ones, spaces, å/ä/ö and `[ ]`? | Yes. With a trailing separator it lists all subdirectories, hidden ones included. **`current` is not a glob in 2.x**: the parent is listed and the last segment is matched by prefix, so names with `[ ]`, `*`, `?` need **no escaping**; escaping them actually breaks the match. | The client sends paths as-is (URL-encoded) with a trailing separator, and filters exact names when checking that a directory still exists. |
| 2 | Browse timing | ~1 ms per listing locally. Re-measure on the largest NAS share. | Throttle stays at 3 concurrent requests per server. |
| 3a | Long-polls and idle timeouts | A 50 s long-poll returns normally. Across a VPN, check with the probe's `--event-wait`. | `event_timeout: 50s` default, configurable. |
| 3b | Event mask names | All 16 names are accepted. A custom mask creates its own buffered subscription on first use; **event IDs are per subscription** and restart at 1 when Syncthing restarts (`globalID` is the global counter). | The collector primes the subscription before the full re-check, detects IDs going backwards or gaps, and re-checks when `startTime` changes. |
| 3c | New directories in `LocalChangeDetected` | Reported with the folder-relative path (server's own separator) and **`action: "modified"`**, not `"added"`. Deleted directories come as `"deleted"`. | Any `type: dir` event other than `deleted` is checked against the project pattern; `deleted` removes known nested folders. |
| 3d | `ConfigSaved` payload | **Contains the whole config, including `gui.apiKey` and the password hash.** | The client drops the payload of `ConfigSaved` before returning events (unit-tested); the event only triggers a re-check. |
| 4 | `FolderCompletion` carries `remoteState`? | Yes (2.x). | Events update completion live; the full re-check also calls `db/completion` per (folder, device). |
| 4b | `db/completion` on a paused folder | **HTTP 404 "folder is paused"**. | Paused folders are skipped; per-folder 4xx answers no longer fail the whole re-check. |
| 5 | Full re-check cost | 17 requests in a few ms for 3 folders × 5 devices locally. Measure on your servers (CPU/RAM) before choosing events vs. polled mode. | Polled mode exists for servers where this is noticeable. |
| 6 | `/metrics` with the API key | Reachable; `syncthing_model_folder_conflicts_total{folder=…}` is present for every folder. | H10 uses deltas, with the Syncthing `startTime` to detect counter resets. |
| 7 | `stats/folder` `lastFile.at` | Present, but **only set for files pulled from other devices** (zero after local changes only, and `0001-01-01` until anything is pulled). | C1 takes the newest `lastFile.at` across all servers holding the folder and skips folders where it was never set. |
| 8 | TLS | Self-signed GUI certificate (CN `syncthing`), valid ~820 days; TOFU pinning by SHA-256 works, and a regenerated certificate is detected during the handshake. | H3 + accept button. |
| 9 | Counts | Simulation: 3 servers, 2 PCs, 6 project folders. | — |

Other observations:

- Two servers that dial each other at the same moment drop one connection and reconnect a few seconds later, so a short H2 blip at startup is normal. H2's 15-minute debounce absorbs it.
- On Windows 11 24H2 Syncthing re-launches itself without a console window, so the process you start isn't the one that keeps running.
- Pinning also protects against **name resolution changing under you**. In a Docker Compose test, a stopped container's short name (`sol`) fell through to the local network's DNS and resolved to a production NAS with the same name. syncwatch saw a different certificate, raised H3 and stopped at the TLS handshake, so no API key or request reached that server. Test stacks now use `*.e2e.invalid` names; in production, prefer IP addresses or fully qualified names in server URLs.
- In the Syncthing container, `docker stop` often needs the full 10 s before Syncthing is killed; the GUI stops answering immediately, so H1 detection is unaffected.

## Structure scan load, simulated — 2026-10-04

`test/loadsim` (Docker): one Syncthing 2.1.5 container with a share of 300 project folders, each with 6 subfolders on each of four levels (142,500 directories); 270 of the project folders are Syncthing folders. syncwatch ran from its image with the default `nested_scan_depth: 3` and `concurrency: 3`. The Docker engine runs in a software-emulated VM (about 20× slower than native), so the times are an upper bound.

| Scan | Listings | Time | |
|---|---|---|---|
| Baseline (every project folder, every `baseline_interval`, default 7d) | 12,901 | 18–30 s | 1 + 300 × (1 + 6 + 36) |
| Regular (project folders not in Syncthing, every `structure_interval`, default 1h) | 1,291 | 2–4 s | 1 + 30 × 43 |

- The dashboard API answered within 40 ms throughout.
- `syncwatch probe --project-pattern '^LIVE-'` against the same container estimated exactly 1,291 and 12,901 listings, so a probe run on each server measures the scan's load without deploying syncwatch.
- Listings grow with the subfolders per level squared (depth 3 lists a project folder, its children and its grandchildren). A project folder with 30 subfolders per level costs 931 listings instead of 43. Measure on the NASes before relying on these numbers.

## Folders added paused through the API, simulated — 2026-10-06

For a possible companion tool (separate from syncwatch, which stays read-only) that registers new project folders as paused, unshared Syncthing folders. Two Syncthing 2.1.5 instances (Windows build) started by `test/harness` on loopback, SIRIUS and SOL, connected to each other. SIRIUS's folder defaults were changed to `rescanIntervalS: 777` with devices SIRIUS and SOL, and its default ignores to `.sync`, `@eaDir`. Each test project folder held Resilio-style content: `.sync/ID`, `.sync/Archive/old.bin`, a directory `.sync-other`, `a.txt`, `sub/b.txt`. These were one-off runs, not a test in the repo.

| # | Test | Result |
|---|---|---|
| 1 | `POST /rest/config/folders` with only `id`, `label`, `path`, `paused: true` | Missing fields come from the **configured** defaults (`rescanIntervalS` 777), **devices included**: SOL listed the folder in `cluster/pending/folders` within seconds, although it was paused. The directory was unchanged. |
| 2 | `POST` of the full defaults object with `paused: true` and devices = SIRIUS only | Nothing on disk changed: no `.stfolder`, no `.stignore`. `index-v2` held only `main.db`. `db/status` answered 200 with zeros (unlike `db/completion`, which answers 404 for paused folders); `GET db/ignores` answered with empty lists. |
| 3 | `POST /rest/db/ignores` on that paused folder | Worked: `.stignore` was written and nothing else; the folder stayed paused. |
| 4 | `browse` with `current=<folder>/.sync` | Returned `.sync` and `.sync-other`: the last segment matches by prefix, and hidden directories are listed. |
| 5 | `POST` again with the same ID and another label | Replaced the folder's configuration without an error. |
| 6 | Resume the way the GUI does it | The GUI's `setFolderPause` sets `paused` in its copy of the config and saves the whole config (`PUT /rest/config`). After resuming: `.stfolder` created, settings unchanged, `.stignore` loaded; `.sync/ID` not in the index, 2 files indexed. |
| 7 | Resume a second folder that had no `.stignore` | `.stignore` was not created from the default ignores. `.sync/ID` and `.sync/Archive/old.bin` were indexed (4 files). |
| 8 | Syncthing issues syncthing/syncthing#10389 and syncthing/syncthing#10746: `PATCH {"paused": true}`, then `false` | Not reproduced: `rescanIntervalS` 86400, `fsWatcherEnabled` false and `versioning.cleanupIntervalS` 7200 all stayed, and also through a GUI-style pause and resume. |
| 9 | Settings names | The defaults' key is `blockIndexing` (default `true`). `syncthing cli config folders add` has `--paused`, `--type` and `--block-indexing`. |
| 10 | The same folder ID registered paused on both instances (SOL's copy with its own `.stignore`), each resumed alone, then shared | When SIRIUS shared it, SOL listed the offer in `cluster/pending/folders` although it had a folder with that ID; adding SIRIUS to SOL's folder paired them. A 32 MB file identical on both (SOL's 48 hours older) was not transferred: 45 KB crossed the connection in total, there was no conflict, and SOL's modification time became SIRIUS's. A file only on SOL was copied to SIRIUS. `c.txt`, different on each, became a conflict copy on both, and the newer content won. `.sync`, ignored on SIRIUS, wasn't sent. |

Still to check on a NAS (Linux, Docker): the owner and permissions of a `.stignore` written by the API, and the inotify watch limit.

## Probe runs

## AKKA (simulated, Windows host) — 2026-10-01 22:32 CEST

URL: `https://127.0.0.1:18400`

### TLS (8)

- Certificate: self-signed, trusted on first use, SHA-256 `B6:CF:ED:46:0C:D4:10:AA`, expires 2028-12-29
- Pinned on first use: `b6cfed460cd410aa5e5dad1f07d2eccce9f629e0df867376bede3fc64635fd06`
- First request took 12ms

### Server

- Version: v2.1.5 (windows/amd64, container: false)
- Device ID: `JRDDY2W-ZNFBC36-U4ZEJ3S-BKUZCT3-XWUSTDZ-KFHIY6T-EW7SETU-FYHIEAM`, path separator `\`, started 2026-10-01T22:25:47+02:00

### Counts (9) and full re-check (5)

- 3 folders, 5 devices, 7 (folder, device) pairs, 4 connected now
- Full re-check: 17 requests in 5ms (3 at a time)
- Slowest `db/status`: 1ms
- Folder states: map[idle:3]
- remoteState from db/completion: map[valid:7]
- Decision gate: watch the NAS CPU and RAM while this runs; if it is noticeable, use the polled mode.

### Folder stats (7)

- `lastFile.at` set for 0 of 3 folders (zero means no change recorded yet; C1 skips those)

### Conflict counters (6)

- `/metrics` reachable with the API key; `syncthing_model_folder_conflicts_total` present for 3 folders (total 0 since start)

### Browse (1, 2)

- `D:\tmp\swsim\st\akka\share\AKKA-LIVE-1SOURCE`: 6 directories in 1ms; hidden/system: []; with spaces, non-ASCII or brackets: [random stuff]
  - browsing into `random stuff` works without escaping
- `D:\tmp\swsim\st\akka\share\AKKA-LIVE-2PROJECTFILES`: 2 directories in 1ms; hidden/system: []; with spaces, non-ASCII or brackets: []
- Slowest listing: 1ms. Compare the counts with `ls` on the NAS (hidden directories are included).

### Events (3, 4)

- Event mask accepted (16 names); subscription at event ID 100
- A 50s long-poll returned normally after 50s (survives the network path)
- Events seen in 40s: map[StateChanged:12]
- No FolderCompletion event seen; the full re-check calls `db/completion` per pair either way.
- Event IDs are per subscription (one per event mask) and restart at 1 when Syncthing restarts; the collector re-checks when they go backwards or `startTime` changes.


