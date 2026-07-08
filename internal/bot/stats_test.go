package bot

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/menand/AntiSpamBot/internal/storage"
)

func TestRenderStatsFailersComplete(t *testing.T) {
	s := storage.Stats{Joined: 10, Passed: 5, Kicked: 3, Banned: 2}
	failers := []storage.UserCount{
		{UserID: 1001, Count: 3},
		{UserID: 1002, Count: 2},
		{UserID: 1003, Count: 1},
	}
	out := renderStats(periodAll, "всё время", s, 7, nil, failers, map[int64]storage.UserInfo{})
	if strings.Contains(out, "…и ещё") {
		t.Fatalf("short list must not be truncated:\n%s", out)
	}
	for _, id := range []string{"id1001", "id1002", "id1003"} {
		if !strings.Contains(out, id) {
			t.Fatalf("missing %s in:\n%s", id, out)
		}
	}
}

func TestRenderStatsFailersTruncatedToMessageLimit(t *testing.T) {
	s := storage.Stats{Joined: 500, Passed: 100, Kicked: 300, Banned: 100}
	failers := make([]storage.UserCount, 200)
	for i := range failers {
		failers[i] = storage.UserCount{UserID: int64(100000000 + i), Count: 2}
	}
	out := renderStats(periodMonth, "месяц", s, 7, nil, failers, map[int64]storage.UserInfo{})
	if !strings.Contains(out, "…и ещё") {
		t.Fatal("long list must end with an «…и ещё N» tail")
	}
	if n := utf8.RuneCountInString(out); n >= 4096 {
		t.Fatalf("rendered stats must fit a Telegram message, got %d runes", n)
	}
}
