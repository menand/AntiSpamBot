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
		if !b.isOwner(query.From.ID) {
			return nil
		}
		return b.renderChatsMenu(ctx, query)
	case "stats":
		if !b.isOwner(query.From.ID) {
			return nil
		}
		if len(parts) != 4 {
			return nil
		}
		chatID, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil {
			return nil
		}
		return b.renderChatStats(ctx, query, chatID, statsPeriod(parts[3]))
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
	}
	if b.isOwner(userID) {
		rows = append(rows, []telego.InlineKeyboardButton{
			tu.InlineKeyboardButton("📊 Мои чаты").WithCallbackData(cbChats),
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
	chats, err := b.db.ListChats(ctx)
	if err != nil {
		b.log.Warn("list chats", "err", err)
		return nil
	}
	var sb strings.Builder
	sb.WriteString("📊 <b>Чаты, где я работаю</b>\n\n")
	if len(chats) == 0 {
		sb.WriteString("<i>Пока ни одного чата не замечено. Добавь меня в группу и дождись первого события.</i>")
	} else {
		fmt.Fprintf(&sb, "Найдено чатов: %d\nВыбери чат для статистики.", len(chats))
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
			tu.InlineKeyboardButton("⬅️ К списку чатов").WithCallbackData(cbChats),
		},
	}
	return b.editWithMenu(ctx, query, text, &telego.InlineKeyboardMarkup{InlineKeyboard: rows})
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
	if message.From == nil || !b.isOwner(message.From.ID) {
		_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(message.Chat.ID),
			"Команда доступна только владельцам бота (OWNER_IDS)."))
		return nil
	}
	chats, err := b.db.ListChats(ctx)
	if err != nil {
		b.log.Warn("list chats", "err", err)
		return nil
	}

	var sb strings.Builder
	sb.WriteString("📊 <b>Чаты, где я работаю</b>\n\n")
	if len(chats) == 0 {
		sb.WriteString("<i>Пока ни одного чата не замечено.</i>")
	} else {
		fmt.Fprintf(&sb, "Найдено чатов: %d\nВыбери чат для статистики.", len(chats))
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
