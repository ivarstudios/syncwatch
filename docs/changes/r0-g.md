# r0-g: configuration validation, durations and identifiers

Branch `sim/r0-g`, base `78ef2d8`. Owner: fixer g. Files owned: `internal/config/*.go`, `internal/config/*_test.go`, `examples/home-assistant.yaml`.

Every item has a unit test that fails on `78ef2d8` and passes now. The run of the new config tests against the original code is in `the test kit` (all fail there except `TestIgnoreDirsNilVersusEmpty`, see item 5). Before each commit: `gofmt -l .` empty, `go vet ./...` and `go vet -tags integration,compose,loadsim ./test/...` clean, `GOOS=linux go build ./...`, `go test ./...` green (Windows, no `-race`).

## Files touched outside my ownership

All are the smallest hunk that the item needed. Merge notes for the integrator:

| File | Lines (base) | Why |
|---|---|---|
| `internal/web/settings.go` | 300 (webhook check in `handleNotifySave`) | store the canonical webhook form (item 7); 2 lines |
| `internal/web/settings.go` | 454-456 (`handleStructureSave`) | the nil → `[]` substitution that dropped the default `ignore_dirs` (item 5); 3 lines deleted |
| `internal/web/structure_form_test.go` | new file | the failing test for item 5; a new file so it can't conflict |
| `internal/web/security_test.go` | 152, 162 | role id `42` → 18 digits (item 7) |
| `internal/app/app_test.go` | 29 | role id `123` → 18 digits (item 7) |
| `test/integration/integration_test.go` | 25 | `role` constant → 18 digits (item 7); the fake Discord only compares the string |
| `test/compose/compose_test.go` | 27 | same |
| `test/compose/docker-compose.yml` | 102 | same |

Not touched although related (for the owners): `internal/web/settings.go:289` caps `weekly_top` at 25 and `web/templates/settings.html:78`, `:148` cap `weekly_top` at 25 and `down_after_failures` at 10 in HTML; validation now allows 1-50 and 1-20. `handleNotifySave` calls `c.Validate()` before `ApplyDefaults`, so `OFF` typed into the quiet-hours field is still refused there (see item 3). The quiet/reminder validation hunk (`config.go` 335-352 in the base) belongs to fixer g2 and is byte-for-byte unchanged.

## 1. Overflowing durations (map 06 hunch 1, map 03 hunch 3, map 05 hunch 15)

**Hunch.** "An overflowing `debounce` bricks the next start and silently resets settings." Verified by the map: `1000000w` became a negative Duration that passed validation; H1 then pinged at once; `Config.Clone` lost every section after `checks:`; the next start failed with "stored configuration is corrupt".

**Root cause.** `duration.go` multiplied the `d`/`w` number as a float and added components without an overflow check, so `time.Duration(n*mult)` wrapped negative on amd64. `Validate` never looked at a debounce. `Clone` was a YAML marshal/unmarshal with both errors ignored: `Duration.String()` of a negative value writes `-1d`, which `ParseDuration` refuses, so decoding stopped there and the rest of the struct stayed at its zero values (then "defaults"). `settings.Load` hit the same parse error on the stored JSON.

**What changed.**
- `internal/config/duration.go`: `MaxDuration` (10 years = 3650d). `ParseDuration` rejects a component or a total above it with `duration "…" is longer than 10 years`, and int64 overflow in Go units stays an "invalid duration" error.
- `internal/config/config.go`: `setDur` fills only a zero value (negatives now reach `Validate`, which rejects them through the ranges of item 2). `Clone` is a typed deep copy (servers, checks with their pointer fields, type tokens, ignore dirs, placement maps, shares) followed by `ApplyDefaults`; it cannot fail, so there is no error to propagate and no section can be dropped. Nil slices stay nil, so an export is unchanged.
- Tests: `TestDurationOverflowAndLimit`, `TestNegativeDurationsRejected`, `TestCloneIsDeepAndComplete`.

**User-visible.** `1000000w`, `999999999d`, `3651d`, `2562047h2562047h` are errors in the YAML import and in every settings form ("Not saved: …"). The largest accepted value is `3650d`. A negative nanosecond number in a hand-edited stored JSON makes the start fail with a range error instead of silently becoming the default.

**README.** Configuration: say that durations take Go units plus `d` and `w`, and that 10 years is the maximum.

## 2. Bounds (map 06 hunch 11)

**Hunch.** "Missing bounds allow pathological settings" (`request_timeout: 1ns`, `down_after_failures: 1000`, `baseline_interval: 1m`, unbounded depth, weekly top, session TTL, thresholds).

**What changed.** `Validate` checks every duration and count with a message of the form `<field> must be between <lo> and <hi>`:

| Field | Range |
|---|---|
| `collector.request_timeout` | 1s-10m |
| `collector.down_after_failures` | 1-20 |
| `collector.concurrency` | 1-16 |
| `collector.event_timeout` | 5s-10m (existed) |
| `collector.full_check_interval` | 1m-7d (was: at least 5m) |
| `collector.poll_interval` | 1m-1d (24h) |
| `collector.structure_interval` | 1m-7d |
| `collector.baseline_interval` | 1h-90d |
| `structure.nested_scan_depth` | 1-10 |
| `notifications.weekly_top` | 1-50 |
| `access.session_ttl` | 1h-365d |
| `checks.<ID>.debounce` | 0-30d |
| `thresholds.*` (all eight) | 10s-3650d (see DECISION) |

Zero keeps its meaning "use the default" for the non-pointer fields (`ApplyDefaults` fills it), so `Validate` passes a zero whether or not defaults were applied first; the web test harness validates before applying defaults and the Structure form sends no `nested_scan_depth`. Test: `TestValidateBounds`.

**User-visible.** Out-of-range values are refused on import and in the settings forms with the field name and range. A stored configuration from before this change that is outside a range makes the next start fail with `stored configuration is invalid: <field> must be between …`; `syncwatch import-config` with a corrected file repairs it. The lower bound of `full_check_interval` dropped from 5m to 1m.

**DECISION.** Thresholds are bounded 10s-10y, not 1m-10y as requested. The repo's own integration and compose suites start syncwatch with `thresholds: {paused: 20s, pc_offline: 40s}` (`test/harness/syncwatch.go:74-75`, `test/compose/docker-compose.yml:98-99`) and the integration test waits 90 s for H6 (`test/integration/integration_test.go:304`); a 1m floor would stop syncwatch at startup in CI, and retuning those waits is timing work in fixer J's suites that I may not run. 10s still rejects the pathological (`1ns`, `1ms`, `1s`), and the engine evaluates every 30 s anyway. To tighten to 1m later: change `minThreshold` in `config.go`, set both suites to `paused: 1m`, `pc_offline: 1m`, and raise the integration H6 wait (90 s is too short for 1m + the 30 s evaluation tick).

**README.** Configuration: add the ranges to the commented YAML reference; mention "0 = default" for durations and counts.

## 3. Clock parsing (map 06 hunch 10)

**Hunch.** "Clock parsing accepts garbage": `8:00pm` meant 08:00, `08:00junk` was accepted, `22:00-22:00` passed validation but the notifier treats it as off, `""` means the default although the struct comments said off.

**Root cause.** `ParseClock` used `fmt.Sscanf("%d:%d")`, which ignores trailing text and accepts signs and single-digit minutes. `ParseRange` never compared the ends. Only the literal `off` disabled a feature.

**What changed.** `ParseClock` matches `^([01]?[0-9]|2[0-3]):([0-5][0-9])$` after trimming (leading zero optional, minutes always two digits). `ParseRange` rejects equal ends with `start and end are the same, use off to disable`. `ApplyDefaults` canonicalises `quiet_hours`, `reminder_time` and `weekly_day` that equal `off` ignoring case and surrounding spaces to `off`, which is what the notifier compares against. The struct comments now say `"" = default`. Test: `TestParseClockStrict`.

**User-visible.** `8:00pm`, `08:00junk`, `8:5`, `+8:00`, `24:00` are errors; `22:00-22:00` is an error; `OFF`, `Off` work in YAML and in imports. An empty value in YAML still means the default (`22:00-07:00`, `08:00`, `monday`); the notification form already turns an empty field into `off`.

**Note for the g2 hunk / web.** `Validate` (lines 335-352, g2's hunk) still compares with `!= "off"`; it works because `ApplyDefaults` runs first in every persisted path. `handleNotifySave` validates a clone before defaults, so `OFF` typed in the form is refused there until that hunk uses `strings.EqualFold` or the handler applies defaults first.

**README.** Notifications and the Configuration reference: `"" = default`, `off` in any case, strict HH:MM, ends must differ.

## 4. Server ids and names (map 06 hunches 8 and 9)

**Hunch.** Ids were not validated (`a,b` breaks the stored observation list, `<b>x</b>` accepted), the duplicate check was case-sensitive while `ServerByKey`/`SharesFor` match ids and names ignoring case, non-ASCII names all slug to `server`, and hyphenated ids break the Home Assistant example's dotted Jinja access.

**What changed.**
- Ids must match `[a-z0-9][a-z0-9-]*`, at most 40 characters (`serverIDRe`); error `server id "…" must be 1-40 characters of a-z, 0-9 and -, starting with a letter or digit`.
- Ids and names are unique ignoring case, and a name must not be another server's id (one lookup key maps to one server): `server <id>: id or name "…" is already used by server <other> (ids and names are compared ignoring case)`.
- `Slug` caps at 40 characters. A generated id (no explicit `id`) that is empty or already taken becomes `<slug>-2`, `<slug>-3`, … (`uniqueSlug`); explicit ids are reserved first so a generated one never collides. The web "add server" form already did the same with its own loop.
- `examples/home-assistant.yaml`: `value_json.servers['akka']` and `$.servers['akka']` everywhere, with a comment explaining why.
- Test: `TestServerIDs`.

**User-visible.** Explicit ids such as `AKKA` or `a,b` are refused (a stored configuration with one fails to start with the message above; the UI never generated such ids). Two servers with the same name (ignoring case) are refused. `ÅÄÖ` and `ÖÄÅ` get `server` and `server-2` instead of "duplicate server id". Non-ASCII slugs are still lossy (`SÖDER` → `s-der`); only the fallback was added.

**README.** Configuration (servers: id charset and uniqueness, generated ids), Home Assistant (bracket access, ids with hyphens).

## 5. `ignore_dirs`: omitted vs empty (map 06 hunch 12, map 05 hunch 6)

**Finding.** `config.go` already distinguishes nil from empty: yaml.v3 decodes an omitted key as nil and `[]` as an empty non-nil slice, `ApplyDefaults` fills only nil, and the stored JSON keeps the difference (`null` vs `[]`). `TestIgnoreDirsNilVersusEmpty` passes on `78ef2d8` (the evidence file shows it), so this is not a config defect. The defect is `internal/web/settings.go:454-456`: the Structure form replaced nil with `[]`, so saving rules without an `ignore_dirs` line dropped `.*`, `@*`, `#recycle`, `#snapshot`, `$RECYCLE.BIN`, `System Volume Information` and `lost+found`, while the same YAML through import kept them (S4/S7 noise from `@Recycle`, `.stversions`).

**What changed.** The three lines were deleted; `SaveConfig` applies defaults, so an omitted key gives the defaults and `ignore_dirs: []` still means none. New `internal/web/structure_form_test.go` (`TestStructureFormKeepsDefaultIgnoreDirs`, fails on the base) and `TestIgnoreDirsNilVersusEmpty` in config.

**User-visible.** Deleting the `ignore_dirs` line in the Structure form restores the defaults instead of ignoring nothing.

**README.** Shares and structure rules: omitted = defaults, `[]` = none.

## 6. Placement attributes and `case_insensitive` (map 06 hunch 13)

**Hunch.** A typo such as `{ ME: true }` against group `me` compiled fine and then every matching project failed placement (S5 flood); `case_insensitive` did not apply to `share_type_from_path`.

**What changed.** `structure.go`: `Compile` collects the named groups of `share_type_from_path` and refuses a `share_must_have` attribute that is not one of them: `structure.placement[0]: share_must_have attribute "ME" is not a named group of share_type_from_path (have: me, type)`. Placement rules without `share_type_from_path` are refused the same way (`have: none, share_type_from_path is not set`), since no attribute could ever be satisfied. `case_insensitive` now adds `(?i)` to `share_type_from_path` as well. Test: `TestPlacementAttributesMatchGroups`.

**User-visible.** Such configurations are refused on import, in the Structure form, and at startup if stored. With `case_insensitive: true`, share directories in another case now match the share pattern (that is what the flag promises; users with the flag on and mixed-case share names will see those shares recognised).

**README.** Shares and structure rules: attribute names must be groups of `share_type_from_path`; `case_insensitive` covers all three patterns.

## 7. Discord role id and webhook canonicalisation (map 06 §3.5, hunch 8 follow-up)

**Root cause.** `discord_role_id` only had to be digits, so a truncated paste (`123`) produced urgent pings that mention nobody (Discord ignores an unknown role). The webhook check accepted any path segment and the raw string was stored, so a leading space failed with an unhelpful parse error and a trailing zero-width character (common when copying from chat clients) was stored and answered 404 later.

**What changed.**
- `roleIDRe` `^\d{17,20}$`; message `notifications.discord_role_id must be the numeric role ID (17-20 digits)`.
- `CanonicalDiscordWebhook(raw) (string, error)`: trims whitespace and Unicode format characters (`Cf`: U+200B, U+FEFF, U+2060 …) at both ends, lower-cases the host, requires `https`, no credentials, no port, a Discord host, and the path `/api/(v<n>/)?webhooks/<digits>/<token>` with `<token>` in `[A-Za-z0-9_-]`; returns the URL without a trailing slash or fragment (the query, e.g. `thread_id`, is kept). `CheckDiscordWebhook` wraps it, so callers are unchanged. With `SYNCWATCH_ALLOW_ANY_WEBHOOK` any http(s) URL without credentials is accepted, trimmed.
- `ApplyDefaults` stores the canonical form on import; `handleNotifySave` (2 lines) stores the canonical form from the form.
- Tests: `TestDiscordRoleID`, `TestDiscordWebhookCanonical`. Fixtures with short role ids were changed (see the table above).

**DECISION.** The `ptb.`/`canary.` subdomains and the `/api/v<n>/webhooks/` form stay accepted: Discord itself issues them, and refusing a working webhook would mean missed alerts. Hosts are still exactly `discord.com` and `discordapp.com` plus those subdomains.

**User-visible.** Short or long role ids are refused. A pasted webhook with surrounding junk now works; one with a non-numeric id or junk inside the token is refused with the existing message.

**README.** Notifications (role id: 17-20 digits, how to copy it), Security (webhook rule, canonical form).

## 8. Server URL host (map 06 §3.2)

**What changed.** `hostOK`: a URL must have a non-empty `Hostname()` (so `https://:8384` is refused; `url.Parse` accepts it) and no whitespace in the host (`url.Parse` refuses ASCII spaces but accepts U+00A0 and other Unicode spaces). Applied to the server `url` and, through the shared `webURL` helper, to `gui_url` and `public_url`. Test: `TestServerURLHost`.

**DECISION.** The same check covers `gui_url` and `public_url`: they go through the same helper and an empty host there is equally broken (links, TLS names).

**README.** None beyond the Configuration reference.

## Not fixed / left for others

- Thresholds floor is 10s instead of 1m (DECISION in item 2).
- `Duration.UnmarshalJSON` still accepts a negative or huge nanosecond number; `Validate` rejects it afterwards, which is where every load path checks. Kept minimal.
- The notification form's own `weekly_top` cap (25) and the HTML limits (`weekly_top` 25, `down_after_failures` 10) are narrower than validation; web owner.
- `OFF` typed in the notification form (item 3 note); g2 hunk / web owner.
- Non-ASCII server names still slug lossily; only a numbered fallback was added.
