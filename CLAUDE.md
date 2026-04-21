# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
go mod tidy                  # pulls deps on first build / after import changes
go run ./cmd/bot             # run locally (needs BOT_TOKEN in env)
go build -o bin/bot ./cmd/bot
go test ./...                # no tests yet; add in internal/<pkg>/*_test.go

docker compose up -d --build # run on server
docker compose logs -f
```

Config is env-only — no config file. Required: `BOT_TOKEN`. Optional: `CAPTCHA_TIMEOUT_SECONDS` (default 30), `MAX_ATTEMPTS` (default 3, triggers permanent ban instead of kick on Nth failure).

## Architecture

Small, single-binary Telegram anti-spam bot. Flow is driven by `chat_member` updates — everything else (messages, commands) is ignored.

**Update pipeline** (`internal/bot/bot.go`): long-polling via telego → `th.BotHandler` with two handlers:
- `th.AnyChatMember()` → `handleChatMember` (join detection)
- `th.CallbackDataPrefix("cap:")` → `handleCallback` (captcha answer)

**Captcha lifecycle** (`internal/bot/handlers.go`):
1. `handleChatMember` detects transition `left|kicked → member|restricted`, filters bots and self, calls `startCaptcha`.
2. `startCaptcha` restricts the user (all-false `ChatPermissions`), sends inline keyboard with 6 shuffled color emoji (palette in `internal/captcha/captcha.go`), stores a `*Pending` in `captcha.Store` keyed by `chatID:userID`, spawns `waitTimeout` goroutine.
3. Race resolution: both the timeout goroutine and `handleCallback` call `store.Take()` — whichever wins gets the `*Pending`, the other is a no-op. `Pending.Cancel()` (via `sync.Once`) closes a channel so the loser exits cleanly.
4. On success: reset attempt counter, delete captcha message, lift restrictions (`release`).
5. On fail: increment attempt counter, delete message, kick if `count < MaxAttempts` else permanent ban.

**Kick semantics** (`actions.go`): Telegram Bot API has no "kick" primitive — `kick()` is `BanChatMember` immediately followed by `UnbanChatMember(OnlyIfBanned: true)`. This removes the user but lets them rejoin. `ban()` omits the unban — permanent.

**State**: fully in-memory.
- `captcha.Store` — active captchas, keyed per `(chatID, userID)`. New captcha for the same key cancels the old one.
- `attempts.Tracker` — failure counts per `(chatID, userID)` with a TTL sweeper goroutine (`Run(ctx)`) started from `Bot.Run`. Counter resets on success or TTL expiry.

Consequence: restart wipes everything. Users mid-captcha stay restricted until an admin intervenes. If persistence becomes a requirement, swap both stores for SQLite/Redis-backed implementations behind the same interfaces.

**Context plumbing**: `Bot.runCtx` is the root ctx from `main`, saved in `Bot.Run`. Timer goroutines use `runCtx` (not the `th.Context` from a handler call, which is scoped to a single update and gets cancelled when the handler returns).

**Permission requirement**: the bot must be an admin with "ban users" and "delete messages" in each chat. Without admin status Telegram does not deliver `chat_member` updates at all — the bot is silent, not broken.

## When making changes

- New update types → add to `AllowedUpdates` in `Bot.Run`, otherwise Telegram won't deliver them.
- Changing the callback data format (`cap:<userID>:<optIdx>`) requires updating both the sender in `startCaptcha` and the parser in `handleCallback` — there's no shared constant.
- `release()` currently sets explicit permissions (Bot API 7.x field set). If Telegram adds new permission fields, restricted users won't get them back — consider reading `getChat().Permissions` and restoring those instead.
