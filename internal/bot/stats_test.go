package bot

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/menand/AntiSpamBot/internal/storage"
)

func TestParsePeriod(t *testing.T) {
	for _, p := range []statsPeriod{periodDay, periodWeek, periodMonth, periodAll} {
		if got := parsePeriod(string(p)); got != p {
			t.Errorf("parsePeriod(%q) = %q, want %q", p, got, p)
		}
	}
	// Мусор из подделанного/устаревшего callback data не должен дойти ни до
	// statsRange, ни до HTML.
	for _, junk := range []string{"", "year", "<b>xss</b>", "day "} {
		if got := parsePeriod(junk); got != periodWeek {
			t.Errorf("parsePeriod(%q) = %q, want fallback %q", junk, got, periodWeek)
		}
	}
}

func fakeUsers(startID int64, n, count int) []storage.UserCount {
	out := make([]storage.UserCount, n)
	for i := range out {
		out[i] = storage.UserCount{UserID: startID + int64(i), Count: count, Secs: -1}
	}
	return out
}

func TestRenderStatsNewMemberSeconds(t *testing.T) {
	s := storage.Stats{Joined: 2, Passed: 2}
	newMembers := []storage.UserCount{
		{UserID: 2001, Count: 1, Secs: 12},
		{UserID: 2002, Count: 1, Secs: -1},
	}
	out := renderStats(periodDay, "сегодня", s, 7,
		newMembers, nil, nil, nil, map[int64]storage.UserInfo{})
	if !strings.Contains(out, "id2001</a> — за 12 сек") {
		t.Fatalf("expected solve time for 2001:\n%s", out)
	}
	if strings.Contains(out, "id2002</a> — за") {
		t.Fatalf("2002 has no recorded join — must render without time:\n%s", out)
	}
}

func TestRenderStatsListsComplete(t *testing.T) {
	s := storage.Stats{Joined: 10, Passed: 5, Kicked: 3, Banned: 2}
	newMembers := fakeUsers(2001, 2, 1)
	failers := fakeUsers(1001, 3, 2)
	banned := fakeUsers(3001, 2, 1)
	out := renderStats(periodAll, "всё время", s, 7,
		newMembers, nil, failers, banned, map[int64]storage.UserInfo{})
	if strings.Contains(out, "…и ещё") {
		t.Fatalf("short lists must not be truncated:\n%s", out)
	}
	for _, id := range []string{
		"id1001", "id1002", "id1003", // провалы
		"id2001", "id2002", // новые участники
		"id3001", "id3002", // забаненые
	} {
		if !strings.Contains(out, id) {
			t.Fatalf("missing %s in:\n%s", id, out)
		}
	}
	for _, header := range []string{"Новые участники", "Провалили капчу", "Забанены"} {
		if !strings.Contains(out, header) {
			t.Fatalf("missing header %q in:\n%s", header, out)
		}
	}
}

func TestRenderStatsTruncatedToMessageLimit(t *testing.T) {
	s := storage.Stats{Joined: 600, Passed: 200, Kicked: 300, Banned: 100}
	out := renderStats(periodMonth, "месяц", s, 7,
		fakeUsers(100000000, 200, 1), // новые участники
		fakeUsers(500000000, 5, 40),  // топ писателей
		fakeUsers(200000000, 200, 2), // провалы
		fakeUsers(300000000, 100, 1), // забаненые
		map[int64]storage.UserInfo{})
	if !strings.Contains(out, "…и ещё") {
		t.Fatal("huge lists must end with «…и ещё N» tails")
	}
	for _, header := range []string{"Новые участники", "Провалили капчу", "Забанены"} {
		if !strings.Contains(out, header) {
			t.Fatalf("header %q must survive truncation:\n%s", header, out)
		}
	}
	if n := utf8.RuneCountInString(out); n >= 4096 {
		t.Fatalf("rendered stats must fit a Telegram message, got %d runes", n)
	}
}
