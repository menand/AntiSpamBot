package bot

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegoapi"

	"github.com/menand/AntiSpamBot/internal/storage"
)

func TestReplyGreetingStageSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reply.db")
	b, fc := replyAnchorBot(t, path)
	serviceableChat(t, b, b.db, testChatID)
	if err := b.db.SetWelcomeMode(b.runCtx, testChatID, string(welcomeReply)); err != nil {
		t.Fatal(err)
	}
	fc.respSeq["sendMessage"] = []string{replyAnchorResponse(101), replyAnchorResponse(102)}

	// Первую стадию делаем истёкшей и запускаем обычный таймер после
	// привязки. Следующая получит полный интервал и переживёт рестарт.
	expires := time.Now().Add(-time.Second)
	p := b.replies.Put(testChatID, testUserID, expires, 77, 1, 0)
	if err := b.db.PutPendingReply(b.runCtx, storage.PendingReply{
		ChatID: testChatID, UserID: testUserID, ExpiresAt: expires, Stage: 1, ThreadID: 77,
	}); err != nil {
		t.Fatal(err)
	}
	old := &telego.Message{Chat: telego.Chat{ID: testChatID}, MessageID: 101}
	clickReplyAnchor(t, b, "rpok", old)
	if got, _ := b.replies.Get(testChatID, testUserID); got != p {
		t.Fatal("an unbound stage accepted a button")
	}
	s := b.chatSettings(b.runCtx, testChatID)
	if !b.sendReplyGreeting(b.runCtx, s, p) {
		t.Fatal("initial greeting was not sent")
	}
	b.goSafe("replyWaitLoop test", func() { b.replyWaitLoop(testChatID, testUserID, p) })
	waitFor(t, func() bool {
		return b.replies.MatchGreeting(testChatID, testUserID, 102) != nil
	})
	if err := b.db.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, fc := replyAnchorBot(t, path)
	restoreReplyAnchor(t, restarted)
	if calls := fc.callCount("sendMessage"); calls != 0 {
		t.Fatalf("a proven anchor was sent again: %v", fc.callList())
	}

	current := &telego.Message{Chat: old.Chat, MessageID: 102}
	for _, action := range []string{"rpok", "rpspam", "rpspamx", "rpspamc"} {
		clickReplyAnchor(t, restarted, action, old)
		clickReplyAnchor(t, restarted, action, &telego.Message{Chat: old.Chat})
		clickReplyAnchor(t, restarted, action, &telego.InaccessibleMessage{Chat: old.Chat, MessageID: 102})
	}
	if fc.callCount("banChatMember") != 0 || fc.callCount("editMessageText") != 0 ||
		fc.callCount("deleteMessage") != 0 || restarted.replies.MatchGreeting(testChatID, testUserID, 102) == nil {
		t.Fatalf("invalid callback changed the current wait: %v", fc.callList())
	}
	clickReplyAnchor(t, restarted, "rpspam", current)
	clickReplyAnchor(t, restarted, "rpspamx", current)
	edits := fc.callBodies("editMessageText")
	if len(edits) != 2 || !strings.Contains(edits[0], "rpspamc:") ||
		!strings.Contains(edits[1], "Напоминание") || !strings.Contains(edits[1], "rpok:") {
		t.Fatalf("current anchor did not open and cancel confirmation: %v", edits)
	}
	clickReplyAnchor(t, restarted, "rpok", current)
	assertReplyAnchorPassed(t, restarted)
}

func TestReplyGreetingReissuesUnboundAnchor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reply.db")
	b, _ := replyAnchorBot(t, path)
	serviceableChat(t, b, b.db, testChatID)
	ctx := b.runCtx
	if err := b.db.SetWelcomeMode(ctx, testChatID, string(welcomeReply)); err != nil {
		t.Fatal(err)
	}
	if err := b.db.PutPendingReply(ctx, storage.PendingReply{
		ChatID: testChatID, UserID: testUserID, ExpiresAt: time.Now().Add(time.Hour), Stage: 2, ThreadID: 77,
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.db.PutGreeting(ctx, testChatID, testUserID, 101, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := b.db.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, fc := replyAnchorBot(t, path)
	fc.resp["sendMessage"] = replyAnchorResponse(103)
	restoreReplyAnchor(t, restarted)
	bodies := fc.callBodies("sendMessage")
	if len(bodies) != 1 || !strings.Contains(bodies[0], `"message_thread_id":77`) ||
		!strings.Contains(bodies[0], "Напоминание") {
		t.Fatalf("missing refreshed stage 2 anchor in original topic: %v", bodies)
	}
	old := &telego.Message{Chat: telego.Chat{ID: testChatID, Type: "supergroup"}, MessageID: 101}
	clickReplyAnchor(t, restarted, "rpok", old)
	if restarted.replies.MatchGreeting(testChatID, testUserID, 103) == nil {
		t.Fatal("the old greeting changed the wait")
	}
	if swallowed := restarted.replyWaitMiddleware(&telego.Message{
		Chat: old.Chat, MessageID: 104, From: &telego.User{ID: testUserID}, Text: "Всем добрый вечер, я здесь впервые",
	}); swallowed {
		t.Fatal("ordinary reply was swallowed")
	}
	assertReplyAnchorPassed(t, restarted)
}

func TestReplyGreetingCurrentSpamConfirmation(t *testing.T) {
	b, fc := replyAnchorBot(t, filepath.Join(t.TempDir(), "reply.db"))
	serviceableChat(t, b, b.db, testChatID)
	ctx := b.runCtx
	if err := b.db.SetWelcomeMode(ctx, testChatID, string(welcomeReply)); err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour)
	if err := b.db.PutPendingReply(ctx, storage.PendingReply{
		ChatID: testChatID, UserID: testUserID, ExpiresAt: expires, Stage: 1, GreetingMsgID: 101,
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.db.PutGreeting(ctx, testChatID, testUserID, 101, time.Now()); err != nil {
		t.Fatal(err)
	}
	// После удаления якоря запись стадии могла пережить неудавшийся таймаут.
	if err := b.db.DeleteGreetingIfMsg(ctx, testChatID, testUserID, 101); err != nil {
		t.Fatal(err)
	}
	fc.resp["sendMessage"] = replyAnchorResponse(103)
	restoreReplyAnchor(t, b)
	msg := &telego.Message{Chat: telego.Chat{ID: testChatID, Type: "supergroup"}, MessageID: 103}
	if err := b.db.RecordEvent(ctx, testChatID, testUserID, storage.EventJoin, time.Now(), ""); err != nil {
		t.Fatal(err)
	}
	if err := b.db.RecordEvent(ctx, testChatID, testUserID+1, storage.EventJoin, time.Now(), ""); err != nil {
		t.Fatal(err)
	}
	clickReplyAnchor(t, b, "rpspam", msg)
	clickReplyAnchor(t, b, "rpspamc", msg)
	if fc.callCount("banChatMember") != 1 {
		t.Fatalf("confirmation did not ban the user once: %v", fc.callList())
	}
	if _, ok := b.replies.Get(testChatID, testUserID); ok {
		t.Fatal("reply wait is still active after the ban")
	}
	s, err := b.db.QueryStats(ctx, testChatID, time.Now().Add(-time.Minute), time.Now().Add(time.Minute))
	if err != nil || s.Pending != 1 || s.SpamBanned != 1 || s.Passed != 0 {
		t.Fatalf("spam ban should close exactly one entry without a pass: %+v / %v", s, err)
	}
	menu := renderStats(periodDay, "сегодня", s, 7, 0, nil, nil, nil, nil, nil)
	digest := reportLine(storage.ChatInfo{ChatID: testChatID, Title: "Тест"}, s)
	if !strings.Contains(menu, "В процессе: 1") || !strings.Contains(digest, "в процессе 1") {
		t.Fatalf("menu and DM digest disagree: %q / %q", menu, digest)
	}
}

func replyAnchorBot(t *testing.T, path string) (*Bot, *fakeCaller) {
	t.Helper()
	b, _, fc := newFlowBot(t)
	ctx, cancel := context.WithCancel(context.Background())
	b.runCtx = ctx
	b.db = openPendingRestoreDB(t, ctx, path)
	t.Cleanup(cancel)
	b.cfg.OwnerIDs = map[int64]struct{}{200: {}}
	return b, fc
}

func replyAnchorResponse(id int) string {
	return fmt.Sprintf(`{"message_id":%d,"date":1700000000,"chat":{"id":%d,"type":"supergroup"}}`, id, testChatID)
}

func restoreReplyAnchor(t *testing.T, b *Bot) {
	t.Helper()
	if n, err := b.restorePendingReplies(b.runCtx); n != 1 || err != nil {
		t.Fatalf("restore = %d, %v, want one reply wait", n, err)
	}
}

func clickReplyAnchor(t *testing.T, b *Bot, action string, msg telego.MaybeInaccessibleMessage) {
	t.Helper()
	query := telego.CallbackQuery{ID: "test", From: telego.User{ID: 200}, Message: msg,
		Data: fmt.Sprintf("%s:%d", action, testUserID)}
	handler := b.handleReplySpamCallback
	if action == "rpok" {
		handler = b.handleReplyApproveCallback
	}
	if err := handler(nil, query); err != nil {
		t.Fatal(err)
	}
}

func assertReplyAnchorPassed(t *testing.T, b *Bot) {
	t.Helper()
	if _, ok := b.replies.Get(testChatID, testUserID); ok {
		t.Fatal("reply wait is still active")
	}
	if rows, err := b.db.LoadAllPendingReplies(b.runCtx); err != nil || len(rows) != 0 {
		t.Fatalf("reply waits on disk = %v, %v", rows, err)
	}
	if k := statsKinds(t, b.db, testChatID, testUserID); k[storage.EventPass] != 1 {
		t.Fatalf("want exactly one pass: %v", k)
	}
}

func TestReplySpamBanFailureLeavesReplyWait(t *testing.T) {
	for _, outcome := range []string{"answer", "retry", "next stage"} {
		t.Run(outcome, func(t *testing.T) {
			t.Parallel()
			b, db, fc := newFlowBot(t)
			serviceableChat(t, b, db, testChatID)
			b.cfg.OwnerIDs = map[int64]struct{}{200: {}}
			expiry := time.Now().Add(time.Hour).Truncate(time.Second)
			chat := telego.Chat{ID: testChatID, Type: telego.ChatTypeSupergroup}
			anchor := &telego.Message{Chat: chat, MessageID: 101}
			p := b.replies.Put(testChatID, testUserID, expiry, 77, 1, anchor.MessageID)
			row := storage.PendingReply{
				ChatID: testChatID, UserID: testUserID, ExpiresAt: expiry,
				ThreadID: 77, Stage: 1, GreetingMsgID: anchor.MessageID,
			}
			if err := db.PutPendingReply(b.runCtx, row); err != nil {
				t.Fatal(err)
			}
			if err := db.PutGreeting(b.runCtx, testChatID, testUserID, anchor.MessageID, time.Now()); err != nil {
				t.Fatal(err)
			}

			// The next stage may be armed while Telegram returns the failure
			// toast, after the ban operation has released the rights lock.
			var next *replyPending
			var transitionErr error
			fc.errWhen = func(method string, _ *telegoapi.RequestData) bool {
				if method == "answerCallbackQuery" && outcome == "next stage" && next == nil {
					row.Stage++
					row.GreetingMsgID++
					transitionErr = db.PutPendingReply(b.runCtx, row)
					next = b.replies.Put(testChatID, testUserID, expiry, 77, row.Stage, row.GreetingMsgID)
				}
				return method == "banChatMember"
			}
			clickReplyAnchor(t, b, "rpspamc", anchor)

			if transitionErr != nil {
				t.Fatal(transitionErr)
			}
			expected := p
			if next != nil {
				expected = next
			}
			if current := b.replies.MatchGreeting(testChatID, testUserID, row.GreetingMsgID); current != expected {
				t.Fatalf("failure changed the current wait: got %v, want %v", current, expected)
			}
			if !b.replies.Deadline(expected).Equal(expiry) {
				t.Fatal("failure changed the reply deadline")
			}
			rows, err := db.LoadAllPendingReplies(b.runCtx)
			if err != nil || len(rows) != 1 || rows[0].Stage != row.Stage ||
				rows[0].GreetingMsgID != row.GreetingMsgID || !rows[0].ExpiresAt.Equal(expiry) {
				t.Fatalf("saved reply wait changed: %v / %v", rows, err)
			}
			if _, claimed, err := db.GetSpamVote(b.runCtx, testChatID, -testUserID); err != nil || claimed {
				t.Fatalf("failed ban kept the spam claim: %v / %v", claimed, err)
			}
			if kinds := statsKinds(t, db, testChatID, testUserID); kinds[storage.EventPass] != 0 || kinds[storage.EventSpamBan] != 0 {
				t.Fatalf("failed ban recorded a terminal event: %v", kinds)
			}
			if fc.callCount("deleteMessage") != 0 || !strings.Contains(strings.Join(fc.callBodies("answerCallbackQuery"), ""), "Забанить не удалось") {
				t.Fatalf("failed ban deleted the anchor or missed the notification: %v", fc.callList())
			}

			switch outcome {
			case "answer":
				if b.replyWaitMiddleware(&telego.Message{
					Chat: chat, MessageID: 110, From: &telego.User{ID: testUserID}, Text: "Всем добрый вечер, я здесь впервые",
				}) {
					t.Fatal("ordinary reply was swallowed")
				}
				assertReplyAnchorPassed(t, b)
			case "retry":
				attempts := fc.callCount("banChatMember")
				fc.mu.Lock()
				fc.errWhen = nil
				fc.mu.Unlock()
				clickReplyAnchor(t, b, "rpspamc", anchor)
				if attempts == 0 || fc.callCount("banChatMember") != attempts+1 {
					t.Fatalf("admin could not retry: %v", fc.callList())
				}
				if rows, err := db.LoadAllPendingReplies(b.runCtx); err != nil || len(rows) != 0 {
					t.Fatalf("confirmed ban left a pending reply: %v / %v", rows, err)
				}
				if kinds := statsKinds(t, db, testChatID, testUserID); kinds[storage.EventSpamBan] != 1 || kinds[storage.EventPass] != 0 {
					t.Fatalf("retry did not record the confirmed ban once: %v", kinds)
				}
			}
		})
	}
}
