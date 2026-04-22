package bot

import (
	"context"

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
