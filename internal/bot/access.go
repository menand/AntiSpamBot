package bot

import (
	"context"
	"time"

	"github.com/menand/AntiSpamBot/internal/captcha"
	"github.com/menand/AntiSpamBot/internal/storage"
)

// userChats returns the subset of known chats that this user is allowed to
// manage: everything for OWNER_IDS, only chats where the user is admin/creator
// for everyone else. Admin lookups go through the 6h cache, so only the first
// menu open costs N API requests for a non-owner with N known chats.
func (b *Bot) userChats(ctx context.Context, userID int64) ([]storage.ChatInfo, error) {
	all, err := b.db.ListChats(ctx)
	if err != nil {
		return nil, err
	}
	if b.isOwner(userID) {
		return all, nil
	}
	out := make([]storage.ChatInfo, 0, len(all))
	for _, c := range all {
		if b.isChatAdminCached(ctx, c.ChatID, userID) {
			out = append(out, c)
		}
	}
	return out, nil
}

// canManageChat reports whether the user may view stats and toggle settings
// for a specific chat: either bot-wide owner, or chat admin/creator. Admin
// status comes from the cache (invalidated on every chat_member event; in
// chats where the bot is not an admin Telegram sends no such events, which is
// why negative results get the short TTL) — the same staleness class already
// accepted for the spam-vote golden voice.
func (b *Bot) canManageChat(ctx context.Context, userID, chatID int64) bool {
	return b.isOwner(userID) || b.isChatAdminCached(ctx, chatID, userID)
}

// chatSettings loads the per-chat settings row for RESOLUTION (captcha
// parameters, greeting, spam thresholds): errors are logged here and the
// returned struct still carries the defaults (GetChatSettings guarantees
// that), so callers can resolve against it unconditionally. Do NOT use it for
// read-modify-write (settings toggles) or for rendering the settings screen —
// those must call GetChatSettings directly and abort on error, otherwise they
// write/show the inverse of a default instead of the stored value.
func (b *Bot) chatSettings(ctx context.Context, chatID int64) storage.ChatSettings {
	s, err := b.db.GetChatSettings(ctx, chatID)
	if err != nil {
		b.log.Warn("get chat settings", "err", err, "chat", chatID)
	}
	return s
}

// effectiveMaxAttempts resolves the max-attempts value: per-chat override if
// set, else the global default from config.
func (b *Bot) effectiveMaxAttempts(s storage.ChatSettings) int {
	if s.MaxAttempts.Valid {
		return int(s.MaxAttempts.Int64)
	}
	return b.cfg.MaxAttempts
}

// effectiveCaptchaTimeout resolves the captcha timeout: per-chat override if
// set, else global default.
func (b *Bot) effectiveCaptchaTimeout(s storage.ChatSettings) time.Duration {
	if s.CaptchaTimeoutSeconds.Valid {
		return time.Duration(s.CaptchaTimeoutSeconds.Int64) * time.Second
	}
	return b.cfg.CaptchaTimeout
}

// effectiveDailyHour resolves the UTC hour of the daily digest: per-chat
// override if set, else global default. (The digest loop itself resolves this
// in SQL via COALESCE — this helper is for Go-side display.)
func (b *Bot) effectiveDailyHour(s storage.ChatSettings) int {
	if s.DailyStatsUTCHour.Valid {
		return int(s.DailyStatsUTCHour.Int64)
	}
	return b.cfg.DailyStatsUTCHour
}

// effectiveCaptchaMode resolves the captcha style. Unknown values stored in
// the DB (future / corrupt) fall back to ModeCircles.
func effectiveCaptchaMode(s storage.ChatSettings) captcha.Mode {
	if !s.CaptchaMode.Valid {
		return captcha.ModeCircles
	}
	switch captcha.Mode(s.CaptchaMode.String) {
	case captcha.ModeEmoji:
		return captcha.ModeEmoji
	case captcha.ModeImage:
		return captcha.ModeImage
	default:
		return captcha.ModeCircles
	}
}
