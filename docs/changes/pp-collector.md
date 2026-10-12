# pp-collector — Pre-release review: the corrections of the area "collector"

Branch `sim/pp-collector`, worktree `the test kit`, base `review/sim-2026-10` at `7f825f1`. The
pre-release review found 6 defects in `internal/collector`, `internal/stclient` and `internal/gate`. This branch has a
commit for each finding (gate-3 has 2). The evidence is in `the test kit`.

| Key | Commit | Files |
|---|---|---|
| gate-5 | `538ac97` | `internal/stclient/client.go`, tests |
| gate-4 | `54d067b` | `internal/gate/gate.go`, `internal/gate/gate_test.go`, `test/compose/compose_test.go` |
| text-2 | `707db5d` | `internal/collector/need.go`, `internal/gate/gate.go` (comment), `docs/changes/r2-3.md`, tests |
| gate-3 | `6b0ca9a`, `75287f1` | `internal/gate/gate.go`, `internal/gate/gate_test.go` |
| gate-1 | `5e0a44f` | `internal/collector/listeners.go`, `internal/collector/r2_collector_test.go` |
| gate-2 | `8eb60b2` | `internal/collector/collector.go`, `internal/collector/manager.go`, `internal/collector/r2_pin_test.go` |

## 1. text-2: H5 "Needed files" listed the files that the PC must delete

**Defect.** A program on a Windows PC keeps 25 files open. A person deletes them on the NAS and changes 1 more file that
is also open. The PC cannot delete or replace them. Syncthing 2.1.5 reports `needItems` 1 and `needDeletes` 25. H5
showed "1 remaining item", but the row "Needed files" had `busy.txt` and 19 deleted files, with no "…and N more". When
the file that the PC needs had a name after the deleted files, the row showed only deleted files.

**Cause.** `/rest/db/remoteneed` lists the deletions that a device did not do, in the order of the names. Syncthing
counts them in `needDeletes`, not in `needItems`. `needList` kept every entry of the page. The decision in
`docs/changes/r2-3.md` ("they count in `Items`") was not correct.

**What changed.** `internal/collector/need.go`: the collector reads 100 entries (`needPage`, the maximum of the gate)
and keeps the first 20 names that are not deletions (`needPerPage`). `docs/changes/r2-3.md`: the decision has a
correction.

**User-visible.** The row "Needed files" of H5 shows only the files that the device needs. Its count agrees with
"Remaining items". When more than 100 deletions come before the needed files in the order of the names, the row shows
only "…and N more".

**Tests.** `TestNeedListsWithoutDeletions` (`internal/collector/r2_collector_test.go`): 2 needed files, one before and
one after 25 deletions. The need list and the H5 row are the 2 files. The fake Syncthing lists the deletions in the
order of the names, as 2.1.5 does. `TestNeedListsFromFullCheck` now checks for `perpage=100`.

**Live check.**

- `live/text-2-native-console.txt` and `live/text-2-native-evidence/`: the native run of the review (Syncthing 2.1.5, a
  program keeps the files open on the PC, the gate of this branch, ports 24520-24524). Phase A: H5 "Needed files" is
  `busy.txt`. Phase C: H5 "Needed files" is `zz-late.txt`.
- `live/kit-s26-h5.txt`: kit slot s26. On SOL, root owns a directory. AKKA deletes 25 files in it and changes 1 file.
  AKKA reports `needItems` 1 and `needDeletes` 25 for SOL. The first page of 20 entries has `locked/busy.txt` and 19
  deleted files. H5 shows "Remaining items 1" and "Needed files `locked/busy.txt`" (`live/kit-s26-shots/`). The gate of
  this branch permits the request of syncwatch for SOL.

## 2. gate-1: the password of a private relay pool showed in H12

**Defect.** A listen address such as `dynamic+https://user:password@relays.example/endpoint` gives Syncthing the user
name and password of a private relay pool. Syncthing reports the address unchanged in `/rest/system/status`. When H12
opened for this pool, its detail "Relay" had the password. Viewers saw it on the Findings page, an API token with the
scope Full read it in `/api/v1/status`, and the database kept it in the finding record.

**Cause.** `relayURI` removed the query and the fragment of the address, but not the user information.

**What changed.** `internal/collector/listeners.go`: `relayURI` also removes the user information. For an address that
is not a URL, it removes all text up to the last `@` of the address.

**User-visible.** The detail "Relay" of H12 shows the address of the pool without the user name and password, for
example `dynamic+https://relays.example/endpoint`.

**Tests.** `TestRelayURI` has a pool with a user name and a password, a pool with only a user name, a relay with user
information and a token, and 2 addresses that are not URLs.

**Live check.** `live/gate-1-native.txt`: the native test of the review with the binary of this branch (Syncthing 2.1.5,
a fake relay pool, the gate and syncwatch on 127.0.0.1:24520-24524). The detail "Relay" is
`dynamic+http://127.0.0.1:24522/endpoint`. The API answer, the open view, the viewer page and the database have no
password. The test is successful.

**Not changed.** The gate still sends the answer of `/rest/system/status` without a change. The answer has the listen
addresses with the pool password and the token of a private relay. Only the holder of the gate token can read it. A
cleaner for this answer is the second layer that the finding proposes. It is not in this branch, because the gate
refuses an answer that it cannot clean, and `/rest/system/status` is the request that tells syncwatch that the server is
up.

## 3. gate-2: 2 alternating CA-signed certificates gave a settings notice for each new connection

**Defect.** The server URL is a name with 2 front ends, and each front end has its own CA-signed certificate for the
name (DNS round robin, 2 proxy servers with their own ACME client). Each new TLS connection that got the other
certificate pinned it again. Each pin gave the settings notice "Server X: new CA-signed certificate". In the review,
this was 1 settings notice each minute, and up to 1 for each request. During a Discord outage, these settings notices
used all 50 kept positions, and syncwatch discarded older kept settings notices about security.

**Cause.** The `OnPin` function of `newCollector` sent the settings notice for each pin that changed the stored
certificate. It did not remember the certificates that it pinned before.

**What changed.** `internal/collector/collector.go`: the collector remembers the certificates that it pinned in the last
24 hours (`pinQuiet`, `pinNews`). It calls `onPin` only for a certificate that is not one of them. The pinned
certificate in the database and the check of the pinned certificate do not change.

**User-visible.** 2 front ends with 2 certificates give 2 settings notices: the first certificate and the second
certificate. A certificate that returns after more than 24 hours gives a settings notice again.

**Tests.** `TestOnPinAlternatingCASigned`: a server with 2 CA-signed certificates for 127.0.0.1 closes each connection
and gives the other certificate at each handshake. The full check makes 25 handshakes and 2 `OnPin` calls. Without the
correction, the test gets 23 calls. `TestPinNews` checks the 24 hours.

**Live check.** `live/gate-2-app-native.txt`: the test of the review in `internal/app` with `app.Run`, a real Syncthing
2.1.5, 2 front ends and a fake Discord (127.0.0.1:24525-24529). In 124 handshakes, the collector makes 2 `OnPin` calls.
Discord gets 1 settings notice "Server AKKA: new CA-signed certificate". During a Discord outage of 4 minutes 30
seconds, syncwatch keeps the settings notice "Server SOL changed" and sends it after the outage. The test is successful.
The log still has 1 INFO line "certificate replaced, new certificate pinned" for each new pin (23 lines in 30 seconds in
this test). `live/gate-2-app-native-run1-phase3-30s.txt` is a first run that waited only 30 seconds for the delivery
after the outage: syncwatch kept the settings notice of SOL and did not discard it.

## 4. gate-3: the gate token gave the complete file list of each folder

**Defect.** For a device that does not share the folder, and for a device ID that the server does not know, Syncthing
answers `/rest/db/remoteneed` with all files of the folder. The gate permitted 1,000 pages of 100 entries. A holder of
the gate token thus read up to 100,000 file names of each folder, with size, time and version.

**Cause.** The gate checked only the form of the device ID and the range of `page` and `perpage`.

**What changed.** `internal/gate/gate.go`:

- `6b0ca9a`: the gate permits only page 1 (or no `page`). The text of the refusal is "the parameter page must be 1".
  `perpage` stays 1 to 100, because the collector now reads 100 entries (text-2).
- `75287f1`: the gate permits the request only for a device that shares the folder (`checkNeed`). The gate uses the
  folder list that it reads for `browse`, and it reads the list again after 10 minutes. For a device that is not in the
  list, it reads the list again at most 1 time each minute. Thus a device that an administrator added works after 1
  minute. A different request gets `400` "the device does not share the folder".

**User-visible.** A holder of the gate token gets only the need list of a device of the folder, which H5 shows. No
change for syncwatch: the collector asks only for the connected devices of a folder. The gate of this branch permits the
request of syncwatch `7f825f1` (page 1, 20 entries). The gate of `7f825f1` permits the request of this branch (page 1,
100 entries).

**Tests.** `TestGateQueryAllowlist` (page 2 and page 1000, a device that shares no folder and an unknown folder: 400),
`TestGateRemoteNeed` (the refusal texts, Syncthing gets only the permitted request, the gate permits a device that an
administrator adds after 1 minute). `TestClientThroughGate` counts 3 devices in the fake folder.

**Live check.**

- `live/gate-3-native.txt`: the native test of the review (Syncthing 2.1.5, 250 files in `proj-secret`, the gate of this
  branch on 127.0.0.1:24528). PC-C and an unknown ID get 0 names. The request of syncwatch for PC-B gives 100 entries
  with `perpage=100`. The test is successful.
- `live/kit-s26-gate-requests.txt`: the gate of AKKA in kit slot s26. SOL gets 200, EDIT-02 (no share) and an unknown ID
  get 400, and page 2 gets 400.

## 5. gate-4: the gate passed event long-polls without the event mask

**Defect.** A holder of the gate token sent `GET /rest/events?since=0&timeout=1` (no `events`) or `events=` to the gate.
The gate sent it to Syncthing. Syncthing answered from its default subscription, with events that the collector does not
ask for: `LoginAttempt` (GUI user name and address), `ListenAddressesChanged` (the token of a private relay), `Starting`
(the home directory) and item events. README tells that "`events` needs the event mask of the collector".

**Cause.** `checkQuery` compared the mask only when `events` was not empty.

**What changed.** `internal/gate/gate.go`: `checkQuery` also refuses a missing or empty `events` with the same text.
`test/compose/compose_test.go`: the compose test sends the mask, and it checks the `400` for a request without it.

**User-visible.** No change for syncwatch: its client always sends the mask. Other requests get `400` "the parameter
events must be the event mask of the collector".

**Tests.** `TestGateQueryAllowlist` (no `events`, `events=` and no mask with `since` and `timeout`: 400). The other gate
tests send the mask.

**Live check.**

- `live/gate-4-native.txt`: the native test of the review (Syncthing 2.1.5 with a GUI user, a login, a relay token and a
  file transfer, the gate on 127.0.0.1:24530). Without the mask and with `events=`: 400 and 0 events. With the mask:
  200. Straight at Syncthing: 43 events of the default subscription.
- `live/kit-s26-gate-requests.txt`: the gate of AKKA in kit slot s26 gives 400 for the 3 requests without the mask.
  syncwatch in s26 shows both servers up, and the gate log has no refusal of a request of syncwatch.

## 6. gate-5: syncwatch did not recognize the refusal of the published gate

**Defect.** The gate of the public version `78ef2d8` refuses an unknown endpoint with `404` "syncwatch gate: endpoint
not allowed". The answer of later gates is "endpoint not permitted". Behind a gate of `78ef2d8`, H5 showed no file
names, and the collector asked `/rest/db/remoteneed` again at each full check. The log had only DEBUG lines, and the
INFO line that README and CHANGES-review promise did not come.

**Cause.** `GateRefusedEndpoint` knew only the later text.

**What changed.** `internal/stclient/client.go`: `GateRefusedEndpoint` accepts both texts.

**User-visible.** Behind a gate of `78ef2d8`, the log of syncwatch has 1 INFO line for each connection to the server:
"the gate does not permit /rest/db/remoteneed: update the gate to see the needed files in H5". syncwatch then asks no
more in that connection.

**Tests.** `TestGateRefusedEndpoint` has the old text (and the old text with 400: not a refusal of the endpoint).
`TestNeedListWithOldGate` runs with both texts.

**Live check.** `live/gate-5-native-gate78ef2d8.txt`: the native test of the review with the gate binary of `78ef2d8`
and syncwatch of this branch (Syncthing 2.1.5, 127.0.0.1:24535-24539). In 4 minutes 30 seconds with 5 full checks, the
gate refused 1 request. The log of syncwatch has 1 INFO line "the gate does not permit /rest/db/remoteneed: update the
gate to see the needed files in H5" and no DEBUG line about it. The test is successful.

## Requests to the integrator

These changes are in files outside the area of this branch:

- `test/compose/compose_test.go` (gate-4, commit `54d067b`): the compose test sends the event mask through the gate, and
  it checks that a request without the mask gets `400`. Without this change, the compose test fails.
- `docs/changes/r2-3.md` (text-2, commit `707db5d`): a correction of the decision about deletions in `Names`.

Proposed lines for `README.md`:

- Line 446, replace with: "At each full check, syncwatch reads the list of the files that a connected device still needs
  (`/rest/db/remoteneed`, the first 100 entries). H5 shows the first 20 names. The list has no deletions, because
  Syncthing does not count them in the remaining items. syncwatch does this only for the folders where the device has
  remaining items."
- Line 528, after "`events` needs the event mask of the collector": "A long-poll without `events` also gets `400`."
- Line 529, replace the first sentence with: "The gate permits `/rest/db/remoteneed` only for page 1, with at most 100
  entries, and only for a device that shares the folder."

Proposed lines for `docs/CHANGES-review.md`:

- "H5: the row \"Needed files\" shows only the files that a device needs. Deletions that the device did not do are not
  in it." (text-2)
- "H12: the detail \"Relay\" shows the address of a relay pool without the user name and password." (gate-1)
- "2 front ends with their own CA-signed certificate for the same server URL give 2 settings notices. Each new
  connection does not give a settings notice." (gate-2)
- "The gate permits `/rest/db/remoteneed` only for page 1 and only for a device that shares the folder. The gate refuses
  an event long-poll without the event mask of the collector." (gate-3, gate-4)
- Item 13 of "What you must do after the update" is now true also for the gate of `78ef2d8`: the log of syncwatch tells
  it. (gate-5)

## Notes

- `TestUndecryptableNoticesWebhook` (`internal/notify`) fails on `7f825f1` without a change of this branch
  (`notify-failure-at-7f825f1.txt`). It fails in each `precommit-*.txt` of this branch for the same reason. The tests of
  the other packages are successful.
- For the runs only, this branch used copies of the live tests of the review in the worktree, with the build tag
  `ppcollector`. No commit has them. Copies are in `the test kit` and in `live/*.go.txt`.
