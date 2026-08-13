package bot

import (
	"reflect"
	"testing"

	"github.com/menand/AntiSpamBot/internal/storage"
)

func TestReportLine(t *testing.T) {
	tests := []struct {
		name string
		c    storage.ChatInfo
		s    storage.Stats
		want string
	}{
		{"empty day", storage.ChatInfo{ChatID: -5, Title: "Бег"}, storage.Stats{},
			"«Бег» — без событий"},
		{"normal day", storage.ChatInfo{ChatID: -5, Title: "Бег"},
			storage.Stats{Joined: 5, Passed: 4, Kicked: 1},
			"«Бег» — вступило 5, прошло 4, вышли сами 0, кик 1, бан 0"},
		{"bans merge spam", storage.ChatInfo{ChatID: -5, Title: "Бег"},
			storage.Stats{Banned: 1, SpamBanned: 2},
			"«Бег» — вступило 0, прошло 0, вышли сами 0, кик 0, бан 3 (из них спам 2)"},
		{"title escaped", storage.ChatInfo{ChatID: -5, Title: "A<b>&"}, storage.Stats{Passed: 1},
			"«A&lt;b&gt;&amp;» — вступило 0, прошло 1, вышли сами 0, кик 0, бан 0"},
		{"public chat linked", storage.ChatInfo{ChatID: -5, Title: "Бег", Username: "run_chat"},
			storage.Stats{Passed: 1},
			`<a href="https://t.me/run_chat">«Бег»</a> — вступило 0, прошло 1, вышли сами 0, кик 0, бан 0`},
		{"left counted", storage.ChatInfo{ChatID: -5, Title: "Бег"},
			storage.Stats{Joined: 5, Passed: 3, Kicked: 1, Left: 1},
			"«Бег» — вступило 5, прошло 3, вышли сами 1, кик 1, бан 0"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := reportLine(tc.c, tc.s); got != tc.want {
				t.Errorf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestDigestHasContent(t *testing.T) {
	if digestHasContent(storage.Stats{}, nil, nil, nil, nil) {
		t.Fatal("fully empty day must skip the digest")
	}
	// День, где единственная активность — прохождение капчи (join остался за
	// границей окна: вошёл 23:59, прошёл 00:01).
	if !digestHasContent(storage.Stats{Passed: 1}, nil, nil,
		[]storage.UserCount{{UserID: 1}}, nil) {
		t.Fatal("pass-only day must produce a digest")
	}
	// День, где был только бан ИИ-антиспамом.
	if !digestHasContent(storage.Stats{SpamBanned: 1}, nil, nil, nil,
		[]storage.UserCount{{UserID: 2}}) {
		t.Fatal("spamban-only day must produce a digest")
	}
	if !digestHasContent(storage.Stats{MsgOldtimer: 5}, nil, nil, nil, nil) {
		t.Fatal("messages-only day must produce a digest")
	}
	// День, где единственное событие — юзер вышел посреди капчи.
	if !digestHasContent(storage.Stats{Left: 1}, nil, nil, nil, nil) {
		t.Fatal("left-only day must produce a digest")
	}
	if !digestHasContent(storage.Stats{}, nil, []storage.UserCount{{UserID: 3}}, nil, nil) {
		t.Fatal("failers-only day must produce a digest")
	}
}

// TestDigestHasContentCoversAllStatsCounters — страж от дрейфа: новый
// счётчик в storage.Stats, забытый в digestHasContent, приводил бы к тихо
// пропущенным дайджестам (SpamBanned уже однажды так потерялся).
func TestDigestHasContentCoversAllStatsCounters(t *testing.T) {
	typ := reflect.TypeOf(storage.Stats{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.Type.Kind() != reflect.Int {
			continue // PeriodFrom/PeriodUntil — не счётчики
		}
		var s storage.Stats
		reflect.ValueOf(&s).Elem().Field(i).SetInt(1)
		if !digestHasContent(s, nil, nil, nil, nil) {
			t.Errorf("Stats.%s=1 must make the digest non-empty — add it to digestHasContent", f.Name)
		}
	}
}
