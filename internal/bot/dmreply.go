package bot

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegoapi"
	tu "github.com/mymmrac/telego/telegoutil"

	"github.com/menand/AntiSpamBot/internal/storage"
)

// ownerDMReplyHint — подсказка под форвардом/карточкой в ЛС. Без неё фича
// неоткрываема: владелец не догадается, что достаточно нажать «Ответить» и
// написать текст, никакой команды не нужно.
const ownerDMReplyHint = "\n\n↩️ Ответь на это сообщение — опубликую твой текст в чате от моего имени."

// ownerDMLinkTTL — сколько живёт связка «ЛС-сообщение → сообщение в чате».
// Сообщения в ЛС не протухают, но карточка месячной давности — уже не повод
// публиковать что-то «от имени бота» задним числом.
const ownerDMLinkTTL = 30 * 24 * time.Hour

// ownerDMQuoteLimit — предел Bot API на частичную цитату: 1024 UTF-16 единицы
// после разбора entities (у рун с эмодзи это меньше рун).
const ownerDMQuoteLimit = 1024

// ownerDMMessageLimit — предел Bot API на текст сообщения в UTF-16 единицах.
// Проверяем до отправки: иначе владелец платит полной лестницей ретраев за
// заведомо отвергаемый текст и получает ложный диагноз «проверь права».
const ownerDMMessageLimit = 4096

// ownerReplyResult — чем закончилась публикация: от этого зависит честная
// отбивка в ЛС (цитата могла не вставиться, исходник — не найтись).
type ownerReplyResult int

const (
	// ownerReplyQuoted — ушло реплаем с частичной цитатой владельца.
	ownerReplyQuoted ownerReplyResult = iota
	// ownerReplyFull — ушло реплаем на исходное сообщение, но без частичной
	// цитаты (её не было, она не влезла в лимит или оригинал отредактировали).
	ownerReplyFull
	// ownerReplyBare — реплай не прикрепился: исходное сообщение удалено.
	ownerReplyBare
)

// ownerQuoteDrop — почему выделенный фрагмент не уедет с ответом. Нужен для
// честной отбивки: владелец явно нажал «Цитировать», и молча потерять
// выделение — соврать о том, что ушло в чат.
type ownerQuoteDrop int

const (
	// ownerQuoteKept — цитата уходит как есть.
	ownerQuoteKept ownerQuoteDrop = iota
	// ownerQuoteAbsent — владелец ничего не выделял.
	ownerQuoteAbsent
	// ownerQuoteFromCard — ответ на карточку: выделенный текст относится к
	// карточке, а не к исходному сообщению, — такая цитата не найдётся.
	ownerQuoteFromCard
	// ownerQuoteTooLong — выделенный фрагмент длиннее лимита Bot API;
	// обрезать нельзя, обрезанная цитата соврала бы о выделении.
	ownerQuoteTooLong
	// ownerQuoteEmpty — выделение из одних пробелов/переводов строк.
	ownerQuoteEmpty
)

// handleOwnerDMReply — владелец ответил реплаем на служебное сообщение бота в
// ЛС (форвард ответа новичка, подозрение на спам или их карточки): текст
// публикуется в исходном чате от имени бота реплаем на исходное сообщение.
// true — апдейт поглощён (дальше его обрабатывать нечего).
//
// Никаких команд: жест «Ответить» и текст — это и есть весь протокол. Работает
// только для владельца и только для сообщений, которые ЭТОТ бот сам связал с
// чатом (owner_dm_links, ключ включает bot_id) — иначе обычное личное
// сообщение осталось бы обычным. Ответ на живой промпт «пришли текст
// приветствия» — не сюда: у промпта намеренно нет связки, его пропускает
// greetingInputPromptMatch.
//
// Каждый провал получает отбивку: молчание здесь опаснее ошибки — владелец
// уверен, что ответил в чат, а без отбивки текст ещё и уйдёт в флоу ввода
// текста приветствия и станет шаблоном чата.
func (b *Bot) handleOwnerDMReply(ctx context.Context, message telego.Message) bool {
	if message.From == nil || message.From.IsBot || message.Chat.Type != telego.ChatTypePrivate {
		return false
	}
	if message.ReplyToMessage == nil || !b.isOwner(message.From.ID) {
		return false
	}
	if b.me == nil {
		// Без собственного id нельзя ни прочитать связку (она привязана к
		// боту), ни отличить наше сообщение от чужого. В рантайме b.me
		// загружен до приёма апдейтов; в тестах — выставляется явно.
		return false
	}
	// Telegram Business и прочие private-чаты: id сообщений локальны для чата,
	// а связка адресуется парой (owner_id, dm_message_id). При chat.id !=
	// from.id чужой «личный» чат мог бы совпасть с нашей личкой по id и
	// отправить текст не туда.
	if message.Chat.ID != message.From.ID {
		return false
	}
	dmChatID := message.Chat.ID

	// Чтение связки — на runCtx: это не ответ пользователю, а работа с БД
	// (та же дисциплина, что у остальных DB-чтений в хендлерах).
	link, ok, err := b.db.OwnerDMLink(b.runCtx, b.me.ID, message.From.ID, message.ReplyToMessage.MessageID)
	if err != nil {
		b.log.Warn("owner dm link", "err", err,
			"owner", message.From.ID, "dm", message.ReplyToMessage.MessageID)
		b.ownerDMNotice(ctx, dmChatID,
			"⚠️ Не смог прочитать, к какому чату относится ответ, — ничего не отправил. Попробуй ещё раз.")
		return true
	}
	if !ok {
		// Ответ на промпт «пришли мне текст приветствия» — это штатный ответ
		// флоу, который сам бот и открыл. У промпта нет и не должно быть
		// owner_dm_links; съесть такой текст отбивкой «связка устарела» значит
		// молча потерять шаблон, который админ только что сочинил. Протухший
		// промпт тоже уходит во флоу: он честно скажет «запрос устарел, текст
		// не сохранён», а не соврёт про связку.
		if fresh, expired := b.greetingInputPromptMatch(message.From.ID, message.ReplyToMessage.MessageID); fresh || expired {
			return false
		}
		// Реплай на наше собственное сообщение, но связки нет: карточка старше
		// 30 дней, отправлена до обновления или запись линка не удалась.
		// Молчать нельзя — владелец считает, что ответил в чат.
		if message.ReplyToMessage.From != nil &&
			message.ReplyToMessage.From.ID == b.me.ID {
			b.ownerDMNotice(ctx, dmChatID,
				"↩️ Не знаю, в какой чат это опубликовать: связка с этим сообщением устарела или не сохранилась. Ответь на свежее уведомление.")
			return true
		}
		return false
	}

	text := strings.TrimSpace(message.Text)
	if text == "" {
		// Подпись медиа — тоже текст владельца; сам файл не публикуем.
		text = strings.TrimSpace(message.Caption)
	}
	if text == "" {
		b.ownerDMNotice(ctx, dmChatID,
			"⚠️ Публикую только текст: пришли ответ словами (стикеры и медиа не переношу).")
		return true
	}
	// Зарегистрированные команды сюда не доходят — их матчат свои хендлеры
	// (порядок в bot.go). Здесь «/…» — почти наверняка случайный /cancel:
	// публиковать такое в чат от имени бота нельзя.
	if strings.HasPrefix(text, "/") {
		b.ownerDMNotice(ctx, dmChatID,
			"⚠️ Это похоже на команду — в чат ничего не отправил. Пришли ответ обычным текстом.")
		return true
	}
	if utf16Units(text) > ownerDMMessageLimit {
		b.ownerDMNotice(ctx, dmChatID,
			"⚠️ Ответ длиннее лимита Telegram (4096 символов) — сократи и пришли ещё раз.")
		return true
	}

	// chatServiceableChecked, а не chatServiceable: транзиентная ошибка БД —
	// это «не смог прочитать», а не «чат больше не обслуживается»; терять
	// набранный текст с ложным диагнозом нельзя.
	serviceable, chkErr := b.chatServiceableChecked(b.runCtx, link.ChatID)
	if chkErr != nil {
		b.log.Warn("owner dm reply chat status", "err", chkErr, "chat", link.ChatID,
			"owner", message.From.ID)
		b.ownerDMNotice(ctx, dmChatID,
			"⚠️ Не смог прочитать статус чата — ничего не отправил. Попробуй ещё раз.")
		return true
	}
	if !serviceable {
		b.ownerDMNotice(ctx, dmChatID, "⚠️ Чат больше не обслуживается — ответ не отправлен.")
		return true
	}

	quote, drop := ownerDMQuote(message, link)
	result, sendErr := b.sendOwnerChatReply(link, text, quote)
	chatLabel := b.chatLink(b.runCtx, link.ChatID)
	switch {
	case sendErr != nil:
		b.log.Warn("owner dm reply send", "err", sendErr, "chat", link.ChatID,
			"owner", message.From.ID, "target", link.SourceMsgID)
		b.ownerDMNotice(ctx, dmChatID, "❌ Не получилось опубликовать в "+chatLabel+": "+
			ownerSendFailureReason(sendErr)+".")
	case result == ownerReplyBare:
		b.ownerDMNotice(ctx, dmChatID,
			"✅ Опубликовано в "+chatLabel+" без реплая: исходное сообщение удалено.")
	case result != ownerReplyQuoted && quote != nil:
		b.ownerDMNotice(ctx, dmChatID, "✅ Опубликовано в "+chatLabel+
			" с реплаем, но без цитаты: похоже, исходное сообщение отредактировали.")
	case drop == ownerQuoteFromCard:
		b.ownerDMNotice(ctx, dmChatID, "✅ Опубликовано в "+chatLabel+
			". Цитату не перенёс: выделять можно текст пересланного сообщения, а не карточки.")
	case drop == ownerQuoteTooLong:
		b.ownerDMNotice(ctx, dmChatID, "✅ Опубликовано в "+chatLabel+
			" с реплаем, но без цитаты: выделенный фрагмент длиннее 1024 символов.")
	default:
		b.ownerDMNotice(ctx, dmChatID, "✅ Опубликовано в "+chatLabel+".")
	}
	b.log.Info("owner dm reply", "chat", link.ChatID, "owner", message.From.ID,
		"target", link.SourceMsgID, "quote", result == ownerReplyQuoted)
	return true
}

// linkOwnerDMSource запоминает связку для только что отправленного в ЛС
// сообщения. dmMsgID == 0 (отправка не удалась) — связывать нечего.
func (b *Bot) linkOwnerDMSource(ownerID int64, dmMsgID int, chatID int64,
	threadID, sourceMsgID int, isForward bool) {
	if dmMsgID <= 0 || b.me == nil {
		return
	}
	if err := b.db.PutOwnerDMLink(b.runCtx, b.me.ID, ownerID, dmMsgID, chatID, threadID,
		sourceMsgID, isForward, time.Now()); err != nil {
		b.log.Warn("link owner dm message", "err", err, "owner", ownerID,
			"dm", dmMsgID, "chat", chatID, "target", sourceMsgID)
	}
}

// ownerDMQuote достаёт частичную цитату из ответа владельца и честно называет
// причину, по которой она не поедет (для отбивки). Применяется только к
// линку-форварду: у карточки цитата была бы фрагментом самой карточки, а не
// исходного сообщения (такая цитата не найдётся в оригинале и отправка
// провалится).
func ownerDMQuote(m telego.Message, link storage.OwnerDMLink) (*telego.TextQuote, ownerQuoteDrop) {
	if m.Quote == nil {
		return nil, ownerQuoteAbsent
	}
	if !link.IsForward {
		return nil, ownerQuoteFromCard
	}
	if strings.TrimSpace(m.Quote.Text) == "" {
		return nil, ownerQuoteEmpty
	}
	// Слишком длинная цитата отбрасывается целиком — обрезанная соврала бы о
	// том, что именно выделил владелец.
	if utf16Units(m.Quote.Text) > ownerDMQuoteLimit {
		return nil, ownerQuoteTooLong
	}
	return m.Quote, ownerQuoteKept
}

// sendOwnerChatReply публикует текст в исходном чате. Цепочка фолбэков:
// с частичной цитатой → полным реплаем → без реплая (оригинал удалён).
// Порядок важен: каждый следующий шаг теряет часть контекста, и теряем мы её
// только после того, как предыдущий шаг реально провалился ИЗ-ЗА контекста
// (удалённый оригинал, отредактированная цитата). Любая другая ошибка —
// права, лимиты, закрытый топик, сеть — фолбэки не лечит: возвращаем сразу,
// не гоняя три лестницы ретраев по заведомо неверным параметрам.
func (b *Bot) sendOwnerChatReply(link storage.OwnerDMLink, text string,
	quote *telego.TextQuote) (ownerReplyResult, error) {
	if quote != nil {
		if _, err := b.sendOwnerReplyOnce(link, text, quote, false); err == nil {
			return ownerReplyQuoted, nil
		} else if !ownerReplyContextLost(err) {
			return ownerReplyFull, err
		} else {
			b.log.Debug("owner dm reply: quote not attached", "err", err,
				"chat", link.ChatID, "target", link.SourceMsgID)
		}
	}
	_, err := b.sendOwnerReplyOnce(link, text, nil, false)
	if err == nil {
		return ownerReplyFull, nil
	}
	if !ownerReplyContextLost(err) {
		return ownerReplyFull, err
	}
	b.log.Debug("owner dm reply: reply target lost", "err", err,
		"chat", link.ChatID, "target", link.SourceMsgID)
	// Исходник мог быть удалён: последняя попытка — без обязательного реплая.
	sent, bareErr := b.sendOwnerReplyOnce(link, text, nil, true)
	if bareErr != nil {
		return ownerReplyFull, bareErr
	}
	// Telegram мог прицепить реплай и сам (оригинал жив) — тогда «без реплая»
	// было бы враньём.
	if sent != nil && sent.ReplyToMessage == nil {
		return ownerReplyBare, nil
	}
	return ownerReplyFull, nil
}

// sendOwnerReplyOnce — одна отправка с реплай-параметрами. allowWithoutReply
// снимает телеграмное «message to be replied not found» — это последний
// фолбэк для удалённого оригинала.
func (b *Bot) sendOwnerReplyOnce(link storage.OwnerDMLink, text string,
	quote *telego.TextQuote, allowWithoutReply bool) (*telego.Message, error) {
	rp := &telego.ReplyParameters{
		MessageID:                link.SourceMsgID,
		AllowSendingWithoutReply: allowWithoutReply,
	}
	if quote != nil {
		rp.Quote = quote.Text
		rp.QuoteEntities = quote.Entities
		if quote.Position > 0 {
			rp.QuotePosition = quote.Position
		}
	}
	params := tu.Message(tu.ID(link.ChatID), text).WithReplyParameters(rp)
	if link.ThreadID != 0 {
		params = params.WithMessageThreadID(link.ThreadID)
	}
	var sent *telego.Message
	if err := retryTG(b.runCtx, func() error {
		var err error
		sent, err = b.api.SendMessage(b.runCtx, params)
		return err
	}); err != nil {
		return nil, err
	}
	return sent, nil
}

// ownerReplyContextLost — ошибка означает именно потерю реплай-контекста
// (оригинал удалён либо цитата не нашлась), а не невозможность отправки.
// Только такие ошибки оправдывают следующий шаг фолбэка.
func ownerReplyContextLost(err error) bool {
	var apiErr *telegoapi.Error
	if !errors.As(err, &apiErr) {
		return false
	}
	d := strings.ToLower(apiErr.Description)
	return strings.Contains(d, "message to be replied not found") ||
		strings.Contains(d, "replied message not found") ||
		strings.Contains(d, "message_id_invalid") ||
		strings.Contains(d, "quote")
}

// ownerSendFailureReason — человеческая причина провала для отбивки. Врать
// «проверь права» на «message is too long» нельзя: владелец пойдёт чинить то,
// что не сломано.
func ownerSendFailureReason(err error) string {
	var apiErr *telegoapi.Error
	if !errors.As(err, &apiErr) {
		return "сетевая ошибка, попробуй ещё раз"
	}
	d := strings.ToLower(apiErr.Description)
	switch {
	case strings.Contains(d, "message is too long"):
		return "текст длиннее лимита Telegram (4096 символов)"
	case strings.Contains(d, "topic_closed"), strings.Contains(d, "topic closed"):
		return "топик закрыт"
	case strings.Contains(d, "not enough rights"), strings.Contains(d, "have no rights"),
		strings.Contains(d, "chat_write_forbidden"):
		return "у меня нет прав на отправку"
	case strings.Contains(d, "chat not found"), strings.Contains(d, "bot was kicked"),
		strings.Contains(d, "user is deactivated"):
		return "чат недоступен"
	}
	return "проверь мои права на отправку и попробуй ещё раз"
}

// ownerDMNotice — короткая служебная отбивка владельцу. Экран screenNotice:
// повторная отбивка заменяет прошлую, панели и флоу не трогает. Превью
// ссылки на чат отключено: это строка статуса, а не карточка чата.
func (b *Bot) ownerDMNotice(ctx context.Context, userID int64, text string) {
	params := tu.Message(tu.ID(userID), text).
		WithParseMode(telego.ModeHTML).
		WithLinkPreviewOptions(&telego.LinkPreviewOptions{IsDisabled: true})
	if _, err := b.sendScreenParams(ctx, userID, screenNotice, params); err != nil {
		b.log.Warn("send owner dm notice", "err", err, "user", userID)
	}
}
