# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
go mod tidy
go run ./cmd/bot             # needs BOT_TOKEN; auto-creates ./bot.db
go test -race ./...
gofmt -l .                   # must be empty before committing
make build | run | test | vet | docker-up | docker-down | docker-logs

docker compose up -d --build
docker compose logs -f
```

Config is env-only — no config file. Required: `BOT_TOKEN`. Optional: `CAPTCHA_TIMEOUT_SECONDS` (30), `MAX_ATTEMPTS` (3), `NEWCOMER_DAYS` (7), `SILENT_ANNOUNCE_DAYS` (30, 0=off), `LOG_LEVEL` (info), `DB_PATH` (bot.db / /data/bot.db in Docker), `ALLOWED_CHATS` (none = all chats), `OWNER_IDS`, `LOG_FILE` (set in Docker to /data/bot.log), `CAPTCHA_DELAY_MS` (2000), `DAILY_STATS_UTC_HOUR` (6).

## Architecture

Telegram anti-spam bot. Driven by four update types: `chat_member` (joins/leaves), `my_chat_member` (bot's own membership), `callback_query` (captcha answers + DM menu), `message` (stats, service messages, DM commands).

### Update pipeline (`internal/bot/bot.go`)

Long-polling via telego → `th.BotHandler`:
- `th.AnyChatMember()` → `handleChatMember` — captcha kickoff; also cancels the captcha quietly (no kick event) when the user leaves mid-captcha
- `th.AnyMyChatMember()` → `handleMyChatMember` — on bot leave: drop chat from registry + cancel/delete its pending captchas; on join/promotion: register chat + post a missing-rights hint (`checkAdminRights`)
- `cap:` callbacks → `handleCallback` — captcha answer
- `capok:` callbacks → `handleApproveCallback` — admin "✅ Впустить" button (manual approve, same path as success)
- `menu:` callbacks → `handleMenuCallback` — DM menu (stats, settings)
- `/stats`, `/greeting` — registered as no-op handlers so the commands are swallowed silently in groups (all management is via the DM menu)
- `/chats`, `/logs`, `/info`, `/start`, `/help` — DM commands
- `privateMessagePredicate` → `handlePrivateText` — greeting-text input flow (see below)
- fallback `HandleMessage` → `handleGroupMessage` — service messages + message counting

Handler registration order matters — telego runs the first matching predicate and stops. `handlePrivateText` must stay after the command handlers and before `handleGroupMessage`.

### Captcha lifecycle (`internal/bot/handlers.go`)

1. `handleChatMember` detects `left|kicked → member|restricted`, filters bots/self/channels/disallowed-chats, records a `join` event, calls `startCaptcha`. `message.new_chat_members` is the fallback path (some group types) — it also carries the forum `threadID` (`IsTopicMessage`), so forum captchas land in the right topic.
2. `startCaptcha` → `BeginKickoff` dedup lock → async `runCaptcha`: **restrict first** (immediately — every pre-restrict second is a spam window; the restriction is invisible to the user), then sleep `CaptchaDelay` (client render time), then send the keyboard (6 emoji + admin approve row), `store.Put` (in-memory, keyed `chatID:userID`), persist to `pending_captchas` (incl. `thread_id`), spawn `waitTimeout`.
3. Race resolution: timeout goroutine, `handleCallback`, `handleApproveCallback`, and the user-left branch all call `store.Take()` — whoever wins wraps up; losers are no-ops. `Pending.Cancel()` uses `sync.Once`.
4. Success (correct answer or admin approve): `ResetAttempts`, `UpsertMember(joined_at=now)`, record `pass` event, delete captcha message, `release` — which applies the **chat's default permissions from getChat** (not blanket all-true; fallback to all-true only if getChat fails), then greeting (custom template or default, sent to the join topic).
5. Fail: `IncrementAttempt` (TTL 24h), record `kick` or `ban` event, delete message, kick if `count < MaxAttempts` else permanent ban.
6. User left mid-captcha: captcha cancelled + message deleted, **no kick event** (stats stay honest); our own post-fail kick is unaffected because `onFail` Takes the pending before kicking.

### Flood control (`internal/bot/actions.go`)

`restrict` retries with backoff and honors Telegram's `retry_after` on 429 (mass-join scenario). `kick` = ban + retried unban (also 429-aware) so transient errors don't turn kicks into permabans. `ban` omits the unban.

### Persistence (`internal/storage/`)

Single SQLite file, pure Go driver (`modernc.org/sqlite`, no CGO). `SetMaxOpenConns(1)`. Schema in `schema.sql` (embedded, idempotent) + additive `ALTER TABLE` migrations in `db.go` that ignore "duplicate column name". **Every new chat_settings/pending_captchas column must be added in BOTH places, and to the `MigrateChat` INSERT — the migrate test checks all settings columns survive.**

Tables:
- `pending_captchas(chat_id, user_id, message_id, correct_idx, expires_at, thread_id)` — active captchas. Deleted on take/timeout/user-left/bot-left.
- `attempts(chat_id, user_id, count, updated_at)` — failure counter, 24h TTL, swept by `attemptsSweepLoop`.
- `events(id, chat_id, user_id, kind, at)` — append-only, `kind ∈ {join,pass,kick,ban}`.
- `members(chat_id, user_id, joined_at)` — upserted on captcha pass; drives newcomer classification + silence baseline.
- `message_counts(chat_id, day, newcomer_count, oldtimer_count)` — daily aggregates.
- `user_activity`, `user_message_counts` — silence detection / top writers.
- `user_info(user_id, …)` — display-name cache for mentions.
- `chats(chat_id, title, type)` — registry for the DM menu; row removed when the bot leaves.
- `chat_settings(chat_id, greeting_enabled, max_attempts, captcha_timeout_seconds, daily_stats_enabled, daily_stats_utc_hour, last_daily_stats_day, captcha_mode, greeting_text)` — per-chat overrides; NULL = global default. Resolved via `effective*` helpers in `access.go`.

Write-through caches (`internal/bot/cache.go`): `rememberChat`/`rememberUser` skip the DB write when the cached value is unchanged — use these instead of calling `db.RememberChat`/`db.RememberUser` directly from hot paths.

### Stats semantics (IMPORTANT)

Events are counted by unix time (`at >= from AND at < until`), messages by calendar day (`day >= fromDay AND day < untilDay`) — **both upper bounds exclusive**. `statsRange` returns calendar-aligned UTC windows (day = today since 00:00 UTC, week = 7 calendar days, month = 30) precisely so both counts cover the same range. The daily digest passes [yesterday 00:00, today 00:00) and labels it «вчера» via `renderStats`'s label parameter. Don't reintroduce rolling `now-24h` windows — they desync event vs message counts.

### DM menu (`internal/bot/menu.go`)

Callback formats: `menu:main|help|add|chats|logs`, `menu:stats:<chat>:<period>`, `menu:settings:<chat>`, toggles `menu:gr|daily:<chat>`, presets `menu:max|tmo|hour:<chat>:<val>`, `menu:cmode:<chat>:<mode>`, `menu:grtxt:<chat>` (arms greeting-text input). Access via `canManageChat` (bot owner or chat admin via getChatMember).

Greeting-text input flow: `menu:grtxt` stores `userID→chatID` in `Bot.greetInput`; the next private message (handled by `handlePrivateText`) becomes the template. `-` resets to default, any `/command` aborts, 500-rune cap. Templates are HTML-escaped at render time; `{name}` is replaced with the mention markup after escaping (`renderGreeting`).

Button labels: always truncate with `truncateLabel` (rune-safe) — byte slicing corrupts Cyrillic titles and Telegram rejects the whole keyboard on invalid UTF-8.

### Daily digest (`internal/bot/daily.go`)

`dailyDigestLoop` ticks every 5 min; `ChatsNeedingDailyStats` gates on per-chat hour (override or `DAILY_STATS_UTC_HOUR`) + `last_daily_stats_day`. Empty digests are skipped but still marked sent. Hour presets in the menu are stored UTC, displayed MSK (UTC+3 hardcoded in `mskHourLabel`).

### Restart behavior

`restorePending` reloads `pending_captchas` (incl. thread_id) into the store with original deadlines; already-expired rows get a 1-second grace timer → auto-kick. Mid-captcha users survive restarts.

### Context discipline

- `Bot.runCtx` — root ctx; long-lived/async work (captcha flow, sweeper, digests, DB writes from callbacks).
- Handler `*th.Context` — short-lived replies only (AnswerCallbackQuery, menu edits).
- `waitTimeout` cleanup uses a detached 10s `context.Background()` so shutdown doesn't drop kicks/bans.

## When making changes

- New update types → add to `AllowedUpdates` in `Bot.Run`, otherwise Telegram doesn't deliver them.
- Callback data formats: captcha `cap:<userID>:<optIdx>`, approve `capok:<userID>`, menu `menu:...` — update both formatter and parser sides. Note `th.CallbackDataPrefix("cap:")` does NOT match `capok:` (prefix includes the colon).
- Schema changes: `CREATE TABLE IF NOT EXISTS` / `ALTER TABLE ... ADD COLUMN` in **both** `schema.sql` and `db.go` migrations, **plus** `MigrateChat` if the table is per-chat keyed.
- New event kinds: `EventKind` const block + `QueryStats` switch.
- Admin-gated UI: reuse `canManageChat` (covers owner + chat admin).
- Stats privacy: `message_counts` is aggregate-only by design. Per-user tables (`user_activity`, `user_message_counts`) exist for tops/silence — keep new per-user data behind the same "admins only" access.
- Messages to forum supergroups: thread the `threadID` through (see `onUserJoined` → `runCaptcha` → `Pending.ThreadID` → greeting) or the message lands in General.
- `gofmt -w .` before committing — CI/tests assume formatted code.
