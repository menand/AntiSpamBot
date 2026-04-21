# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
go mod tidy
go run ./cmd/bot             # needs BOT_TOKEN; auto-creates ./bot.db
go test -race ./...
make build | run | test | vet | docker-up | docker-down | docker-logs

docker compose up -d --build
docker compose logs -f
```

Config is env-only — no config file. Required: `BOT_TOKEN`. Optional: `CAPTCHA_TIMEOUT_SECONDS` (30), `MAX_ATTEMPTS` (3), `NEWCOMER_DAYS` (7), `LOG_LEVEL` (info), `DB_PATH` (bot.db / /data/bot.db in Docker), `ALLOWED_CHATS` (none = all chats).

## Architecture

Small Telegram anti-spam bot. Everything is driven by three update types: `chat_member` (new-joins), `callback_query` (captcha answers), `message` (stats + `/stats`/`/start`/`/help` commands).

### Update pipeline (`internal/bot/bot.go`)

Long-polling via telego → `th.BotHandler`:
- `th.AnyChatMember()` → `handleChatMember` — captcha kickoff
- `th.CallbackDataPrefix("cap:")` → `handleCallback` — captcha answer
- `th.CommandEqual("stats")` → `handleStatsCommand`
- `th.CommandEqual("start"/"help")` → `handlePrivateStart` (filters to private chats)
- fallback `HandleMessage` → `handleGroupMessage` — counts messages for stats

Handler registration order matters — telego runs the first matching predicate and stops.

### Captcha lifecycle (`internal/bot/handlers.go`)

1. `handleChatMember` detects `left|kicked → member|restricted`, filters bots/self/channels/disallowed-chats, records a `join` event, calls `startCaptcha`.
2. `startCaptcha`: restricts user (all-false `ChatPermissions`), sends inline keyboard with 6 shuffled color emoji, stores `*Pending` in `captcha.Store` (in-memory, keyed `chatID:userID`), **also persists to `pending_captchas` table**, spawns `waitTimeout` goroutine.
3. Race resolution: timeout goroutine and `handleCallback` both call `store.Take()` — whichever wins wraps up; loser is a no-op. `Pending.Cancel()` uses `sync.Once` so the loser exits cleanly.
4. Success: `ResetAttempts`, `UpsertMember(joined_at=now)`, record `pass` event, delete captcha message, lift restrictions (`release`).
5. Fail: `IncrementAttempt` (TTL 24h in SQL), record `kick` or `ban` event, delete message, kick if `count < MaxAttempts` else permanent ban.

### Persistence (`internal/storage/`)

Single SQLite file, pure Go driver (`modernc.org/sqlite`, no CGO). `SetMaxOpenConns(1)` to serialize writes and avoid "database is locked" churn. Schema in `schema.sql`, embedded via `//go:embed`, applied on every Open (idempotent `CREATE TABLE IF NOT EXISTS`).

Tables:
- `pending_captchas(chat_id, user_id, message_id, correct_idx, expires_at)` — active captchas. Written on `startCaptcha`, deleted on take/timeout.
- `attempts(chat_id, user_id, count, updated_at)` — failure counter with 24h TTL. Increment uses a transaction that resets `count=1` if `now - updated_at > ttl`.
- `events(id, chat_id, user_id, kind, at)` — append-only event log, `kind ∈ {join,pass,kick,ban}`.
- `members(chat_id, user_id, joined_at)` — upserted on captcha pass. `joined_at` drives newcomer/oldtimer classification.
- `message_counts(chat_id, day, newcomer_count, oldtimer_count)` — daily chat aggregates (no user_id), UPSERT per message.
- `user_activity(chat_id, user_id, first_message_at, last_message_at, message_count)` — per-user cumulative state, drives silence detection and cumulative counts.
- `user_message_counts(chat_id, user_id, day, count)` — per-user per-day counts, drives top-writers over a time window.
- `user_info(user_id, first_name, last_name, username, updated_at)` — cached display names so `/stats` doesn't hit Telegram for every row.

### Per-user message handling (`handleGroupMessage`)

On every non-service group message from a non-bot:
1. `RememberUser` — upsert name/username cache (eventually consistent, outside the main tx).
2. `IncMessage` — aggregate newcomer/oldtimer counter (uses `isNewcomer` = join within `NewcomerDays`).
3. `RecordMessage` — transactional upsert of `user_activity` + `user_message_counts`, returns `MessageRecord{Silence, HasBaseline, WasFirstMessage}`.
4. `maybeAnnounceReturn` — if `HasBaseline && Silence >= SilentAnnounceDays*24h`, posts a tiered announcement ("день/месяц/год" via `humanDaysRU` + `pluralRU`). `SilentAnnounceDays=0` disables.

Baseline selection for silence:
- Prior `last_message_at` → silence since last message.
- Else `members.joined_at` → silence since join; `WasFirstMessage=true`.
- Else no baseline (pre-existing member, first sighting) → no announcement.

### Restart behavior

On `Bot.Run` startup:
1. Open DB (applies schema).
2. `restorePending(ctx)` loads all rows from `pending_captchas`, re-puts each into the in-memory store with its original `expires_at`, spawns `waitTimeout` for each. Already-expired rows get a 1-second grace timer that fires immediately → auto-kick.
3. Background `attemptsSweepLoop` goroutine deletes expired attempt counters every `attemptsTTL/2`.

This means mid-captcha users survive restarts — they see the same captcha keyboard, and if they answer within their original window it still works. If the bot was down past their deadline, they get kicked on the next tick.

### Stats (`internal/bot/stats.go`)

`/stats [period]` with period ∈ day|week|month|all (Russian aliases supported). Admin-gated via `getChatMember` — only `creator` or `administrator` sees output. Non-admins get a polite rejection reply.

`QueryStats` does two queries:
- `events` grouped by `kind` for join/pass/kick/ban counts
- `message_counts` summed over the day range for newcomer/oldtimer sums

Newcomer classification happens at message-ingestion time (`isNewcomer`): look up `members.joined_at`, if within `NewcomerDays * 24h` → newcomer, else (or if not in `members` — pre-existing member) → oldtimer.

### Context discipline

- `Bot.runCtx` is the root ctx from main, saved in `Bot.Run`.
- Handlers use their `*th.Context` for short-lived replies (AnswerCallbackQuery, SendMessage responses).
- Long-lived/async work uses `b.runCtx` (captcha setup, attempts sweeper, background message counting).
- `waitTimeout` cleanup uses a detached `context.WithTimeout(context.Background(), 10s)` — so a timeout landing at shutdown still kicks/bans/deletes instead of failing on cancelled ctx.

### Kick vs ban semantics (`actions.go`)

Telegram Bot API has no native "kick". `kick()` = `BanChatMember` then `UnbanChatMember(OnlyIfBanned:true)`, with up to 3 retries on the unban so transient errors don't turn kicks into permabans. `ban()` omits the unban → permanent.

`release()` sets explicit permission pointers (Bot API 7.x field set). If Telegram adds new permission fields, restricted users won't auto-regain them — may need to switch to reading chat defaults via `getChat`.

## When making changes

- New update types → add to `AllowedUpdates` in `Bot.Run`, otherwise Telegram doesn't deliver them.
- Callback data format is `cap:<userID>:<optIdx>` — update both sides (`startCaptcha` formatter and `parseCallback` parser).
- Schema changes: append to `schema.sql` with `CREATE TABLE IF NOT EXISTS` or `ALTER TABLE ... ADD COLUMN`. No migration framework — keep changes idempotent.
- New event kinds: add to `EventKind` const block + wire into `QueryStats` case switch.
- Admin-gated commands: reuse `isChatAdmin(ctx, chatID, userID)`.
- Stats privacy: `message_counts` is aggregate-only by design (no per-user rows). Don't add per-user message tables without a privacy review.
