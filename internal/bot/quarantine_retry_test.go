package bot

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegoapi"
)

func TestQuarantineReleaseFailureRemainsRetryable(t *testing.T) {
	for _, all := range []bool{false, true} {
		t.Run(map[bool]string{false: "user", true: "chat"}[all], func(t *testing.T) {
			b, db, fc := newFlowBot(t)
			until := time.Now().Add(time.Hour)
			b.armQuarantine(context.Background(), testChatID, testUserID, until)
			fc.resp["getChatMember"] = quarantineStatus(testUserID, until, true)
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

func quarantineStatus(userID int64, until time.Time, canSend bool) string {
	return fmt.Sprintf(`{"status":"restricted","user":{"id":%d},"is_member":true,"until_date":%d,"can_send_messages":%t}`,
		userID, until.Unix(), canSend)
}

func TestTrustPreservesManualMuteWhenQuarantineUnreadable(t *testing.T) {
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	b.cfg.OwnerIDs = map[int64]struct{}{200: {}}
	// Фальшивая форма карантина гарантирует реальную ошибку чтения
	// исходной таблицы при исправных чатах и прочих настройках.
	path := filepath.Join(t.TempDir(), "trust.db")
	ctx := b.runCtx
	b.db = openPendingRestoreDB(t, ctx, path)
	serviceableChat(t, b, b.db, testChatID)
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.ExecContext(ctx, "ALTER TABLE chat_quarantine RENAME TO quarantine_unavailable"); err != nil {
		t.Fatal(err)
	}
	fc.respSeq["getChatMember"] = []string{
		defaultChatMember(&telegoapi.RequestData{BodyRaw: []byte(`{"user_id":42}`)}),
		quarantineStatus(testUserID, time.Now().Add(time.Hour), false),
	}
	cmd := muteRightsCommand(testChatID, testUserID)
	cmd.Text = "/trust"
	if err := b.handleTrustCommand(nil, cmd); err != nil {
		t.Fatal(err)
	}
	if n := fc.callCount("restrictChatMember"); n != 0 {
		t.Fatalf("unreadable quarantine sent a release: %v", fc.callList())
	}
	if replies := strings.Join(fc.callBodies("sendMessage"), " "); !strings.Contains(replies, "Снятие отложено") {
		t.Fatalf("missing deferred result: %s", replies)
	}
}

func TestQuarantineReleaseKeepsNewerTelegramRestriction(t *testing.T) {
	b, db, fc := newFlowBot(t)
	ctx := context.Background()
	until := time.Now().Add(time.Hour)
	b.armQuarantine(ctx, testChatID, testUserID, until)
	fc.resp["getChatMember"] = quarantineStatus(testUserID, until, false)
	released, err := b.releaseQuarantineUser(testChatID, testUserID)
	if err != nil || released || fc.callCount("restrictChatMember") != 0 {
		t.Fatalf("newer Telegram mute was modified or reported as released: %v, %v, %v", released, err, fc.callList())
	}
	if has, err := db.HasQuarantine(ctx, testChatID, testUserID, time.Now().Unix()); err != nil || has {
		t.Fatalf("stale quarantine survived: %v, %v", has, err)
	}
}
