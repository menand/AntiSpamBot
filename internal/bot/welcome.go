package bot

import (
	"fmt"
	"strings"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"

	"github.com/menand/AntiSpamBot/internal/storage"
)

// welcomeMode — что бот делает после пройденной капчи (пер-чатовый режим
// «После капчи»):
//   - none  — ничего: размьютил и всё;
//   - greet — обычное приветствие (текст приветствия);
//   - reply — приветствие-проверка с серией напоминаний (текст проверки).
type welcomeMode string

const (
	welcomeNone  welcomeMode = "none"
	welcomeGreet welcomeMode = "greet"
	welcomeReply welcomeMode = "reply"
)

// effectiveWelcomeMode резолвит режим: явный welcome_mode чата, иначе —
// деривация из легаси-флагов (reply_check_enabled → reply, greeting_enabled →
// greet, иначе none). Чат без строки настроек несёт GreetingEnabled=true из
// defaultChatSettings, поэтому дефолт новых чатов — greet. Неизвестное
// значение из БД (будущее/битое) откатывается к деривации.
func effectiveWelcomeMode(s storage.ChatSettings) welcomeMode {
	if s.WelcomeMode.Valid {
		switch m := welcomeMode(s.WelcomeMode.String); m {
		case welcomeNone, welcomeGreet, welcomeReply:
			return m
		}
	}
	if s.ReplyCheckEnabled {
		return welcomeReply
	}
	if s.GreetingEnabled {
		return welcomeGreet
	}
	return welcomeNone
}

// welcomeModeLabel — метка режима для статус-строки настроек.
func welcomeModeLabel(m welcomeMode) string {
	switch m {
	case welcomeGreet:
		return "приветствие"
	case welcomeReply:
		return "приветствие с ответом"
	default:
		return "ничего"
	}
}

// welcomeModeShort — короткая метка для кнопки-входа в подэкран: полное
// «приветствие с ответом» с префиксом не влезает в бюджет одиночного ряда.
func welcomeModeShort(m welcomeMode) string {
	switch m {
	case welcomeGreet:
		return "приветствие"
	case welcomeReply:
		return "с ответом"
	default:
		return "ничего"
	}
}

// welcomeEntryLabel — подпись кнопки входа в подэкран «После капчи».
func welcomeEntryLabel(m welcomeMode) string {
	return "✉️ После капчи: " + welcomeModeShort(m)
}

// welcomeModeButtons — кнопки выбора режима для подэкрана: «Ничего» и
// «Приветствие» делят ряд (бюджет 17), длинное «Приветствие с ответом» —
// своим рядом (бюджет 33).
func welcomeModeButtons(chatID int64, current welcomeMode) [][]telego.InlineKeyboardButton {
	modeBtn := func(m welcomeMode, label string) telego.InlineKeyboardButton {
		if m == current {
			label = "• " + label
		}
		return tu.InlineKeyboardButton(label).
			WithCallbackData(fmt.Sprintf("menu:wset:%d:%s", chatID, m))
	}
	return [][]telego.InlineKeyboardButton{
		{modeBtn(welcomeNone, "🚫 Ничего"), modeBtn(welcomeGreet, "🎉 Приветствие")},
		{modeBtn(welcomeReply, "⏳ Приветствие с ответом")},
	}
}

// welcomeTextState — хвост «(текст: свой/стандартный)» для статус-строки
// настроек и подэкрана: смотрит пару, которую читает ТЕКУЩИЙ режим (greet →
// greeting_*, reply → reply_*); у «ничего» текста нет — пусто. Один источник
// на оба экрана, иначе формулировки расходятся.
func welcomeTextState(s storage.ChatSettings, mode welcomeMode) string {
	switch mode {
	case welcomeGreet:
		if s.GreetingText.Valid && strings.TrimSpace(s.GreetingText.String) != "" {
			return " (текст: свой)"
		}
		return " (текст: стандартный)"
	case welcomeReply:
		if s.ReplyText.Valid && strings.TrimSpace(s.ReplyText.String) != "" {
			return " (текст проверки: свой)"
		}
		return " (текст проверки: стандартный)"
	}
	return ""
}

// welcomeTextKindNoun — родительный падеж названия текста для сообщений флоу
// ввода: «текст приветствия» / «текст проверки».
func welcomeTextKindNoun(k welcomeMode) string {
	if k == welcomeReply {
		return "проверки"
	}
	return "приветствия"
}

// welcomeTextActionNames — словесные формы подтверждений флоу ввода.
// resetLine открывает ответ на «-», savedLine — ответ на сохранение.
func welcomeTextActionNames(k welcomeMode) (resetLine, savedLine string) {
	if k == welcomeReply {
		return "Вернул стандартный текст проверки:",
			"Сохранил! Так будет выглядеть сообщение-проверка:"
	}
	return "Вернул стандартное приветствие:",
		"Сохранил! Так будет выглядеть приветствие:"
}
