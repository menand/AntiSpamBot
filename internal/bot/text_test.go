package bot

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateLabel(t *testing.T) {
	// 25 Cyrillic runes = 50 bytes: would break the old byte-based cut.
	long := "Очень длинное название чата для проверки обрезки"
	got := truncateLabel(long, 40)
	if !utf8.ValidString(got) {
		t.Fatalf("invalid UTF-8 after truncation: %q", got)
	}
	if r := []rune(got); len(r) != 40 {
		t.Errorf("got %d runes, want 40", len(r))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("expected ellipsis suffix, got %q", got)
	}

	short := "Чат"
	if truncateLabel(short, 40) != short {
		t.Error("short labels must pass through unchanged")
	}
}

func TestRenderGreeting(t *testing.T) {
	mention := `<a href="tg://user?id=1">Вася</a>`

	// Default when no custom template.
	if got := renderGreeting("", mention); !strings.Contains(got, mention) {
		t.Errorf("default greeting must mention the user: %q", got)
	}

	// Custom template: {name} substituted, user HTML escaped.
	got := renderGreeting("Привет, {name}! <b>Прочти правила</b>", mention)
	if !strings.Contains(got, mention) {
		t.Errorf("{name} not substituted: %q", got)
	}
	if strings.Contains(got, "<b>") {
		t.Errorf("user-supplied HTML must be escaped: %q", got)
	}
	if !strings.Contains(got, "&lt;b&gt;") {
		t.Errorf("expected escaped tags in output: %q", got)
	}
}

func TestPluralRU(t *testing.T) {
	tests := []struct {
		n    int
		want string
	}{
		{0, "дней"},
		{1, "день"},
		{2, "дня"},
		{4, "дня"},
		{5, "дней"},
		{10, "дней"},
		{11, "дней"},
		{12, "дней"},
		{14, "дней"},
		{15, "дней"},
		{21, "день"},
		{22, "дня"},
		{25, "дней"},
		{101, "день"},
		{111, "дней"},
		{121, "день"},
	}
	for _, tc := range tests {
		if got := pluralRU(tc.n, "день", "дня", "дней"); got != tc.want {
			t.Errorf("pluralRU(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

func TestHumanDaysRU(t *testing.T) {
	tests := []struct {
		days int
		want string
	}{
		{0, "0 дней"},
		{1, "1 день"},
		{5, "5 дней"},
		{29, "29 дней"},
		{30, "1 месяц"},
		{60, "2 месяца"},
		{150, "5 месяцев"},
		{365, "1 год"},
		{730, "2 года"},
		{2000, "5 лет"},
	}
	for _, tc := range tests {
		if got := humanDaysRU(tc.days); got != tc.want {
			t.Errorf("humanDaysRU(%d) = %q, want %q", tc.days, got, tc.want)
		}
	}
}
