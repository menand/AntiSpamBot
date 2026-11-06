package bot

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegoapi"
)

func TestIsNotModified(t *testing.T) {
	if isNotModified(nil) {
		t.Fatal("nil is not a not-modified error")
	}
	if isNotModified(errors.New("Bad Request: message is not modified")) {
		t.Fatal("plain errors must not match — only telegoapi.Error")
	}
	notMod := &telegoapi.Error{
		ErrorCode:   400,
		Description: "Bad Request: message is not modified: specified new message content and reply markup are exactly the same",
	}
	if !isNotModified(notMod) {
		t.Fatal("telegram not-modified error must match")
	}
	if !isNotModified(fmt.Errorf("edit: %w", notMod)) {
		t.Fatal("wrapped not-modified error must match")
	}
	if isNotModified(&telegoapi.Error{ErrorCode: 400, Description: "Bad Request: chat not found"}) {
		t.Fatal("other API errors must not match")
	}
}

func TestLeaveChatAndCleanup(t *testing.T) {
	t.Run("happy path drops chat", func(t *testing.T) {
		ctx := context.Background()
		b, db, fc := newFlowBot(t)
		chatID := int64(-100200)
		serviceableChat(t, b, db, chatID)

		done := make(chan error, 1)
		b.leaveChatAndCleanup(chatID, "test leave", func(err error) {
			done <- err
		})

		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("leaveChatAndCleanup timed out")
		}

		if fc.callCount("leaveChat") != 1 {
			t.Fatalf("expected 1 leaveChat call, got %d", fc.callCount("leaveChat"))
		}

		chats, _ := db.ListChats(ctx)
		for _, c := range chats {
			if c.ChatID == chatID {
				t.Fatal("chat must be dropped from registry after leave")
			}
		}
	})

	t.Run("leaveChat error still cleans up", func(t *testing.T) {
		b, db, fc := newFlowBot(t)
		chatID := int64(-100300)
		serviceableChat(t, b, db, chatID)
		fc.err = map[string]*telegoapi.Error{
			"leaveChat": {ErrorCode: 400, Description: "Bad Request: chat not found"},
		}

		done := make(chan error, 1)
		b.leaveChatAndCleanup(chatID, "test leave error", func(err error) {
			done <- err
		})

		select {
		case err := <-done:
			if err == nil {
				t.Fatal("expected error from leaveChat failure")
			}
		case <-time.After(15 * time.Second):
			t.Fatal("leaveChatAndCleanup timed out")
		}

		if fc.callCount("leaveChat") < 1 {
			t.Fatalf("expected at least 1 leaveChat call, got %d", fc.callCount("leaveChat"))
		}
	})
}

func TestLeaveChatPreservesUnfinishedCaptcha(t *testing.T) {
	for _, preparing := range []bool{false, true} {
		t.Run(fmt.Sprintf("preparing=%v", preparing), func(t *testing.T) {
			b, db, fc := newFlowBot(t)
			serviceableChat(t, b, db, testChatID)
			if preparing {
				b.store.BeginKickoff(testChatID, testUserID)
			} else {
				putCaptcha(b, db, testChatID, testUserID, 77)
			}
			done := make(chan error, 1)
			b.leaveChatAndCleanup(testChatID, "test leave", func(err error) { done <- err })
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("leaving with an unfinished captcha must be refused")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("leave result timed out")
			}
			if fc.callCount("leaveChat") != 0 || !b.store.IsCaptchaActive(testChatID, testUserID) {
				t.Fatal("leave discarded an active captcha and its permanent mute")
			}
		})
	}
}

func TestCaptchaCannotStartDuringChatLeave(t *testing.T) {
	b, _, fc := newFlowBot(t)
	b.leaveInflight[testChatID] = true
	if b.startCaptcha(testChatID, telego.User{ID: testUserID}, 0) {
		t.Fatal("new captcha started while leaving")
	}
	if n := fc.callCount("restrictChatMember"); n != 0 {
		t.Fatal("leaving chat must not acquire a new captcha mute")
	}
}

func TestLeaveInflightDedup(t *testing.T) {
	b, db, fc := newFlowBot(t)
	chatID := int64(-100200)
	serviceableChat(t, b, db, chatID)

	b.leaveMu.Lock()
	b.leaveInflight[chatID] = true
	b.leaveMu.Unlock()

	done := make(chan error, 1)
	b.leaveChatAndCleanup(chatID, "second attempt", func(err error) {
		done <- err
	})

	b.leaveMu.Lock()
	delete(b.leaveInflight, chatID)
	b.leaveMu.Unlock()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("leaveChatAndCleanup timed out")
	}

	if fc.callCount("leaveChat") != 1 {
		t.Fatalf("expected 1 leaveChat call, got %d", fc.callCount("leaveChat"))
	}
}

func TestDropChatRemovesRegistry(t *testing.T) {
	ctx := context.Background()
	b, db, _ := newFlowBot(t)
	chatID := int64(-100500)
	serviceableChat(t, b, db, chatID)

	// Verify chat is registered
	chats, _ := db.ListChats(ctx)
	found := false
	for _, c := range chats {
		if c.ChatID == chatID {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("chat must be registered before drop")
	}

	b.dropChat(ctx, chatID, "test drop")

	chats, _ = db.ListChats(ctx)
	for _, c := range chats {
		if c.ChatID == chatID {
			t.Fatal("chat must be dropped from registry")
		}
	}
}
