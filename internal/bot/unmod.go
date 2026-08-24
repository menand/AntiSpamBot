package bot

import (
	"fmt"
	"html"
	"strconv"
	"strings"
	"time"

	"github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
	tu "github.com/mymmrac/telego/telegoutil"

	"github.com/menand/AntiSpamBot/internal/storage"
)

// «Восстанавливающие» команды модерации: /unban, /unmute, /whitelist. Две
// формы у каждой: с целью (@username / text_mention / реплай — резолв тот же,
// что у /kick) — действие сразу; без цели — публичная плашка «10 последних» с
// кнопками выбора. Весь стейт выбора живёт в callback data (mc:<a>:<userID>) —
// pending-структур и TTL нет, рестарты безразличны, жать может любой админ.

// unmodListLimit — сколько юзеров показывает плашка выбора.
const unmodListLimit = 10

// unmodAction описывает одну команду: какие события формируют список
// «10 последних» и тексты плашки.
type unmodAction struct {
	kinds   []storage.EventKind
	reasons []string // пустой = любые причины
	title   string
	empty   string
}

var unmodActions = map[string]unmodAction{
	"u": {kinds: []storage.EventKind{storage.EventBan, storage.EventSpamBan},
		title: "🚫 Последние забаненные",
		empty: "Банов не помню — некого разбанивать."},
	"m": {kinds: []storage.EventKind{storage.EventMute},
		title: "🔇 Последние замьюченные",
		empty: "Мьютов не помню — некого размьючивать."},
	// «Провалившие капчу» — кики/баны с причиной captcha|noreply; спам-баны и
	// ручные /kick|/ban сюда не попадают.
	"w": {kinds: []storage.EventKind{storage.EventKick, storage.EventBan},
		reasons: []string{storage.ReasonCaptcha, storage.ReasonNoReply},
		title:   "🧩 Последние провалившие капчу",
		empty:   "Провалов капчи не помню — некого добавлять в доверенные."},
}

func (b *Bot) handleUnbanCommand(ctx *th.Context, message telego.Message) error {
	return b.handleUnmodCommand(ctx, message, "u")
}

func (b *Bot) handleUnmuteCommand(ctx *th.Context, message telego.Message) error {
	return b.handleUnmodCommand(ctx, message, "m")
}

func (b *Bot) handleWhitelistCommand(ctx *th.Context, message telego.Message) error {
	return b.handleUnmodCommand(ctx, message, "w")
}

func (b *Bot) handleUnmodCommand(ctx *th.Context, message telego.Message, action string) error {
	chatID, ok := b.modPrologue(ctx, message)
	if !ok {
		return nil
	}
	// Снятие мьюта — RestrictChatMember, он работает только в супергруппах
	// (тот же честный гейт, что у /mute).
	if action == "m" && message.Chat.Type != "supergroup" {
		b.refuseAndDelete(ctx, message, "Мьют работает только в супергруппах.")
		return nil
	}
	a := unmodActions[action]

	// Форма с целью: действие сразу, без плашки.
	if targetID, _, found := b.resolveModTarget(message); found {
		// Тот же гард, что у наказывающих команд: unban/unmute/whitelist —
		// тоже действия над юзером, и целиться ими в админа/владельца/себя
		// нельзя (release снял бы админу посторонние ограничения).
		if !b.guardModTarget(ctx, message, targetID) {
			return nil
		}
		if err := b.deleteMessage(b.runCtx, chatID, message.MessageID); err != nil {
			b.log.Debug("delete unmod command", "err", err, "chat", chatID)
		}
		text, err := b.execUnmod(action, chatID, targetID)
		if err != nil {
			b.log.Warn("unmod action failed", "err", err,
				"action", action, "chat", chatID, "target", targetID)
			b.sendPlain(chatID, threadOf(message), b.modReceiver(chatID, message),
				"Не получилось — проверь мои права.")
			return nil
		}
		b.log.Info("unmod command", "action", action, "chat", chatID,
			"target", targetID, "by", message.From.ID)
		b.sendHTML(chatID, threadOf(message), b.modReceiver(chatID, message), text)
		return nil
	}
	// @username в аргументе был, но резолв не удался — плашка тут сбила бы с
	// толку. Ответ реплаем (якорь — команда), затем команду сносим, как в
	// punishNonAdmin.
	if uname := firstUsernameArg(message.Text); uname != "" {
		b.replyTo(ctx, message, "Не знаю @"+uname+" — я запоминаю только тех, кого видел в чатах.")
		if err := b.deleteMessage(b.runCtx, chatID, message.MessageID); err != nil {
			b.log.Debug("delete unmod command", "err", err, "chat", chatID)
		}
		return nil
	}

	// Списочная форма: плашка «10 последних» с кнопками.
	recent, err := b.db.RecentEventUsers(b.runCtx, chatID, unmodListLimit, a.kinds, a.reasons)
	if err != nil {
		b.log.Warn("unmod: recent users", "err", err, "action", action, "chat", chatID)
		return nil
	}
	if err := b.deleteMessage(b.runCtx, chatID, message.MessageID); err != nil {
		b.log.Debug("delete unmod command", "err", err, "chat", chatID)
	}
	if len(recent) == 0 {
		b.sendPlain(chatID, threadOf(message), b.modReceiver(chatID, message), a.empty)
		return nil
	}

	ids := make([]int64, len(recent))
	for i, r := range recent {
		ids[i] = r.UserID
	}
	infos, err := b.db.GetUserInfos(b.runCtx, ids)
	if err != nil {
		b.log.Warn("unmod: user infos", "err", err, "chat", chatID)
		infos = map[int64]storage.UserInfo{}
	}
	text, kb := unmodListView(a.title, action, recent, infos)
	// Плашка ПУБЛИЧНАЯ намеренно (даже в эфемерном режиме): выбрать кнопкой
	// может любой админ чата — гейт в handleModChoiceCallback.
	params := tu.Message(tu.ID(chatID), text).
		WithParseMode(telego.ModeHTML).
		WithReplyMarkup(kb)
	if t := threadOf(message); t != 0 {
		params = params.WithMessageThreadID(t)
	}
	if _, err := b.api.SendMessage(b.runCtx, params); err != nil {
		b.log.Warn("send unmod list", "err", err, "chat", chatID)
	}
	return nil
}

// unmodListView — текст и клавиатура плашки выбора: нумерованный список,
// кнопки 1..N по 5 в ряд, ряд «Отмена».
func unmodListView(title, action string, recent []storage.RecentUser, infos map[int64]storage.UserInfo) (string, *telego.InlineKeyboardMarkup) {
	var sb strings.Builder
	sb.WriteString(title + ":\n")
	for i, r := range recent {
		fmt.Fprintf(&sb, "%d. %s — %s назад\n", i+1,
			mentionWithUsername(infos, r.UserID), humanDurationRU(time.Since(r.At)))
	}
	sb.WriteString("\nВыбери кнопкой, к кому применить.")

	var rows [][]telego.InlineKeyboardButton
	var row []telego.InlineKeyboardButton
	for i, r := range recent {
		row = append(row, tu.InlineKeyboardButton(strconv.Itoa(i+1)).
			WithCallbackData(fmt.Sprintf("mc:%s:%d", action, r.UserID)))
		if len(row) == 5 {
			rows = append(rows, row)
			row = nil
		}
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}
	rows = append(rows, tu.InlineKeyboardRow(
		tu.InlineKeyboardButton("✖️ Отмена").WithCallbackData("mc:x")))
	return sb.String(), &telego.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// execUnmod применяет действие к юзеру и возвращает HTML-строку итога.
// Все ветки идемпотентны — повторный клик или гонка двух админов безвредны.
func (b *Bot) execUnmod(action string, chatID, targetID int64) (string, error) {
	mention := b.mentionFor(targetID)

	// Та же дисциплина, что у /kick|/ban (cleanupTargetTraces): восстанавли-
	// вающая команда гасит активные проверки цели — иначе таймаут капчи
	// кикнул бы только что размученного/доверенного юзера, а reply-wait дал
	// бы фантомный noreply поверх решения админа. Событий не пишет.
	b.cancelCaptchaSilent(chatID, targetID)
	b.cancelReplyWait(chatID, targetID)
	switch action {
	case "u":
		if err := b.unban(b.runCtx, chatID, targetID); err != nil {
			return "", err
		}
		// Наш собственный UnbanChatMember не проходит forgiveness-хук
		// handleChatMember (тот игнорирует события от самого бота) — снимаем
		// глобальный флаг спамера сами, иначе join-хук банил бы заново.
		if _, err := b.db.DeleteSpamBanned(b.runCtx, targetID); err != nil {
			b.log.Warn("unban: delete spam banned", "err", err, "user", targetID)
		}
		if err := b.db.ResetAttempts(b.runCtx, chatID, targetID); err != nil {
			b.log.Warn("unban: reset attempts", "err", err, "chat", chatID, "user", targetID)
		}
		return "♻️ " + mention + " разбанен — может зайти заново.", nil
	case "m":
		// Снятие мьюта = вернуть дефолтные права чата — ровно то, что release
		// делает после пройденной капчи.
		if err := b.release(b.runCtx, chatID, targetID); err != nil {
			return "", err
		}
		return "🔊 " + mention + " снова может писать.", nil
	case "w":
		if err := b.db.AddTrusted(b.runCtx, chatID, targetID, time.Now()); err != nil {
			return "", err
		}
		// Провал капчи мог кончиться перманентным баном — снимаем (no-op без
		// бана), иначе «доверенный» не смог бы даже войти.
		if err := b.unban(b.runCtx, chatID, targetID); err != nil {
			b.log.Warn("whitelist: unban", "err", err, "chat", chatID, "user", targetID)
		}
		if err := b.db.ResetAttempts(b.runCtx, chatID, targetID); err != nil {
			b.log.Warn("whitelist: reset attempts", "err", err, "chat", chatID, "user", targetID)
		}
		return "🤝 " + mention + " в доверенных — войдёт без капчи.", nil
	}
	return "", fmt.Errorf("unknown unmod action %q", action)
}

// handleModChoiceCallback — кнопки плашек /unban|/unmute|/whitelist:
// «mc:<a>:<userID>» — применить действие, «mc:x» — закрыть плашку.
func (b *Bot) handleModChoiceCallback(ctx *th.Context, query telego.CallbackQuery) error {
	if query.Message == nil {
		return nil
	}
	chatID := query.Message.GetChat().ID
	// Мёртвый чат (отклонён/кикнут/вне ALLOWED_CHATS) кнопками не
	// обслуживается — как и все остальные пути.
	if !b.chatServiceable(chatID) {
		_ = b.api.AnswerCallbackQuery(ctx,
			tu.CallbackQuery(query.ID).WithText("Чат больше не обслуживается.").WithShowAlert())
		return nil
	}
	if !b.canManageChat(ctx, query.From.ID, chatID) {
		_ = b.api.AnswerCallbackQuery(ctx, tu.CallbackQuery(query.ID).
			WithText("Эти кнопки только для админов чата.").WithShowAlert())
		return nil
	}
	if query.Data == "mc:x" {
		_ = b.api.AnswerCallbackQuery(ctx, tu.CallbackQuery(query.ID))
		if err := b.deleteMessage(b.runCtx, chatID, query.Message.GetMessageID()); err != nil {
			b.log.Debug("delete unmod list", "err", err, "chat", chatID)
		}
		return nil
	}
	action, targetID, ok := parseModChoice(query.Data)
	if !ok {
		_ = b.api.AnswerCallbackQuery(ctx, tu.CallbackQuery(query.ID))
		return nil
	}
	// Лёгкий гард цели без якоря-команды (кнопка): не бот, не сам нажавший,
	// не админ/владелец. Отказ — тостом: реплаять не на что.
	switch {
	case b.me != nil && targetID == b.me.ID:
		_ = b.api.AnswerCallbackQuery(ctx, tu.CallbackQuery(query.ID).
			WithText("Себя трогать не дам.").WithShowAlert())
		return nil
	case targetID == query.From.ID:
		_ = b.api.AnswerCallbackQuery(ctx, tu.CallbackQuery(query.ID).
			WithText("Себя-то за что?").WithShowAlert())
		return nil
	case b.canManageChat(ctx, targetID, chatID):
		_ = b.api.AnswerCallbackQuery(ctx, tu.CallbackQuery(query.ID).
			WithText("Это админ — не трону.").WithShowAlert())
		return nil
	}

	text, err := b.execUnmod(action, chatID, targetID)
	if err != nil {
		b.log.Warn("unmod choice failed", "err", err,
			"action", action, "chat", chatID, "target", targetID)
		_ = b.api.AnswerCallbackQuery(ctx, tu.CallbackQuery(query.ID).
			WithText("Не получилось — проверь мои права.").WithShowAlert())
		return nil
	}
	_ = b.api.AnswerCallbackQuery(ctx, tu.CallbackQuery(query.ID).WithText("Готово."))
	b.log.Info("unmod choice", "action", action, "chat", chatID,
		"target", targetID, "by", query.From.ID)
	// Итог редактируется в саму плашку, клавиатура снимается. Гонка двух
	// админов даёт повторный edit тем же текстом — isNotModified глушим.
	result := text + "\n<i>(" + html.EscapeString(userLabel(query.From)) + ")</i>"
	if _, err := b.api.EditMessageText(b.runCtx, &telego.EditMessageTextParams{
		ChatID:    tu.ID(chatID),
		MessageID: query.Message.GetMessageID(),
		Text:      result,
		ParseMode: telego.ModeHTML,
	}); err != nil && !isNotModified(err) {
		b.log.Warn("edit unmod list result", "err", err, "chat", chatID)
	}
	return nil
}

// parseModChoice разбирает callback data «mc:<a>:<userID>».
func parseModChoice(data string) (action string, targetID int64, ok bool) {
	parts := strings.Split(data, ":")
	if len(parts) != 3 || parts[0] != "mc" {
		return "", 0, false
	}
	if _, known := unmodActions[parts[1]]; !known {
		return "", 0, false
	}
	id, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return "", 0, false
	}
	return parts[1], id, true
}
