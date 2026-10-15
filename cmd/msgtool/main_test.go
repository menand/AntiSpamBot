package main

import (
	"testing"
	"time"

	"github.com/menand/AntiSpamBot/internal/archive"
	"github.com/menand/AntiSpamBot/internal/storage"
)

func TestDayInRange(t *testing.T) {
	since, _ := time.Parse("2006-01-02", "2026-10-01")
	until, _ := time.Parse("2006-01-02", "2026-11-01") // уже +1 день в main
	cases := []struct {
		day  string
		want bool
	}{
		{"2026-09-30", false}, // до since
		{"2026-10-01", true},  // нижняя граница включительно
		{"2026-10-31", true},
		{"2026-11-01", false}, // верхняя исключительно
		{"not-a-day", false},
	}
	for _, c := range cases {
		if got := dayInRange(c.day, since, until); got != c.want {
			t.Errorf("dayInRange(%q) = %v, want %v", c.day, got, c.want)
		}
	}
	// Нулевые границы = без фильтра.
	if !dayInRange("2020-01-01", time.Time{}, time.Time{}) {
		t.Error("zero bounds must include any valid day")
	}
}

func TestEntryMatches(t *testing.T) {
	cases := []struct {
		name            string
		chat, user      int64
		text, replyText string
		fChat, fUser    int64
		fWord           string
		want            bool
	}{
		{"no filters", -1001, 10, "ананас", "", 0, 0, "", true},
		{"chat mismatch", -1001, 10, "x", "", -1002, 0, "", false},
		{"chat match", -1001, 10, "x", "", -1001, 0, "", true},
		{"user mismatch", -1001, 10, "x", "", 0, 11, "", false},
		{"word in text", -1001, 10, "про ананас", "", 0, 0, "ананас", true},
		{"word in reply only", -1001, 10, "x", "ананас", 0, 0, "ананас", true},
		{"word miss", -1001, 10, "x", "y", 0, 0, "ананас", false},
		{"word is wildcard literal", -1001, 10, "100% скидка", "", 0, 0, "100%", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := entryMatches(c.chat, c.user, c.text, c.replyText, c.fChat, c.fUser, c.fWord)
			if got != c.want {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestParseDay(t *testing.T) {
	if d, err := parseDay(""); err != nil || !d.IsZero() {
		t.Fatalf("empty: got %v err %v, want zero", d, err)
	}
	d, err := parseDay("2026-10-15")
	if err != nil {
		t.Fatalf("valid: %v", err)
	}
	if d.Year() != 2026 || d.Month() != time.October || d.Day() != 15 {
		t.Fatalf("parsed wrong: %v", d)
	}
	if _, err := parseDay("15.10.2026"); err == nil {
		t.Fatal("non-YYYY-MM-DD must error")
	}
}

func TestDerefStr(t *testing.T) {
	if derefStr(nil) != "" {
		t.Fatal("nil must yield empty")
	}
	s := "abc"
	if derefStr(&s) != "abc" {
		t.Fatal("pointer must deref")
	}
}

// Оба источника сводятся к одному wireEntry: source/time/RFC3339 и разыменование
// reply-указателей hot-стороны (nil → пустые строки, не мусор и не паника).
func TestFromArchiveAndFromHotMapping(t *testing.T) {
	rid := int64(7)
	when := time.Date(2026, 10, 1, 21, 30, 0, 0, time.UTC)

	a := fromArchive(archive.Entry{
		Chat: -100, User: 5, TGMsgID: 9, TS: when.Unix(), Kind: "text", Text: "t",
		ReplyMsgID: &rid,
	})
	if a.Source != "archive" || a.Chat != -100 || a.TGMsgID != 9 {
		t.Fatalf("fromArchive identity: %+v", a)
	}
	if a.Time != "2026-10-01T21:30:00Z" {
		t.Fatalf("fromArchive time = %q", a.Time)
	}
	if a.ReplyMsgID == nil || *a.ReplyMsgID != 7 {
		t.Fatalf("fromArchive reply msg id: %+v", a.ReplyMsgID)
	}

	author, text := "Автор", "цитата"
	h := fromHot(storage.StoredMessage{
		ChatID: -100, UserID: 5, TGMsgID: 9, TS: when, Kind: "edit", Text: "t2",
		ReplyAuthor: &author, ReplyText: &text,
	})
	if h.Source != "hot" || h.Kind != "edit" || h.ReplyAuthor != author || h.ReplyText != text {
		t.Fatalf("fromHot mapping: %+v", h)
	}
	if h.Time != "2026-10-01T21:30:00Z" {
		t.Fatalf("fromHot time = %q", h.Time)
	}

	// Nil-reply запись: пустые строки, без паники.
	plain := fromHot(storage.StoredMessage{TS: when})
	if plain.ReplyAuthor != "" || plain.ReplyText != "" || plain.ReplyMsgID != nil {
		t.Fatalf("nil reply deref: %+v", plain)
	}
}
