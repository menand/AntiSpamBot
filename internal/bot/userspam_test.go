package bot

import (
	"strings"
	"testing"
)

func TestManualVoteText(t *testing.T) {
	reporter := `<a href="tg://user?id=7">Репортёр</a>`
	suspect := `<a href="tg://user?id=42">Спамер</a>`
	got := manualVoteText(reporter, suspect, 2, 1, 3)

	for _, want := range []string{
		"🚩", reporter, suspect, "Забанить?",
		"перевес в 3 голоса", "Голос админа решает сразу",
		"не голосуют",
		"🚫 Спам: <b>2</b>", "✅ Не спам: <b>1</b>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("manualVoteText missing %q in:\n%s", want, got)
		}
	}
}

func TestRenderUserInfoCard(t *testing.T) {
	t.Run("полная карточка", func(t *testing.T) {
		full := userInfoCard{
			header:   `👤 <a href="tg://user?id=1">Иван</a> · @ivan`,
			joined:   "🗓 Последний вход: <b>03.02.2026</b>",
			messages: "💬 Сообщения: сегодня <b>3</b> · вчера 0 · неделю 12 · месяц 40 · всего 210",
			violations: []string{
				"капча не пройдена: 2",
				"подозрения на спам: 1",
				"мьюты: 2",
			},
			flags: []string{"✅ доверенный (/whitelist)"},
			live:  "🔇 Сейчас в мьюте — до 22.08.2026 18:40 МСК",
		}
		got := renderUserInfoCard(full)
		for _, want := range []string{
			full.header, full.joined, full.messages,
			"⚠️ За ~180 дней:", "капча не пройдена: 2 · подозрения на спам: 1 · мьюты: 2",
			"🏷 ✅ доверенный (/whitelist)", "🔇 Сейчас в мьюте — до 22.08.2026 18:40 МСК",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("card missing %q in:\n%s", want, got)
			}
		}
	})

	t.Run("минимальная без пустых секций", func(t *testing.T) {
		minimal := renderUserInfoCard(userInfoCard{
			header: `👤 <a href="tg://user?id=2">id2</a>`,
			joined: "🗓 Вход: неизвестно",
		})
		if strings.Contains(minimal, "💬") || strings.Contains(minimal, "⚠️") ||
			strings.Contains(minimal, "🏷") || strings.Contains(minimal, "\n\n") {
			t.Errorf("minimal card has stray sections:\n%s", minimal)
		}
		if !strings.Contains(minimal, "🧼 Нарушений и проверок не было") {
			t.Errorf("minimal card missing clean line:\n%s", minimal)
		}
	})
}
