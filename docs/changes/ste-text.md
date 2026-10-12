# Changes ste/text: the product text in ASD-STE100 style

Branch `ste/text`, base `review/sim-2026-10` (`9e454e9`), 2026-10-10. The rewrite proposals of the STE pass
(`the test kit`, 788 entries in the groups ui, discord, findings, errors, cli, logs)
were applied to the templates, the Go string literals and the log messages; the strings that the r0 fixers added
after the inventory was made were rewritten in the same style. The rules are `STYLE.md` (the glossary, the
decisions and the rule subset); the apply order and the traps follow `apply-notes.md`.

Nothing but text changed: no route, form field name, YAML key, environment variable, CLI flag name, API value,
notification kind, log attribute key or detail key used by a test (`New fingerprint (full)`) is different. The
`/problems` URL and the `Nav` key stay; the page is called Findings.

## Counts

| | Entries | Note |
|---|--:|---|
| Mapping entries | 788 | 430 rewrites, 358 keeps |
| Rewrites applied with the mapping text | 390 | 352 by the apply script (`applied-tools/apply.py`: exact match at the cited line, in the file, or every bounded occurrence of the same literal), 38 by hand (strings built from parts, template conditions, HTML with inline tags); 3 of them in a file the r0 fixers had moved the string to |
| Rewrites adapted | 39 | the r0 fixers had rewritten the original text; the new text was written in STYLE.md terms instead (list below) |
| Rewrites unmatched (dropped) | 3 | FND-050 (the H4 reason `watcher: …` became a separate H4 finding in r0-i; the new finding text is rewritten), FND-082 (r0-i removed the H7 message for a never-connected device; C2 reports it, FND-112), ERR-101 (r0-d removed the `db/status %s: %w` wrapper) |
| Keeps found unchanged | 340 | |
| Keeps located in a changed form | 16 | value lists and strings now built with the plural helper or by concatenation (UI-075, UI-085, UI-105, UI-249, UI-265, UI-268, UI-276, UI-277, DIS-002, DIS-043, DIS-045, FND-025, FND-026, FND-027, FND-048, ERR-100); the visible text is the same |
| Strings added by r0 | 168 literals reviewed | 83 user-facing entries in the fresh inventory (`applied-inventory.json`, IDs `-9xx`); 65 of them rewritten, the rest already compliant or machine values |
| Score | 99.9 % weighted | ui 100, discord 100, findings 100, errors 99.4, cli 100, logs 100 (`applied-score.md`); one false positive remains (ERR-920: `since` is a query parameter name) |

## Commits

One commit per area, in the order of `apply-notes.md` section 1:

1. `text(config)`: check titles (H2, S1, S4, S5, C1, C2, C3), severity display names (`Severity.Label()` is
   `ERROR`/`WARNING`/`INFO`; `String()` and `Lower()` keep the API values), validation messages with the field
   label and the YAML key, `FieldLabel`, `Duration.Words`, the export header.
2. `text(findings)`: finding messages, reasons, detail labels, the reason joiner ` · `, the C3 suffix, `rules.Plural`.
3. `text(web)`: templates, views, flash and validation messages, the settings notices, the field-name → label map,
   the `sentence` helper, the flash CSS.
4. `text(discord)`: notification titles and section titles, the health line, the delayed-notice sentence, the
   delivery errors.
5. `text(collector)`: tile errors, log lines and errors of the collector, the client, the engine and the store.
6. `text(cli)`: usage, flag help, command output, startup errors, the gate, the probe report.
7. `docs`: this file.

## Decisions

- **Plural helpers instead of template conditions** (UI-029, UI-059, UI-063, D18). Fixer k had added the `plural`
  template function and `web.plural`; they are used for every count that needs a singular (`1 folder`, `1 failed
  item`, `1 remaining item`, `1 directory listing`, `1 secret`). `rules.Plural(n, one, many)` and
  `main.plural` were added for the finding texts (`1 failed item`, `1 remaining item`, `1 new sync conflict`) and
  the `import-config` output (`1 server`, `1 secret`). `notify.plural(n, noun)` (k) serves the reminder title
  (`⏰ Still open: 1 urgent finding`) and the health line (`1 server up`, `1 PC`).
- **Severity display names** (FND-024, D2). `model.Severity.Label()` returns `ERROR`, `WARNING`, `INFO` (fixer k had
  made it `error`, `warning`, `suggestion`); the badge, the severity filter options and the Checks table use it.
  The Findings summary line is `ERROR 2 · WARNING 5 · INFO 3`; the health line too. `ParseSeverity` still accepts
  the old words.
- **Validation messages name the field by its label** (ERR-034, ERR-066 to ERR-096, STYLE 3.4). Fixer g had
  replaced the per-field messages by `durRange`/`intRange` with the YAML key. `config.FieldLabel(key)` maps the
  YAML keys to the labels of settings.html (`Event long-poll timeout (collector.event_timeout): the value must be
  between 5 seconds and 10 minutes.`); keys without a dashboard label (`access.session_ttl`,
  `structure.nested_scan_depth`) keep the key; a check debounce is `H1 debounce (checks.H1.debounce)`. The same
  labels exist once more as `web.fieldLabels` for the Checks and Collector forms, because the templates are
  static text; a label change must be made in both places (the label strings are not shared with the templates,
  as apply-notes suggested with Go constants: that would have meant passing 23 labels into the template data).
  `Duration.Words()` writes the bounds as digits and the unit word (D19).
- **Machine text after a flash prefix** (ERR-018, ERR-049, ERR-032). The prefixes are `Not saved. `, `Import
  failed. `, `The test message failed. `; `web.sentence()` gives the appended YAML parser, store or Discord text a
  capital first letter and a final period, on every line. `config.Validate` joins its messages with a newline, so
  `.flash` got `white-space: pre-line` (one message per line). The Checks and Collector forms join their messages
  with a space (was `; `).
- **Settings notices** (DIS-040 to DIS-071). Fixer e had made one generic `configChanges` list (`key: old → new`)
  and one title `Server {name} changed` for every change of a server. The generic line is now `key changed from
  {old} to {new}.`; the server, check, role and access lines are complete sentences (`The server X is disabled.
  syncwatch does not poll it now.`, `The check H4 (Folder sync stopped) does not send urgent messages now.`,
  `The check S1 (…) waits 30 days instead of 5 minutes before an urgent message.` with `rules.Ago`); the viewer
  access line uses the option texts of the Viewer access field (`administrator only`, `with the viewer
  password`, `open, without login`). The titles stay noun phrases (`Server {name} changed`, `API token
  replaced`, `Administrator password changed`, `Urgent finding snoozed`).
- **Gate error match** (ERR-113, ERR-119). Fixer b's `upstreamError` found a changed certificate by the text
  `certificate changed`; the new text is `the certificate of Syncthing changed (pinned …, new …)`. The gate now
  wraps a sentinel `errCertChanged` with `%w` and matches it with `errors.Is` (and the sentinel's text as a
  fallback through the HTTP client), so the wording cannot break the match again.
- **`timed out` → `timeout`** (ERR-099). Only the dashboard text changed; the match side of `netErrs`
  (`{"timed out", "timeout"}`) still matches Go's error text.
- **Log line kept**: `gate refused a request` (LOG-041, kept in the mapping) stays although r0-b moved it.
- **Texts that outside systems read** (apply-notes section 2): the health line is now `3 servers up · 14 PCs, 2
  offline more than 7 days · ERROR 2 · WARNING 5 · INFO 3 · 3 snoozed`; `1 of 2 servers up (SOL down)`; servers
  without a result yet are `(AKKA, SOL not contacted)`. An outside check or Home Assistant template that compared
  `servers OK` must be updated. Everything else that the API returns is unchanged (`warn`, `cert-changed`, `watch
  error`, `{"error": …}`, `ok`).
- **Capital letters in Go errors**: `errUnknownFinding` ("No open or recent finding has this ID."), `checkNewPassword`,
  the two `AcceptCert` errors and the config validation messages start with a capital letter and end with a
  period because the handlers show them directly as a flash (apply-notes section 2 accepts the ST1005 lint).
- **Adapted entries** (the mapping text could not be applied because an r0 fixer had rewritten the string; the new
  text follows STYLE.md): UI-029, UI-059, UI-063, UI-073, UI-252, UI-256, UI-267 (plural helper), DIS-019, DIS-027,
  DIS-028 (count-aware health line and reminder), DIS-044, DIS-046 (generic server notice of r0-e), FND-024
  (`Label()`), FND-049, FND-055, FND-068 (plural), FND-108, FND-112 (r0-i's C1 basis text and never-connected C2),
  ERR-012 (r0-k's end-of-day snooze), ERR-022, ERR-052, ERR-053 (parts), ERR-067 (r0-g's ID rules), ERR-074 to
  ERR-078 (`durRange`/`intRange`), ERR-082 (role ID), ERR-113, ERR-119 (gate), CLI-022, CLI-035, CLI-049 (r0-b
  and r0-h flag texts and import output), CLI-054 (export header), LOG-003 (r0-h's longer warning), LOG-006 (moved
  to `web/server.go` by r0-e), LOG-012, LOG-039 (moved into `stclient` and `gate` by r0-a). The texts are in
  `applied-tools/adapt.json`.

## Strings left as they are

- `ERR-920` `the parameter since must be 0 or a positive integer` (gate, HTTP 400 to syncwatch's client): `since`
  is the parameter name of `/rest/events`.
- The netErrs match texts (`No connection could be made`, `broken pipe`, `server misbehaving`): Go's error texts,
  not shown.
- API and status values: `error/warn/info`, `up/down/cert-changed/unknown`, `watch error`, `up to date`, the
  weekday values, the notification kinds, the channel value `discord`, `ok` on `/healthz`.
- Debug-level log lines were not in the inventory; the two with `re-check` were rewritten, the others were not reviewed.
- Comments, test names and test fixture texts keep the old words where they are not assertions (apply-notes section 3).
- README, plan.md, docs and examples: not in this pass; they must adopt the check titles (Servers not connected,
  Syncthing folder inside another, Incorrect name, Incorrect share, Inactive project folder, Inactive device,
  Folder not used by device), the page name Findings, the `Discord:` error prefix, the health line, the
  finding messages and the setup log line `no administrator password is set: open the dashboard and type the setup token`.

## Test changes

Assertions on text were updated in `cmd/syncwatch/main_test.go`, `internal/app/app_test.go`,
`internal/collector/collector_test.go`, `internal/config/config_test.go`, `internal/gate/gate_test.go`,
`internal/gate/pin_test.go`, `internal/notify/notifier_test.go`, `internal/rules/rules_test.go`,
`internal/stclient/client_test.go`, `internal/stclient/pin_test.go`, `internal/store/store_test.go`,
`internal/web/security_test.go`, `internal/web/settings_test.go`, `internal/web/web_test.go` and
`test/integration/integration_test.go` (the S5–S7 substrings `… in share …`). The 12 assertions of apply-notes
section 3 are among them; the rest assert on r0-added texts. No test was removed; the severity test of fixer k now
expects `ERROR`, `WARNING`, `INFO`. `gofmt`, `go vet` (plain and with the suite tags), `go build` for Windows and
Linux, `go test ./...` and the integration suite with Syncthing 2.1.5 pass.
