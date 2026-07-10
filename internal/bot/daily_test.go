package bot

import (
	"reflect"
	"testing"

	"github.com/menand/AntiSpamBot/internal/storage"
)

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
