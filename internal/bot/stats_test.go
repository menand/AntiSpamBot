package bot

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/menand/AntiSpamBot/internal/storage"
)

func TestParsePeriod(t *testing.T) {
	for _, p := range []statsPeriod{periodDay, periodYesterday, periodDayBefore, periodWeek, periodMonth, periodAll} {
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

func TestStatsRange(t *testing.T) {
	// 12 июля 01:30 МСК = 11 июля 22:30 UTC: «сегодня» уже 12-е по Москве,
	// хотя по UTC ещё 11-е — ровно тот случай, ради которого статистика
	// выровнена по storage.StatsLocation.
	now := time.Date(2026, 7, 11, 22, 30, 0, 0, time.UTC)
	msk := storage.StatsLocation
	midnight := time.Date(2026, 7, 12, 0, 0, 0, 0, msk)

	tests := []struct {
		p           statsPeriod
		from, until time.Time
	}{
		{periodDay, midnight, midnight.AddDate(0, 0, 1)},
		{periodYesterday, midnight.AddDate(0, 0, -1), midnight},
		{periodDayBefore, midnight.AddDate(0, 0, -2), midnight.AddDate(0, 0, -1)},
		{periodWeek, midnight.AddDate(0, 0, -6), midnight.AddDate(0, 0, 1)},
		{periodMonth, midnight.AddDate(0, 0, -29), midnight.AddDate(0, 0, 1)},
		{periodAll, time.Unix(0, 0), midnight.AddDate(0, 0, 1)},
	}
	for _, tc := range tests {
		from, until := statsRange(tc.p, now)
		if !from.Equal(tc.from) || !until.Equal(tc.until) {
			t.Errorf("statsRange(%s) = [%v, %v), want [%v, %v)",
				tc.p, from, until, tc.from, tc.until)
		}
	}

	// «Позавчера», «вчера» и «сегодня» стыкуются без зазоров и пересечений.
	bFrom, bUntil := statsRange(periodDayBefore, now)
	yFrom, yUntil := statsRange(periodYesterday, now)
	dFrom, _ := statsRange(periodDay, now)
	if !bUntil.Equal(yFrom) {
		t.Errorf("daybefore.until (%v) must equal yesterday.from (%v)", bUntil, yFrom)
	}
	if !yUntil.Equal(dFrom) {
		t.Errorf("yesterday.until (%v) must equal day.from (%v)", yUntil, dFrom)
	}
	if got := dFrom.Sub(yFrom); got != 24*time.Hour {
		t.Errorf("yesterday window = %v, want 24h", got)
	}
	if got := yFrom.Sub(bFrom); got != 24*time.Hour {
		t.Errorf("daybefore window = %v, want 24h", got)
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
	for _, header := range []string{"Новые участники", "Кикнуты/забанены", "Забанены"} {
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
	for _, header := range []string{"Новые участники", "Кикнуты/забанены", "Забанены"} {
		if !strings.Contains(out, header) {
			t.Fatalf("header %q must survive truncation:\n%s", header, out)
		}
	}
	if n := utf8.RuneCountInString(out); n >= 4096 {
		t.Fatalf("rendered stats must fit a Telegram message, got %d runes", n)
	}
}
