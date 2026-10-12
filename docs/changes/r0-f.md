# r0-f: Discord delivery, redaction and message safety

Fixer f, branch `sim/r0-f`, base `78ef2d8`. Map: `04-notify.md`.

Files changed, all inside `internal/notify`: `discord.go`, `message.go`, `notifier.go`, `notifier_test.go`, and the new `discord_test.go`. Nothing outside the package was touched. The map experiments that fit (redaction, payload limits, the blocking notice, the undecryptable webhook, the long role ID) were turned into the unit tests named below.

Checks before each commit: `gofmt -l .` empty, `go vet ./...`, `go test ./...` (Windows), `GOOS=linux go build ./...` and `GOOS=linux go vet ./internal/notify/`. The integration, compose and loadsim suites were not run (rule 3); their Discord assertions were read instead: they match on `Mentions(role)`, `Embeds[0].URL` and plain words such as `Resolved`, `SOL`, `LIVE-BETA`, `certificate`, none of which the new escaping changes.

## 1. The webhook token can leak in error text (map hunch 1) — security

**Root cause.** `redactURL` replaced only the exact stored string. net/http prints the parsed URL (`u.String()`): lower-case scheme, percent-encoded path. A webhook saved as `HTTPS://…`, with a trailing U+200B or with a space in the token passed validation and leaked the token into the Warn log, the notification log (Settings), `Delivery.Error` (banner, `/api/v1/status`) and the test flash.

**What changed** (`discord.go`): `redactURL` now also replaces the parsed form (`url.Parse(u).String()`), the `<id>/<token>` path segments in their raw and path-escaped spellings, and finally anything shaped like `/api/(vN/)webhooks/<id>/<token>` (regexp `webhookPath`), with `<webhook>`. The errors syncwatch builds itself (`invalid webhook URL`, the new send-time refusal of item 5) never include the URL.

**Test.** `TestRedactsWebhookTokenInAnySpelling` (a failing dialer, six spellings, plus error strings that spell the path differently).

**User-visible.** Transport errors read `discord: Post "<webhook>": dial tcp …` for every spelling; an error that mentions another webhook path shows `/api/webhooks/<webhook>`.

**README.** Security → Secrets ("never appear in logs …"): now true for the webhook in error text as well; nothing else to say.

## 2. One undeliverable settings notice blocks every notification for 24 h (hunch 2)

**Root cause.** Three things: `Payload` never capped `Summary` (a notice summary with a 5000-digit role ID gave a 5033-byte description, Discord's limit is 4096); `retryNotices` retried every held-back notice first and the tick returned at the first error, so a permanently rejected notice held back urgent pings until it expired after a day; and nothing told a permanent rejection (HTTP 400) from an outage.

**What changed.**
- `discord.go` `Payload`: the summary is cut to the description budget with the new `clipBytes` (byte budget, cut at a rune boundary, "…" appended); the 6000 total check uses it too. Description ≤ 4096 bytes and total ≤ 6000 bytes for any input (`TestPayloadStaysWithinDiscordLimits`: 5000- and 7000-character summaries, multibyte text, a 1000-character title, a 5000-character line, the role-ID notice).
- `discord.go`: non-2xx replies are returned as `*httpError{code, text}` (same text as before: `discord: HTTP 404: Unknown Webhook`), and `rejected(err)` says whether a reply is a 4xx other than 429.
- `notifier.go` `retryNotices`: a retry that is rejected drops the notice with one Warn (`dropping a settings notice the channel rejected`); any other failure stops the notice retries for this tick (one failed attempt per tick) and is returned.
- `notifier.go` `Tick`: the notice error no longer aborts the tick. The overnight, urgent, resolved, reminder and weekly stages run as before; the notice failure is passed to `fail` (back-off) only after them, and only if no later stage failed itself.

**Tests.** `TestRejectedNoticeIsDropped` (HTTP 400 notice: urgent ping goes out in the same tick, queue empty, no banner, exactly two warnings, no further attempts in the next hour). `TestFailingNoticeDoesNotHoldUpUrgent` (HTTP 502: two notices stay queued, one retry per tick, the urgent ping still goes out, the back-off still applies, both notices go out once the channel takes them).

**User-visible.** Urgent pings are no longer held up by a notice Discord won't take. A notice Discord rejects (400, 401, 403, 404) is logged once and dropped instead of being retried for a day. A notice that fails for other reasons costs each tick one attempt (up to 20 s) before the urgent messages; if the urgent message then succeeds, the banner is clear (the notification log still shows the notice failure) and the notice is retried after the usual back-off.

**README.** Notifications → Settings notice ("If it can't be sent, it is retried with the next notifications for up to a day"): add that a notice Discord rejects is dropped, and that held-back notices never block urgent pings.

DECISION: a notice's *first* attempt (to the channels captured before the change) is still queued on any failure, 4xx included, because the retry goes to the channels configured now: a dead old webhook answering 404 must not stop the "webhook replaced" notice from reaching the new one (`TestFailedNoticeIsRetried`). Only the retry drops on 4xx.
DECISION: the tick keeps "notices first" ordering; a notice failure is counted after the other stages instead of before them, which is the least change that stops the blocking.

## 3. An undecryptable webhook gives no sender, no banner and no error (hunch 6)

**Root cause.** `defaultSenders` logged `reading Discord webhook` and returned no senders, which the notifier treats as "no channel configured": nothing marked sent, no banner, no back-off, `SendTest` answered `no notification channel is configured` while Settings said the webhook is set.

**What changed** (`notifier.go`): on a read error `defaultSenders` returns a `brokenChannel` named `discord` whose every send fails with `the Discord webhook can't be decrypted (secret key changed?)`. From there the usual paths apply: notification log row, `Delivery.Error` and the banner, back-off, and the test button's flash `Test failed: the Discord webhook can't be decrypted (secret key changed?)`. The underlying store error is still logged at Error level.

**Test.** `TestUndecryptableWebhookIsReported` (store written with one key, reopened with another).

**User-visible.** The banner and the test button say why nothing arrives; once the webhook is saved again (re-encrypted with the current key) `ChannelsChanged` retries at once and the due findings go out.

**README.** Security → Secrets: a changed `SYNCWATCH_SECRET_KEY` or lost `secret.key` now shows as "the Discord webhook can't be decrypted" in the banner, the status API and the test button; the fix is to save the webhook again.

DECISION: the text is the same for any error from reading the secret (the only realistic one is a decryption failure; a database error at shutdown would show the same words). The banner appears at the first send attempt (a due message or the test button), not at start-up, like every other delivery failure.

## 4. Escaping is incomplete (hunch 11)

**Root cause.** Embed titles and summaries were raw; `escapeMD` left bare URLs (autolinked by Discord), line breaks and line-start list markers alone, and passed bidi controls through.

**What changed.**
- `message.go` `escapeMD`: control characters (C0, DEL, C1) and U+2028/U+2029 become spaces (`\r\n` one space); bidi overrides, embeddings and isolates (U+202A–U+202E, U+2066–U+2069) are removed; the markdown replacer is unchanged (`@` still gets a zero-width space); a zero-width space is inserted after the colon of any `scheme://` so Discord doesn't autolink it; a leading `- `, `+ ` or `1. ` list marker is escaped.
- `discord.go` `Payload`: the embed title is `escapeMD(m.Title)` clipped to 256, the same text as in `content`.
- `notifier.go` `WeeklyMessage`: the health line is escaped (server names); the dashboard link is appended after it, so syncwatch's own markdown stays intact. `HealthLine` itself (also used by the dashboard and `/api/v1/status`) is unchanged.
- Lines were already escaped; the details link, bold check IDs, the stale suffix and "(open …)" are added after escaping, as before.

**Tests.** `TestHostileNamesAreNeutralised`: eight hostile names (HTML/JS, every markdown form, @everyone/@here/`<@&123>`/`<@123>`/`<#123>`/`<t:…>`, bare http/https/steam links, zero-width and bidi characters, a 500-character name, emoji with ZWJ and flag sequences, line breaks, list markers and control characters) through every message builder (urgent, resolved, reminder, overnight, weekly incl. the health line) and `Payload`; checks content, embed title and description for unescaped markdown, mentions, `://`, line breaks and bidi controls, limits, and that emoji sequences, `**H9**`, `[details](…)` and `[Open the dashboard](…)` survive. The exact spelling is asserted for one string. `TestDiscordEscapesNames` and `TestWeeklyReport` were updated to the new spelling (zero-width space after `https:`, `\(SOL down\)` in the raw summary).

**User-visible.** Names can no longer link, format, mention, break lines or reorder the surrounding text, in the embed title and the weekly health line too. The raw weekly summary now contains backslashes (`0/2 servers OK \(SOL down\)`), which Discord renders as before. A URL inside a finding message (for example a collector error `Get "https://nas:8384/rest/…"`) is no longer clickable in Discord.

**README.** Notifications: the sentence about names being escaped (if kept) can say "titles, summaries and lines"; Security: nothing from Syncthing can mention, link or format.

DECISION: embed titles get the same escaping as `content`. Discord doesn't render markdown in embed titles, so a name with `_`, `(`, `)`, `[`, `*` or `\` shows the backslashes there (`Photos \(2024\)`); the bold content line right above renders correctly. Chosen as required, for the day Discord changes what titles render.
DECISION: bidi controls are removed from names (a known spoofing vector, e.g. making text read as "resolved"); zero-width space/non-joiner/joiner, BOM and LRM/RLM are kept because emoji sequences need ZWJ and the others are harmless.
DECISION: settings-notice summaries are not escaped. They are composed by the web layer from admin input (server names and URLs) and joined with newlines that must survive; the title is escaped like every title. Noted for fixer g2/the web area if admin text should be escaped too.

## 5. The stored webhook is not re-validated at send time; redirects are followed (hunch 19)

**Root cause.** The host allowlist ran only when the webhook was saved or imported. A URL saved under `SYNCWATCH_ALLOW_ANY_WEBHOOK=true` kept receiving every message after the variable was removed. `Discord.Send` used a default `http.Client`, which follows redirects (re-POSTing the body on 307/308), so a redirecting webhook would hand the message to another host.

**What changed** (`discord.go`): `Send` first calls `usable()`: unless `config.AllowAnyWebhook` is set, the stored URL must parse, use `https` and satisfy `config.DiscordHost`; otherwise it returns `discord: the stored webhook isn't a Discord webhook URL (https://discord.com/api/webhooks/…); save it again` without the URL. The client gets `CheckRedirect` → `http.ErrUseLastResponse` (a caller-supplied `d.HTTP` is copied, not modified), and a 3xx reply is returned as `discord: HTTP 307: the webhook redirects, which isn't followed`.

**Tests.** `TestStoredWebhookIsCheckedAtSendTime` (httptest host refused without the override, accepted with it, no request made when refused), `TestSendDoesNotFollowRedirects` (307 to another path, with and without a caller-supplied client). Existing tests that post to httptest servers call `allowAnyWebhook(t)`, which sets and restores `config.AllowAnyWebhook`.

**User-visible.** After removing `SYNCWATCH_ALLOW_ANY_WEBHOOK`, a non-Discord webhook fails with the error above (banner, back-off, test button) until a Discord webhook is saved. The integration/compose suites set the variable (`test/harness/syncwatch.go`, `docker-compose.yml`), so they are unaffected.

**README.** Notifications (first paragraph: "it can't be pointed at another host"): add "checked again on every send" and that redirects aren't followed; Development/testing where `SYNCWATCH_ALLOW_ANY_WEBHOOK` is described: removing it also stops an already saved non-Discord webhook.

DECISION: the send-time check is `https` + `config.DiscordHost` as specified, not the full `CheckDiscordWebhook` path regexp, so a webhook saved under earlier rules keeps working as long as it is https on a Discord host; fixer g's canonicalisation of the saved URL doesn't interact with it.

## 6. Tick holds the mutex across HTTP; an in-flight failure undoes ChannelsChanged (hunch 20)

**Root cause.** `ChannelsChanged` reset `failures`, `retryAt` and the banner, but a send that was already in flight on the old webhook then failed and set them again (`fail`, `recordDelivery`): the new webhook waited for the back-off and the banner showed the old webhook's error.

**What changed** (`notifier.go`): a generation counter `gen` under `dmu`, bumped by `ChannelsChanged`. A send snapshots it before building its senders (`send`, `SendTest`, `notice`), and `Tick` snapshots it with the back-off check. `recordDelivery(now, gen, err)` ignores a failure from an older generation (a success is always applied); `fail(now, gen, err)` skips the back-off for an older generation. `ChannelsChanged` also clears `lastErr`, so a new webhook that fails with the same text as the old one gets its own notification-log row.

**Test.** `TestStaleFailureDoesNotUndoChannelChange` (a send blocked in flight, `ChannelsChanged` during it, the old send fails: no banner, and the next tick 30 s later sends on the new channel).

**User-visible.** Saving a new webhook (or a successful test) really retries at once, whatever an old send returns afterwards, and the banner never shows a replaced webhook's error.

**Not changed, on purpose.** `n.mu` is still held for the whole tick, HTTP included. Nothing waits on it: the only caller of `Tick` is `Run` (sequential), and `SendTest`, `NoticeLater`, `ChannelsChanged` and `Delivery` use `dmu` only (grep for `n.mu` finds `Tick` alone). The pattern "snapshot under the lock, send outside it, re-lock to apply" is what `sendTo` and `Tick` do with `dmu` and the generation. Making the HTTP calls run outside `n.mu` would let two ticks send the same message and has no caller to benefit. If a later phase adds a caller that must not wait, use `TryLock` there.

**README.** Notifications → "A failing webhook … A test message or a new webhook retries at once": now also true during an in-flight send.

## 7. discordError drops Discord's field-level errors for HTTP 400 (hunch 13)

**What changed** (`discord.go`): the reply's `errors` object is compacted and its first 200 characters are appended after the message: `discord: HTTP 400: Invalid Form Body {"embeds":{"0":{"description":{"_errors":[{"code":"BASE_TYPE_MAX_LENGTH","message":"Must be 4096 or fewer in length."}]}}}}`. Still only for Discord hosts and still clipped. Test `TestDiscordErrorIncludesFieldErrors`.

**User-visible.** The banner, the notification log and the test flash say which field Discord rejected (for example an invalid role in `allowed_mentions.roles`).

**README.** none.

## 8. "…and N more" counts limitLines' own marker (hunch 16, first part)

**Root cause.** When `Payload` ran out of budget inside a section whose lines came from `limitLines`, it counted the remaining lines, including the trailing "…and N more" line, as one each: 17 hidden findings showed as "…and 13 more".

**What changed** (`discord.go`, `message.go`): the marker format is the shared `moreLine`; `hiddenLines` parses a trailing marker and counts its N instead of 1. Test `TestTruncationCountsHiddenLines` (30 lines of 300 bytes → shown + N == 30; an untruncated section keeps limitLines' count).

**User-visible.** The count is right. Sections dropped silently when the header doesn't fit, and the weekly "New this week (n)" counting INFO it doesn't list, are the rest of hunch 16 and were not part of this assignment.

**README.** none.

## Not done / out of scope

- Hunch 12 (the status API shows the notification error to viewers) is in the web area.
- Hunch 16's other parts (silently dropped sections, weekly counts, no overflow marker on the top list) were not in the items.
- Notice summaries (admin text) are not escaped; see the decision under item 4.
