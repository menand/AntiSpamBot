package bot

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegoapi"

	"github.com/menand/AntiSpamBot/internal/storage"
)

func TestInterruptedCaptchaRestoreDoesNotRestartNeighborTimer(t *testing.T) {
	b, db, fc := newFlowBot(t)
	ctx := b.runCtx
	serviceableChat(t, b, db, testChatID)
	for i := range 2 {
		if err := db.PutPending(ctx, storage.PendingRow{ChatID: testChatID, UserID: testUserID + int64(i),
			MessageID: 101 + i, Stage: 1, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if err := db.SuspendPendingChecks(ctx, testChatID); err != nil {
				t.Fatal(err)
			}
		}
	}
	fc.err["getChatMember"] = &telegoapi.Error{ErrorCode: 429}
	if n, err := b.restorePending(ctx); err != nil || n != 1 {
		t.Fatalf("initial restore = %d / %v", n, err)
	}
	neighbor, ok := b.store.Get(testChatID, testUserID+1)
	if !ok || len(pendingRows(t, db)) != 2 {
		t.Fatalf("failure should preserve the suspended row and restore the healthy timer")
	}
	delete(fc.err, "getChatMember")
	before := fc.callCount("getChatMember")
	b.reconcileCaptchaRestorations(ctx)
	if calls := fc.callCount("getChatMember") - before; calls != 1 || len(pendingRows(t, db)) != 1 {
		t.Fatalf("periodic pass should finish just the failed row, got %d probes and %+v", calls, pendingRows(t, db))
	}
	if n, err := b.restorePending(ctx); err != nil || n != 0 {
		t.Errorf("second pass restored %d extra timers: %v", n, err)
	}
	if got, ok := b.store.Get(testChatID, testUserID+1); !ok || got != neighbor || len(pendingRows(t, db)) != 1 {
		t.Fatalf("rollback repeated or skipped work: same timer=%t, rows=%+v", got == neighbor, pendingRows(t, db))
	}
}

func TestCaptchaRestorationsResumeAfterStartupBudget(t *testing.T) {
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	if err := db.PutPending(b.runCtx, storage.PendingRow{ChatID: testChatID, UserID: testUserID,
		MessageID: 101, Stage: 1, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := db.SuspendPendingChecks(b.runCtx, testChatID); err != nil {
		t.Fatal(err)
	}
	if _, err := b.newCaptchaPreparation(b.runCtx, testChatID, testUserID+1, 0, b.chatSettings(b.runCtx, testChatID)); err != nil {
		t.Fatal(err)
	}
	startup, stop := context.WithCancel(b.runCtx)
	defer stop()
	caller := interruptedCaptchaCaller{fakeCaller: fc, before: func(url string, _ *telegoapi.RequestData) {
		if strings.HasSuffix(url, "/getChatMember") {
			stop()
		}
	}}
	api, err := telego.NewBot("123456789:ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghi", telego.WithAPICaller(caller), telego.WithDiscardLogger())
	if err != nil {
		t.Fatal(err)
	}
	b.api = api
	if n, err := b.restorePending(startup); n != 0 || err != nil || len(pendingRows(t, db)) != 2 {
		t.Fatalf("startup budget must leave both rows for retry: %d / %v / %+v", n, err, pendingRows(t, db))
	}
	b.reconcileCaptchaRestorations(b.runCtx)
	b.reconcileCaptchaRestorations(b.runCtx)
	if rows := pendingRows(t, db); len(rows) != 0 {
		t.Fatalf("unfinished rows after retry: %+v", rows)
	}
	if kinds := statsKinds(t, db, testChatID, testUserID); kinds[storage.EventAbort] != 2 {
		t.Fatalf("expected two closed checks: %v", kinds)
	}
}

func TestCaptchaRestorationChecksCurrentRowAndMute(t *testing.T) {
	for _, change := range []string{"replacement", "mute", "unapproved"} {
		t.Run(change, func(t *testing.T) {
			b, db, fc := newFlowBot(t)
			ctx := b.runCtx
			serviceableChat(t, b, db, testChatID)
			row := storage.PendingRow{ChatID: testChatID, UserID: testUserID, MessageID: 101, Stage: 1, ExpiresAt: time.Now().Add(time.Hour)}
			if err := db.PutPending(ctx, row); err != nil {
				t.Fatal(err)
			}
			if err := db.SuspendPendingChecks(ctx, testChatID); err != nil {
				t.Fatal(err)
			}
			snapshot := pendingRows(t, db)[0]
			switch change {
			case "replacement":
				row.MessageID++
				if err := db.PutPending(ctx, row); err != nil {
					t.Fatal(err)
				}
				if err := db.SuspendPendingChecks(ctx, testChatID); err != nil {
					t.Fatal(err)
				}
			case "mute":
				if err := db.PutManualMute(ctx, storage.ManualMute{ChatID: testChatID, UserID: testUserID,
					Generation: "newer", UntilDate: time.Now().Add(time.Hour).Unix()}); err != nil {
					t.Fatal(err)
				}
			case "unapproved":
				if err := db.SetChatApproval(ctx, testChatID, storage.ChatPending); err != nil {
					t.Fatal(err)
				}
			}
			if err := b.restoreCaptchaCheckpoint(ctx, snapshot); err != nil {
				t.Fatal(err)
			}
			if change == "replacement" {
				if err := db.FinishSuspendedCaptcha(ctx, snapshot, storage.EventAbort); err != nil {
					t.Fatal(err)
				}
			}
			if got := pendingRows(t, db); len(got) != 1 || got[0].MessageID != row.MessageID || len(fc.callList()) != 0 {
				t.Fatalf("stale work touched the checkpoint: %+v / %v", got, fc.callList())
			}
			if change == "mute" {
				if m, found, err := db.GetManualMute(ctx, testChatID, testUserID); err != nil || !found || m.Generation != "newer" {
					t.Fatalf("newer mute changed: %+v / %t / %v", m, found, err)
				}
			}
		})
	}
}

func TestPendingRestoreRetriesApprovalRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b, _, fc := newFlowBot(t)
	b.runCtx = ctx
	path := filepath.Join(t.TempDir(), "restore.db")
	db := openPendingRestoreDB(t, ctx, path)
	b.db = db
	seedPendingRestore(t, b, time.Now().Add(time.Hour))
	if err := db.SetWelcomeMode(ctx, testChatID, string(welcomeReply)); err != nil {
		t.Fatal(err)
	}

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.ExecContext(ctx, "ALTER TABLE chats RENAME TO unavailable_chats"); err != nil {
		t.Fatal(err)
	}

	restores := []func(context.Context) (int, error){b.restorePending, b.restorePendingReplies}
	for _, restore := range restores {
		if n, err := restore(ctx); err == nil || n != 0 {
			t.Fatalf("restore with unreadable approval = %d, %v", n, err)
		}
	}
	assertPendingRestoreRows(t, b, false, 1, 1)
	if calls := fc.callList(); len(calls) != 0 {
		t.Fatalf("Telegram called with unreadable approval: %v", calls)
	}
	attempts := 0
	n, err := b.retryPendingRestore(ctx, "captchas", func(ctx context.Context) (int, error) {
		attempts++
		n, err := b.restorePending(ctx)
		if attempts == 1 {
			if err == nil || n != 0 {
				t.Fatalf("first attempt = %d, %v", n, err)
			}
			if _, renameErr := raw.ExecContext(ctx, "ALTER TABLE unavailable_chats RENAME TO chats"); renameErr != nil {
				t.Fatal(renameErr)
			}
		}
		return n, err
	})
	if err != nil || n != 1 || attempts != 2 {
		t.Fatalf("captcha retry = %d, %v after %d attempts", n, err, attempts)
	}
	n, err = b.retryPendingRestore(ctx, "replies", b.restorePendingReplies)
	if err != nil || n != 1 {
		t.Fatalf("reply retry = %d, %v", n, err)
	}
	if _, ok := b.store.Get(testChatID, testUserID); !ok {
		t.Fatal("captcha timer was not restored")
	}
	if _, ok := b.replies.Get(testChatID, testUserID+1); !ok {
		t.Fatal("reply timer was not restored")
	}
	assertPendingRestoreRows(t, b, false, 1, 1)
}

func TestPendingRestoreAfterAllowlistReturn(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "restore.db")
	excluded, _, fc := newFlowBot(t)
	excluded.db = openPendingRestoreDB(t, ctx, path)
	excluded.cfg.AllowedChats = map[int64]struct{}{-999: {}}
	seedPendingRestore(t, excluded, time.Now().Add(-time.Hour))
	// Второй пользователь получил временный мут за время отключения.
	if err := excluded.db.PutPending(ctx, storage.PendingRow{
		ChatID: testChatID, UserID: testUserID + 2, MessageID: 102, ExpiresAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	restorePendingChecksInTest(t, excluded)
	excluded.reconcileChats(ctx)
	assertPendingRestoreRows(t, excluded, true, 2, 1)
	if calls := fc.callList(); len(calls) != 0 {
		t.Fatalf("Telegram called for excluded chat: %v", calls)
	}
	if err := excluded.db.Close(); err != nil {
		t.Fatal(err)
	}

	returned, _, fc := newFlowBot(t)
	db := openPendingRestoreDB(t, ctx, path)
	returned.db = db
	fc.resp["getChat"] = `{"permissions":{"can_send_messages":true,"can_send_polls":false}}`
	fc.respSeq["getChatMember"] = []string{
		`{"status":"restricted","user":{"id":7},"is_member":true,"until_date":0}`,
		`{"status":"restricted","user":{"id":9},"is_member":true,"until_date":2000000000}`,
	}
	restorePendingChecksInTest(t, returned)
	restorePendingChecksInTest(t, returned)
	assertPendingRestoreRows(t, returned, false, 0, 0)

	calls := fc.callList()
	if slices.Contains(calls, "banChatMember") || slices.Contains(calls, "unbanChatMember") {
		t.Fatalf("punishment after allowlist return: %v", calls)
	}
	if got := fc.callBodies("restrictChatMember"); len(got) != 1 {
		t.Fatalf("want one release, got %v", got)
	} else if !strings.Contains(got[0], `"user_id":7`) ||
		!strings.Contains(got[0], `"can_send_messages":true`) ||
		!strings.Contains(got[0], `"can_send_polls":false`) {
		t.Fatalf("release did not use chat defaults for the captcha user: %s", got[0])
	}
	if kinds := statsKinds(t, db, testChatID, testUserID); kinds[storage.EventAbort] != 3 ||
		kinds[storage.EventKick] != 0 || kinds[storage.EventBan] != 0 {
		t.Fatalf("want one abort for each of the three checks, got %v", kinds)
	}
}

func openPendingRestoreDB(t *testing.T, ctx context.Context, path string) *storage.DB {
	t.Helper()
	db, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func seedPendingRestore(t *testing.T, b *Bot, expires time.Time) {
	t.Helper()
	serviceableChat(t, b, b.db, testChatID)
	ctx := b.runCtx
	if err := b.db.PutPending(ctx, storage.PendingRow{
		ChatID: testChatID, UserID: testUserID, MessageID: 101, ExpiresAt: expires,
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.db.PutPendingReply(ctx, storage.PendingReply{
		ChatID: testChatID, UserID: testUserID + 1, ExpiresAt: expires,
	}); err != nil {
		t.Fatal(err)
	}
}

func restorePendingChecksInTest(t *testing.T, b *Bot) {
	t.Helper()
	for _, restore := range []func(context.Context) (int, error){b.restorePending, b.restorePendingReplies} {
		if n, err := restore(b.runCtx); n != 0 || err != nil {
			t.Fatalf("expected no timers, restore = %d, %v", n, err)
		}
	}
}

func assertPendingRestoreRows(t *testing.T, b *Bot, suspended bool, captchas, replies int) {
	t.Helper()
	pending, err := b.db.LoadAllPending(b.runCtx)
	if err != nil || len(pending) != captchas {
		t.Fatalf("pending captchas = %+v, %v", pending, err)
	}
	for _, row := range pending {
		if row.Suspended != suspended {
			t.Fatalf("captcha suspended = %v, want %v", row.Suspended, suspended)
		}
	}
	waiting, err := b.db.LoadAllPendingReplies(b.runCtx)
	if err != nil || len(waiting) != replies {
		t.Fatalf("pending replies = %+v, %v", waiting, err)
	}
	for _, row := range waiting {
		if row.Suspended != suspended {
			t.Fatalf("reply suspended = %v, want %v", row.Suspended, suspended)
		}
	}
}
