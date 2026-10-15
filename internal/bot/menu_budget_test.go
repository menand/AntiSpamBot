package bot

import (
	"fmt"
	"testing"

	"github.com/mymmrac/telego"

	"github.com/menand/AntiSpamBot/internal/captcha"
	"github.com/menand/AntiSpamBot/internal/textwidth"
)

// Закрепляет закон CLAUDE.md: бюджет ряда — ЖЁСТКИЙ потолок для подписи.
// Подпись шире бюджета клиент режет сам (измерено вживую: 1→33, 2→17,
// 3→10, 4→7, 5+→5, Telegram Desktop, октябрь 2026, нулевой запас).
//
// Это не ручная сверка арифметики в комментариях, а прогон реальных
// строителей рядов экрана «Настройки чата»: каждый пресет берётся по
// очереди как текущий, чтобы покрыть и обычную кнопку, и активную с
// маркером «• » (+2 единицы).
func TestSettingsRowsFitBudget(t *testing.T) {
	const chatID = int64(1)

	checkRow := func(name string, row []telego.InlineKeyboardButton) {
		t.Helper()
		if len(row) == 0 {
			t.Fatalf("%s: пустой ряд", name)
		}
		budget := textwidth.LabelBudget(len(row))
		for _, btn := range row {
			if w := textwidth.Width(btn.Text); w > budget {
				t.Errorf("%s: %q шириной %d больше бюджета ряда из %d кнопок (%d)",
					name, btn.Text, w, len(row), budget)
			}
		}
	}

	// checkLabel меряет готовую подпись против бюджета ряда из rowSize кнопок.
	checkLabel := func(name, label string, rowSize int) {
		t.Helper()
		budget := textwidth.LabelBudget(rowSize)
		if w := textwidth.Width(label); w > budget {
			t.Errorf("%s: %q шириной %d больше бюджета ряда из %d кнопок (%d)",
				name, label, w, rowSize, budget)
		}
	}

	// Режим капчи: ряд из 3, бюджет 10. Все три режима по очереди — активный
	// получает маркер «• » и становится самым широким.
	for _, m := range []captcha.Mode{captcha.ModeCircles, captcha.ModeEmoji, captcha.ModeImage} {
		row := captchaModeRow(chatID, m)
		if len(row) != 3 {
			t.Fatalf("captchaModeRow(%v): %d кнопок, ожидалось 3", m, len(row))
		}
		checkRow(fmt.Sprintf("captchaModeRow(%v)", m), row)
	}

	// Пресеты с меткой: ряд из 3..4, бюджет 10 или 7. Значения — те же,
	// что передаёт renderChatSettings.
	presetCases := []struct {
		name    string
		presets []int
		build   func(current int) []telego.InlineKeyboardButton
	}{
		{"max", []int{2, 3, 5, 10}, func(c int) []telego.InlineKeyboardButton {
			return intPresetRow(chatID, "max", c, []int{2, 3, 5, 10}, "х")
		}},
		{"tmo", []int{1, 2, 3}, func(c int) []telego.InlineKeyboardButton {
			return intPresetRow(chatID, "tmo", c, []int{1, 2, 3}, "мин")
		}},
		{"swl", []int{5, 10, 20}, func(c int) []telego.InlineKeyboardButton {
			return intPresetRow(chatID, "swl", c, []int{5, 10, 20}, " смс")
		}},
		{"svm", []int{2, 3, 5}, func(c int) []telego.InlineKeyboardButton {
			return intPresetRow(chatID, "svm", c, []int{2, 3, 5}, " гол.")
		}},
		{"hours", []int{21, 1, 5, 9, 13, 17}, func(c int) []telego.InlineKeyboardButton {
			return hourPresetRow(chatID, c, []int{21, 1, 5, 9, 13, 17})
		}},
		{"qhours", []int{1, 24, 168}, func(c int) []telego.InlineKeyboardButton {
			return quarantineHoursRow(chatID, c)
		}},
	}
	for _, pc := range presetCases {
		for _, cur := range pc.presets {
			checkRow(fmt.Sprintf("%s(current=%d)", pc.name, cur), pc.build(cur))
		}
	}

	// Статические ряды экрана настроек. Подписи продублированы из литералов
	// в renderChatSettings: если одна изменится там, обнови и здесь.
	//
	// Полноценной выемки в чистую функцию-строитель здесь не делаем — ряды
	// собираются из состояния БД, которое тянет за собой Bot и контекст, а
	// перед пушем это лишний рефакторинг. Динамические ряды (капча, пресеты)
	// покрыты выше по-настоящему.
	for _, on := range []bool{true, false} {
		// Ряд из 2 (бюджет 17): переключатель приветствия + правка текста.
		checkLabel("toggleLabel(Приветствие)", toggleLabel("🎉Приветствие", on), 2)
		// Ряд из 2 (бюджет 17): ответ + эфемерность.
		checkLabel("toggleLabel(Ждать ответ)", toggleLabel("💬Ждать ответ", on), 2)
		checkLabel("toggleLabel(Эфемерно)", toggleLabel("👻 Эфемерно", on), 2)
		// Одиночные (бюджет 33).
		checkLabel("toggleLabel(Сводка)", toggleLabel("📊 Сводка", on), 1)
		checkLabel("toggleLabel(Анонс молчунов)", toggleLabel("😴 Анонс молчунов", on), 1)
		checkLabel("toggleLabel(ИИ-антиспам)", toggleLabel("🤖 ИИ-антиспам", on), 1)
		checkLabel("toggleLabel(Карантин)", toggleLabel("🧫 Карантин новичков", on), 1)
	}
	checkLabel("Свой текст", "✏️ Свой текст", 2)
	checkLabel("Выйти из чата", "🚪 Выйти из чата", 1)
	checkLabel("К статистике", "⬅️ К статистике", 1)

	// Ряд периода статистики: 3 кнопки, бюджет 10.
	//
	// ИСКЛЮЧЕНИЕ по прямому распоряжению владельца: «• Позавчера» = 11 при
	// бюджете 10. Володя засёк, что на его клиенте влезает, и велел не
	// трогать. Если на каком-то клиенте подпись обрежется — причина именно
	// здесь, чинить переименованием («Позавч.»), а не правкой бюджета.
	const ownerApprovedOverBudget = "• Позавчера"
	periods := []struct {
		key   statsPeriod
		title string
	}{
		{periodDay, "Сегодня"},
		{periodYesterday, "Вчера"},
		{periodDayBefore, "Позавчера"},
		{periodWeek, "Неделя"},
		{periodMonth, "Месяц"},
		{periodAll, "Всегда"},
	}
	for _, per := range periods {
		btn := periodButton(chatID, per.key, periodDayBefore, per.title)
		if btn.Text == ownerApprovedOverBudget {
			continue // осознанное исключение, см. комментарий выше
		}
		checkLabel("periodButton("+per.title+")", btn.Text, 3)
	}
}
