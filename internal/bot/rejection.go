package bot

import (
	"context"
	"fmt"
	"strings"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"

	"github.com/menand/AntiSpamBot/internal/storage"
)

const rejectionPromptPrefix = "Причина отказа для чата"

type rejectionPrompt struct {
	chatID    int64
	approval  telego.MaybeInaccessibleMessage
	messageID int
	delivered bool
}

func (b *Bot) askRejectionReason(ctx context.Context, query telego.CallbackQuery, chatID int64) {
	ownerID := query.From.ID
	prompt := &rejectionPrompt{chatID: chatID, approval: query.Message}
	b.leaveMu.Lock()
	if b.leaveInflight[chatID] {
		b.leaveMu.Unlock()
		b.ownerDMNotice(ctx, ownerID, "ℹ️ Бот уже обрабатывает выход из этого чата.")
		return
	}
	b.approvalMu.Lock()
	prev := b.rejectionPrompts[ownerID]
	if prev != nil && prev.messageID == 0 {
		b.approvalMu.Unlock()
		b.leaveMu.Unlock()
		return
	}
	if b.rejectionPrompts == nil {
		b.rejectionPrompts = make(map[int64]*rejectionPrompt)
	}
	if prev != nil && prev.chatID == chatID {
		prompt.delivered = prev.delivered
	}
	delivered := prompt.delivered
	b.rejectionPrompts[ownerID] = prompt
	b.approvalMu.Unlock()
	b.leaveMu.Unlock()

	title := fmt.Sprintf("%s «%s»", rejectionPromptPrefix, b.chatTitle(b.runCtx, chatID))
	text := title + ". Напиши причину ответом на это сообщение. Я отправлю её в чат и выйду."
	if delivered {
		text = title + " уже отправлена. Ответь на это сообщение, чтобы повторить выход."
	}
	sent, err := b.sendScreenParams(ctx, ownerID, screenRejection, tu.Message(tu.ID(ownerID), text).
		WithReplyMarkup(&telego.ForceReply{ForceReply: true}).
		WithReplyParameters(&telego.ReplyParameters{MessageID: query.Message.GetMessageID(), AllowSendingWithoutReply: true}))
	b.approvalMu.Lock()
	if b.rejectionPrompts[ownerID] == prompt {
		if err == nil && sent.MessageID > 0 {
			prompt.messageID = sent.MessageID
		} else {
			delete(b.rejectionPrompts, ownerID)
		}
	}
	b.approvalMu.Unlock()
	if err != nil {
		b.log.Warn("ask rejection reason", "err", err, "chat", chatID)
	}
}

func (b *Bot) handleRejectionReason(ctx context.Context, message telego.Message) bool {
	reply := message.ReplyToMessage
	if reply == nil || message.From == nil || message.From.IsBot ||
		!isOwnPrivateChat(message.Chat, message.From.ID) || !b.isOwner(message.From.ID) {
		return false
	}
	ownerID := message.From.ID
	b.approvalMu.Lock()
	prompt := b.rejectionPrompts[ownerID]
	matches := prompt != nil && prompt.messageID > 0 && prompt.messageID == reply.MessageID
	b.approvalMu.Unlock()
	if !matches {
		if b.me != nil && reply.From != nil && reply.From.ID == b.me.ID && strings.HasPrefix(reply.Text, rejectionPromptPrefix) {
			b.ownerDMNotice(ctx, ownerID, "ℹ️ Запрос устарел. Нажми «Нет, выйти» в уведомлении ещё раз.")
			return true
		}
		return false
	}
	if text := strings.TrimSpace(message.Text); text == "" || strings.HasPrefix(text, "/") {
		b.ownerDMNotice(ctx, ownerID, "Пришли текст причины ответом на запрос.")
		return true
	}
	b.finishRejection(ctx, message, prompt)
	return true
}

func (b *Bot) finishRejection(ctx context.Context, message telego.Message, prompt *rejectionPrompt) {
	ownerID, chatID := message.Chat.ID, prompt.chatID
	b.leaveMu.Lock()
	if b.leaveInflight[chatID] {
		b.leaveMu.Unlock()
		b.ownerDMNotice(ctx, ownerID, "ℹ️ Бот уже обрабатывает выход из этого чата.")
		return
	}
	b.leaveInflight[chatID] = true
	b.leaveMu.Unlock()
	leaving := false
	defer func() {
		if !leaving {
			b.endChatLeave(chatID)
		}
	}()

	status, exists, err := b.db.GetChatApproval(b.runCtx, chatID)
	if err != nil {
		b.log.Warn("read approval before rejection", "err", err, "chat", chatID)
		b.ownerDMNotice(ctx, ownerID, "⚠️ Не удалось проверить статус чата. Ответь на запрос ещё раз.")
		return
	}
	b.approvalMu.Lock()
	active, delivered := b.rejectionPrompts[ownerID] == prompt, prompt.delivered
	b.approvalMu.Unlock()
	if !active || !exists || status == storage.ChatApproved {
		b.ownerDMNotice(ctx, ownerID, "ℹ️ Запрос больше не действует. Проверь уведомление о чате.")
		return
	}

	if !delivered {
		// Копия сохраняет текст и форматирование, не раскрывая профиль владельца.
		sent, err := b.api.CopyMessage(ctx, &telego.CopyMessageParams{
			ChatID:     tu.ID(chatID),
			FromChatID: tu.ID(ownerID),
			MessageID:  message.MessageID,
		})
		if err != nil || sent == nil || sent.MessageID <= 0 {
			b.log.Warn("publish rejection reason", "err", err, "chat", chatID)
			b.ownerDMNotice(ctx, ownerID, "⚠️ Не удалось отправить причину. Бот остался в чате. Ответь на запрос ещё раз.")
			return
		}
		b.approvalMu.Lock()
		prompt.delivered = true
		b.approvalMu.Unlock()
	}

	if status == storage.ChatPending {
		claimed, err := b.db.ClaimChatApproval(b.runCtx, chatID, storage.ChatRejected)
		if err != nil {
			b.log.Warn("claim reject", "err", err, "chat", chatID)
			b.ownerDMNotice(ctx, ownerID, "⚠️ Причина отправлена, но решение не удалось сохранить. Ответь на запрос ещё раз.")
			return
		}
		if !claimed {
			b.ownerDMNotice(ctx, ownerID, "ℹ️ Причина отправлена, но статус чата уже изменился.")
			return
		}
	}
	b.setApprovalCache(chatID, false)
	b.leaveChatAndCleanup(chatID, "owner rejected chat", func(leaveErr error) {
		replyCtx, cancel := b.liveCtx()
		defer cancel()
		if leaveErr != nil {
			b.ownerDMNotice(replyCtx, ownerID, "⚠️ Причина отправлена, но боту не удалось выйти. Ответь на запрос ещё раз, чтобы повторить выход.")
			return
		}
		b.replyToApproval(replyCtx, prompt.approval, "🚪 Бот вышел из чата")
	})
	leaving = true
}
