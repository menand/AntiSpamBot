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

func TestReplyAnsweredCard(t *testing.T) {
	tests := []struct {
		name  string
		chat  storage.ChatInfo
		who   string
		stage int
		want  string
	}{
		{
			name: "публичный чат, стадия 1",
			chat: storage.ChatInfo{ChatID: -1001, Title: "Чат", Username: "mysuperchat"},
			who:  `<a href="tg://user?id=7">Вася</a> - @vasya`,
			want: `💬 Прошёл 2-й уровень защиты в <a href="https://t.me/mysuperchat">«Чат»</a>` +
				"\nКто: <a href=\"tg://user?id=7\">Вася</a> - @vasya",
		},
		{
			name: "приватная супергруппа рендерится ссылкой t.me/c",
			chat: storage.ChatInfo{ChatID: -1002147483648, Title: "Закрытый"},
			who:  `<a href="tg://user?id=7">Вася</a>`,
			want: "💬 Прошёл 2-й уровень защиты в " +
				`<a href="https://t.me/c/2147483648/999999999">«Закрытый»</a>` +
				"\nКто: <a href=\"tg://user?id=7\">Вася</a>",
		},
		{
			name:  "поздний ответ (стадия 2) — с пометкой",
			chat:  storage.ChatInfo{ChatID: -1001, Title: "Чат"},
			who:   "Вася",
			stage: 2,
			want:  "💬 Прошёл 2-й уровень защиты в «Чат»\nКто: Вася\nОтветил только после напоминания.",
		},
		{
			name:  "ответ после последнего предупреждения (стадия 3) — отдельная формулировка",
			chat:  storage.ChatInfo{ChatID: -1001, Title: "Чат"},
			who:   "Вася",
			stage: 3,
			want:  "💬 Прошёл 2-й уровень защиты в «Чат»\nКто: Вася\nОтветил после последнего предупреждения.",
		},
		{
			name:  "имя с HTML-спецсимволами уже экранировано у вызывающего",
			chat:  storage.ChatInfo{ChatID: -1001, Title: "Чат"},
			who:   `A&lt;B &gt; C`,
			stage: 1,
			want:  "💬 Прошёл 2-й уровень защиты в «Чат»\nКто: A&lt;B &gt; C",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := replyAnsweredCard(tc.chat, tc.who, tc.stage); got != tc.want {
				t.Errorf("replyAnsweredCard() =\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}
