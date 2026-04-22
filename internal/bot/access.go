package bot

import (
	"context"
	"time"

	"github.com/menand/AntiSpamBot/internal/captcha"
	"github.com/menand/AntiSpamBot/internal/storage"
)

// userChats returns the subset of known chats that this user is allowed to
// manage: everything for OWNER_IDS, only chats where the user is admin/creator
// for everyone else. Per-chat admin lookup goes through getChatMember, so
// each call costs N API requests for a non-owner with N known chats — fine
// for small deployments.
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
		isAdmin, err := b.isChatAdmin(ctx, c.ChatID, userID)
		if err != nil || !isAdmin {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

// canManageChat reports whether the user may view stats and toggle settings
// for a specific chat: either bot-wide owner, or chat admin/creator.
func (b *Bot) canManageChat(ctx context.Context, userID, chatID int64) bool {
	if b.isOwner(userID) {
		return true
	}
	isAdmin, err := b.isChatAdmin(ctx, chatID, userID)
	return err == nil && isAdmin
}

// effectiveMaxAttempts resolves the max-attempts value for a chat: per-chat
// override if set, else the global default from config. Errors fall back to
// the global default.
func (b *Bot) effectiveMaxAttempts(ctx context.Context, chatID int64) int {
	s, err := b.db.GetChatSettings(ctx, chatID)
	if err == nil && s.MaxAttempts.Valid {
		return int(s.MaxAttempts.Int64)
	}
	return b.cfg.MaxAttempts
}

// effectiveCaptchaTimeout resolves the captcha timeout for a chat: per-chat
// override if set, else global default.
func (b *Bot) effectiveCaptchaTimeout(ctx context.Context, chatID int64) time.Duration {
	s, err := b.db.GetChatSettings(ctx, chatID)
	if err == nil && s.CaptchaTimeoutSeconds.Valid {
		return time.Duration(s.CaptchaTimeoutSeconds.Int64) * time.Second
	}
	return b.cfg.CaptchaTimeout
}

// effectiveCaptchaMode resolves the captcha style for a chat. Unknown
// values stored in the DB (future / corrupt) fall back to ModeCircles.
func (b *Bot) effectiveCaptchaMode(ctx context.Context, chatID int64) captcha.Mode {
	s, err := b.db.GetChatSettings(ctx, chatID)
	if err != nil || !s.CaptchaMode.Valid {
		return captcha.ModeCircles
	}
	switch captcha.Mode(s.CaptchaMode.String) {
	case captcha.ModeEmoji:
		return captcha.ModeEmoji
	default:
		return captcha.ModeCircles
	}
}
