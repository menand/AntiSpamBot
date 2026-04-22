package bot

import (
	"fmt"
	"html"
	"strconv"
	"strings"

	"github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
	tu "github.com/mymmrac/telego/telegoutil"

	"github.com/menand/AntiSpamBot/internal/storage"
)

// Callback data formats (all prefixed "menu:"):
//   menu:main               — back to main menu
//   menu:help               — help text
//   menu:add                — "how to add me to a group" instructions
//   menu:chats              — list of chats (owner only)
//   menu:stats:<chat>:<p>   — stats for chat over period p ∈ {day,week,month,all}
const (
	cbMain  = "menu:main"
	cbHelp  = "menu:help"
	cbAdd   = "menu:add"
	cbChats = "menu:chats"
)

func (b *Bot) handleMenuCallback(ctx *th.Context, query telego.CallbackQuery) error {
	_ = b.api.AnswerCallbackQuery(ctx, tu.CallbackQuery(query.ID))

	if query.Message == nil {
		return nil
	}
	parts := strings.Split(query.Data, ":")
	if len(parts) < 2 {
		return nil
	}

	switch parts[1] {
	case "main":
		return b.editWithMenu(ctx, query, b.mainMenuText(query.From.ID), b.mainMenuKeyboard(query.From.ID))
	case "help":
		return b.editWithMenu(ctx, query, helpText, backKeyboard())
	case "add":
		return b.editWithMenu(ctx, query, b.addInstructionsText(), backKeyboard())
	case "chats":
		return b.renderChatsMenu(ctx, query)
	case "logs":
		if !b.isOwner(query.From.ID) {
			return nil
		}
		// Reuse the command handler — it takes a Message; synthesize one with the
		// essentials (from/chat). Easier than duplicating the logic here.
		synthetic := telego.Message{
			From: &query.From,
			Chat: telego.Chat{ID: query.Message.GetChat().ID, Type: "private"},
		}
		return b.handleLogsCommand(ctx, synthetic)
	case "stats":
		if len(parts) != 4 {
			return nil
		}
		chatID, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil {
			return nil
		}
		if !b.canManageChat(ctx, query.From.ID, chatID) {
			return nil
		}
		return b.renderChatStats(ctx, query, chatID, statsPeriod(parts[3]))
	case "settings":
		if len(parts) != 3 {
			return nil
		}
		chatID, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil {
			return nil
		}
		if !b.canManageChat(ctx, query.From.ID, chatID) {
			return nil
		}
		return b.renderChatSettings(ctx, query, chatID)
	case "gr":
		// Toggles greeting. Supports both old format (menu:gr:chat:period)
		// which comes from stale inline buttons, and new format
		// (menu:gr:chat) from the settings submenu.
		if len(parts) < 3 {
			return nil
		}
		chatID, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil {
			return nil
		}
		if !b.canManageChat(ctx, query.From.ID, chatID) {
			return nil
		}
		enabled, err := b.db.GetGreetingEnabled(ctx, chatID)
		if err != nil {
			b.log.Warn("get greeting in menu", "err", err)
			return nil
		}
		if err := b.db.SetGreetingEnabled(ctx, chatID, !enabled); err != nil {
			b.log.Warn("set greeting in menu", "err", err)
			return nil
		}
		return b.renderChatSettings(ctx, query, chatID)
	case "max":
		if len(parts) != 4 {
			return nil
		}
		chatID, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil {
			return nil
		}
		if !b.canManageChat(ctx, query.From.ID, chatID) {
			return nil
		}
		v, err := strconv.Atoi(parts[3])
		if err != nil || v < 1 || v > 100 {
			return nil
		}
		if err := b.db.SetMaxAttempts(ctx, chatID, &v); err != nil {
			b.log.Warn("set max_attempts", "err", err)
		}
		return b.renderChatSettings(ctx, query, chatID)
	case "tmo":
		if len(parts) != 4 {
			return nil
		}
		chatID, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil {
			return nil
		}
		if !b.canManageChat(ctx, query.From.ID, chatID) {
			return nil
		}
		v, err := strconv.Atoi(parts[3])
		if err != nil || v < 5 || v > 600 {
			return nil
		}
		if err := b.db.SetCaptchaTimeoutSec(ctx, chatID, &v); err != nil {
			b.log.Warn("set captcha_timeout", "err", err)
		}
		return b.renderChatSettings(ctx, query, chatID)
	case "daily":
		if len(parts) != 3 {
			return nil
		}
		chatID, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil {
			return nil
		}
		if !b.canManageChat(ctx, query.From.ID, chatID) {
			return nil
		}
		s, _ := b.db.GetChatSettings(ctx, chatID)
		if err := b.db.SetDailyStatsEnabled(ctx, chatID, !s.DailyStatsEnabled); err != nil {
			b.log.Warn("set daily stats", "err", err)
		}
		return b.renderChatSettings(ctx, query, chatID)
	case "hour":
		if len(parts) != 4 {
			return nil
		}
		chatID, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil {
			return nil
		}
		if !b.canManageChat(ctx, query.From.ID, chatID) {
			return nil
		}
		v, err := strconv.Atoi(parts[3])
		if err != nil || v < 0 || v > 23 {
			return nil
		}
		if err := b.db.SetDailyStatsHour(ctx, chatID, &v); err != nil {
			b.log.Warn("set daily hour", "err", err)
		}
		return b.renderChatSettings(ctx, query, chatID)
	}
	return nil
}

func (b *Bot) mainMenuText(userID int64) string {
	text := "🤖 <b>Меню</b>\n\n" +
		"Я анти-спам бот для Telegram-групп. Показываю новым участникам капчу с цветными кружками — живые пропускаются, боты кикаются.\n\n" +
		"Выбери раздел:"
	if b.isOwner(userID) {
		text += "\n\n<i>Ты владелец бота (OWNER_IDS).</i>"
	}
	return text
}

func (b *Bot) mainMenuKeyboard(userID int64) *telego.InlineKeyboardMarkup {
	rows := [][]telego.InlineKeyboardButton{
		{
			tu.InlineKeyboardButton("📖 Справка").WithCallbackData(cbHelp),
			tu.InlineKeyboardButton("➕ В группу").WithCallbackData(cbAdd),
		},
		{
			tu.InlineKeyboardButton("📊 Мои чаты").WithCallbackData(cbChats),
		},
	}
	if b.isOwner(userID) {
		rows = append(rows, []telego.InlineKeyboardButton{
			tu.InlineKeyboardButton("📄 Прислать лог").WithCallbackData("menu:logs"),
		})
	}
	return &telego.InlineKeyboardMarkup{InlineKeyboard: rows}
}

const helpText = `📖 <b>Справка</b>

<b>Как работает капча</b>
Когда в чат входит новый участник, я отправляю ему сообщение «выбери <i>красный</i> кружок» и 6 цветных кнопок. Правильный выбор — ограничения снимаются, сообщение удаляется. Неправильный или таймаут 30 сек — кик. 3 провала подряд за сутки — перманентный бан.

<b>Команды в группах</b>
/stats [day|week|month|all] — статистика чата за период (только для админов чата или владельцев бота)

<b>Команды в личке</b>
/start, /help — это меню
/chats — список моих чатов (только для владельцев)

<b>«Молчаливые возвращенцы»</b>
Если кто-то не писал 30+ дней и вдруг написал — я об этом сообщу в чат с шутливым комментарием.`

func (b *Bot) addInstructionsText() string {
	username := b.Username()
	if username == "" {
		username = "your_bot"
	}
	return fmt.Sprintf(`➕ <b>Добавить меня в группу</b>

1. Открой нужную группу
2. «Управление группой» → «Администраторы» → «Добавить администратора»
3. Найди @%s и добавь
4. Выдай права:
   ✅ Блокировать пользователей
   ✅ Удалять сообщения
5. У @BotFather выключи Privacy Mode для меня (иначе не увижу сообщения):
   <code>/mybots → @%s → Bot Settings → Group Privacy → Turn off</code>

Готово! Следующий, кто зайдёт в чат, получит капчу.`, username, username)
}

func backKeyboard() *telego.InlineKeyboardMarkup {
	return &telego.InlineKeyboardMarkup{
		InlineKeyboard: [][]telego.InlineKeyboardButton{
			{tu.InlineKeyboardButton("⬅️ Назад").WithCallbackData(cbMain)},
		},
	}
}

func (b *Bot) renderChatsMenu(ctx *th.Context, query telego.CallbackQuery) error {
	chats, err := b.userChats(ctx, query.From.ID)
	if err != nil {
		b.log.Warn("user chats", "err", err)
		return nil
	}
	var sb strings.Builder
	sb.WriteString("📊 <b>Твои чаты</b>\n\n")
	if len(chats) == 0 {
		sb.WriteString("<i>У тебя нет чатов, которыми я управляю. Добавь меня в группу как администратора — и ты сможешь настраивать её отсюда.</i>")
	} else {
		fmt.Fprintf(&sb, "Найдено чатов: %d\nВыбери чат для настроек и статистики.", len(chats))
	}

	rows := make([][]telego.InlineKeyboardButton, 0, len(chats)+1)
	for _, c := range chats {
		label := c.Title
		if label == "" {
			label = fmt.Sprintf("Chat %d", c.ChatID)
		}
		if len(label) > 40 {
			label = label[:37] + "…"
		}
		cb := fmt.Sprintf("menu:stats:%d:%s", c.ChatID, periodWeek)
		rows = append(rows, []telego.InlineKeyboardButton{
			tu.InlineKeyboardButton(label).WithCallbackData(cb),
		})
	}
	rows = append(rows, []telego.InlineKeyboardButton{
		tu.InlineKeyboardButton("⬅️ Назад").WithCallbackData(cbMain),
	})
	return b.editWithMenu(ctx, query, sb.String(), &telego.InlineKeyboardMarkup{InlineKeyboard: rows})
}

func (b *Bot) renderChatStats(ctx *th.Context, query telego.CallbackQuery, chatID int64, p statsPeriod) error {
	from, until := statsRange(p)
	s, err := b.db.QueryStats(ctx, chatID, from, until)
	if err != nil {
		b.log.Warn("query stats (menu)", "err", err)
		return nil
	}
	topWriters, _ := b.db.TopWriters(ctx, chatID, from, until, 5)
	topFailers, _ := b.db.TopFailers(ctx, chatID, from, until, 5)
	infos, _ := b.db.GetUserInfos(ctx, collectUserIDs(topWriters, topFailers))
	if infos == nil {
		infos = map[int64]storage.UserInfo{}
	}

	title := b.chatTitle(ctx, chatID)
	text := fmt.Sprintf("<b>%s</b>\n\n%s",
		html.EscapeString(title),
		renderStats(p, s, b.cfg.NewcomerDays, topWriters, topFailers, infos))

	rows := [][]telego.InlineKeyboardButton{
		{
			periodButton(chatID, periodDay, p, "Сутки"),
			periodButton(chatID, periodWeek, p, "Неделя"),
			periodButton(chatID, periodMonth, p, "Месяц"),
			periodButton(chatID, periodAll, p, "Всё"),
		},
		{
			tu.InlineKeyboardButton("⚙️ Настройки").
				WithCallbackData(fmt.Sprintf("menu:settings:%d", chatID)),
		},
		{
			tu.InlineKeyboardButton("⬅️ К списку чатов").WithCallbackData(cbChats),
		},
	}
	return b.editWithMenu(ctx, query, text, &telego.InlineKeyboardMarkup{InlineKeyboard: rows})
}

func (b *Bot) renderChatSettings(ctx *th.Context, query telego.CallbackQuery, chatID int64) error {
	s, err := b.db.GetChatSettings(ctx, chatID)
	if err != nil {
		b.log.Warn("get chat settings", "err", err)
		return nil
	}

	maxAttempts := b.cfg.MaxAttempts
	if s.MaxAttempts.Valid {
		maxAttempts = int(s.MaxAttempts.Int64)
	}
	timeoutSec := int(b.cfg.CaptchaTimeout.Seconds())
	if s.CaptchaTimeoutSeconds.Valid {
		timeoutSec = int(s.CaptchaTimeoutSeconds.Int64)
	}
	digestHourUTC := b.cfg.DailyStatsUTCHour
	if s.DailyStatsUTCHour.Valid {
		digestHourUTC = int(s.DailyStatsUTCHour.Int64)
	}

	title := b.chatTitle(ctx, chatID)
	text := fmt.Sprintf(
		"⚙️ <b>Настройки: %s</b>\n\n"+
			"🔄 Попыток до бана: <b>%d</b>\n"+
			"⏱ Секунд на ответ: <b>%d</b>\n"+
			"🎉 Приветствие: <b>%s</b>\n"+
			"📊 Ежедневная сводка в чат: <b>%s</b> в <b>%s МСК</b>",
		html.EscapeString(title),
		maxAttempts, timeoutSec,
		onOffLabel(s.GreetingEnabled),
		onOffLabel(s.DailyStatsEnabled),
		mskHourLabel(digestHourUTC),
	)

	rows := [][]telego.InlineKeyboardButton{
		intPresetRow(chatID, "max", maxAttempts, []int{2, 3, 5, 10}),
		intPresetRow(chatID, "tmo", timeoutSec, []int{15, 30, 60, 120}),
		{
			tu.InlineKeyboardButton(toggleLabel("🎉 Приветствие", s.GreetingEnabled)).
				WithCallbackData(fmt.Sprintf("menu:gr:%d", chatID)),
			tu.InlineKeyboardButton(toggleLabel("📊 Сводка", s.DailyStatsEnabled)).
				WithCallbackData(fmt.Sprintf("menu:daily:%d", chatID)),
		},
		hourPresetRow(chatID, digestHourUTC, []int{6, 9, 12, 15, 18, 21}),
		{
			tu.InlineKeyboardButton("⬅️ К статистике").
				WithCallbackData(fmt.Sprintf("menu:stats:%d:%s", chatID, periodWeek)),
		},
	}
	return b.editWithMenu(ctx, query, text, &telego.InlineKeyboardMarkup{InlineKeyboard: rows})
}

// hourPresetRow renders a row of UTC hours as buttons, but labels them in
// MSK (UTC+3) for user-friendliness. Stores the UTC value in the callback.
func hourPresetRow(chatID int64, currentUTC int, presetsUTC []int) []telego.InlineKeyboardButton {
	row := make([]telego.InlineKeyboardButton, 0, len(presetsUTC))
	for _, utcHour := range presetsUTC {
		label := mskHourLabel(utcHour)
		if utcHour == currentUTC {
			label = "• " + label + " •"
		}
		row = append(row,
			tu.InlineKeyboardButton(label).
				WithCallbackData(fmt.Sprintf("menu:hour:%d:%d", chatID, utcHour)))
	}
	return row
}

// mskHourLabel formats a UTC hour as "HH:00" in Moscow time.
func mskHourLabel(utcHour int) string {
	msk := (utcHour + 3) % 24
	return fmt.Sprintf("%02d:00", msk)
}

func intPresetRow(chatID int64, key string, current int, presets []int) []telego.InlineKeyboardButton {
	row := make([]telego.InlineKeyboardButton, 0, len(presets))
	for _, v := range presets {
		label := strconv.Itoa(v)
		if v == current {
			label = "• " + label + " •"
		}
		row = append(row,
			tu.InlineKeyboardButton(label).
				WithCallbackData(fmt.Sprintf("menu:%s:%d:%d", key, chatID, v)))
	}
	return row
}

func onOffLabel(on bool) string {
	if on {
		return "✅"
	}
	return "❌"
}

func toggleLabel(prefix string, on bool) string {
	return prefix + " " + onOffLabel(on)
}

func periodButton(chatID int64, want, current statsPeriod, label string) telego.InlineKeyboardButton {
	if want == current {
		label = "• " + label + " •"
	}
	return tu.InlineKeyboardButton(label).
		WithCallbackData(fmt.Sprintf("menu:stats:%d:%s", chatID, want))
}

func (b *Bot) chatTitle(ctx *th.Context, chatID int64) string {
	chats, err := b.db.ListChats(ctx)
	if err == nil {
		for _, c := range chats {
			if c.ChatID == chatID && c.Title != "" {
				return c.Title
			}
		}
	}
	return fmt.Sprintf("Chat %d", chatID)
}

func (b *Bot) editWithMenu(ctx *th.Context, query telego.CallbackQuery, text string, kb *telego.InlineKeyboardMarkup) error {
	_, err := b.api.EditMessageText(ctx, &telego.EditMessageTextParams{
		ChatID:      tu.ID(query.Message.GetChat().ID),
		MessageID:   query.Message.GetMessageID(),
		Text:        text,
		ParseMode:   telego.ModeHTML,
		ReplyMarkup: kb,
	})
	if err != nil {
		b.log.Warn("edit menu", "err", err)
	}
	return nil
}

func (b *Bot) handleChatsCommand(ctx *th.Context, message telego.Message) error {
	if message.Chat.Type != "private" {
		return nil
	}
	if message.From == nil {
		return nil
	}
	chats, err := b.userChats(ctx, message.From.ID)
	if err != nil {
		b.log.Warn("user chats", "err", err)
		return nil
	}

	var sb strings.Builder
	sb.WriteString("📊 <b>Твои чаты</b>\n\n")
	if len(chats) == 0 {
		sb.WriteString("<i>У тебя нет чатов, которыми я управляю. Добавь меня в группу как администратора — и ты сможешь настраивать её отсюда.</i>")
	} else {
		fmt.Fprintf(&sb, "Найдено чатов: %d\nВыбери чат для настроек и статистики.", len(chats))
	}

	rows := make([][]telego.InlineKeyboardButton, 0, len(chats))
	for _, c := range chats {
		label := c.Title
		if label == "" {
			label = fmt.Sprintf("Chat %d", c.ChatID)
		}
		if len(label) > 40 {
			label = label[:37] + "…"
		}
		cb := fmt.Sprintf("menu:stats:%d:%s", c.ChatID, periodWeek)
		rows = append(rows, []telego.InlineKeyboardButton{
			tu.InlineKeyboardButton(label).WithCallbackData(cb),
		})
	}
	_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(message.Chat.ID), sb.String()).
		WithParseMode(telego.ModeHTML).
		WithReplyMarkup(&telego.InlineKeyboardMarkup{InlineKeyboard: rows}))
	return nil
}
