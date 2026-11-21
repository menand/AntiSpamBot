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

	"github.com/menand/AntiSpamBot/internal/storage"
)

// «Восстанавливающие» команды модерации: /unban, /fullunban, /unmute,
// /whitelist. Две формы у каждой: с целью (@username / text_mention / реплай —
// резолв тот же, что у /kick) — действие сразу; без цели — публичная плашка
// «10 последних» с кнопками выбора. Весь стейт выбора живёт в callback data
// (mc:<a>:<userID>) — pending-структур и TTL нет, рестарты безразличны, жать
// может любой админ.

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
	// Полный разбан: тот же список источников, что у «u», но действие
	// снимает баны во ВСЕХ чатах бота и чистит глобальную базу спамеров.
	"f": {kinds: []storage.EventKind{storage.EventBan, storage.EventSpamBan},
		title: "🌍 Последние забаненные (полный разбан)",
		empty: "Банов не помню — некого полностью разбанивать."},
	"m": {kinds: []storage.EventKind{storage.EventMute},
		title: "🔇 Последние замьюченные",
		empty: "Мьютов не помню — некого размьючивать."},
	// «Провалившие проверку входа» — кики/баны с причиной captcha|noreply
	// (noreply = режим «ответь на приветствие»); спам-баны и ручные
	// /kick|/ban сюда не попадают.
	"w": {kinds: []storage.EventKind{storage.EventKick, storage.EventBan},
		reasons: []string{storage.ReasonCaptcha, storage.ReasonNoReply},
		title:   "🧩 Последние провалившие проверку входа",
		empty:   "Проваливших проверку входа не помню — некого добавлять в доверенных."},
}

func (b *Bot) handleUnbanCommand(ctx *th.Context, message telego.Message) error {
	return b.handleUnmodCommand(ctx, message, "u")
}

// handleFullunbanCommand — /fullunban: снять баны юзера во ВСЕХ чатах бота,
// вычеркнуть его из глобальной базы спамеров и сбросить попытки капчи везде.
// Форма и гейты те же, что у остальных восстанавливающих команд.
func (b *Bot) handleFullunbanCommand(ctx *th.Context, message telego.Message) error {
	return b.handleUnmodCommand(ctx, message, "f")
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
	// (тот же честный гейт, что у /mute). Просили же РАЗМьютить — копия
	// «Мьют работает…» сбивала с толку.
	if action == "m" && message.Chat.Type != "supergroup" {
		b.refuseAndDelete(ctx, message, "Мьют и размьют работают только в супергруппах.")
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
		var full fullUnbanRequest
		text, err := b.execUnmod(action, chatID, targetID, &full)
		if err != nil {
			b.log.Warn("unmod action failed", "err", err,
				"action", action, "chat", chatID, "target", targetID)
			if text == "" {
				text = "Не получилось — проверь мои права."
			}
			b.sendPlain(chatID, threadOf(message), b.modReceiver(chatID, message), text)
			return nil
		}
		b.log.Info("unmod command", "action", action, "chat", chatID,
			"target", targetID, "by", message.From.ID)
		requesterID := message.From.ID
		if message.From.IsBot {
			requesterID = 0 // Telegram does not supply the anonymous admin's ID.
		}
		if action == "f" {
			text += fullUnbanAnnouncement(requesterID)
		}
		b.sendHTML(chatID, threadOf(message), b.modReceiver(chatID, message), text)
		if action == "f" {
			b.startFullUnbanFanout(targetID, requesterID, full)
		}
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
	var row []telego.InlineKeyboardButton //nolint:prealloc // row is built incrementally with dynamic grouping
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
func (b *Bot) execUnmod(action string, chatID, targetID int64, full *fullUnbanRequest) (string, error) {
	if action == storage.AdmissionUnban || action == storage.AdmissionWhitelist {
		return b.execPersistentUnmod(action, chatID, targetID)
	}
	if action == "f" {
		ctx, cancel := b.liveCtx()
		defer cancel()
		return b.execFullUnban(ctx, chatID, targetID, true, full)
	}
	unlock, err := b.rights.lock(b.runCtx, chatID, targetID, false)
	if err != nil {
		return "", err
	}
	defer unlock()

	if err := b.checkManualMute(b.runCtx, chatID, targetID); err != nil {
		return "Бот сверяет предыдущий мут. Повторите команду после сверки.", err
	}
	mention := b.mentionFor(targetID)

	// Та же дисциплина, что у /kick|/ban (cleanupTargetTraces): восстанав-
	// ливающая команда гасит активные проверки цели — иначе таймаут капчи
	// кикнул бы только что размученного/доверенного юзера, а reply-wait дал
	// бы фантомный noreply поверх решения админа.
	captchaLive := b.cancelCaptchaSilent(chatID, targetID)
	replyLive := b.cancelReplyWait(chatID, targetID)
	if captchaLive {
		// Гашение капчи само по себе капча-мьют НЕ снимает — без release
		// юзер, сидевший за пристом, остался бы замьюченным навсегда
		// (доверенность и unban рестрикт не трогают). Release идемпотентен:
		// ветка «m» ниже при необходимости отпустит права ещё раз.
		if err := b.release(b.runCtx, chatID, targetID); err != nil {
			b.log.Warn("unmod: release captcha mute", "err", err, "chat", chatID, "target", targetID)
		}
	}
	// Закрывающий pass — только если сняли именно живую проверку (иначе
	// join без терминального события навсегда остался бы в «В процессе»):
	// пройти проверку юзер уже не может — прецедент /mute и
	// releaseMigratedCaptchas. Капча и reply-wait одновременно активны не
	// бывают, pass пишется максимум один.
	if captchaLive || replyLive {
		if err := b.db.RecordEvent(b.runCtx, chatID, targetID, storage.EventPass, time.Now(), ""); err != nil {
			b.log.Warn("record pass event (unmod)", "err", err)
		}
	}
	switch action {
	case "m":
		// Снятие мьюта = вернуть дефолтные права чата — ровно то, что release
		// делает после пройденной капчи.
		if err := b.release(b.runCtx, chatID, targetID); err != nil {
			return "", err
		}
		// Карантин снимаем строкой/зеркалом (release выше уже вернул
		// дефолтные права — серверный release карантина не нужен): иначе
		// пережившая мьют строка после размьюта воскресила бы текстовый
		// рестрикт до его until_date из ниоткуда.
		b.dropQuarantineState(chatID, targetID)
		return "🔊 " + mention + " снова может писать.", nil

	}
	return "", fmt.Errorf("unknown unmod action %q", action)
}

func fullUnbanAnnouncement(requesterID int64) string {
	if requesterID <= 0 {
		return "\nЗапускаю обход остальных чатов. Для анонимной команды личный отчёт недоступен."
	}
	return "\nЗапускаю обход остальных чатов. Итог отправлю в ЛС."
}

type fullUnbanRequest struct {
	chats    []storage.ChatInfo
	pardonID int64
	listErr  error
}

func (b *Bot) startFullUnbanFanout(userID, requesterID int64, full fullUnbanRequest) {
	b.goSafe("fullunbanFanout", func() {
		b.fullUnbanEverywhere(userID, requesterID, full)
	})
}

func (b *Bot) fullUnbanEverywhere(userID, requesterID int64, full fullUnbanRequest) {
	if full.listErr != nil {
		b.log.Warn("fullunban: list chats", "err", full.listErr)
		b.sendFullUnbanResult(requesterID, "Не удалось получить список чатов для обхода.")
		return
	}
	chats := full.chats
	results := make([]string, len(chats))
	for i, chat := range chats {
		results[i] = b.fullUnbanChat(chat.ChatID, userID, full.pardonID)
	}
	if requesterID <= 0 {
		return
	}
	var lines []string
	for i, chat := range chats {
		if !b.isOwner(requesterID) {
			admin, sure := b.isChatAdminFresh(b.runCtx, chat.ChatID, requesterID)
			if !sure || !admin {
				continue
			}
		}
		chat.Title = truncateLabel(titleOrID(chat), 60)
		lines = append(lines, chatLinkHTML(chat)+" — "+results[i])
	}
	if len(lines) == 0 {
		b.sendFullUnbanResult(requesterID, "Обход завершён. В отчёт входят только чаты, которыми вы управляете.")
		return
	}
	header := "🌍 <b>Итоги обхода для " + b.mentionFor(userID) + "</b>\n"
	var body strings.Builder
	body.WriteString(header)
	budget := reportRuneBudget
	for _, line := range lines {
		size := len([]rune(line)) + 1
		if size > budget {
			b.sendFullUnbanResult(requesterID, body.String())
			body.Reset()
			body.WriteString(header)
			budget = reportRuneBudget
		}
		body.WriteByte('\n')
		body.WriteString(line)
		budget -= size
	}
	b.sendFullUnbanResult(requesterID, body.String())
}

func (b *Bot) fullUnbanChat(chatID, userID, pardonID int64) string {
	text, err := b.execFullUnban(b.runCtx, chatID, userID, false, &fullUnbanRequest{pardonID: pardonID})
	if err != nil {
		b.log.Warn("fullunban: cross-chat unban", "err", err, "chat", chatID, "user", userID)
	}
	return text
}

func (b *Bot) execFullUnban(ctx context.Context, chatID, userID int64, origin bool, full *fullUnbanRequest) (string, error) {
	unlock, err := b.rights.lock(ctx, chatID, userID, false)
	if err != nil {
		return "проверка прервана", err
	}
	defer unlock()
	allowed, err := b.chatServiceableChecked(ctx, chatID)
	if err != nil {
		return "не удалось проверить доступ к чату", err
	}
	if !allowed {
		return "чат больше не обслуживается", fmt.Errorf("chat %d no longer serviceable", chatID)
	}
	if err := b.checkManualMute(ctx, chatID, userID); err != nil {
		return "Бот сверяет предыдущий мут. Повторите команду после сверки.", err
	}
	action := storage.AdmissionUnbanChat
	var wasSpamBanned bool
	var otherChats []int64
	if origin {
		action = storage.AdmissionUnban
		full.chats, full.listErr = b.serviceableGroupChatsExcept(chatID)
		otherChats = make([]int64, len(full.chats))
		for i, chat := range full.chats {
			otherChats[i] = chat.ChatID
		}
		wasSpamBanned, err = b.db.IsSpamBanned(ctx, userID)
		if err != nil {
			b.log.Warn("fullunban: read spam banned", "err", err, "user", userID)
		}
	} else {
		if full.pardonID > 0 {
			newer, err := b.db.NewerSpamDecision(ctx, chatID, userID, full.pardonID)
			if err != nil {
				return "не удалось сверить спам-вердикты", err
			}
			if newer {
				return "действует более свежий спам-вердикт", nil
			}
		}
		// В чужом чате обычная проверка входа остаётся активной.
		_, pending, err := b.db.GetPunishment(ctx, chatID, userID)
		if err != nil {
			return "не удалось проверить предыдущие наказания", err
		}
		if !pending {
			row, found, err := b.db.GetPendingAdmission(ctx, chatID, userID)
			if err != nil {
				return "не удалось проверить текущий допуск", err
			}
			if !found || row.Action != action {
				if err := b.unban(ctx, chatID, userID); err != nil {
					return "разбан не подтверждён", err
				}
				if err := b.db.ResetAttempts(ctx, chatID, userID); err != nil {
					return "разбан подтверждён, попытки не сброшены", err
				}
				return "разбан подтверждён, попытки сброшены", nil
			}
		}
	}
	row, err := b.prepareManualAdmission(ctx, action, chatID, userID, full.pardonID, otherChats)
	if err != nil {
		return "не удалось сохранить решение о разбане", err
	}
	if origin {
		full.pardonID = row.PardonID
	}
	confirmed, err := b.applyManualAdmission(ctx, row)
	if !confirmed {
		return "разбан не подтверждён. Решение сохранено, бот повторит запрос.", err
	}
	if err != nil {
		b.log.Warn("fullunban: confirmed unban still pending local cleanup", "err", err, "chat", chatID, "user", userID)
		return "Разбан подтверждён. Восстановление прав и сброс попыток ещё не завершены, бот повторит сверку.", nil
	}
	if !origin {
		return "разбан подтверждён, попытки сброшены", nil
	}
	text := "Разбан в этом чате подтверждён для " + b.mentionFor(userID) + "."
	if wasSpamBanned {
		if remains, err := b.db.IsSpamBanned(ctx, userID); err == nil && !remains {
			text += " Запись в базе спамеров удалена."
		}
	}
	return text, nil
}

func (b *Bot) sendFullUnbanResult(userID int64, text string) {
	if userID > 0 {
		b.sendHTML(userID, 0, 0, text)
	}
}

// handleModChoiceCallback — кнопки плашек /unban|/fullunban|/unmute|/whitelist:
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
	// не админ/владелец, не сервисный/канальный id. Отказ — тостом:
	// реплаять не на что. Последний случай зеркалит хвост guardModTarget
	// (modcmd.go): сегодня RecentEventUsers таких id не выдаёт, но первый же
	// новый kind события с channel/service id не должен доехать до
	// release/unban с ложным «проверь мои права».
	switch {
	case b.me != nil && targetID == b.me.ID:
		_ = b.api.AnswerCallbackQuery(ctx, tu.CallbackQuery(query.ID).
			WithText("Себя трогать не дам.").WithShowAlert())
		return nil
	case targetID == query.From.ID:
		_ = b.api.AnswerCallbackQuery(ctx, tu.CallbackQuery(query.ID).
			WithText("Себя-то за что?").WithShowAlert())
		return nil
	case targetID == telegramServiceUserID || targetID <= 0:
		_ = b.api.AnswerCallbackQuery(ctx, tu.CallbackQuery(query.ID).
			WithText("Это не участник чата.").WithShowAlert())
		return nil
	case b.canManageChat(ctx, targetID, chatID):
		_ = b.api.AnswerCallbackQuery(ctx, tu.CallbackQuery(query.ID).
			WithText("Это админ — не трону.").WithShowAlert())
		return nil
	}

	var full fullUnbanRequest
	text, err := b.execUnmod(action, chatID, targetID, &full)
	if err != nil {
		b.log.Warn("unmod choice failed", "err", err,
			"action", action, "chat", chatID, "target", targetID)
		if text == "" {
			text = "Не получилось — проверь мои права."
		}
		_ = b.api.AnswerCallbackQuery(ctx, tu.CallbackQuery(query.ID).WithText(text).WithShowAlert())
		return nil
	}
	_ = b.api.AnswerCallbackQuery(ctx, tu.CallbackQuery(query.ID).WithText("Готово."))
	b.log.Info("unmod choice", "action", action, "chat", chatID,
		"target", targetID, "by", query.From.ID)
	// Итог редактируется в саму плашку, клавиатура снимается. Гонка двух
	// админов даёт повторный edit тем же текстом — isNotModified глушим.
	if action == "f" {
		text += fullUnbanAnnouncement(query.From.ID)
	}
	result := text + "\n<i>(" + html.EscapeString(userLabel(query.From)) + ")</i>"
	if _, err := b.api.EditMessageText(b.runCtx, &telego.EditMessageTextParams{
		ChatID:    tu.ID(chatID),
		MessageID: query.Message.GetMessageID(),
		Text:      result,
		ParseMode: telego.ModeHTML,
	}); err != nil && !isNotModified(err) {
		b.log.Warn("edit unmod list result", "err", err, "chat", chatID)
	}
	if action == "f" {
		b.startFullUnbanFanout(targetID, query.From.ID, full)
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
