package bot

import (
	"testing"
	"time"

	"github.com/menand/AntiSpamBot/internal/storage"
)

func TestParseModChoice(t *testing.T) {
	if a, id, ok := parseModChoice("mc:u:42"); !ok || a != "u" || id != 42 {
		t.Fatalf("mc:u:42 → (%q,%d,%v)", a, id, ok)
	}
	for _, bad := range []string{"mc:x", "mc:z:42", "mc:u:abc", "mc:u", "sv:1", "mc:u:1:2"} {
		if _, _, ok := parseModChoice(bad); ok {
			t.Errorf("parseModChoice(%q) must fail", bad)
		}
	}
}

func TestUnmodListView(t *testing.T) {
	recent := make([]storage.RecentUser, 7)
	for i := range recent {
		recent[i] = storage.RecentUser{UserID: int64(100 + i), At: time.Now().Add(-time.Hour)}
	}
	_, kb := unmodListView("🚫 Тест", "u", recent, map[int64]storage.UserInfo{})
	// 7 юзеров → ряд из 5 + ряд из 2 + ряд «Отмена».
	if len(kb.InlineKeyboard) != 3 {
		t.Fatalf("want 3 rows, got %d", len(kb.InlineKeyboard))
	}
	if len(kb.InlineKeyboard[0]) != 5 || len(kb.InlineKeyboard[1]) != 2 || len(kb.InlineKeyboard[2]) != 1 {
		t.Fatalf("wrong row sizes: %d/%d/%d",
			len(kb.InlineKeyboard[0]), len(kb.InlineKeyboard[1]), len(kb.InlineKeyboard[2]))
	}
	// Кнопка N ведёт на юзера N-го пункта списка.
	if got := kb.InlineKeyboard[1][1].CallbackData; got != "mc:u:106" {
		t.Fatalf("button 7 callback = %q, want mc:u:106", got)
	}
	if got := kb.InlineKeyboard[2][0].CallbackData; got != "mc:x" {
		t.Fatalf("cancel callback = %q, want mc:x", got)
	}
	// Каждая кнопка выбора парсится собственным парсером.
	for _, row := range kb.InlineKeyboard[:2] {
		for _, btn := range row {
			if _, _, ok := parseModChoice(btn.CallbackData); !ok {
				t.Errorf("button %q does not round-trip through parseModChoice", btn.CallbackData)
			}
		}
	}
}
