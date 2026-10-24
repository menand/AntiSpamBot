package bot

import (
	"strings"

	"github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
	tu "github.com/mymmrac/telego/telegoutil"
)

// handleReplyWithBotCommand — /replywithbot <текст>: владелец бота отвечает
// на сообщение пользователя от имени бота. Сообщение с командой удаляется,
// текст публикуется реплаем на сообщение пользователя обычным текстом (без
// parse_mode — разметка/HTML не парсятся) и в том же топике форума.
//
// Команда только для OWNER_IDS: админ чата, пусть и проходит общие ворота
// modPrologue, говорить от имени бота не должен. Анонимного админа
// идентифицировать как владельца нельзя — ему тоже отказ.
//
// Эфемерный режим на текст не распространяется: смысл команды — сказать
// что-то всему чату от имени бота. Эфемерным (если режим включён) может быть
// только служебный отказ/ошибка этому владельцу.
func (b *Bot) handleReplyWithBotCommand(ctx *th.Context, message telego.Message) error {
	chatID, ok := b.modPrologue(ctx, message)
	if !ok {
		return nil
	}
	if !b.isOwner(message.From.ID) {
		b.refuseAndDelete(ctx, message, "🙅 Команда только для владельца бота.")
		return nil
	}

	target, ok := replyWithBotTarget(message.ReplyToMessage)
	if !ok {
		b.refuseAndDelete(ctx, message,
			"Ответь командой на сообщение пользователя — я опубликую текст от своего имени реплаем на него.")
		return nil
	}
	text := replyWithBotText(message.Text)
	if text == "" {
		b.refuseAndDelete(ctx, message, "Напиши текст после команды: /replywithbot <текст>.")
		return nil
	}

	// Сначала отправка, потом удаление команды: если отправка не удалась,
	// команда остаётся в чате — владельцу не придётся набирать текст заново.
	params := tu.Message(tu.ID(chatID), text).
		WithReplyParameters(&telego.ReplyParameters{
			MessageID:                target.MessageID,
			AllowSendingWithoutReply: true,
		})
	if message.IsTopicMessage {
		params = params.WithMessageThreadID(message.MessageThreadID)
	}
	if err := retryTG(b.runCtx, func() error {
		_, err := b.api.SendMessage(b.runCtx, params)
		return err
	}); err != nil {
		b.log.Warn("replywithbot: send", "err", err, "chat", chatID,
			"by", message.From.ID, "target", target.From.ID, "msg", target.MessageID)
		// Ответ владельцу: при включённом эфемерном режиме — только ему.
		b.sendPlainCtx(b.runCtx, chatID, threadOf(message), b.modReceiver(chatID, message),
			"Не получилось отправить — проверь мои права на отправку сообщений.")
		return nil
	}
	if err := b.deleteMessage(b.runCtx, chatID, message.MessageID); err != nil {
		b.log.Debug("delete replywithbot command", "err", err, "chat", chatID)
	}
	b.log.Info("reply with bot", "chat", chatID, "by", message.From.ID,
		"target", target.From.ID, "msg", target.MessageID)
	return nil
}

// replyWithBotTarget — валидация сообщения, на которое отвечает владелец.
// Нужно именно сообщение пользователя с реальным контентом: боты, каналы,
// сервисный «Telegram», автофорварды привязанного канала, неявный корень
// топика и сервисные события целью не считаются.
func replyWithBotTarget(m *telego.Message) (*telego.Message, bool) {
	if m == nil || m.From == nil {
		return nil, false
	}
	if m.ForumTopicCreated != nil || m.IsAutomaticForward {
		return nil, false
	}
	// Сообщение от имени канала/чата (sender_chat) — не рука пользователя.
	if m.SenderChat != nil && m.SenderChat.ID != 0 {
		return nil, false
	}
	if m.From.IsBot || m.From.ID <= 0 || m.From.ID == telegramServiceUserID {
		return nil, false
	}
	if !messageHasUserContent(m) {
		return nil, false
	}
	return m, true
}

// replyWithBotText берёт всё после первого токена команды, сохраняя переносы
// строк внутри текста. /replywithbot@ДругойБот сюда не доходит — его отсеивает
// modPrologue.commandForUs.
func replyWithBotText(text string) string {
	s := strings.TrimLeft(text, " \t")
	if !strings.HasPrefix(s, "/") {
		return ""
	}
	i := strings.IndexAny(s, " \t\r\n")
	if i < 0 {
		return ""
	}
	return strings.TrimSpace(s[i+1:])
}
