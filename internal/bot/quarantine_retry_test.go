package bot

import (
	"context"
	"testing"
	"time"

	"github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegoapi"
)

func TestQuarantineReleaseFailureRemainsRetryable(t *testing.T) {
	for _, all := range []bool{false, true} {
		t.Run(map[bool]string{false: "user", true: "chat"}[all], func(t *testing.T) {
			b, db, fc := newFlowBot(t)
			b.armQuarantine(context.Background(), testChatID, testUserID, time.Now().Add(time.Hour))
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			b.runCtx = ctx
			fc.err["restrictChatMember"] = &telegoapi.Error{ErrorCode: 403, Description: "Forbidden"}
			if all {
				b.quarantineReleaseChat(testChatID)
			} else {
				b.releaseQuarantineUser(testChatID, testUserID)
			}
			if has, err := db.HasQuarantine(context.Background(), testChatID, testUserID, time.Now().Unix()); err != nil || !has {
				t.Fatalf("failed release lost retry state: has=%v err=%v", has, err)
			}
			b.runCtx = context.Background()
			delete(fc.err, "restrictChatMember")
			b.releaseQuarantineUser(testChatID, testUserID)
			if has, err := db.HasQuarantine(context.Background(), testChatID, testUserID, time.Now().Unix()); err != nil || has {
				t.Fatalf("successful retry retained quarantine: has=%v err=%v", has, err)
			}
		})
	}
}

func TestQuarantineEscalationRetriesAfterFailure(t *testing.T) {
	b, db, fc := newFlowBot(t)
	b.armQuarantine(context.Background(), testChatID, testUserID, time.Now().Add(time.Hour))
	b.quar.ClaimWarned(testChatID, testUserID)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	b.runCtx = ctx
	fc.err["restrictChatMember"] = &telegoapi.Error{ErrorCode: 403, Description: "Forbidden"}
	m := &telego.Message{MessageID: 1, Chat: telego.Chat{ID: testChatID, Type: "supergroup"}, From: &telego.User{ID: testUserID}, Text: "https://example.com"}
	for i := 0; i < censorEscalateAfter; i++ {
		e, _ := b.quar.Get(testChatID, testUserID)
		b.censorQuarantineMessage(m, e)
	}
	b.runCtx = context.Background()
	delete(fc.err, "restrictChatMember")
	e, _ := b.quar.Get(testChatID, testUserID)
	b.censorQuarantineMessage(m, e)
	if has, err := db.HasQuarantine(context.Background(), testChatID, testUserID, time.Now().Unix()); err != nil || has {
		t.Fatalf("next violation did not retry escalation: has=%v err=%v", has, err)
	}
}
