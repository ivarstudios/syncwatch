# Syncthing API findings

`syncwatch probe` wrote the probe reports in this file. The notes come out of the development against real Syncthing **2.1.5** servers (3 "servers" and 2 "PCs" on one Windows host). To check your own servers, run `syncwatch probe` against each of them (see the README). The probe appends a section to this file, the same as the sections under [Probe runs](#probe-runs).

## Summary

| # | Question | Answer (2.1.5) | Consequence in syncwatch |
|---|---|---|---|
| 1 | Does `browse` list each top-level directory, hidden directories, spaces, å/ä/ö and `[ ]` included? | Yes. With a separator at the end, it lists all subdirectories, hidden directories included. **`current` is not a glob in 2.x**: Syncthing lists the parent and compares the last segment as a prefix. Thus names with `[ ]`, `*` and `?` need **no escape**. An escape makes the comparison fail. | The client sends the paths as they are (URL-encoded) with a separator at the end. It filters exact names when it checks that a directory still exists. |
| 2 | Browse time | Approximately 1 ms for each listing on the local host. Measure it again on the largest NAS share. | The limit stays at 3 parallel requests for each server. |
| 3a | Long-polls and idle timeouts | A long-poll of 50 s returns normally. Through a VPN, check it with `--event-wait` of the probe. | `event_timeout: 50s` is the default. You can change it. |
| 3b | Event mask names | Syncthing accepts all 16 names. A custom mask makes its own buffered subscription on first use. **Event IDs are for each subscription**, and they start at 1 again when Syncthing restarts (`globalID` is the global counter). | The collector starts the subscription before the full check. It finds IDs that decrease or gaps, and it does a full check when `startTime` changes. |
| 3c | New directories in `LocalChangeDetected` | Syncthing reports them with the path relative to the folder (the separator of the server) and **`action: "modified"`**, not `"added"`. Deleted directories come as `"deleted"`. | The collector compares each `type: dir` event other than `deleted` with the project pattern. A `deleted` event removes known nested project folders. |
| 3d | `ConfigSaved` payload | **It has the complete configuration, `gui.apiKey` and the password hash included.** | The client discards the payload of `ConfigSaved` before it returns the events (a unit test checks this). The event only starts a full check. |
| 4 | Does `FolderCompletion` have `remoteState`? | Yes (2.x). | Events update the completion in real time. The full check also calls `db/completion` for each (folder, device) pair. |
| 4b | `db/completion` on a paused folder | **HTTP 404 "folder is paused"**. | The collector skips paused folders. A 4xx or 5xx answer for one folder marks that folder, not the server. |
| 5 | Load of a full check | 17 requests in some milliseconds for 3 folders and 5 devices on the local host. Measure the CPU and the RAM on your servers before you select events mode or polled mode. | Polled mode exists for servers where the load is large. |
| 6 | `/metrics` with the API key | Reachable. `syncthing_model_folder_conflicts_total{folder=…}` is present for each folder. | H10 uses the differences, with the Syncthing `startTime` to find counter resets. syncwatch reads `/metrics` at each full check and each reconnect. |
| 7 | `stats/folder` `lastFile.at` | Present, but **Syncthing sets it only for files pulled from other devices** (zero after local changes only, and `0001-01-01` until Syncthing pulled a file). | C1 takes the newest `lastFile.at` on all servers with the folder. In events mode, C1 also uses the newest `LocalChangeDetected` time that the collector saw. It skips folders without a time. |
| 8 | TLS | A self-signed GUI certificate (CN `syncthing`), valid approximately 820 days. The pinned certificate (SHA-256) works, and syncwatch finds a replaced certificate during the handshake. | H3 and the Accept certificate button. |
| 9 | Counts | Simulation: 3 servers, 2 PCs, 6 project folders. | — |

Other observations:

- 2 servers that dial each other at the same moment lose one connection and connect again some seconds later. Thus a short H2 at a start is normal. The debounce of 15 minutes of H2 hides it.
- On Windows 11 24H2, Syncthing starts itself again without a console window. Thus the process that you start is not the process that continues.
- The pinned certificate also protects against **a changed name resolution**. In a Docker Compose test, the short name (`sol`) of a stopped container went to the DNS of the local network. The name resolved to a production NAS with the same name. syncwatch saw a different certificate, opened H3 and stopped at the TLS handshake. Thus no API key and no request reached that server. Test stacks now use `*.e2e.invalid` names. In production, use IP addresses or fully qualified names in server URLs.
- In the Syncthing container, `docker stop` frequently needs the full 10 s before Syncthing stops. The GUI stops immediately, thus H1 opens at the usual time.

## Structure scan load, simulated — 2026-10-04

`test/loadsim` (Docker): one Syncthing 2.1.5 container with a share of 300 project folders. Each project folder has 6 subdirectories on each of 4 levels (142,500 directories). 270 of the project folders are Syncthing folders. syncwatch ran from its image with the default `nested_scan_depth: 3` and `concurrency: 3`. The Docker engine runs in a software-emulated VM (approximately 20 times slower than native). Thus the times are an upper limit.

| Scan | Listings | Time | Calculation |
|---|---|---|---|
| Baseline scan (each project folder, every `baseline_interval`, default 7d) | 12,901 | 18–30 s | 1 + 300 × (1 + 6 + 36) |
| Structure scan in events mode (project folders not in Syncthing, every `structure_interval`, default 1h) | 1,291 | 2–4 s | 1 + 30 × 43 |
| Structure scan in polled mode (each project folder, every `structure_interval`) | 12,901 | 18–30 s | the same as the baseline scan |

- The dashboard API answered in less than 40 ms during the complete run.
- `syncwatch probe --project-pattern '^LIVE-'` against the same container estimated exactly 1,291 and 12,901 listings. Thus a probe run on each server measures the load of the scan without an installation of syncwatch.
- The listings grow with the square of the subdirectories for each level (depth 3 lists a project folder, its children and its grandchildren). A project folder with 30 subdirectories for each level needs 931 listings instead of 43. Measure on the NAS hardware before you depend on these numbers.
- Missed events (a full check during a large sync over a slow link): syncwatch scans the affected folders again 30 s after the last gap. It does at most one gap-caused full check every 10 minutes.

## Folders added paused through the API, simulated — 2026-10-06

For a possible companion tool (separate from syncwatch, which stays read-only) that registers new project folders as paused, unshared Syncthing folders. 2 Syncthing 2.1.5 servers (Windows build), SIRIUS and SOL, started by `test/harness` on loopback and connected to each other. The folder defaults of SIRIUS were set to `rescanIntervalS: 777` with the devices SIRIUS and SOL, and its default ignores to `.sync`, `@eaDir`. Each test project folder had Resilio-style content: `.sync/ID`, `.sync/Archive/old.bin`, a directory `.sync-other`, `a.txt`, `sub/b.txt`. These were one-time runs, not a test in the repository.

| # | Test | Result |
|---|---|---|
| 1 | `POST /rest/config/folders` with only `id`, `label`, `path`, `paused: true` | Missing fields come out of the **configured** defaults (`rescanIntervalS` 777), **devices included**. SOL listed the folder in `cluster/pending/folders` in some seconds, but the folder was paused. The directory was unchanged. |
| 2 | `POST` of the full defaults object with `paused: true` and devices = SIRIUS only | No change on disk: no `.stfolder`, no `.stignore`. `index-v2` had only `main.db`. `db/status` answered 200 with zeros (`db/completion` answers 404 for paused folders). `GET db/ignores` answered with empty lists. |
| 3 | `POST /rest/db/ignores` on that paused folder | It worked: Syncthing wrote `.stignore` and no other file. The folder stayed paused. |
| 4 | `browse` with `current=<folder>/.sync` | It returned `.sync` and `.sync-other`: the last segment is a prefix, and hidden directories are in the list. |
| 5 | `POST` again with the same ID and a different label | Syncthing replaced the folder configuration without an error. |
| 6 | Resume the same as the GUI does it | The `setFolderPause` of the GUI sets `paused` in its copy of the configuration and saves the complete configuration (`PUT /rest/config`). After the resume: `.stfolder` made, settings unchanged, `.stignore` loaded, `.sync/ID` not in the index, 2 files in the index. |
| 7 | Resume a second folder that had no `.stignore` | Syncthing did not make `.stignore` from the default ignores. `.sync/ID` and `.sync/Archive/old.bin` went into the index (4 files). |
| 8 | Syncthing issues syncthing/syncthing#10389 and syncthing/syncthing#10746: `PATCH {"paused": true}`, then `false` | Not reproduced: `rescanIntervalS` 86400, `fsWatcherEnabled` false and `versioning.cleanupIntervalS` 7200 all stayed, also through a GUI-style pause and resume. |
| 9 | Setting names | The key of the defaults is `blockIndexing` (default `true`). `syncthing cli config folders add` has `--paused`, `--type` and `--block-indexing`. |
| 10 | The same folder ID registered paused on both servers (the copy of SOL with its own `.stignore`), each resumed alone, then shared | When SIRIUS shared it, SOL listed the offer in `cluster/pending/folders`, but it had a folder with that ID. The addition of SIRIUS to the folder of SOL paired them. Syncthing did not transfer a 32 MB file that was identical on both (the copy of SOL 48 hours older). 45 KB crossed the connection in total, and there was no conflict. The change time of SOL became the time of SIRIUS. Syncthing copied a file that was only on SOL to SIRIUS. `c.txt`, different on each, became a conflict copy on both, and the newer content won. Syncthing did not send `.sync`, ignored on SIRIUS. |

Still to check on a NAS (Linux, Docker): the owner and the permissions of a `.stignore` written by the API, and the inotify limit.

## Probe runs

The run below is a record of 2026-10-01. It keeps the output of the probe of that date, with the old texts (for example `full re-check` and `trusted on first use`). A probe run now writes the texts of the current version.

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

