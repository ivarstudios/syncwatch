# Changes ste/docs: documentation in ASD-STE100 style with the r0 behavior

Branch `ste/docs` (base `review/sim-2026-10` at `9e454e9`), 2026-10-10. Scope: `README.md`, `plan.md`, `docs/api-findings.md` (prose only, the probe output stays), `docs/icon/icon-design.md`, the comments in `examples/*.yaml`, `examples/*compose.yml`, `Dockerfile`, `.github/workflows/*.yml` and `.dockerignore`. New: `docs/CHANGES-review.md` (the plain-language changelog) and this file. No Go file changed.

The style is `the test kit` (glossary, rules, decisions). The check titles, the page names and the labels in the README are the names of the product-text rewrite (`rewrite/findings.json`, `ui.json`, `discord.json`): Findings page, administrator, urgent message, settings notice, full check, structure scan, pinned certificate, ERROR, WARNING, INFO.

## 1. What changed

### README.md (rewritten section by section)

Structure kept: Install (Build it yourself, Where to run syncwatch, HTTPS, Updates), First start (Bootstrap file), Configuration (Shares and structure rules), Checks, Notifications, Security (Read-only gate, Behind a reverse proxy, Security reports), JSON status API, Home Assistant, Server probe, Development, Known limitations, License. Each command, path and fact of the old README is kept. New content, all checked against the code in this worktree:

- Install: `chmod 700 data`, `/data` mode 0700 in the image, database files 0600, the data directory default and `SYNCWATCH_DATA`, the H1 reason when the secret key is lost.
- HTTPS: certificate file reload (1 minute), self-signed renewal at 30 days, `SYNCWATCH_TLS_HOSTS` forms, the permitted values of `SYNCWATCH_TLS_SELF_SIGNED`, HTTP/2, the Host check (421) and what to set.
- Updates: `latest` only for final releases, pre-release tags, numbered steps.
- First start: the setup token on stderr, pinned certificates for self-signed and CA-signed servers, server ID rules, the bootstrap file (symlinks, single-file mounts, `null`), `import-config` while syncwatch runs (10 seconds), the maintenance commands need an existing data directory and the secret key.
- Configuration: the range table (from `config.Validate`), durations up to 10 years, `0` = default, strict clock values, `off`, different ends, the reminder rule, thresholds do not close open findings, `ignore_dirs` semantics (e's decision: an omitted key keeps the current list in the form; an import gives the defaults), placement attributes, `case_insensitive`, the gate limitation for `shares.add`.
- Checks: the table with the new titles and a Description column; H4 watcher error as WARNING after 1 hour; H6/C3 remembered state; H7/C2 for never-seen devices; S1 equal paths; C1 local changes; the one-time ID change for S3 to S7; stale findings (restart, first scan, failed listing, cross-server rule, disabled servers); `/metrics` at each full check and reconnect; per-folder HTTP errors; missed events; polled-mode scans.
- Notifications: the health line in the rewrite form, debounce from "became urgent", resolve hysteresis, reminder skip, overnight summary rules, settings notice rules (retry, drop on 4xx, never blocks urgent), snooze to the end of the day with a reason, the snoozed and ignored list, 90-day window, webhook rules (canonical form, send-time check, no redirects, role ID digits), undecryptable webhook, escaping.
- Security: pinned CA-signed certificates and accepted renewals, certificate backoff (5 minutes, hourly warning), the limiter per role and under parallel load, `SameSite=Lax`, the revision check of the settings forms, the automatic refresh pause.
- Read-only gate: token first (health included), query rules and the 400 body, browse roots and `SYNCWATCH_GATE_BROWSE_ROOTS`, folder list refresh, `SYNCWATCH_GATE_LISTEN` and `:8385`, `SYNCWATCH_HEALTHCHECK_URL`, the pin file permission, the certificate-change instruction, the refusal log line.
- Reverse proxy: the forwarded `Host` must be a known name.
- JSON status API and Home Assistant: `notifications.error` for administrators only, `scanning` and `watch_error` counts, the status `watch error`, the bracket access, traffic gaps, the container-name case.
- Server probe: `--pin`.
- Development: `SW_IT_BASEPORT`, `SW_IT_DIR`, `KEEP_COMPOSE`, `KEEP_LOADSIM`, the isolated Compose stacks, the harness tests with `SYNCTHING_BIN`, the Windows LF note, CI and release rules, `SYNCWATCH_ALLOW_ANY_WEBHOOK`, `--version`, `-h`, Go 1.27.2.
- Known limitations: the depth limit for scans and events, the 10-minute staleness after a gap.

### plan.md

"Where things stand" is "Current state (2026-10-10)" with the r0 state of tests, CI and release. The road to v1.0 has the rc rule and the test gate. New section "Open items from the review of October 2026" with links to `docs/changes/r0-*.md`. The decisions follow the r0 behavior (pinned CA-signed certificates, gate address and query rules, settings notices, `SameSite=Lax`, Host check, stale findings, resolve hysteresis, watcher error, polled-mode scans). Known gaps: the gap staleness and the S1 path spellings. The test matrix row says what the suites do now.

### docs/api-findings.md

Prose only: the intro, the summary table, the observations, the load table (with the polled-mode row from r0-s) and the paused-folders table. Row 4b (per-folder errors), 6 (`/metrics` at each full check and reconnect) and 7 (C1 with `LocalChangeDetected`) have the r0 consequences. The probe runs section is generated output and is unchanged.

### docs/icon/icon-design.md, examples, Dockerfile, workflows, .dockerignore

Rewritten comments in STE. `examples/gate-compose.yml` keeps b's `SYNCWATCH_GATE_LISTEN`, `SYNCWATCH_HEALTHCHECK_URL` and `SYNCWATCH_GATE_BROWSE_ROOTS` lines. `examples/docker-compose.yml` adds `chmod 700` to step 1 and the Host check note to `SYNCWATCH_TLS_HOSTS`. `examples/ivar.yaml` has the role ID digits, the reminder rule and the gate note for `shares.add`. `examples/home-assistant.yaml`: the `json_attributes` lists now include `folders_scanning` and `folders_watch_error` (the fields that r0-k added to `/api/v1/ha`); this is the one change beyond comments. The Dockerfile label says "urgent messages for Syncthing installations".

### docs/CHANGES-review.md (new)

The changelog for the studio, grouped by area, with the list "What you must do after the update" first.

## 2. Score

Measured with `inventory-tools` (`docs_extract.extract_md` for the Markdown files, a comment extractor for the examples and the Dockerfile) against this worktree and `tools/stecheck.py`. Targets: README 80 percent, docs and examples 75 percent.

| Group | Before | After |
|---|--:|--:|
| readme | 39.1 % (279 entries) | 100.0 % (613 entries) |
| docsother | 44.8 % (277) | 97.6 % (458) |
| examples | 44.9 % (69, own extractor) | 100.0 % (109) |

The entry counts grew because the rewrite splits long sentences. The full residue is in `the test kit`. The remaining 27 violations are all in the generated probe output of `docs/api-findings.md` (kept as the probe writes it).

## 3. Discrepancies between the changes files and the code

The code wins. Found while each README claim was checked against the worktree:

1. **Check titles, severity words, health line.** The README uses the names of the product-text rewrite (the task's rule), but the code in this worktree still has the old catalog titles (`Server link down`, `Overlapping Syncthing folders`, `Bad name`, `Wrong share`, `Inactive project`, `Old device`, `Dead share` in `internal/config/catalog.go`), r0-k's lower-case severity words (`error`, `warning`, `suggestion` in `model.Severity.Label`), the page name "Problems" and the health line `N/M servers OK (X down; Y not yet checked)` (`internal/notify/notifier.go`). The README says "Servers not connected", ERROR/WARNING/INFO, "Findings" and `3 servers up · … · ERROR 2 · WARNING 5 · INFO 3`. The dashboard and the README agree only after the product groups of the rewrite are applied (`apply-notes.md`). Until then, this is the largest visible difference.
2. **`ignore_dirs` in the Structure form.** r0-g says an omitted key restores the defaults; r0-e says it keeps the current list. The code has e's behavior (`internal/web/settings.go`, the `IgnoreDirs == nil` branch), as the integrator decided. The README documents e's behavior and says how to get the defaults back (paste them).
3. **Health line "not yet checked".** r0-g2 added it; the rewrite (DIS-026 to DIS-029, written against the base commit) does not know it. The README describes it in words ("marks a server without a result as not checked") instead of a quoted string, because the final spelling depends on the product apply. Open question for the author (section 4).
4. **Form limits.** r0-g notes that the notification form limits `weekly_top` to 25 and `down_after_failures` to 10 while validation permits 50 and 20. The code still has both limits (`settings.html` `max="25"`, `max="10"`). The README range table shows both numbers.
5. **Gate port in the Compose test stack.** r0-b left the e2e stack on 8080 through `SYNCWATCH_GATE_LISTEN: ":8080"`. Unchanged in the code; not user-facing, listed in plan.md as open.
6. **`usage()` text.** r0-h says the usage text still lists `syncwatch version` only. True in the code (`cmd/syncwatch/main.go`): `--version` and `-h` work but the usage block does not name `--version`. The README names `--version`; the usage text is product text (cli group), not in this scope.
7. **r0-i item 5 wording.** The C2 message in the code says "has never connected since syncwatch started watching it, over 90 days ago" (old wording with "since" and "watching"); the rewrite (FND-112) has a different text. The README describes the behavior, not the string.
8. **Example attributes.** `examples/home-assistant.yaml` did not list the new counts of r0-k; now it does (see above).

No changes file claimed a behavior that the code contradicts beyond items 1 to 3; the other items are gaps, not contradictions.

## 4. Open questions for the author

1. Should the health line keep the form `N/M servers OK (…)` or become `N of M servers up (…)` as the rewrite proposes (DIS-027)? The README uses the rewrite form. The part `(SOL down; SIRIUS not yet checked)` joins 2 facts with a semicolon, which STYLE.md forbids in prose; a form such as `(SOL down · SIRIUS not checked)` would follow decision 22.
2. r0-k chose lower-case severity words (`error`, `warning`, `suggestion`) for the badges; STYLE.md decision 2 chooses ERROR, WARNING, INFO. The README follows STYLE.md. One of the two must change in the code.
3. The README documents both the validation range and the narrower form limit for `weekly_top` and `down_after_failures`. Which one is intended?
4. The README says that a request by a container name needs the name in `SYNCWATCH_TLS_HOSTS` (r0-e's decision: every request is checked, API tokens included). If service-name calls from Home Assistant are common, the author may want to exempt valid API tokens (r0-e names this as a possible follow-up).
5. `docs/api-findings.md` "Probe runs" keeps the old probe wording (`full re-check`, `trusted on first use`, `per subscription`). The cli group of the rewrite (CLI-128 and following) changes the templates; the recorded run could be regenerated after the product apply, or kept as a historical record.
6. The README check table quotes the catalog titles of the rewrite. If a title changes during the product apply, the table must follow (`apply-notes.md`: the README table is applied last).

## 5. Facts that could not be checked

- The screenshots in `docs/screenshots/` show the old dashboard (the "Problems" page, old severity words). Not regenerated: the dashboard text is outside this scope.
- The exact text of the 421 answer, the H1 reason for an unreadable API key and the gate bodies are quoted in the README only in words, not as strings, because the product apply changes them.

## 6. Commits

One commit for each file group: `docs(readme)`, `docs(plan)`, `docs(api-findings, icon)`, `docs(examples)`, `build(docker, ci)`, `docs(changes)`.
