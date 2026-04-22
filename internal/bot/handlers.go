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

	oldStatus := upd.OldChatMember.MemberStatus()
	newStatus := upd.NewChatMember.MemberStatus()
	user := upd.NewChatMember.MemberUser()

	b.log.Info("chat_member event",
		"chat", upd.Chat.ID,
		"chat_type", upd.Chat.Type,
		"user", user.ID,
		"old", oldStatus,
		"new", newStatus)

	if upd.Chat.Type != "group" && upd.Chat.Type != "supergroup" {
		return nil
	}
	if !b.chatAllowed(upd.Chat.ID) {
		return nil
	}

	joined := (oldStatus == "left" || oldStatus == "kicked") &&
		(newStatus == "member" || newStatus == "restricted")
	if !joined {
		return nil
	}
	if user.IsBot {
		return nil
	}
	if b.me != nil && user.ID == b.me.ID {
		return nil
	}

	b.onUserJoined(upd.Chat.ID, upd.Chat.Title, upd.Chat.Type, user)
	return nil
}

// handleMyChatMember tracks the bot's own membership across chats. When the
// bot leaves (voluntarily or via kick), we remove the chat from the registry
// so it stops appearing in owners'/admins' "Мои чаты" menu. Historical
// stats for the chat stay in the DB for archival.
func (b *Bot) handleMyChatMember(ctx *th.Context, update telego.Update) error {
	upd := update.MyChatMember
	if upd == nil {
		return nil
	}
	oldStatus := upd.OldChatMember.MemberStatus()
	newStatus := upd.NewChatMember.MemberStatus()
	b.log.Info("my_chat_member event",
		"chat", upd.Chat.ID,
		"chat_type", upd.Chat.Type,
		"old", oldStatus, "new", newStatus)

	if newStatus == "left" || newStatus == "kicked" {
		if err := b.db.DeleteChat(b.runCtx, upd.Chat.ID); err != nil {
			b.log.Warn("delete chat on bot leave",
				"err", err, "chat", upd.Chat.ID)
		}
	}
	return nil
}

// onUserJoined is the common kickoff for both chat_member events and
// message.new_chat_members service messages. Safe to call multiple times
// for the same user — startCaptcha dedups via the in-memory store.
func (b *Bot) onUserJoined(chatID int64, chatTitle, chatType string, user telego.User) {
	_ = b.db.RememberChat(b.runCtx, storage.ChatInfo{
		ChatID: chatID,
		Title:  chatTitle,
		Type:   chatType,
	})
	if err := b.db.RecordEvent(b.runCtx, chatID, user.ID, storage.EventJoin, time.Now()); err != nil {
		b.log.Warn("record join event", "err", err)
	}
	b.startCaptcha(chatID, user)
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

	userID := int64(0)
	if message.From != nil {
		userID = message.From.ID
	}

	text := b.mainMenuText(userID)
	if message.From != nil {
		text += fmt.Sprintf("\n\n<i>Твой Telegram ID: <code>%d</code></i>", message.From.ID)
	}

	_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(message.Chat.ID), text).
		WithParseMode(telego.ModeHTML).
		WithReplyMarkup(b.mainMenuKeyboard(userID)))
	return nil
}

func (b *Bot) handleGroupMessage(ctx *th.Context, message telego.Message) error {
	if message.Chat.Type != "group" && message.Chat.Type != "supergroup" {
		return nil
	}

	// Service message: basic group upgraded to supergroup. Telegram emits
	// MigrateToChatID in the old group and MigrateFromChatID in the new one;
	// we handle both as insurance. MigrateChat is idempotent, so a double
	// fire is harmless.
	if message.MigrateToChatID != 0 {
		oldID := message.Chat.ID
		newID := message.MigrateToChatID
		b.log.Info("chat migrating to supergroup", "old", oldID, "new", newID)
		if err := b.db.MigrateChat(b.runCtx, oldID, newID); err != nil {
			b.log.Error("migrate chat data", "err", err, "old", oldID, "new", newID)
		}
		return nil
	}
	if message.MigrateFromChatID != 0 {
		oldID := message.MigrateFromChatID
		newID := message.Chat.ID
		b.log.Info("chat migrated from basic group", "old", oldID, "new", newID)
		if err := b.db.MigrateChat(b.runCtx, oldID, newID); err != nil {
			b.log.Error("migrate chat data", "err", err, "old", oldID, "new", newID)
		}
		return nil
	}

	// Service message: new members joined. This is a fallback for cases where
	// Telegram doesn't emit a chat_member update (some group types, some
	// rejoin scenarios). startCaptcha dedups via the in-memory store, so even
	// if chat_member also fires for the same user, only one captcha is shown.
	if len(message.NewChatMembers) > 0 {
		if b.chatAllowed(message.Chat.ID) {
			hadHuman := false
			for _, nm := range message.NewChatMembers {
				if nm.IsBot {
					continue
				}
				if b.me != nil && nm.ID == b.me.ID {
					continue
				}
				hadHuman = true
				b.log.Info("new_chat_members service message",
					"chat", message.Chat.ID, "user", nm.ID)
				b.onUserJoined(message.Chat.ID, message.Chat.Title, message.Chat.Type, nm)
			}
			// Remove Telegram's "X joined the chat" service message — clutters
			// the chat and we're already showing the captcha.
			if hadHuman {
				if err := b.deleteMessage(b.runCtx, message.Chat.ID, message.MessageID); err != nil {
					b.log.Warn("delete join service message",
						"err", err, "chat", message.Chat.ID, "msg", message.MessageID)
				}
			}
		}
		return nil
	}

	// Service message: member left or was kicked. Delete it (same rationale
	// as new_chat_members — "bot kicked X" / "X left the chat" spam).
	if message.LeftChatMember != nil {
		if b.chatAllowed(message.Chat.ID) {
			if err := b.deleteMessage(b.runCtx, message.Chat.ID, message.MessageID); err != nil {
				b.log.Warn("delete leave service message",
					"err", err, "chat", message.Chat.ID, "msg", message.MessageID)
			}
		}
		return nil
	}

	if message.From == nil || message.From.IsBot {
		return nil
	}
	// Skip other service messages (title changes, pins, etc.)
	if message.NewChatTitle != "" || message.NewChatPhoto != nil ||
		message.PinnedMessage != nil {
		return nil
	}

	// User is in the pre-captcha delay window (or still has an active captcha
	// that somehow slipped past restriction): delete whatever they wrote.
	// Don't proceed to stats/silence detection for these messages.
	if b.store.IsCaptchaActive(message.Chat.ID, message.From.ID) {
		if err := b.deleteMessage(b.runCtx, message.Chat.ID, message.MessageID); err != nil {
			b.log.Warn("delete pre-captcha message",
				"err", err, "chat", message.Chat.ID, "user", message.From.ID)
		}
		return nil
	}

	chatID := message.Chat.ID
	user := *message.From
	when := time.Unix(int64(message.Date), 0)

	_ = b.db.RememberChat(b.runCtx, storage.ChatInfo{
		ChatID: chatID,
		Title:  message.Chat.Title,
		Type:   message.Chat.Type,
	})

	if err := b.db.RememberUser(b.runCtx, storage.UserInfo{
		UserID:    user.ID,
		FirstName: user.FirstName,
		LastName:  user.LastName,
		Username:  user.Username,
	}); err != nil {
		b.log.Warn("remember user", "err", err)
	}

	newcomer := b.isNewcomer(b.runCtx, chatID, user.ID, when)
	if err := b.db.IncMessage(b.runCtx, chatID, when, newcomer); err != nil {
		b.log.Warn("inc message", "err", err)
	}

	rec, err := b.db.RecordMessage(b.runCtx, chatID, user.ID, when)
	if err != nil {
		b.log.Warn("record message", "err", err)
		return nil
	}
	b.maybeAnnounceReturn(ctx, message, user, rec)
	return nil
}

func (b *Bot) maybeAnnounceReturn(ctx *th.Context, message telego.Message, user telego.User, rec storage.MessageRecord) {
	if b.cfg.SilentAnnounceDays == 0 || !rec.HasBaseline {
		return
	}
	threshold := time.Duration(b.cfg.SilentAnnounceDays) * 24 * time.Hour
	if rec.Silence < threshold {
		return
	}
	days := int(rec.Silence / (24 * time.Hour))
	mention := mentionHTML(user)
	var text string
	switch {
	case rec.WasFirstMessage:
		text = fmt.Sprintf("🎉 Смотрите-ка! %s был(а) в чате <b>%s</b> и наконец-то впервые что-то пишет.",
			mention, humanDaysRU(days))
	case days >= 365:
		text = fmt.Sprintf("🎊 Сенсация! %s молчал(а) <b>%s</b> и вот наконец-то написал(а)!",
			mention, humanDaysRU(days))
	case days >= 90:
		text = fmt.Sprintf("👀 Ого! %s вернулся после <b>%s</b> тишины.",
			mention, humanDaysRU(days))
	default:
		text = fmt.Sprintf("✨ %s снова с нами после <b>%s</b> молчания.",
			mention, humanDaysRU(days))
	}
	_, err := b.api.SendMessage(ctx, tu.Message(tu.ID(message.Chat.ID), text).
		WithParseMode(telego.ModeHTML).
		WithReplyParameters(&telego.ReplyParameters{MessageID: message.MessageID}))
	if err != nil {
		b.log.Warn("announce return", "err", err, "chat", message.Chat.ID, "user", user.ID)
	}
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
	// Race guard: chat_member events and message.new_chat_members can both
	// fire for the same join. Without a kickoff lock they race through the
	// pre-Put phase (restrict + send) and produce two captcha messages.
	if !b.store.BeginKickoff(chatID, user.ID) {
		b.log.Debug("captcha already in progress, skipping duplicate kickoff",
			"chat", chatID, "user", user.ID)
		return
	}
	// Run the captcha flow asynchronously — it sleeps for CaptchaDelay to let
	// the user's Telegram client fully render the chat before the captcha
	// arrives. During that window, handleGroupMessage deletes any messages
	// they send (store.IsCaptchaActive returns true while inflight is held).
	go b.runCaptcha(chatID, user)
}

func (b *Bot) runCaptcha(chatID int64, user telego.User) {
	defer b.store.FinishKickoff(chatID, user.ID)

	ctx := b.runCtx

	// Cache display name now — we'll need it when sending the greeting after a
	// successful pass (by then the user hasn't written anything, so user_info
	// wouldn't be populated from message-handling path).
	_ = b.db.RememberUser(ctx, storage.UserInfo{
		UserID:    user.ID,
		FirstName: user.FirstName,
		LastName:  user.LastName,
		Username:  user.Username,
	})

	// Sleep before the captcha flow so the user's client has time to fully
	// open the chat. Without this, the captcha message + immediate restrict
	// event sometimes don't merge into the user's already-rendered view and
	// they only see the captcha after reopening the chat.
	if b.cfg.CaptchaDelay > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(b.cfg.CaptchaDelay):
		}
	}

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
	if err := b.deleteMessage(ctx, p.ChatID, p.MessageID); err != nil {
		b.log.Warn("delete captcha on pass",
			"err", err, "chat", p.ChatID, "msg", p.MessageID)
	}
	if err := b.release(ctx, p.ChatID, p.UserID); err != nil {
		return err
	}
	b.maybeSendGreeting(ctx, p.ChatID, p.UserID)
	return nil
}

func (b *Bot) onFail(ctx context.Context, p *captcha.Pending, reason string) error {
	count, err := b.db.IncrementAttempt(ctx, p.ChatID, p.UserID, attemptsTTL)
	if err != nil {
		b.log.Warn("increment attempt", "err", err)
		count = 1 // fall forward as first attempt
	}
	if err := b.deleteMessage(ctx, p.ChatID, p.MessageID); err != nil {
		b.log.Warn("delete captcha on fail/timeout",
			"err", err, "chat", p.ChatID, "msg", p.MessageID, "reason", reason)
	}

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
