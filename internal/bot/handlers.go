package bot

import (
	"context"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
	tu "github.com/mymmrac/telego/telegoutil"

	"github.com/menand/AntiSpamBot/internal/captcha"
)

func (b *Bot) handleChatMember(ctx *th.Context, update telego.Update) error {
	upd := update.ChatMember
	if upd == nil {
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

	b.startCaptcha(ctx, upd.Chat.ID, user)
	return nil
}

func (b *Bot) handleCallback(ctx *th.Context, query telego.CallbackQuery) error {
	var targetUserID int64
	var optIdx int
	if _, err := fmt.Sscanf(query.Data, "cap:%d:%d", &targetUserID, &optIdx); err != nil {
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

	chatID := query.Message.GetChat().ID
	p, ok := b.store.Take(chatID, query.From.ID)
	if !ok {
		_ = b.api.AnswerCallbackQuery(ctx,
			tu.CallbackQuery(query.ID).WithText("Время вышло."))
		return nil
	}
	p.Cancel()

	if optIdx == p.CorrectIdx {
		_ = b.api.AnswerCallbackQuery(ctx,
			tu.CallbackQuery(query.ID).WithText("Правильно, добро пожаловать!"))
		if err := b.onSuccess(ctx, p); err != nil {
			b.log.Error("on success", "err", err, "chat", chatID, "user", query.From.ID)
		}
	} else {
		_ = b.api.AnswerCallbackQuery(ctx,
			tu.CallbackQuery(query.ID).WithText("Неверно.").WithShowAlert())
		if err := b.onFail(ctx, p, "неверный ответ"); err != nil {
			b.log.Error("on fail", "err", err, "chat", chatID, "user", query.From.ID)
		}
	}
	return nil
}

func (b *Bot) startCaptcha(ctx context.Context, chatID int64, user telego.User) {
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

	p := b.store.Put(chatID, user.ID, msg.MessageID, ch.CorrectIdx, b.cfg.CaptchaTimeout)
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
	if err := b.onFail(b.runCtx, p, "таймаут"); err != nil {
		b.log.Error("on fail timeout", "err", err, "chat", p.ChatID, "user", p.UserID)
	}
}

func (b *Bot) onSuccess(ctx context.Context, p *captcha.Pending) error {
	b.attempts.Reset(p.ChatID, p.UserID)
	b.log.Info("captcha passed", "chat", p.ChatID, "user", p.UserID)
	_ = b.deleteMessage(ctx, p.ChatID, p.MessageID)
	return b.release(ctx, p.ChatID, p.UserID)
}

func (b *Bot) onFail(ctx context.Context, p *captcha.Pending, reason string) error {
	count := b.attempts.Increment(p.ChatID, p.UserID)
	_ = b.deleteMessage(ctx, p.ChatID, p.MessageID)

	if count >= b.cfg.MaxAttempts {
		b.log.Info("banning user", "chat", p.ChatID, "user", p.UserID, "reason", reason, "attempts", count)
		return b.ban(ctx, p.ChatID, p.UserID)
	}
	b.log.Info("kicking user", "chat", p.ChatID, "user", p.UserID, "reason", reason, "attempts", count)
	return b.kick(ctx, p.ChatID, p.UserID)
}

func mentionHTML(u telego.User) string {
	name := strings.TrimSpace(u.FirstName + " " + u.LastName)
	if name == "" {
		name = fmt.Sprintf("id%d", u.ID)
	}
	return fmt.Sprintf(`<a href="tg://user?id=%d">%s</a>`, u.ID, html.EscapeString(name))
}
