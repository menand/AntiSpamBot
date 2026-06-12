package bot

import (
	"context"

	"github.com/menand/AntiSpamBot/internal/storage"
)

// rememberChat upserts the chat registry row, skipping the DB write when the
// cached value is identical. Called on every group message, so without the
// cache this is one extra SQLite write per message. The cache is updated only
// after a successful write so a failed upsert retries on the next message.
// Unbounded by design: entries are ~100 bytes and the key space is the set of
// chats the bot lives in.
func (b *Bot) rememberChat(ctx context.Context, info storage.ChatInfo) {
	b.cacheMu.Lock()
	cached, ok := b.chatCache[info.ChatID]
	b.cacheMu.Unlock()
	if ok && cached == info {
		return
	}
	if err := b.db.RememberChat(ctx, info); err != nil {
		b.log.Warn("remember chat", "err", err, "chat", info.ChatID)
		return
	}
	b.cacheMu.Lock()
	b.chatCache[info.ChatID] = info
	b.cacheMu.Unlock()
}

// rememberUser is the same write-through pattern for user display names.
// Memory grows with the number of distinct active users seen during the
// process lifetime — acceptable for group-bot scale.
func (b *Bot) rememberUser(ctx context.Context, info storage.UserInfo) {
	b.cacheMu.Lock()
	cached, ok := b.userCache[info.UserID]
	b.cacheMu.Unlock()
	if ok && cached == info {
		return
	}
	if err := b.db.RememberUser(ctx, info); err != nil {
		b.log.Warn("remember user", "err", err, "user", info.UserID)
		return
	}
	b.cacheMu.Lock()
	b.userCache[info.UserID] = info
	b.cacheMu.Unlock()
}
