package bot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html"
	"strconv"
	"strings"
	"time"

	"github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegoapi"
	th "github.com/mymmrac/telego/telegohandler"
	tu "github.com/mymmrac/telego/telegoutil"

	"github.com/menand/AntiSpamBot/internal/captcha"
	"github.com/menand/AntiSpamBot/internal/storage"
)

// telegramServiceUserID is the "Telegram" pseudo-user (not marked as a bot)
// that authors linked-channel auto-forwards and other service posts.
const telegramServiceUserID = 777000

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

	// Любая смена статуса могла дать или отнять админку — кэш для белого
	// списка антиспама и золотого голоса не должен пережить это событие.
	b.invalidateAdminCache(upd.Chat.ID, user.ID)

	if upd.Chat.Type != "group" && upd.Chat.Type != "supergroup" {
		return nil
	}
	if !b.chatAllowed(upd.Chat.ID) {
		return nil
	}
	if user.IsBot {
		return nil
	}
	if b.me != nil && user.ID == b.me.ID {
		return nil
	}

	joined := (oldStatus == "left" || oldStatus == "kicked") &&
		(newStatus == "member" || newStatus == "restricted")
	if joined {
		// chat_member updates carry no topic info; captcha goes to General (0).
		b.onUserJoined(upd.Chat.ID, upd.Chat.Title, upd.Chat.Type, user, 0)
		return nil
	}

	// User left (or was removed by an admin) while their captcha was still
	// active: cancel it quietly. They didn't fail the check — recording a
	// kick event here would skew the failure stats, and our own post-fail
	// kick is not affected (onFail always Takes the pending before kicking,
	// so this lookup misses for those).
	if newStatus == "left" || newStatus == "kicked" {
		if p, ok := b.store.Take(upd.Chat.ID, user.ID); ok {
			p.Cancel()
			_ = b.db.DeletePending(b.runCtx, upd.Chat.ID, user.ID)
			if err := b.deleteMessage(b.runCtx, upd.Chat.ID, p.MessageID); err != nil {
				b.log.Warn("delete captcha after user left",
					"err", err, "chat", upd.Chat.ID, "msg", p.MessageID)
			}
			b.log.Info("captcha cancelled — user left mid-captcha",
				"chat", upd.Chat.ID, "user", user.ID)
		}
	}
	return nil
}

// handleMyChatMember tracks the bot's own membership across chats. On leave
// (voluntary or kicked) the chat is dropped from the registry and its pending
// captchas are cancelled — their timeouts would otherwise fire kick/ban calls
// in a chat the bot no longer belongs to. Historical stats stay for archival.
// On join/promotion it registers the chat and tells admins which rights are
// missing, instead of failing silently later.
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
		b.dropChat(b.runCtx, upd.Chat.ID, "bot left/kicked")
		return nil
	}

	if upd.Chat.Type != "group" && upd.Chat.Type != "supergroup" {
		return nil
	}
	if !b.chatAllowed(upd.Chat.ID) {
		b.log.Info("chat not in ALLOWED_CHATS, ignoring",
			"chat", upd.Chat.ID, "title", upd.Chat.Title)
		return nil
	}
	b.rememberChat(b.runCtx, storage.ChatInfo{
		ChatID: upd.Chat.ID,
		Title:  upd.Chat.Title,
		Type:   upd.Chat.Type,
	})
	b.checkAdminRights(upd)
	return nil
}

// dropChat removes a chat from the DM-menu registry and cancels its pending
// captchas. Historical stats stay for archival. Evicting the write-through
// cache is essential: otherwise a later rememberChat with an unchanged title
// skips the DB write and the chat never reappears in the registry.
func (b *Bot) dropChat(ctx context.Context, chatID int64, why string) {
	b.log.Info("dropping chat from registry", "chat", chatID, "reason", why)
	for _, p := range b.store.TakeChat(chatID) {
		p.Cancel()
	}
	if err := b.db.DeletePendingChat(ctx, chatID); err != nil {
		b.log.Warn("delete pending captchas", "err", err, "chat", chatID)
	}
	if err := b.db.DeleteChat(ctx, chatID); err != nil {
		b.log.Warn("delete chat", "err", err, "chat", chatID)
	}
	b.cacheMu.Lock()
	delete(b.chatCache, chatID)
	b.cacheMu.Unlock()
}

// reconcileChats sweeps the chat registry once at startup and drops rows for
// chats the bot is not actually in. Rows outlive membership when BOT_TOKEN is
// switched to a different bot (the old bot's chats stay in the shared DB) or
// when the bot was kicked while offline — my_chat_member never fires for
// either, so the DM menu keeps showing dead chats forever.
func (b *Bot) reconcileChats(ctx context.Context) {
	chats, err := b.db.ListChats(ctx)
	if err != nil {
		b.log.Warn("reconcile chats: list", "err", err)
		return
	}
	for _, c := range chats {
		if !b.chatAllowed(c.ChatID) {
			b.dropChat(ctx, c.ChatID, "not in ALLOWED_CHATS")
			continue
		}
		m, err := b.api.GetChatMember(ctx, &telego.GetChatMemberParams{
			ChatID: tu.ID(c.ChatID),
			UserID: b.me.ID,
		})
		if reason, stale := staleChatReason(m, err); stale {
			b.dropChat(ctx, c.ChatID, reason)
		} else if err != nil {
			// Transient (network, 429, 5xx): keep the row, next restart retries.
			b.log.Warn("reconcile chats: check membership", "err", err, "chat", c.ChatID)
		}
	}
}

// staleChatReason decides whether a getChatMember(self) result proves the bot
// is not in the chat. Telegram answers 400 "chat not found" for chats this
// bot has never seen and 403 "bot was kicked"/"not a member" for lost
// membership — both definitive. Anything else must NOT drop the row.
func staleChatReason(m telego.ChatMember, err error) (string, bool) {
	if err != nil {
		var apiErr *telegoapi.Error
		if errors.As(err, &apiErr) && (apiErr.ErrorCode == 400 || apiErr.ErrorCode == 403) {
			return apiErr.Description, true
		}
		return "", false
	}
	if s := m.MemberStatus(); s == "left" || s == "kicked" {
		return "status " + s, true
	}
	return "", false
}

// checkAdminRights posts a setup hint into the chat when the bot was added
// without the rights it needs (restrict + delete), and a confirmation once
// the missing rights get granted. Quiet when nothing is wrong from the start.
func (b *Bot) checkAdminRights(upd *telego.ChatMemberUpdated) {
	missing := missingRights(upd.NewChatMember)
	if len(missing) > 0 {
		text := "⚠️ Мне не хватает прав, капча работать не будет.\nВыдай мне: " +
			strings.Join(missing, ", ") + "."
		if _, err := b.api.SendMessage(b.runCtx,
			tu.Message(tu.ID(upd.Chat.ID), text)); err != nil {
			b.log.Warn("send missing-rights hint", "err", err, "chat", upd.Chat.ID)
		}
		return
	}
	// Confirm only as a transition out of a broken state — not on every
	// unrelated promotion/permission change.
	if len(missingRights(upd.OldChatMember)) > 0 {
		if _, err := b.api.SendMessage(b.runCtx,
			tu.Message(tu.ID(upd.Chat.ID), "✅ Все нужные права на месте — я работаю.")); err != nil {
			b.log.Warn("send rights-ok confirmation", "err", err, "chat", upd.Chat.ID)
		}
	}
}

// missingRights lists human-readable admin rights the bot lacks for the
// captcha flow. A plain member lacks everything; an administrator may still
// miss individual toggles.
func missingRights(m telego.ChatMember) []string {
	switch v := m.(type) {
	case *telego.ChatMemberAdministrator:
		var missing []string
		if !v.CanRestrictMembers {
			missing = append(missing, "«Блокировка пользователей»")
		}
		if !v.CanDeleteMessages {
			missing = append(missing, "«Удаление сообщений»")
		}
		return missing
	case *telego.ChatMemberOwner:
		return nil
	case *telego.ChatMemberMember:
		return []string{"права администратора («Блокировка пользователей», «Удаление сообщений»)"}
	default:
		// restricted/left/banned states are handled elsewhere.
		return nil
	}
}

// onUserJoined is the common kickoff for both chat_member events and
// message.new_chat_members service messages. Safe to call multiple times
// for the same user — startCaptcha dedups via the in-memory store, and the
// join event is recorded only by the call that actually starts the captcha,
// so a join delivered through both update types counts once in stats.
// threadID is the forum topic the join was seen in (0 = none/General).
func (b *Bot) onUserJoined(chatID int64, chatTitle, chatType string, user telego.User, threadID int) {
	b.rememberChat(b.runCtx, storage.ChatInfo{
		ChatID: chatID,
		Title:  chatTitle,
		Type:   chatType,
	})
	if !b.startCaptcha(chatID, user, threadID) {
		// Duplicate delivery (chat_member + new_chat_members) — already counted.
		return
	}
	if err := b.db.RecordEvent(b.runCtx, chatID, user.ID, storage.EventJoin, time.Now()); err != nil {
		b.log.Warn("record join event", "err", err)
	}
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

// handleApproveCallback handles the "✅ Впустить" button on the captcha
// keyboard (callback data "capok:<userID>"). Chat admins and bot owners can
// approve a struggling human manually — same effect as a correct answer.
func (b *Bot) handleApproveCallback(ctx *th.Context, query telego.CallbackQuery) error {
	if query.Message == nil {
		return nil
	}
	targetUserID, err := strconv.ParseInt(strings.TrimPrefix(query.Data, "capok:"), 10, 64)
	if err != nil {
		_ = b.api.AnswerCallbackQuery(ctx, tu.CallbackQuery(query.ID))
		return nil
	}
	chatID := query.Message.GetChat().ID

	if !b.canManageChat(ctx, query.From.ID, chatID) {
		_ = b.api.AnswerCallbackQuery(ctx,
			tu.CallbackQuery(query.ID).
				WithText("Эта кнопка только для админов чата.").
				WithShowAlert())
		return nil
	}
	p, ok := b.store.Take(chatID, targetUserID)
	if !ok {
		_ = b.api.AnswerCallbackQuery(ctx,
			tu.CallbackQuery(query.ID).WithText("Капча уже не активна."))
		return nil
	}
	p.Cancel()
	_ = b.db.DeletePending(b.runCtx, chatID, targetUserID)
	_ = b.api.AnswerCallbackQuery(ctx,
		tu.CallbackQuery(query.ID).WithText("Пользователь впущен."))
	b.log.Info("captcha approved by admin",
		"chat", chatID, "user", targetUserID, "admin", query.From.ID)
	if err := b.onSuccess(b.runCtx, p); err != nil {
		b.log.Error("on success (admin approve)", "err", err, "chat", chatID, "user", targetUserID)
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
			// In forum supergroups the join service message lands in a topic;
			// send the captcha to the same one so the user actually sees it.
			threadID := 0
			if message.IsTopicMessage {
				threadID = message.MessageThreadID
			}
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
				b.onUserJoined(message.Chat.ID, message.Chat.Title, message.Chat.Type, nm, threadID)
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
	// Auto-forwarded posts from a linked channel arrive from the service user
	// 777000 ("Telegram", is_bot=false) — without this filter a rarely-posting
	// channel earns "silent returner" announcements and pollutes top-writer
	// stats.
	if message.From.ID == telegramServiceUserID || message.IsAutomaticForward {
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

	b.rememberChat(b.runCtx, storage.ChatInfo{
		ChatID: chatID,
		Title:  message.Chat.Title,
		Type:   message.Chat.Type,
	})
	b.rememberUser(b.runCtx, storage.UserInfo{
		UserID:    user.ID,
		FirstName: user.FirstName,
		LastName:  user.LastName,
		Username:  user.Username,
	})

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
	b.maybeSpamCheck(message)
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
	// Per-chat toggle; checked after the threshold so the settings query only
	// runs on the rare announce-worthy message, not on every message.
	if !b.chatSettings(b.runCtx, message.Chat.ID).SilentAnnounceEnabled {
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
		text = fmt.Sprintf("👀 Ого! %s вернулся(ась) после <b>%s</b> тишины.",
			mention, humanDaysGenRU(days))
	default:
		text = fmt.Sprintf("✨ %s снова с нами после <b>%s</b> молчания.",
			mention, humanDaysGenRU(days))
	}
	params := tu.Message(tu.ID(message.Chat.ID), text).
		WithParseMode(telego.ModeHTML).
		WithReplyParameters(&telego.ReplyParameters{MessageID: message.MessageID})
	if message.IsTopicMessage {
		params = params.WithMessageThreadID(message.MessageThreadID)
	}
	_, err := b.api.SendMessage(ctx, params)
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

// startCaptcha reports whether this call won the kickoff and actually started
// a captcha flow; false means one is already active or being set up for this
// user (duplicate join delivery) and the call was a no-op.
func (b *Bot) startCaptcha(chatID int64, user telego.User, threadID int) bool {
	// Race guard: chat_member events and message.new_chat_members can both
	// fire for the same join. Without a kickoff lock they race through the
	// pre-Put phase (restrict + send) and produce two captcha messages.
	if !b.store.BeginKickoff(chatID, user.ID) {
		b.log.Debug("captcha already in progress, skipping duplicate kickoff",
			"chat", chatID, "user", user.ID)
		return false
	}
	// Run the captcha flow asynchronously — it restricts immediately, then
	// sleeps for CaptchaDelay before sending the captcha message. During the
	// whole window handleGroupMessage deletes anything the user sends
	// (store.IsCaptchaActive returns true while inflight is held).
	go b.runCaptcha(chatID, user, threadID)
	return true
}

func (b *Bot) runCaptcha(chatID int64, user telego.User, threadID int) {
	defer b.store.FinishKickoff(chatID, user.ID)

	ctx := b.runCtx

	// Cache display name now — we'll need it when sending the greeting after a
	// successful pass (by then the user hasn't written anything, so user_info
	// wouldn't be populated from message-handling path).
	b.rememberUser(ctx, storage.UserInfo{
		UserID:    user.ID,
		FirstName: user.FirstName,
		LastName:  user.LastName,
		Username:  user.Username,
	})

	// Restrict FIRST — every second before this call is an open window for
	// join-and-post spam bots: the message would get deleted, but push
	// notifications have already gone out. The restriction itself is
	// invisible to the user, so it doesn't need the render delay below.
	if err := b.restrict(ctx, chatID, user.ID); err != nil {
		b.log.Error("restrict", "err", err, "chat", chatID, "user", user.ID)
		return
	}

	// Now give the user's client time to fully open the chat. Without this,
	// the captcha message sometimes doesn't merge into the user's
	// already-rendered view and they only see it after reopening the chat.
	if b.cfg.CaptchaDelay > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(b.cfg.CaptchaDelay):
		}
	}

	settings := b.chatSettings(ctx, chatID)
	mode := effectiveCaptchaMode(settings)
	ch := captcha.New(mode)
	captchaTimeout := b.effectiveCaptchaTimeout(settings)
	correct := ch.Correct()

	// Image mode: pre-render the photo. On any render failure fall back to
	// the text prompt — a captcha must always go out.
	var photo []byte
	if mode == captcha.ModeImage {
		var rerr error
		photo, rerr = captcha.RenderImage(correct)
		if rerr != nil {
			b.log.Warn("render image captcha, falling back to text",
				"err", rerr, "emoji", correct.Emoji)
		}
	}

	buttons := make([]telego.InlineKeyboardButton, 0, len(ch.Options))
	for i, c := range ch.Options {
		buttons = append(buttons,
			tu.InlineKeyboardButton(c.Emoji).
				WithCallbackData(fmt.Sprintf("cap:%d:%d", user.ID, i)))
	}
	kb := tu.InlineKeyboard(
		tu.InlineKeyboardRow(buttons...),
		tu.InlineKeyboardRow(
			tu.InlineKeyboardButton("✅ Впустить (для админов)").
				WithCallbackData(fmt.Sprintf("capok:%d", user.ID))),
	)

	var msg *telego.Message
	var err error
	if photo != nil {
		caption := fmt.Sprintf(
			"Привет, %s!\nДля защиты от спама выбери эмодзи, наиболее похожую на картинку, за %d секунд.",
			mentionHTML(user), int(captchaTimeout.Seconds()),
		)
		p := tu.Photo(tu.ID(chatID),
			tu.File(tu.NameReader(bytes.NewReader(photo), "captcha.png"))).
			WithCaption(caption).
			WithParseMode(telego.ModeHTML).
			WithReplyMarkup(kb)
		if threadID != 0 {
			p = p.WithMessageThreadID(threadID)
		}
		msg, err = b.api.SendPhoto(ctx, p)
	} else {
		text := fmt.Sprintf(
			"Привет, %s!\nДля защиты от спама выбери <b>%s</b> за %d секунд.",
			mentionHTML(user), correct.Prompt, int(captchaTimeout.Seconds()),
		)
		params := tu.Message(tu.ID(chatID), text).
			WithParseMode(telego.ModeHTML).
			WithReplyMarkup(kb)
		if threadID != 0 {
			params = params.WithMessageThreadID(threadID)
		}
		msg, err = b.api.SendMessage(ctx, params)
	}
	if err != nil {
		b.log.Error("send captcha", "err", err, "chat", chatID, "user", user.ID)
		_ = b.release(ctx, chatID, user.ID)
		return
	}

	expires := time.Now().Add(captchaTimeout)
	p := b.store.Put(chatID, user.ID, msg.MessageID, ch.CorrectIdx, expires, threadID)

	if err := b.db.PutPending(ctx, storage.PendingRow{
		ChatID:     chatID,
		UserID:     user.ID,
		MessageID:  msg.MessageID,
		CorrectIdx: ch.CorrectIdx,
		ExpiresAt:  expires,
		ThreadID:   threadID,
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
	b.maybeSendGreeting(ctx, p.ChatID, p.UserID, p.ThreadID)
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

	if count >= b.effectiveMaxAttempts(b.chatSettings(ctx, p.ChatID)) {
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
