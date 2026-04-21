package bot

import (
	"context"
	"fmt"
	"html"
	"strconv"
	"strings"
	"time"

	"github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
	tu "github.com/mymmrac/telego/telegoutil"

	"github.com/menand/AntiSpamBot/internal/captcha"
	"github.com/menand/AntiSpamBot/internal/storage"
)

func (b *Bot) handleChatMember(ctx *th.Context, update telego.Update) error {
	upd := update.ChatMember
	if upd == nil {
		return nil
	}
	if upd.Chat.Type != "group" && upd.Chat.Type != "supergroup" {
		return nil
	}
	if !b.chatAllowed(upd.Chat.ID) {
		return nil
	}

	oldStatus := upd.OldChatMember.MemberStatus()
	newStatus := upd.NewChatMember.MemberStatus()
	joined := (oldStatus == "left" || oldStatus == "kicked") &&
		(newStatus == "member" || newStatus == "restricted")
	if !joined {
		return nil
	}

	user := upd.NewChatMember.MemberUser()
	if user.IsBot {
		return nil
	}
	if b.me != nil && user.ID == b.me.ID {
		return nil
	}

	if err := b.db.RecordEvent(b.runCtx, upd.Chat.ID, user.ID, storage.EventJoin, time.Now()); err != nil {
		b.log.Warn("record join event", "err", err)
	}
	b.startCaptcha(upd.Chat.ID, user)
	return nil
}

func (b *Bot) handleCallback(ctx *th.Context, query telego.CallbackQuery) error {
	targetUserID, optIdx, ok := parseCallback(query.Data)
	if !ok {
		_ = b.api.AnswerCallbackQuery(ctx, tu.CallbackQuery(query.ID))
		return nil
	}
	if query.From.ID != targetUserID {
		_ = b.api.AnswerCallbackQuery(ctx,
			tu.CallbackQuery(query.ID).
				WithText("Эта капча не для тебя.").
				WithShowAlert())
		return nil
	}
	if query.Message == nil {
		return nil
	}

	chatID := query.Message.GetChat().ID
	p, ok := b.store.Take(chatID, query.From.ID)
	if !ok {
		_ = b.api.AnswerCallbackQuery(ctx,
			tu.CallbackQuery(query.ID).WithText("Время вышло."))
		return nil
	}
	p.Cancel()
	_ = b.db.DeletePending(b.runCtx, chatID, query.From.ID)

	if optIdx == p.CorrectIdx {
		_ = b.api.AnswerCallbackQuery(ctx,
			tu.CallbackQuery(query.ID).WithText("Правильно, добро пожаловать!"))
		if err := b.onSuccess(b.runCtx, p); err != nil {
			b.log.Error("on success", "err", err, "chat", chatID, "user", query.From.ID)
		}
	} else {
		_ = b.api.AnswerCallbackQuery(ctx,
			tu.CallbackQuery(query.ID).WithText("Неверно.").WithShowAlert())
		if err := b.onFail(b.runCtx, p, "неверный ответ"); err != nil {
			b.log.Error("on fail", "err", err, "chat", chatID, "user", query.From.ID)
		}
	}
	return nil
}

func (b *Bot) handlePrivateStart(ctx *th.Context, message telego.Message) error {
	if message.Chat.Type != "private" {
		return nil
	}
	text := "Привет! Я анти-спам бот.\n\n" +
		"Добавь меня в свою группу как <b>администратора</b> с правами <b>«Банить участников»</b> и <b>«Удалять сообщения»</b> — и я буду проверять новых участников капчей.\n\n" +
		"Проверка: пользователь должен выбрать кружок указанного цвета из 6 вариантов.\n\n" +
		"В группе работают команды:\n" +
		"/stats — статистика чата (только для админов)"
	_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(message.Chat.ID), text).
		WithParseMode(telego.ModeHTML))
	return nil
}

func (b *Bot) handleGroupMessage(ctx *th.Context, message telego.Message) error {
	if message.Chat.Type != "group" && message.Chat.Type != "supergroup" {
		return nil
	}
	if message.From == nil || message.From.IsBot {
		return nil
	}
	// Skip service messages (joins, leaves, title changes, etc.)
	if len(message.NewChatMembers) > 0 || message.LeftChatMember != nil ||
		message.NewChatTitle != "" || message.NewChatPhoto != nil ||
		message.PinnedMessage != nil {
		return nil
	}

	chatID := message.Chat.ID
	userID := message.From.ID
	when := time.Unix(int64(message.Date), 0)

	newcomer := b.isNewcomer(b.runCtx, chatID, userID, when)
	if err := b.db.IncMessage(b.runCtx, chatID, when, newcomer); err != nil {
		b.log.Warn("inc message", "err", err)
	}
	return nil
}

func (b *Bot) isNewcomer(ctx context.Context, chatID, userID int64, when time.Time) bool {
	joinedAt, ok, err := b.db.MemberJoinedAt(ctx, chatID, userID)
	if err != nil {
		b.log.Warn("member joined_at", "err", err)
		return false
	}
	if !ok {
		// Pre-existing member before the bot was added.
		return false
	}
	window := time.Duration(b.cfg.NewcomerDays) * 24 * time.Hour
	return when.Sub(joinedAt) < window
}

func (b *Bot) startCaptcha(chatID int64, user telego.User) {
	ctx := b.runCtx
	ch := captcha.New()

	if err := b.restrict(ctx, chatID, user.ID); err != nil {
		b.log.Error("restrict", "err", err, "chat", chatID, "user", user.ID)
		return
	}

	correct := ch.Correct()
	text := fmt.Sprintf(
		"Привет, %s!\nДля защиты от спама выбери <b>%s</b> кружок за %d секунд.",
		mentionHTML(user), correct.Name, int(b.cfg.CaptchaTimeout.Seconds()),
	)

	buttons := make([]telego.InlineKeyboardButton, 0, len(ch.Options))
	for i, c := range ch.Options {
		buttons = append(buttons,
			tu.InlineKeyboardButton(c.Emoji).
				WithCallbackData(fmt.Sprintf("cap:%d:%d", user.ID, i)))
	}
	kb := tu.InlineKeyboard(tu.InlineKeyboardRow(buttons...))

	msg, err := b.api.SendMessage(ctx,
		tu.Message(tu.ID(chatID), text).
			WithParseMode(telego.ModeHTML).
			WithReplyMarkup(kb))
	if err != nil {
		b.log.Error("send captcha", "err", err, "chat", chatID, "user", user.ID)
		_ = b.release(ctx, chatID, user.ID)
		return
	}

	expires := time.Now().Add(b.cfg.CaptchaTimeout)
	p := b.store.Put(chatID, user.ID, msg.MessageID, ch.CorrectIdx, expires)

	if err := b.db.PutPending(ctx, storage.PendingRow{
		ChatID:     chatID,
		UserID:     user.ID,
		MessageID:  msg.MessageID,
		CorrectIdx: ch.CorrectIdx,
		ExpiresAt:  expires,
	}); err != nil {
		b.log.Warn("persist pending", "err", err)
	}

	go b.waitTimeout(p)
}

func (b *Bot) waitTimeout(p *captcha.Pending) {
	timer := time.NewTimer(time.Until(p.ExpiresAt))
	defer timer.Stop()

	select {
	case <-p.Done():
		return
	case <-b.runCtx.Done():
		return
	case <-timer.C:
	}

	existing, ok := b.store.Take(p.ChatID, p.UserID)
	if !ok || existing != p {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = b.db.DeletePending(cleanupCtx, p.ChatID, p.UserID)
	if err := b.onFail(cleanupCtx, p, "таймаут"); err != nil {
		b.log.Error("on fail timeout", "err", err, "chat", p.ChatID, "user", p.UserID)
	}
}

func (b *Bot) onSuccess(ctx context.Context, p *captcha.Pending) error {
	_ = b.db.ResetAttempts(ctx, p.ChatID, p.UserID)
	if err := b.db.UpsertMember(ctx, p.ChatID, p.UserID, time.Now()); err != nil {
		b.log.Warn("upsert member", "err", err)
	}
	if err := b.db.RecordEvent(ctx, p.ChatID, p.UserID, storage.EventPass, time.Now()); err != nil {
		b.log.Warn("record pass event", "err", err)
	}
	b.log.Info("captcha passed", "chat", p.ChatID, "user", p.UserID)
	_ = b.deleteMessage(ctx, p.ChatID, p.MessageID)
	return b.release(ctx, p.ChatID, p.UserID)
}

func (b *Bot) onFail(ctx context.Context, p *captcha.Pending, reason string) error {
	count, err := b.db.IncrementAttempt(ctx, p.ChatID, p.UserID, attemptsTTL)
	if err != nil {
		b.log.Warn("increment attempt", "err", err)
		count = 1 // fall forward as first attempt
	}
	_ = b.deleteMessage(ctx, p.ChatID, p.MessageID)

	if count >= b.cfg.MaxAttempts {
		b.log.Info("banning user", "chat", p.ChatID, "user", p.UserID, "reason", reason, "attempts", count)
		_ = b.db.RecordEvent(ctx, p.ChatID, p.UserID, storage.EventBan, time.Now())
		return b.ban(ctx, p.ChatID, p.UserID)
	}
	b.log.Info("kicking user", "chat", p.ChatID, "user", p.UserID, "reason", reason, "attempts", count)
	_ = b.db.RecordEvent(ctx, p.ChatID, p.UserID, storage.EventKick, time.Now())
	return b.kick(ctx, p.ChatID, p.UserID)
}

func (b *Bot) chatAllowed(chatID int64) bool {
	if b.cfg.AllowedChats == nil {
		return true
	}
	_, ok := b.cfg.AllowedChats[chatID]
	return ok
}

func parseCallback(data string) (userID int64, optIdx int, ok bool) {
	parts := strings.Split(data, ":")
	if len(parts) != 3 || parts[0] != "cap" {
		return 0, 0, false
	}
	uid, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	idx, err := strconv.Atoi(parts[2])
	if err != nil {
		return 0, 0, false
	}
	return uid, idx, true
}

func mentionHTML(u telego.User) string {
	name := strings.TrimSpace(u.FirstName + " " + u.LastName)
	if name == "" {
		name = fmt.Sprintf("id%d", u.ID)
	}
	return fmt.Sprintf(`<a href="tg://user?id=%d">%s</a>`, u.ID, html.EscapeString(name))
}
