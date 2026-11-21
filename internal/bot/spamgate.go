package bot

import (
	"context"

	"github.com/menand/AntiSpamBot/internal/storage"
)

type spamGate struct {
	enabled bool
	ctx     context.Context
	cancel  context.CancelFunc
}

func (b *Bot) cachedSpamGate(chatID int64, readSettings func(context.Context, int64) (storage.ChatSettings, error)) spamGate {
	b.spamGateMu.Lock()
	gate, cached := b.spamGateCache[chatID]
	b.spamGateMu.Unlock()
	if cached {
		return gate
	}
	s, err := readSettings(b.runCtx, chatID)
	if err != nil {
		b.log.Warn("get chat settings (spam gate)", "err", err, "chat", chatID)
		return spamGate{}
	}

	b.spamGateMu.Lock()
	defer b.spamGateMu.Unlock()
	// A toggle or another reader may have filled the cache since the read.
	if current, exists := b.spamGateCache[chatID]; exists {
		return current
	}
	return b.publishSpamGate(chatID, s.SpamCheckEnabled)
}

func (b *Bot) setSpamCheckEnabled(chatID int64, enabled bool) error {
	b.spamGateMu.Lock()
	defer b.spamGateMu.Unlock()
	if err := b.db.SetSpamCheckEnabled(b.runCtx, chatID, enabled); err != nil {
		return err
	}
	b.publishSpamGate(chatID, enabled)
	return nil
}

// publishSpamGate replaces the context for a chat under spamGateMu.
func (b *Bot) publishSpamGate(chatID int64, enabled bool) spamGate {
	if old := b.spamGateCache[chatID]; old.cancel != nil {
		old.cancel()
	}
	gate := spamGate{enabled: enabled}
	if enabled {
		gate.ctx, gate.cancel = context.WithCancel(b.runCtx)
	}
	b.spamGateCache[chatID] = gate
	return gate
}
