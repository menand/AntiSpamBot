package bot

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/mymmrac/telego"

	"github.com/menand/AntiSpamBot/internal/storage"
)

func TestVoteVerdict(t *testing.T) {
	tests := []struct {
		yes, no     int
		wantSpam    bool
		wantDecided bool
	}{
		{0, 0, false, false},
		{2, 0, false, false},
		{3, 0, true, true},
		{5, 2, true, true},
		{0, 3, false, true},
		{2, 5, false, true},
		{4, 2, false, false}, // перевес 2 — мало
	}
	for _, tc := range tests {
		spam, decided := voteVerdict(tc.yes, tc.no)
		if spam != tc.wantSpam || decided != tc.wantDecided {
			t.Errorf("voteVerdict(%d, %d) = (%v, %v), want (%v, %v)",
				tc.yes, tc.no, spam, decided, tc.wantSpam, tc.wantDecided)
		}
	}
}

func TestEffectiveSpamSettings(t *testing.T) {
	var s storage.ChatSettings
	if effectiveSpamThreshold(s) != defaultSpamThreshold {
		t.Errorf("NULL threshold must fall back to %d", defaultSpamThreshold)
	}
	if effectiveSpamWhitelist(s) != defaultSpamWhitelist {
		t.Errorf("NULL whitelist must fall back to %d", defaultSpamWhitelist)
	}
	s.SpamThreshold = sql.NullInt64{Int64: 75, Valid: true}
	s.SpamWhitelistMsgs = sql.NullInt64{Int64: 10, Valid: true}
	if effectiveSpamThreshold(s) != 75 || effectiveSpamWhitelist(s) != 10 {
		t.Error("valid overrides must win")
	}
	// Мусор в БД → дефолт.
	s.SpamThreshold = sql.NullInt64{Int64: 500, Valid: true}
	s.SpamWhitelistMsgs = sql.NullInt64{Int64: -3, Valid: true}
	if effectiveSpamThreshold(s) != defaultSpamThreshold {
		t.Error("out-of-range threshold must fall back")
	}
	if effectiveSpamWhitelist(s) != defaultSpamWhitelist {
		t.Error("non-positive whitelist must fall back")
	}
}

func TestBuildSpamFacts(t *testing.T) {
	m := telego.Message{
		From: &telego.User{FirstName: "Вася", LastName: "Пупкин", Username: "vasya"},
		Text: "Заработок от 500$ в день, пиши в личку!",
	}
	facts := buildSpamFacts(m, "5 минут", 1)
	for _, want := range []string{"Вася Пупкин", "@vasya", "в чате 5 минут", "всего сообщений: 1", "Заработок от 500$"} {
		if !strings.Contains(facts, want) {
			t.Errorf("facts missing %q:\n%s", want, facts)
		}
	}

	// Гифка без текста + форвард из канала.
	m2 := telego.Message{
		From:      &telego.User{FirstName: "X"},
		Animation: &telego.Animation{},
		Document:  &telego.Document{}, // гифки выставляют оба поля
		ForwardOrigin: &telego.MessageOriginChannel{
			Chat: telego.Chat{Title: "Крипта Сигналы"},
		},
	}
	facts2 := buildSpamFacts(m2, "", 1)
	for _, want := range []string{"гифка, без текста", "Переслано из канала «Крипта Сигналы»"} {
		if !strings.Contains(facts2, want) {
			t.Errorf("facts missing %q:\n%s", want, facts2)
		}
	}
	if strings.Contains(facts2, "файл") {
		t.Errorf("animation must not be reported as document:\n%s", facts2)
	}

	// Фото с подписью.
	m3 := telego.Message{
		From:    &telego.User{FirstName: "Y"},
		Photo:   []telego.PhotoSize{{}},
		Caption: "Скидки на всё!",
	}
	facts3 := buildSpamFacts(m3, "", 2)
	for _, want := range []string{"фото с подписью", "Скидки на всё!"} {
		if !strings.Contains(facts3, want) {
			t.Errorf("facts missing %q:\n%s", want, facts3)
		}
	}
}

func TestHumanDurationRU(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "меньше минуты"},
		{5 * time.Minute, "5 минут"},
		{2 * time.Hour, "2 часа"},
		{72 * time.Hour, "3 дня"},
	}
	for _, tc := range tests {
		if got := humanDurationRU(tc.d); got != tc.want {
			t.Errorf("humanDurationRU(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}
