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

// Форматы callback data (все с префиксом "menu:"):
//
//	menu:main               — назад в главное меню
//	menu:help               — справка
//	menu:add                — инструкция «как добавить меня в группу»
//	menu:chats              — список чатов
//	menu:stats:<chat>:<p>   — статистика чата за период p ∈ {day,yesterday,daybefore,week,month,all}
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
	case "aicheck":
		// Диагностика LLM-провайдеров: реальный тестовый запрос каждому.
		// Ключи глобальные (env сервера), поэтому кнопка только владельцам.
		if !b.isOwner(query.From.ID) {
			return nil
		}
		sent, err := b.api.SendMessage(ctx, tu.Message(
			tu.ID(query.Message.GetChat().ID), "⏳ Проверяю ИИ-провайдеров…"))
		if err != nil {
			b.log.Warn("send ai check placeholder", "err", err)
			return nil
		}
		b.goSafe("runAICheck", func() { b.runAICheck(sent.Chat.ID, sent.MessageID) })
		return nil
	case "spamnotify":
		// Глобальный (не пер-чатовый) тумблер владельца: слать ли ему в ЛС
		// подозрения на спам и вердикты голосований.
		if !b.isOwner(query.From.ID) {
			return nil
		}
		on, err := b.db.SpamNotifyEnabled(ctx, query.From.ID)
		if err != nil {
			b.log.Warn("get spam notify", "err", err, "owner", query.From.ID)
			return nil
		}
		if err := b.db.SetSpamNotify(ctx, query.From.ID, !on); err != nil {
			b.log.Warn("set spam notify", "err", err, "owner", query.From.ID)
			return nil
		}
		return b.editWithMenu(ctx, query, b.mainMenuText(query.From.ID), b.mainMenuKeyboard(query.From.ID))
	case "modnotify":
		// Глобальный тумблер владельца: слать ли ему в ЛС кики/баны
		// (капча, молчание, /kick, /ban, глобальная база).
		if !b.isOwner(query.From.ID) {
			return nil
		}
		on, err := b.db.ModNotifyEnabled(ctx, query.From.ID)
		if err != nil {
			b.log.Warn("get mod notify", "err", err, "owner", query.From.ID)
			return nil
		}
		if err := b.db.SetModNotify(ctx, query.From.ID, !on); err != nil {
			b.log.Warn("set mod notify", "err", err, "owner", query.From.ID)
			return nil
		}
		return b.editWithMenu(ctx, query, b.mainMenuText(query.From.ID), b.mainMenuKeyboard(query.From.ID))
	case "logs":
		if !b.isOwner(query.From.ID) {
			return nil
		}
		// Переиспользуем командный хендлер — он принимает Message; синтезируем
		// его из необходимого (from/chat). Проще, чем дублировать логику здесь.
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
		return b.renderChatStats(ctx, query, chatID, parsePeriod(parts[3]))
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
		// Тоггл приветствия. Понимает и старый формат (menu:gr:chat:period)
		// с устаревших inline-кнопок, и новый (menu:gr:chat) из подменю
		// настроек.
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
		// Тогглы — read-modify-write: на ошибке чтения прерываемся, иначе мы
		// запишем инверсию ДЕФОЛТА вместо инверсии реального значения.
		s, err := b.db.GetChatSettings(ctx, chatID)
		if err != nil {
			b.log.Warn("get chat settings", "err", err, "chat", chatID)
			return nil
		}
		if err := b.db.SetGreetingEnabled(ctx, chatID, !s.GreetingEnabled); err != nil {
			b.log.Warn("set greeting in menu", "err", err)
			return nil
		}
		return b.renderChatSettings(ctx, query, chatID)
	case "grtxt":
		// Взводим флоу «пришли мне новый текст приветствия»: следующее личное
		// сообщение юзера станет шаблоном приветствия чата.
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
		// Экран настроек: читаем напрямую и прерываемся на ошибке, иначе
		// покажем «стандартный» вместо реально сохранённого текста. Арм
		// состояния — только после удачного чтения, чтобы не оставить
		// взведённый ввод без отправленного промпта.
		s, err := b.db.GetChatSettings(ctx, chatID)
		if err != nil {
			b.log.Warn("get chat settings", "err", err, "chat", chatID)
			return nil
		}
		b.setGreetingInputPending(query.From.ID, chatID)
		current := "стандартный"
		if s.GreetingText.Valid && strings.TrimSpace(s.GreetingText.String) != "" {
			current = "<code>" + html.EscapeString(s.GreetingText.String) + "</code>"
		}
		text := fmt.Sprintf(
			"✏️ Пришли мне текст приветствия для чата <b>%s</b> одним сообщением.\n\n"+
				"Подстановка: <code>{name}</code> — имя новичка.\n"+
				"Отправь «-», чтобы вернуть стандартный текст. /cancel — отмена.\n"+
				"Запрос действует 15 минут.\n\n"+
				"Текущий текст: %s",
			html.EscapeString(b.chatTitle(ctx, chatID)), current)
		_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(query.Message.GetChat().ID), text).
			WithParseMode(telego.ModeHTML))
		return nil
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
		// Границы диапазонов здесь и ниже — защита от бессмысленных значений
		// из подделанного callback data; штатные пресеты всегда внутри.
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
		// < 5 c человек не успеет физически, > 10 мин — капча теряет смысл.
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
		s, err := b.db.GetChatSettings(ctx, chatID)
		if err != nil {
			b.log.Warn("get chat settings", "err", err, "chat", chatID)
			return nil
		}
		if err := b.db.SetDailyStatsEnabled(ctx, chatID, !s.DailyStatsEnabled); err != nil {
			b.log.Warn("set daily stats", "err", err)
		}
		return b.renderChatSettings(ctx, query, chatID)
	case "rpl":
		// Тоггл режима «требовать ответа на приветствие».
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
		s, err := b.db.GetChatSettings(ctx, chatID)
		if err != nil {
			b.log.Warn("get chat settings", "err", err, "chat", chatID)
			return nil
		}
		if err := b.db.SetReplyCheckEnabled(ctx, chatID, !s.ReplyCheckEnabled); err != nil {
			b.log.Warn("set reply_check_enabled", "err", err)
		}
		return b.renderChatSettings(ctx, query, chatID)
	case "rplt":
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
		// < 10 c не успеет и человек, > 10 мин — ожидание теряет смысл.
		if err != nil || v < 10 || v > 600 {
			return nil
		}
		if err := b.db.SetReplyCheckSeconds(ctx, chatID, &v); err != nil {
			b.log.Warn("set reply_check_seconds", "err", err)
		}
		return b.renderChatSettings(ctx, query, chatID)
	case "sil":
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
		s, err := b.db.GetChatSettings(ctx, chatID)
		if err != nil {
			b.log.Warn("get chat settings", "err", err, "chat", chatID)
			return nil
		}
		if err := b.db.SetSilentAnnounceEnabled(ctx, chatID, !s.SilentAnnounceEnabled); err != nil {
			b.log.Warn("set silent announce", "err", err)
		}
		return b.renderChatSettings(ctx, query, chatID)
	case "spam":
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
		s, err := b.db.GetChatSettings(ctx, chatID)
		if err != nil {
			b.log.Warn("get chat settings", "err", err, "chat", chatID)
			return nil
		}
		if !s.SpamCheckEnabled && !b.spamAIEnabled() {
			// На query уже ответили в начале хендлера — второй Answer (алерт)
			// Telegram отбросил бы молча, поэтому объясняем обычным
			// сообщением: меню живёт в личке, оно ляжет прямо под ним.
			_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(query.Message.GetChat().ID),
				"⚠️ Ни GROQ_API_KEY, ни GIGACHAT_AUTH_KEY не заданы на сервере — включить ИИ-антиспам нельзя."))
			return nil
		}
		if err := b.db.SetSpamCheckEnabled(ctx, chatID, !s.SpamCheckEnabled); err != nil {
			b.log.Warn("set spam_check_enabled", "err", err)
		}
		return b.renderChatSettings(ctx, query, chatID)
	case "sthr":
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
		// Порог < 50% вешал бы плашку на половину обычных сообщений, а 100%
		// недостижим (LLM редко отвечает ровно сотней) — фича бы «отключилась».
		if err != nil || v < 50 || v > 99 {
			return nil
		}
		if err := b.db.SetSpamThreshold(ctx, chatID, &v); err != nil {
			b.log.Warn("set spam_threshold", "err", err)
		}
		return b.renderChatSettings(ctx, query, chatID)
	case "swl":
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
		// > 1000 сообщений до доверия — это уже не «новичок», а вечная слежка.
		if err != nil || v < 1 || v > 1000 {
			return nil
		}
		if err := b.db.SetSpamWhitelistMsgs(ctx, chatID, &v); err != nil {
			b.log.Warn("set spam_whitelist_msgs", "err", err)
		}
		return b.renderChatSettings(ctx, query, chatID)
	case "svm":
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
		// Перевес > 10 голосов в живом чате не собрать — вердикт бы не выносился.
		if err != nil || v < 1 || v > 10 {
			return nil
		}
		if err := b.db.SetSpamVoteMargin(ctx, chatID, &v); err != nil {
			b.log.Warn("set spam_vote_margin", "err", err)
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
	case "cmode":
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
		mode := parts[3]
		if mode != string(captcha.ModeCircles) && mode != string(captcha.ModeEmoji) &&
			mode != string(captcha.ModeImage) {
			return nil
		}
		if err := b.db.SetCaptchaMode(ctx, chatID, &mode); err != nil {
			b.log.Warn("set captcha mode", "err", err)
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
			tu.InlineKeyboardButton("🔌 Проверить ИИ").WithCallbackData("menu:aicheck"),
		})
		notifyOn, err := b.db.SpamNotifyEnabled(b.runCtx, userID)
		if err != nil {
			notifyOn = false // ошибка чтения — показываем «выключено», тумблер починит
		}
		modOn, err := b.db.ModNotifyEnabled(b.runCtx, userID)
		if err != nil {
			modOn = false
		}
		rows = append(rows, []telego.InlineKeyboardButton{
			tu.InlineKeyboardButton(toggleLabel("🔔 Спам-уведомления в ЛС", notifyOn)).
				WithCallbackData("menu:spamnotify"),
		})
		rows = append(rows, []telego.InlineKeyboardButton{
			tu.InlineKeyboardButton(toggleLabel("🛡 Кики и баны в ЛС", modOn)).
				WithCallbackData("menu:modnotify"),
		})
	}
	return &telego.InlineKeyboardMarkup{InlineKeyboard: rows}
}

const helpText = `📖 <b>Справка</b>

<b>Как работает капча</b>
Когда в чат входит новый участник, я ограничиваю его и отправляю сообщение «выбери <i>красный</i> кружок» с 6 кнопками. Правильный выбор — ограничения снимаются, сообщение удаляется. Неправильный ответ или таймаут — кик; несколько провалов подряд за сутки — перманентный бан. Время на ответ, число попыток и вид капчи (кружки/эмодзи/картинка) настраиваются per-chat в этом меню.

Админ чата может впустить человека вручную — кнопкой «✅ Впустить» под капчей.

<b>Статистика и настройки</b>
«📊 Мои чаты» → выбери чат — там статистика за периоды, ежедневная сводка в чат, приветствие (включая свой текст с подстановкой {name}) и параметры капчи.

<b>Команды в личке</b>
/start, /help — это меню
/chats — список твоих чатов

<b>«Молчаливые возвращенцы»</b>
Если кто-то долго не писал и вдруг написал — я сообщу об этом в чат с шутливым комментарием.`

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

// chatsListView собирает текст «Твои чаты» + клавиатуру выбора чата, общие
// для команды /chats и кнопки меню. withBack добавляет ряд «Назад».
func chatsListView(chats []storage.ChatInfo, withBack bool) (string, *telego.InlineKeyboardMarkup) {
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
		cb := fmt.Sprintf("menu:stats:%d:%s", c.ChatID, periodWeek)
		rows = append(rows, []telego.InlineKeyboardButton{
			tu.InlineKeyboardButton(truncateLabel(label, 40)).WithCallbackData(cb),
		})
	}
	if withBack {
		rows = append(rows, []telego.InlineKeyboardButton{
			tu.InlineKeyboardButton("⬅️ Назад").WithCallbackData(cbMain),
		})
	}
	return sb.String(), &telego.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// truncateLabel укорачивает подпись кнопки до max рун. Срез по байтам резал
// бы многобайтовую руну пополам — Telegram отклоняет всю клавиатуру при
// невалидном UTF-8, что для кириллических названий значит сломанный список
// чатов.
func truncateLabel(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

func (b *Bot) renderChatsMenu(ctx *th.Context, query telego.CallbackQuery) error {
	chats, err := b.userChats(ctx, query.From.ID)
	if err != nil {
		b.log.Warn("user chats", "err", err)
		return nil
	}
	text, kb := chatsListView(chats, true)
	return b.editWithMenu(ctx, query, text, kb)
}

func (b *Bot) renderChatStats(ctx *th.Context, query telego.CallbackQuery, chatID int64, p statsPeriod) error {
	from, until := statsRange(p, time.Now())
	s, err := b.db.QueryStats(ctx, chatID, from, until)
	if err != nil {
		b.log.Warn("query stats (menu)", "err", err)
		return nil
	}
	topWriters, _ := b.db.TopWriters(ctx, chatID, from, until, 5)
	// -1 = без лимита (SQLite: LIMIT -1); длину сообщения режет renderStats.
	topFailers, _ := b.db.TopFailers(ctx, chatID, from, until, -1)
	newMembers, _ := b.db.PassedUsers(ctx, chatID, from, until)
	banned, _ := b.db.EventUsers(ctx, chatID, from, until, storage.EventBan, storage.EventSpamBan)
	infos, _ := b.db.GetUserInfos(ctx,
		collectUserIDs(topWriters, topFailers, newMembers, banned))
	if infos == nil {
		infos = map[int64]storage.UserInfo{}
	}

	title := b.chatTitle(ctx, chatID)
	text := fmt.Sprintf("<b>%s</b>\n\n%s",
		html.EscapeString(title),
		renderStats(p, periodLabel(p), s, b.cfg.NewcomerDays,
			newMembers, topWriters, topFailers, banned, infos))

	rows := [][]telego.InlineKeyboardButton{
		{
			periodButton(chatID, periodDay, p, "Сегодня"),
			periodButton(chatID, periodYesterday, p, "Вчера"),
			periodButton(chatID, periodDayBefore, p, "Позавчера"),
		},
		{
			periodButton(chatID, periodWeek, p, "Неделя"),
			periodButton(chatID, periodMonth, p, "Месяц"),
			periodButton(chatID, periodAll, p, "Всегда"),
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

	maxAttempts := b.effectiveMaxAttempts(s)
	timeoutSec := int(b.effectiveCaptchaTimeout(s).Seconds())
	digestHourUTC := b.effectiveDailyHour(s)
	captchaMode := effectiveCaptchaMode(s)

	greetingText := "стандартный"
	if s.GreetingText.Valid && strings.TrimSpace(s.GreetingText.String) != "" {
		greetingText = "свой"
	}

	spamThreshold := effectiveSpamThreshold(s)
	spamWhitelist := effectiveSpamWhitelist(s)
	spamMargin := effectiveSpamVoteMargin(s)
	spamLabel := onOffLabel(s.SpamCheckEnabled)
	if !b.spamAIEnabled() {
		spamLabel = "нет ключа 🔑"
	}
	replySeconds := effectiveReplyCheckSeconds(s)

	title := b.chatTitle(ctx, chatID)
	text := fmt.Sprintf(
		"⚙️ <b>Настройки: %s</b>\n\n"+
			"🧩 Капча: <b>%s</b>\n"+
			"🔄 Попыток до бана: <b>%d</b>\n"+
			"⏱ Секунд на ответ: <b>%d</b>\n"+
			"🎉 Приветствие: <b>%s</b> (текст: %s)\n"+
			"💬 Требовать ответа на приветствие: <b>%s</b> (%d сек)\n"+
			"📊 Ежедневная сводка в чат: <b>%s</b> в <b>%s МСК</b>\n"+
			"😴 Анонс вернувшихся молчунов: <b>%s</b>\n"+
			"🤖 ИИ-антиспам: <b>%s</b> (порог %d%%, белый список после %d сообщ., перевес %d)",
		html.EscapeString(title),
		captchaModeLabel(captchaMode),
		maxAttempts, timeoutSec,
		onOffLabel(s.GreetingEnabled), greetingText,
		onOffLabel(s.ReplyCheckEnabled), replySeconds,
		onOffLabel(s.DailyStatsEnabled),
		mskHourLabel(digestHourUTC),
		onOffLabel(s.SilentAnnounceEnabled),
		spamLabel, spamThreshold, spamWhitelist, spamMargin,
	)

	rows := [][]telego.InlineKeyboardButton{
		captchaModeRow(chatID, captchaMode),
		intPresetRow(chatID, "max", maxAttempts, []int{2, 3, 5, 10}, "х"),
		intPresetRow(chatID, "tmo", timeoutSec, []int{15, 30, 45, 60}, "с"),
		{
			tu.InlineKeyboardButton(toggleLabel("🎉 Приветствие", s.GreetingEnabled)).
				WithCallbackData(fmt.Sprintf("menu:gr:%d", chatID)),
			tu.InlineKeyboardButton("✏️ Текст").
				WithCallbackData(fmt.Sprintf("menu:grtxt:%d", chatID)),
			tu.InlineKeyboardButton(toggleLabel("📊 Сводка", s.DailyStatsEnabled)).
				WithCallbackData(fmt.Sprintf("menu:daily:%d", chatID)),
		},
		// UTC-часы подобраны так, чтобы после сдвига МСК = UTC+3 (его делает
		// mskHourLabel) показываться как 00/04/08/12/16/20 МСК.
		hourPresetRow(chatID, digestHourUTC, []int{21, 1, 5, 9, 13, 17}),
		{
			tu.InlineKeyboardButton(toggleLabel("😴 Анонс молчунов", s.SilentAnnounceEnabled)).
				WithCallbackData(fmt.Sprintf("menu:sil:%d", chatID)),
			tu.InlineKeyboardButton(toggleLabel("🤖 ИИ-антиспам", s.SpamCheckEnabled)).
				WithCallbackData(fmt.Sprintf("menu:spam:%d", chatID)),
		},
		{
			tu.InlineKeyboardButton(toggleLabel("💬 Требовать ответ", s.ReplyCheckEnabled)).
				WithCallbackData(fmt.Sprintf("menu:rpl:%d", chatID)),
		},
	}
	// Пресеты секунд ожидания — только при включённом режиме (как у антиспама).
	if s.ReplyCheckEnabled {
		rows = append(rows,
			intPresetRow(chatID, "rplt", replySeconds, []int{30, 60, 90, 120}, "с"))
	}
	// Пресеты антиспама показываем только при включённой фиче — экран и так
	// плотный.
	if s.SpamCheckEnabled {
		rows = append(rows,
			intPresetRow(chatID, "sthr", spamThreshold, []int{70, 80, 90}, "%"),
			intPresetRow(chatID, "swl", spamWhitelist, []int{5, 10, 20}, " смс"),
			intPresetRow(chatID, "svm", spamMargin, []int{2, 3, 5}, " гол."))
	}
	rows = append(rows, []telego.InlineKeyboardButton{
		tu.InlineKeyboardButton("⬅️ К статистике").
			WithCallbackData(fmt.Sprintf("menu:stats:%d:%s", chatID, periodWeek)),
	})
	return b.editWithMenu(ctx, query, text, &telego.InlineKeyboardMarkup{InlineKeyboard: rows})
}

func captchaModeRow(chatID int64, current captcha.Mode) []telego.InlineKeyboardButton {
	opts := []struct {
		mode  captcha.Mode
		label string
	}{
		{captcha.ModeCircles, "🟢 Кружки"},
		{captcha.ModeEmoji, "🦋 Эмодзи"},
		{captcha.ModeImage, "🖼 Картинка"},
	}
	row := make([]telego.InlineKeyboardButton, 0, len(opts))
	for _, o := range opts {
		label := o.label
		if o.mode == current {
			label = "• " + label
		}
		row = append(row,
			tu.InlineKeyboardButton(label).
				WithCallbackData(fmt.Sprintf("menu:cmode:%d:%s", chatID, o.mode)))
	}
	return row
}

func captchaModeLabel(m captcha.Mode) string {
	switch m {
	case captcha.ModeEmoji:
		return "Эмодзи"
	case captcha.ModeImage:
		return "Картинка"
	default:
		return "Кружки"
	}
}

// hourPresetRow рендерит ряд UTC-часов кнопками с подписями в МСК (UTC+3),
// только цифры часа (например «04»). Компактно, чтобы ряд влезал на узких
// клиентах и Telegram не схлопывал кнопки.
func hourPresetRow(chatID int64, currentUTC int, presetsUTC []int) []telego.InlineKeyboardButton {
	row := make([]telego.InlineKeyboardButton, 0, len(presetsUTC))
	for _, utcHour := range presetsUTC {
		msk := (utcHour + storage.MSKOffsetHours) % 24
		label := fmt.Sprintf("%02d", msk)
		if utcHour == currentUTC {
			label = "• " + label
		}
		row = append(row,
			tu.InlineKeyboardButton(label).
				WithCallbackData(fmt.Sprintf("menu:hour:%d:%d", chatID, utcHour)))
	}
	return row
}

// mskHourLabel форматирует UTC-час как «HH:00» по Москве. Используется в
// тексте настроек, где «:00» делает час суток очевидным.
func mskHourLabel(utcHour int) string {
	msk := (utcHour + storage.MSKOffsetHours) % 24
	return fmt.Sprintf("%02d:00", msk)
}

func intPresetRow(chatID int64, key string, current int, presets []int, suffix string) []telego.InlineKeyboardButton {
	row := make([]telego.InlineKeyboardButton, 0, len(presets))
	for _, v := range presets {
		label := strconv.Itoa(v) + suffix
		if v == current {
			label = "• " + label
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

// toggleLabel — статус ✅/❌ ПЕРВЫМ: на узких экранах Telegram обрезает конец
// подписи, и галка в хвосте терялась бы первой.
func toggleLabel(prefix string, on bool) string {
	return onOffLabel(on) + " " + prefix
}

func periodButton(chatID int64, want, current statsPeriod, label string) telego.InlineKeyboardButton {
	if want == current {
		label = "• " + label
	}
	return tu.InlineKeyboardButton(label).
		WithCallbackData(fmt.Sprintf("menu:stats:%d:%s", chatID, want))
}

// ctx — context.Context, а не *th.Context: хелпером пользуются и уведомления
// антиспама на runCtx.
func (b *Bot) chatTitle(ctx context.Context, chatID int64) string {
	c, ok, err := b.db.GetChat(ctx, chatID)
	if err == nil && ok {
		return titleOrID(c)
	}
	return fmt.Sprintf("Chat %d", chatID)
}

// titleOrID — «Название» или «Chat <id>», когда названия в реестре нет.
func titleOrID(c storage.ChatInfo) string {
	if c.Title != "" {
		return c.Title
	}
	return fmt.Sprintf("Chat %d", c.ChatID)
}

func (b *Bot) editWithMenu(ctx *th.Context, query telego.CallbackQuery, text string, kb *telego.InlineKeyboardMarkup) error {
	_, err := b.api.EditMessageText(ctx, &telego.EditMessageTextParams{
		ChatID:      tu.ID(query.Message.GetChat().ID),
		MessageID:   query.Message.GetMessageID(),
		Text:        text,
		ParseMode:   telego.ModeHTML,
		ReplyMarkup: kb,
	})
	if err != nil && !isNotModified(err) {
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
	text, kb := chatsListView(chats, false)
	_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(message.Chat.ID), text).
		WithParseMode(telego.ModeHTML).
		WithReplyMarkup(kb))
	return nil
}
