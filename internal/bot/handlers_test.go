package bot

import (
	"errors"
	"testing"

	"github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegoapi"
)

func TestParseCallback(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		wantUID int64
		wantIdx int
		wantOK  bool
	}{
		{"valid", "cap:12345:3", 12345, 3, true},
		{"negative user id", "cap:-1001234:0", -1001234, 0, true},
		{"wrong prefix", "foo:1:2", 0, 0, false},
		{"not enough parts", "cap:1", 0, 0, false},
		{"too many parts", "cap:1:2:3", 0, 0, false},
		{"bad user id", "cap:abc:1", 0, 0, false},
		{"bad index", "cap:1:x", 0, 0, false},
		{"empty", "", 0, 0, false},
		{"trailing garbage", "cap:1:2trailing", 0, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			uid, idx, ok := parseCallback(tc.data)
			if ok != tc.wantOK || uid != tc.wantUID || idx != tc.wantIdx {
				t.Fatalf("parseCallback(%q) = (%d, %d, %v), want (%d, %d, %v)",
					tc.data, uid, idx, ok, tc.wantUID, tc.wantIdx, tc.wantOK)
			}
		})
	}
}

func TestStaleChatReason(t *testing.T) {
	tests := []struct {
		name      string
		m         telego.ChatMember
		err       error
		wantStale bool
	}{
		{"member", &telego.ChatMemberMember{}, nil, false},
		{"admin", &telego.ChatMemberAdministrator{}, nil, false},
		{"left", &telego.ChatMemberLeft{}, nil, true},
		{"kicked", &telego.ChatMemberBanned{}, nil, true},
		{"400 chat not found", nil, &telegoapi.Error{ErrorCode: 400, Description: "Bad Request: chat not found"}, true},
		{"403 bot kicked", nil, &telegoapi.Error{ErrorCode: 403, Description: "Forbidden: bot was kicked"}, true},
		{"429 flood", nil, &telegoapi.Error{ErrorCode: 429, Description: "Too Many Requests"}, false},
		{"500 server error", nil, &telegoapi.Error{ErrorCode: 500, Description: "Internal Server Error"}, false},
		{"network error", nil, errors.New("dial tcp: timeout"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reason, stale := staleChatReason(tc.m, tc.err)
			if stale != tc.wantStale {
				t.Fatalf("staleChatReason() = (%q, %v), want stale=%v", reason, stale, tc.wantStale)
			}
			if stale && reason == "" {
				t.Fatal("stale chat must carry a non-empty reason for the log")
			}
		})
	}
}

func TestPickedVsCorrect(t *testing.T) {
	kb := &telego.Message{ReplyMarkup: &telego.InlineKeyboardMarkup{
		InlineKeyboard: [][]telego.InlineKeyboardButton{
			{{Text: "🔴"}, {Text: "🟢"}, {Text: "🔵"}},
			{{Text: "✅ Впустить (для админов)"}},
		},
	}}
	tests := []struct {
		name            string
		msg             *telego.Message
		picked, correct int
		want            string
	}{
		{"with keyboard", kb, 0, 2, ": выбрал 1-й (🔴), верный 3-й (🔵)"},
		{"nil message", nil, 1, 2, ": выбрал 2-й, верный 3-й"},
		{"picked out of row", kb, 5, 2, ": выбрал 6-й, верный 3-й (🔵)"},
		{"correct out of row", kb, 1, 5, ": выбрал 2-й (🟢), верный 6-й"},
		{"negative picked", kb, -1, 2, ": выбрал 0-й, верный 3-й (🔵)"},
		{"no markup", &telego.Message{}, 0, 1, ": выбрал 1-й, верный 2-й"},
		{"empty keyboard", &telego.Message{ReplyMarkup: &telego.InlineKeyboardMarkup{}}, 0, 1, ": выбрал 1-й, верный 2-й"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pickedVsCorrect(tt.msg, tt.picked, tt.correct); got != tt.want {
				t.Errorf("pickedVsCorrect() = %q, want %q", got, tt.want)
			}
		})
	}
}
