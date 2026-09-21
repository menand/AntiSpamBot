package bot

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mymmrac/telego"

	"github.com/menand/AntiSpamBot/internal/captcha"
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

func TestStaleCaptchaClick(t *testing.T) {
	live := &captcha.Pending{MessageID: 100, EphemeralID: 0}
	if staleCaptchaClick(live, &telego.Message{MessageID: 100}) {
		t.Fatal("same message id must not be stale")
	}
	if !staleCaptchaClick(live, &telego.Message{MessageID: 200}) {
		t.Fatal("different message id must be stale")
	}
	// Эфемерная капча: обычные message_id нулевые, сравнивается ephemeral id.
	eph := &captcha.Pending{MessageID: 0, EphemeralID: 55}
	if staleCaptchaClick(eph, &telego.Message{EphemeralMessageID: 55}) {
		t.Fatal("matching ephemeral id must not be stale")
	}
	if !staleCaptchaClick(eph, &telego.Message{EphemeralMessageID: 56}) {
		t.Fatal("different ephemeral id must be stale")
	}
	// Недоступное сообщение (nil) проверке не поддаётся — не stale.
	if staleCaptchaClick(live, nil) || staleCaptchaClick(nil, &telego.Message{}) {
		t.Fatal("nil inputs must not be treated as stale")
	}
}

func TestReconcileChats(t *testing.T) {
	t.Run("orphan drop on 403", func(t *testing.T) {
		ctx := context.Background()
		b, db, fc := newFlowBot(t)
		aliveChat := int64(-100100)
		orphanChat := int64(-100200)
		serviceableChat(t, b, db, aliveChat)
		serviceableChat(t, b, db, orphanChat)

		fc.errWhen = func(method string, data *telegoapi.RequestData) bool {
			if method != "getChatMember" {
				return false
			}
			body := string(data.BodyRaw)
			return strings.Contains(body, `"chat_id":-100200`)
		}
		fc.err["getChatMember"] = &telegoapi.Error{ErrorCode: 403, Description: "Forbidden: bot was kicked"}

		b.reconcileChats(ctx)

		chats, _ := db.ListChats(ctx)
		for _, c := range chats {
			if c.ChatID == orphanChat {
				t.Fatal("orphan chat must be dropped by reconcileChats")
			}
		}
	})

	t.Run("transient error keeps chat", func(t *testing.T) {
		ctx := context.Background()
		b, db, fc := newFlowBot(t)
		chatID := int64(-100100)
		serviceableChat(t, b, db, chatID)

		fc.err["getChatMember"] = &telegoapi.Error{ErrorCode: 429, Description: "Too Many Requests"}

		b.reconcileChats(ctx)

		chats, _ := db.ListChats(ctx)
		found := false
		for _, c := range chats {
			if c.ChatID == chatID {
				found = true
			}
		}
		if !found {
			t.Fatal("transient error must not drop the chat")
		}
	})

	t.Run("not in ALLOWED_CHATS drops chat", func(t *testing.T) {
		ctx := context.Background()
		b, db, _ := newFlowBot(t)
		chatID := int64(-100100)
		serviceableChat(t, b, db, chatID)
		b.cfg.AllowedChats = map[int64]struct{}{-999: {}}

		b.reconcileChats(ctx)

		chats, _ := db.ListChats(ctx)
		for _, c := range chats {
			if c.ChatID == chatID {
				t.Fatal("chat outside ALLOWED_CHATS must be dropped")
			}
		}
	})
}

func TestEditedGroupMessage(t *testing.T) {
	t.Run("spam edit triggers check", func(t *testing.T) {
		b, db, _ := newFlowBot(t)
		serviceableChat(t, b, db, testChatID)
		_ = db.SetSpamCheckEnabled(context.Background(), testChatID, true)
		zero := 0
		_ = db.SetSpamWhitelistMsgs(context.Background(), testChatID, &zero)
		b.spamGateCache[testChatID] = true
		llm := &fakeLLM{enabled: true, spam: false}
		b.groqc = llm
		// Mark user 999 as non-admin to avoid skip in spamGatesPass.
		b.adminCache[chatUser{testChatID, 999}] = adminCacheEntry{isAdmin: false, until: time.Now().Add(time.Hour)}

		msg := telego.Message{
			MessageID: 100, Chat: telego.Chat{ID: testChatID, Type: "supergroup"},
			From: &telego.User{ID: 999}, Text: "edited spam http://evil.com",
		}
		if err := b.handleEditedGroupMessage(nil, msg); err != nil {
			t.Fatal(err)
		}
		// maybeSpamCheck is async — wait for the goroutine to complete.
		deadline := time.Now().Add(3 * time.Second)
		for llm.callCount() == 0 && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if llm.callCount() == 0 {
			t.Fatal("edited message must trigger LLM check")
		}
	})

	t.Run("mid-captcha edit is deleted", func(t *testing.T) {
		b, db, fc := newFlowBot(t)
		serviceableChat(t, b, db, testChatID)
		putCaptcha(b, db, testChatID, 999, 200)

		msg := telego.Message{
			MessageID: 300, Chat: telego.Chat{ID: testChatID, Type: "supergroup"},
			From: &telego.User{ID: 999}, Text: "sneaky edit",
		}
		if err := b.handleEditedGroupMessage(nil, msg); err != nil {
			t.Fatal(err)
		}
		if n := fc.callCount("deleteMessage"); n != 1 {
			t.Fatalf("mid-captcha edit must be deleted, deleteMessage calls = %d", n)
		}
	})

	t.Run("location edit skips", func(t *testing.T) {
		b, db, fc := newFlowBot(t)
		serviceableChat(t, b, db, testChatID)

		msg := telego.Message{
			MessageID: 400, Chat: telego.Chat{ID: testChatID, Type: "supergroup"},
			From:     &telego.User{ID: 999},
			Location: &telego.Location{Latitude: 55.75, Longitude: 37.62},
		}
		if err := b.handleEditedGroupMessage(nil, msg); err != nil {
			t.Fatal(err)
		}
		if n := fc.callCount("deleteMessage") + fc.callCount("sendMessage"); n != 0 {
			t.Fatalf("location edit must be skipped entirely, API calls = %d", n)
		}
	})

	t.Run("cooldown skips re-check", func(t *testing.T) {
		b, db, fc := newFlowBot(t)
		serviceableChat(t, b, db, testChatID)
		_ = db.SetSpamCheckEnabled(context.Background(), testChatID, true)
		b.spamGateCache[testChatID] = true
		llm := &fakeLLM{enabled: true, spam: false}
		b.groqc = llm

		msg := telego.Message{
			MessageID: 500, Chat: telego.Chat{ID: testChatID, Type: "supergroup"},
			From: &telego.User{ID: 888}, Text: "first edit",
		}
		if err := b.handleEditedGroupMessage(nil, msg); err != nil {
			t.Fatal(err)
		}
		first := llm.callCount()

		msg.Text = "second edit within cooldown"
		if err := b.handleEditedGroupMessage(nil, msg); err != nil {
			t.Fatal(err)
		}
		if llm.callCount() != first {
			t.Fatal("second edit within cooldown must not trigger another LLM check")
		}
		_ = fc
	})
}
