package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/mymmrac/telego/telegoapi"
)

const replyProbeCopyKey = "reply_probe_copy"

// The probe loop creates one copy at a time and keeps only its address.
type replyProbeCopy struct {
	ChatID    int64
	MessageID int
	saved     bool
}

func (b *Bot) cleanupReplyProbeCopy(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, replyWatchProbeTimeout)
	defer cancel()
	copy := &b.replyProbeCopy
	if copy.MessageID == 0 {
		raw, err := b.db.GetMeta(ctx, replyProbeCopyKey)
		if err != nil {
			b.log.Warn("load reply probe copy", "err", err)
			return false
		}
		if raw == "" {
			return true
		}
		var pending replyProbeCopy
		if err := json.Unmarshal([]byte(raw), &pending); err != nil || pending.ChatID == 0 || pending.MessageID <= 0 {
			b.log.Error("invalid saved reply probe copy", "err", err)
			return false
		}
		*copy = pending
		copy.saved = true
	}
	if !copy.saved {
		raw, err := json.Marshal(copy)
		if err == nil {
			err = b.db.SetMeta(ctx, replyProbeCopyKey, string(raw))
		}
		if err != nil {
			b.log.Error("save reply probe copy", "err", err, "chat", copy.ChatID, "message", copy.MessageID)
		} else {
			copy.saved = true
		}
	}

	err := retryWith(ctx, kickUnbanBackoffs, func() error {
		err := b.deleteMessage(ctx, copy.ChatID, copy.MessageID)
		var apiErr *telegoapi.Error
		if errors.As(err, &apiErr) && apiErr.ErrorCode == 400 &&
			strings.EqualFold(strings.TrimPrefix(apiErr.Description, "Bad Request: "), "message to delete not found") {
			return nil
		}
		return err
	})
	if err != nil {
		b.log.Warn("delete reply probe copy", "err", err, "chat", copy.ChatID, "message", copy.MessageID)
		if !b.replyProbeCleanupWarned {
			b.replyProbeCleanupWarned = true
			b.notifyOwners(ctx, fmt.Sprintf("⚠️ Пробы ответов приостановлены. Бот повторит удаление служебной копии (чат %d, сообщение %d).", copy.ChatID, copy.MessageID))
		}
		return false
	}
	if copy.saved {
		if err := b.db.SetMeta(ctx, replyProbeCopyKey, ""); err != nil {
			b.log.Warn("clear reply probe copy", "err", err)
			return false
		}
	}
	*copy = replyProbeCopy{}
	b.replyProbeCleanupWarned = false
	return true
}
