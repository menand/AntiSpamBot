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
		0, newMembers, nil, nil, nil, map[int64]storage.UserInfo{})
	// Ниже минуты рендерятся честные секунды; выше — humanDurationRU.
	if !strings.Contains(out, "id2001</a> — за 12 сек") {
		t.Fatalf("expected solve time for 2001:\n%s", out)
	}
	if strings.Contains(out, "id2002</a> — за") {
		t.Fatalf("2002 has no recorded join — must render without time:\n%s", out)
	}
}

func TestRenderStatsLeft(t *testing.T) {
	// Воронка с «вышли сами»: строка есть, процент от Joined, «В процессе»
	// учитывает Left (5+3+1+1=10, значит 0 в процессе).
	s := storage.Stats{Joined: 10, Passed: 5, Kicked: 3, Banned: 1, Left: 1}
	out := renderStats(periodDay, "сегодня", s, 7,
		0, nil, nil, nil, nil, map[int64]storage.UserInfo{})
	if !strings.Contains(out, "Вышли сами: 1 (10%)") {
		t.Fatalf("expected «Вышли сами» line:\n%s", out)
	}
	if strings.Contains(out, "В процессе") {
		t.Fatalf("funnel must close with Left counted, got:\n%s", out)
	}

	// Left = 0 — строки нет (не шумим).
	s2 := storage.Stats{Joined: 10, Passed: 5, Kicked: 3, Banned: 1}
	out2 := renderStats(periodDay, "сегодня", s2, 7,
		0, nil, nil, nil, nil, map[int64]storage.UserInfo{})
	if strings.Contains(out2, "Вышли сами") {
		t.Fatalf("no Left — no line expected:\n%s", out2)
	}
}

func TestRenderStatsListsComplete(t *testing.T) {
	s := storage.Stats{Joined: 10, Passed: 5, Kicked: 3, Banned: 2}
	newMembers := fakeUsers(2001, 2, 1)
	failers := fakeUsers(1001, 3, 2)
	banned := fakeUsers(3001, 2, 1)
	out := renderStats(periodAll, "всё время", s, 7,
		0, newMembers, nil, failers, banned, map[int64]storage.UserInfo{})
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
		5,                            // compact view: 5 элементов на секцию
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

func TestHasMoreItems(t *testing.T) {
	if hasMoreItems(nil, nil, nil, nil) {
		t.Fatal("empty lists must return false")
	}
	if hasMoreItems(fakeUsers(1, 5, 1)) {
		t.Fatal("5 items must return false (compact limit)")
	}
	if !hasMoreItems(fakeUsers(1, 6, 1)) {
		t.Fatal("6 items must return true")
	}
	if !hasMoreItems(nil, fakeUsers(1, 10, 1), nil, nil) {
		t.Fatal("10 items in second list must return true")
	}
}

func TestRenderStatsCompactView(t *testing.T) {
	s := storage.Stats{Joined: 20, Passed: 15, Kicked: 5, Banned: 3}
	out := renderStats(periodWeek, "неделю", s, 7,
		5,                      // compact: 5 элементов
		fakeUsers(1000, 15, 1), // 15 новичков
		fakeUsers(2000, 8, 10), // 8 топ-писателей
		fakeUsers(3000, 10, 2), // 10 фейлеров
		fakeUsers(4000, 5, 1),  // 5 забаненных
		map[int64]storage.UserInfo{})
	// Новички: показаны 5, остальные10 скрыты
	if !strings.Contains(out, "…и ещё 10 человек") {
		t.Fatalf("newcomers must show «…и ещё 10»:\n%s", out)
	}
	// Топ-писатели: показаны 5, остальные3 скрыты
	if !strings.Contains(out, "…и ещё 3 человек") {
		t.Fatalf("top writers must show «…и ещё 3»:\n%s", out)
	}
	// Фейлеры: показаны 5, остальные5 скрыты
	if !strings.Contains(out, "…и ещё 5 человек") {
		t.Fatalf("failers must show «…и ещё 5»:\n%s", out)
	}
	// Забаненные: ровно 5 — без «…и ещё»
	if strings.Contains(out, "…и ещё") && strings.Contains(out, "Забанены") {
		// Проверяем что «…и ещё» после «Забанены» нет
		bannedIdx := strings.Index(out, "⛔️")
		if bannedIdx > 0 {
			tail := out[bannedIdx:]
			if strings.Contains(tail, "…и ещё") {
				t.Fatalf("banned list (5 items) must not truncate:\n%s", out)
			}
		}
	}
	if n := utf8.RuneCountInString(out); n >= 4096 {
		t.Fatalf("compact view must fit Telegram message, got %d runes", n)
	}
}

func TestExtendedStatsKeyboard(t *testing.T) {
	kb := extendedStatsKeyboard(-100123, "n", 0, 3, periodWeek)
	if len(kb.InlineKeyboard) != 4 {
		t.Fatalf("expected 4 rows, got %d", len(kb.InlineKeyboard))
	}
	// Row 0: tabs
	row0 := kb.InlineKeyboard[0]
	if len(row0) != 2 {
		t.Fatalf("tab row must have 2 buttons, got %d", len(row0))
	}
	// Active tab "Новички" must have • prefix
	if !strings.Contains(row0[0].Text, "•") {
		t.Fatalf("active tab must have • prefix, got %q", row0[0].Text)
	}
	// Callback data format
	if !strings.HasPrefix(row0[0].CallbackData, "estats:") {
		t.Fatalf("tab callback must start with estats:, got %q", row0[0].CallbackData)
	}
	// Row 2: pagination (3 pages → has navigation)
	row2 := kb.InlineKeyboard[2]
	if len(row2) != 2 { // [1/3, ▶️] — page 0, no ◀️
		t.Fatalf("pagination row must have 2 buttons on page 0, got %d", len(row2))
	}
	// Row 3: close button
	row3 := kb.InlineKeyboard[3]
	if !strings.Contains(row3[0].Text, "Закрыть") {
		t.Fatalf("last row must be close button, got %q", row3[0].Text)
	}
	if !strings.HasPrefix(row3[0].CallbackData, "menu:stats:") {
		t.Fatalf("close callback must go back to stats, got %q", row3[0].CallbackData)
	}

	// Middle page: both ◀️ and ▶️
	kb2 := extendedStatsKeyboard(-100123, "w", 1, 3, periodMonth)
	row2b := kb2.InlineKeyboard[2]
	if len(row2b) != 3 { // [◀️, 2/3, ▶️]
		t.Fatalf("middle page must have 3 nav buttons, got %d", len(row2b))
	}

	// Last page: only ◀️
	kb3 := extendedStatsKeyboard(-100123, "k", 2, 3, periodAll)
	row2c := kb3.InlineKeyboard[2]
	if len(row2c) != 2 { // [◀️, 3/3]
		t.Fatalf("last page must have 2 nav buttons, got %d", len(row2c))
	}
}
