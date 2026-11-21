package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegoapi"

	"github.com/menand/AntiSpamBot/internal/storage"
)

func TestManualMuteWinsOverCaptchaAdmission(t *testing.T) {
	for _, tc := range []struct {
		name       string
		muteFirst  bool
		quarantine bool
	}{
		{name: "допуск первым"},
		{name: "ручной мут первым", muteFirst: true},
		{name: "допуск с текстовым карантином", quarantine: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "captcha-rights.db")
			b, fc := replyAnchorBot(t, path)
			db := b.db
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			b.runCtx = ctx
			b.cfg.OwnerIDs = map[int64]struct{}{200: {}}
			otherChat := int64(-100200)
			serviceableChat(t, b, db, testChatID)
			serviceableChat(t, b, db, otherChat)
			if err := db.SetWelcomeMode(ctx, testChatID, string(welcomeReply)); err != nil {
				t.Fatal(err)
			}
			if err := db.SetQuarantineEnabled(ctx, testChatID, tc.quarantine); err != nil {
				t.Fatal(err)
			}
			p := putCaptcha(b, db, testChatID, testUserID, 77)
			answer := captchaQuery("correct", testUserID, p.CorrectIdx, int64(p.MessageID))
			mute := muteRightsCommand(testChatID, testUserID)

			entered, unblock := holdFirstRestriction(t, b, fc, testChatID, testUserID)
			defer unblock()
			runAnswer := func() error { return b.handleCallback(nil, answer) }
			runMute := func() error { return b.handleMuteCommand(nil, mute) }
			first, second := runAnswer, runMute
			if tc.muteFirst {
				first, second = runMute, runAnswer
			}
			a := startRightsAction(first)
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("first action did not reach Telegram")
			}
			z := startRightsAction(second)
			waitFor(t, func() bool {
				b.rights.mu.Lock()
				defer b.rights.mu.Unlock()
				return len(b.rights.lanes[chatUser{testChatID, testUserID}]) == 2
			})
			if tc.muteFirst {
				// Последний таймаут тоже ждёт ручной мут и после него
				// обнаружит, что старая проверка уже снята.
				timeout := startRightsAction(func() error {
					b.advanceCaptchaStage(ctx, testChatID, testUserID, p)
					return nil
				})
				waitFor(t, func() bool {
					b.rights.mu.Lock()
					defer b.rights.mu.Unlock()
					return len(b.rights.lanes[chatUser{testChatID, testUserID}]) == 3
				})
				t.Cleanup(func() { waitRightsAction(t, timeout) })
			} else {
				for _, independent := range []telego.Message{
					muteRightsCommand(testChatID, testUserID+1),
					muteRightsCommand(otherChat, testUserID),
				} {
					waitRightsAction(t, startRightsAction(func() error { return b.handleMuteCommand(nil, independent) }))
				}
			}

			unblock()
			waitRightsAction(t, a)
			waitRightsAction(t, z)
			b.reconcileAdmissions(ctx)

			var target []string
			for _, body := range fc.callBodies("restrictChatMember") {
				if strings.Contains(body, fmt.Sprintf(`"chat_id":%d`, testChatID)) &&
					strings.Contains(body, fmt.Sprintf(`"user_id":%d,`, testUserID)) {
					target = append(target, body)
				}
			}
			wantCalls := 2
			if tc.muteFirst {
				wantCalls = 1
			}
			if len(target) != wantCalls {
				t.Fatalf("restrict calls for target = %v, want %d", target, wantCalls)
			}
			last := target[len(target)-1]
			if !strings.Contains(last, `"permissions":{}`) || !strings.Contains(last, `"until_date":`) {
				t.Fatalf("final request did not leave the manual mute: %s", last)
			}
			if tc.quarantine && !strings.Contains(target[0], `"can_add_web_page_previews":false`) {
				t.Fatalf("initial permission change was not text-only: %v", target)
			}
			if rows, err := db.LoadAllPendingReplies(ctx); err != nil || len(rows) != 0 {
				t.Fatalf("reply waits: %v, %v", rows, err)
			}
			if _, active := b.replies.Get(testChatID, testUserID); active {
				t.Fatal("manual mute left a reply timer")
			}
			if len(pendingRows(t, db)) != 0 || fc.callCount("banChatMember") != 0 {
				t.Fatalf("old captcha survived or punished the target: %v", fc.callList())
			}
			if _, pending, err := db.GetPendingAdmission(ctx, testChatID, testUserID); err != nil || pending {
				t.Fatalf("manual mute left a retryable admission: %v, %v", pending, err)
			}
			if active, err := db.HasQuarantine(ctx, testChatID, testUserID, time.Now().Unix()); err != nil || active {
				t.Fatalf("manual mute left quarantine: %v, %v", active, err)
			}
			for _, body := range fc.callBodies("sendMessage") {
				if strings.Contains(body, "пару слов по делу") {
					t.Fatalf("admission created a reply challenge before the queued mute: %s", body)
				}
			}
			if stats := statsKinds(t, db, testChatID, testUserID); stats[storage.EventPass] != 1 {
				t.Fatalf("want one terminal event for the check: %v", stats)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			restarted, recovered := replyAnchorBot(t, path)
			assertMuteRecoveryQuiescent(t, restarted)
			if calls := recovered.callList(); len(calls) != 0 {
				t.Fatalf("restart attempted to change the final manual mute: %v", calls)
			}
			if stats := statsKinds(t, restarted.db, testChatID, testUserID); stats[storage.EventPass] != 1 {
				t.Fatalf("restart added another outcome: %v", stats)
			}
		})
	}
}

func holdFirstRestriction(t *testing.T, b *Bot, fc *fakeCaller, chatID, userID int64) (<-chan struct{}, func()) {
	t.Helper()
	entered, released := make(chan struct{}), make(chan struct{})
	var signalOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(released) }) }
	t.Cleanup(unblock)
	caller := interruptedCaptchaCaller{fakeCaller: fc, before: func(url string, data *telegoapi.RequestData) {
		if !strings.HasSuffix(url, "/restrictChatMember") {
			return
		}
		var request struct {
			ChatID int64 `json:"chat_id"`
			UserID int64 `json:"user_id"`
		}
		if json.Unmarshal(data.BodyRaw, &request) == nil && request.ChatID == chatID && request.UserID == userID {
			signalOnce.Do(func() { close(entered) })
			<-released
		}
	}}
	api, err := telego.NewBot("123456789:ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghi", telego.WithAPICaller(caller), telego.WithDiscardLogger())
	if err != nil {
		t.Fatal(err)
	}
	b.api = api
	return entered, unblock
}

func muteRightsCommand(chatID, userID int64) telego.Message {
	chat := telego.Chat{ID: chatID, Type: "supergroup"}
	return telego.Message{MessageID: 93, Chat: chat, Text: "/mute 45m", From: &telego.User{ID: 200},
		ReplyToMessage: &telego.Message{MessageID: 94, Chat: chat, From: &telego.User{ID: userID}}}
}

func startRightsAction(action func() error) <-chan error {
	done := make(chan error, 1)
	go func() { done <- action() }()
	return done
}

func waitRightsAction(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("rights action did not finish")
	}
}

func TestQuarantineReleaseChecksStateAfterManualMute(t *testing.T) {
	b, db, fc := newFlowBot(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.runCtx = ctx
	b.cfg.OwnerIDs = map[int64]struct{}{200: {}}
	serviceableChat(t, b, db, testChatID)
	b.armQuarantine(ctx, testChatID, testUserID, time.Now().Add(time.Hour))
	entered, unblock := holdFirstRestriction(t, b, fc, testChatID, testUserID)
	defer unblock()

	muted := startRightsAction(func() error { return b.handleMuteCommand(nil, muteRightsCommand(testChatID, testUserID)) })
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("manual mute did not reach Telegram")
	}
	release := startRightsAction(func() error {
		released, err := b.releaseQuarantine(ctx, testChatID, testUserID)
		if err == nil && released {
			return fmt.Errorf("stale quarantine release changed permissions")
		}
		return err
	})
	waitFor(t, func() bool {
		b.rights.mu.Lock()
		defer b.rights.mu.Unlock()
		return len(b.rights.lanes[chatUser{testChatID, testUserID}]) == 2
	})
	unblock()
	waitRightsAction(t, muted)
	waitRightsAction(t, release)
	if n := fc.callCount("restrictChatMember"); n != 1 {
		t.Fatalf("only the mute should set permissions: %v", fc.callList())
	}
	if _, ok := b.quar.Get(testChatID, testUserID); ok {
		t.Fatal("quarantine remained in memory")
	}
}
