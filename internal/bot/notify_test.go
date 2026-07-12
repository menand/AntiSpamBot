package bot

import (
	"testing"

	"github.com/menand/AntiSpamBot/internal/storage"
)

func TestHumanReasonWith(t *testing.T) {
	infos := map[int64]storage.UserInfo{
		10: {UserID: 10, FirstName: "Админ", Username: "admin"},
		20: {UserID: 20, FirstName: "Пётр"},
		30: {UserID: 30, FirstName: "Аня"},
	}
	lookup := func(ids []int64) map[int64]storage.UserInfo { return infos }

	tests := []struct {
		reason string
		want   string
	}{
		{"", ""},
		{storage.ReasonCaptcha, "не прошёл капчу"},
		{storage.ReasonNoReply, "не ответил на приветствие"},
		{storage.ReasonGlobal, "в глобальной базе спамеров"},
		{storage.ReasonModPrefix + "10", `команда админа <a href="tg://user?id=10">Админ</a> - @admin`},
		{storage.ReasonVotePrefix + "20,30", `голосование: <a href="tg://user?id=20">Пётр</a>, <a href="tg://user?id=30">Аня</a>`},
		{storage.ReasonVotePrefix, "голосование чата"}, // золотой голос без бюллетеней
	}
	for _, tc := range tests {
		if got := humanReasonWith(tc.reason, lookup); got != tc.want {
			t.Errorf("humanReasonWith(%q) = %q, want %q", tc.reason, got, tc.want)
		}
	}
}

func TestReasonUserIDs(t *testing.T) {
	lists := [][]storage.UserCount{{
		{UserID: 1, LastReason: storage.ReasonCaptcha},
		{UserID: 2, LastReason: storage.ReasonModPrefix + "10"},
		{UserID: 3, LastReason: storage.ReasonVotePrefix + "20,30"},
	}}
	got := reasonUserIDs(lists...)
	want := map[int64]bool{10: true, 20: true, 30: true}
	if len(got) != 3 {
		t.Fatalf("got %v, want 3 ids", got)
	}
	for _, id := range got {
		if !want[id] {
			t.Errorf("unexpected id %d", id)
		}
	}
}

func TestParseVoteIDs(t *testing.T) {
	if ids := parseVoteIDs("vote:1,2,3"); len(ids) != 3 || ids[0] != 1 || ids[2] != 3 {
		t.Errorf("parseVoteIDs = %v", ids)
	}
	if ids := parseVoteIDs("vote:"); ids != nil {
		t.Errorf("empty vote must give nil, got %v", ids)
	}
}
