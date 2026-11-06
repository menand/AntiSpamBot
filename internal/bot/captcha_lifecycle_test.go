package bot

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegoapi"

	"github.com/menand/AntiSpamBot/internal/storage"
)

func TestCaptchaExpiredLoopPreservesReplacement(t *testing.T) {
	b, _, fc := newFlowBot(t)
	old := b.store.Put(testChatID, testUserID, 10, 1, time.Now().Add(-time.Second), 0, 0, 1)
	// Другой обработчик уже забрал старую проверку; её таймер успел проснуться.
	b.store.Take(testChatID, testUserID)
	live := b.store.Put(testChatID, testUserID, 20, 2, time.Now().Add(time.Hour), 0, 0, 1)
	b.captchaStageLoop(testChatID, testUserID, old)
	if got, ok := b.store.Get(testChatID, testUserID); !ok || got != live {
		t.Fatal("old timer removed the replacement captcha")
	}
	if calls := fc.callList(); len(calls) != 0 {
		t.Fatalf("old timer must not perform Telegram actions: %v", calls)
	}
}

func TestCaptchaCancellationDuringKickoff(t *testing.T) {
	b, db, fc := newFlowBot(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	b.runCtx = ctx
	if !b.store.BeginKickoff(testChatID, testUserID) {
		t.Fatal("kickoff not started")
	}
	var fired, cancelled bool
	fc.errWhen = func(method string, _ *telegoapi.RequestData) bool {
		if method == "restrictChatMember" && !fired {
			fired = true
			// Модерация пришла после применения мута, до публикации капчи.
			cancelled = b.cancelCaptchaSilent(testChatID, testUserID)
		}
		return false
	}
	b.runCaptcha(testChatID, telego.User{ID: testUserID, FirstName: "Member"}, 0)
	if !cancelled || b.store.IsCaptchaActive(testChatID, testUserID) {
		t.Fatal("manual cancellation did not stop the preparing captcha")
	}
	if n := fc.callCount("sendMessage"); n != 0 {
		t.Fatalf("cancelled kickoff sent %d captcha messages", n)
	}
	if rows := pendingRows(t, db); len(rows) != 0 {
		t.Fatalf("cancelled kickoff persisted a captcha: %+v", rows)
	}
}

func TestChatCleanupCancelsPreparingCaptcha(t *testing.T) {
	for _, migrate := range []bool{false, true} {
		t.Run(fmt.Sprintf("migrate=%v", migrate), func(t *testing.T) {
			b, db, fc := newFlowBot(t)
			serviceableChat(t, b, db, testChatID)
			b.store.BeginKickoff(testChatID, testUserID)
			generation := b.store.KickoffContext(testChatID, testUserID)
			if migrate {
				b.releaseMigratedCaptchas(testChatID, migratedNewID)
				if fc.callCount("restrictChatMember") != 1 {
					t.Fatal("preparing captcha must be released in the new chat")
				}
			} else {
				b.dropChat(context.Background(), testChatID, "test")
			}
			if generation.Err() == nil || b.store.IsCaptchaActive(testChatID, testUserID) {
				t.Fatal("preparing captcha survived chat cleanup")
			}
			if p := b.store.Put(testChatID, testUserID, 77, 0, time.Now().Add(time.Minute), 0, 0, 1); p != nil {
				t.Fatal("old sender resurrected a captcha after chat cleanup")
			}
		})
	}
}

func TestReplyDisarmPreservesReplacement(t *testing.T) {
	b, _, fc := newFlowBot(t)
	old := b.replies.Put(testChatID, testUserID, time.Now(), 0, 1, 10)
	live := b.replies.Put(testChatID, testUserID, time.Now().Add(time.Hour), 0, 1, 20)
	b.disarmReplyWait(context.Background(), testChatID, testUserID, old)
	if got, ok := b.replies.Get(testChatID, testUserID); !ok || got != live {
		t.Fatal("old handler removed the replacement reply wait")
	}
	if calls := fc.callList(); len(calls) != 0 {
		t.Fatalf("old handler must not perform Telegram actions: %v", calls)
	}
}

func TestReplyAnchorCompletionPreservesReplacement(t *testing.T) {
	b, _, _ := newFlowBot(t)
	old := b.replies.Put(testChatID, testUserID, time.Now().Add(time.Hour), 0, 1, 10)
	live := b.replies.Put(testChatID, testUserID, time.Now().Add(time.Hour), 0, 1, 20)
	if b.replies.SetGreetingMsgID(old, 99) || b.cancelReplyWaitIf(testChatID, testUserID, old) {
		t.Fatal("old greeting completion changed the replacement wait")
	}
	if got, ok := b.replies.Get(testChatID, testUserID); !ok || got != live || !b.replies.MatchesGreeting(testChatID, testUserID, 20) {
		t.Fatal("replacement greeting identity was lost")
	}
}

func TestCaptchaCancellationDuringTransition(t *testing.T) {
	b, db, fc := newFlowBot(t)
	p := b.store.Put(testChatID, testUserID, 10, 1, time.Now().Add(-time.Second), 0, 0, 1)
	if err := db.PutPending(context.Background(), storage.PendingRow{
		ChatID: testChatID, UserID: testUserID, MessageID: 10, CorrectIdx: 1, ExpiresAt: p.ExpiresAt, Stage: 1,
	}); err != nil {
		t.Fatal(err)
	}
	var cancelled bool
	fc.errWhen = func(method string, _ *telegoapi.RequestData) bool {
		if method == "deleteMessage" {
			cancelled = b.cancelCaptchaSilent(testChatID, testUserID)
		}
		return false
	}
	b.captchaStageLoop(testChatID, testUserID, p)
	if !cancelled || b.store.IsCaptchaActive(testChatID, testUserID) {
		t.Fatal("transition was invisible to manual cancellation")
	}
	if fc.callCount("sendMessage") != 0 || len(pendingRows(t, db)) != 0 {
		t.Fatal("cancelled transition resurrected a captcha")
	}
}
