package bot

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegoapi"

	"github.com/menand/AntiSpamBot/internal/storage"
)

// TestPromotionCancelsCaptcha — назначение админом посреди активной капчи:
// серия погашена (таймер, строка в БД, сообщение капчи), капча-мьют снят
// серверным release, воронка закрыта пассом — без кика/бана.
func TestPromotionCancelsCaptcha(t *testing.T) {
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	putCaptcha(b, db, testChatID, testUserID, 77)

	from := telego.User{ID: 9, FirstName: "Аня"}
	upd := memberUpdate(testChatID, from, telego.User{ID: testUserID}, "member", "administrator")
	if err := b.handleChatMember(nil, upd); err != nil {
		t.Fatal(err)
	}

	if _, ok := b.store.Get(testChatID, testUserID); ok {
		t.Fatal("promotion must take the pending captcha")
	}
	if rows := pendingRows(t, db); len(rows) != 0 {
		t.Fatalf("pending rows = %d, want 0 (restart must not resurrect the series)", len(rows))
	}
	if n := fc.callCount("deleteMessage"); n != 1 {
		t.Fatalf("deleteMessage calls = %d, want 1 (captcha message must go)", n)
	}
	if n := fc.callCount("restrictChatMember"); n == 0 {
		t.Fatal("captcha mute must be released on promotion")
	}
	kinds := statsKinds(t, db, testChatID, testUserID)
	if kinds[storage.EventPass] != 1 {
		t.Fatalf("pass events = %d, want 1 (%v) — funnel must close", kinds[storage.EventPass], kinds)
	}
	if kinds[storage.EventKick]+kinds[storage.EventBan] != 0 {
		t.Fatalf("promotion must not punish: %v", kinds)
	}
}

// TestPromotionCancelsReplyWait — назначение админом посреди ожидания
// ответа: ожидание и персистентная строка сняты, воронка закрыта пассом.
func TestPromotionCancelsReplyWait(t *testing.T) {
	ctx := context.Background()
	b, db, _ := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)

	p := b.replies.Put(testChatID, testUserID, time.Now().Add(time.Minute), 0, 1, 0)
	if err := db.PutPendingReply(ctx, storage.PendingReply{
		ChatID: testChatID, UserID: testUserID, ExpiresAt: p.ExpiresAt, Stage: 1,
	}); err != nil {
		t.Fatal(err)
	}

	from := telego.User{ID: 9, FirstName: "Аня"}
	upd := memberUpdate(testChatID, from, telego.User{ID: testUserID}, "member", "administrator")
	if err := b.handleChatMember(nil, upd); err != nil {
		t.Fatal(err)
	}

	if _, ok := b.replies.Get(testChatID, testUserID); ok {
		t.Fatal("promotion must take the pending reply wait")
	}
	rows, err := db.LoadAllPendingReplies(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("pending_replies rows = %d, want 0 (restart must not resurrect the series)", len(rows))
	}
	kinds := statsKinds(t, db, testChatID, testUserID)
	if kinds[storage.EventPass] != 1 {
		t.Fatalf("pass events = %d, want 1 (%v) — funnel must close", kinds[storage.EventPass], kinds)
	}
	if kinds[storage.EventKick]+kinds[storage.EventBan] != 0 {
		t.Fatalf("promotion must not punish: %v", kinds)
	}
}

// TestPunishSkippedForFreshAdmin — бэкстоп гонки «событие промоушена опоздало
// к карательному хвосту»: live-проверка видит админа, наказание не летит,
// залипший капча-мьют снимается release'ом, воронка закрывается пассом.
func TestPunishSkippedForFreshAdmin(t *testing.T) {
	ctx := context.Background()
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	b.cfg.MaxAttempts = 3
	for range 2 { // две попытки уже были — серия на пороге бана
		if _, err := db.IncrementAttempt(ctx, testChatID, testUserID, attemptsTTL); err != nil {
			t.Fatal(err)
		}
	}
	p := putCaptcha(b, db, testChatID, testUserID, 77)
	// Опаздывающий хвост видит юзера уже админом (промоушен обогнал серию).
	fc.resp["getChatMember"] = `{"status":"administrator","user":{"id":7,"is_bot":false,"first_name":"Юзер"}}`

	if err := b.onFail(ctx, p, "тест"); err != nil {
		t.Fatalf("onFail must not fail the skip: %v", err)
	}
	if n := fc.callCount("banChatMember") + fc.callCount("unbanChatMember"); n != 0 {
		t.Fatalf("promoted admin must not be punished (calls: %v)", fc.callList())
	}
	if n := fc.callCount("restrictChatMember"); n == 0 {
		t.Fatal("stale captcha mute must be released on the skip")
	}
	kinds := statsKinds(t, db, testChatID, testUserID)
	if kinds[storage.EventPass] != 1 {
		t.Fatalf("pass events = %d, want 1 (%v)", kinds[storage.EventPass], kinds)
	}
	if kinds[storage.EventKick]+kinds[storage.EventBan]+kinds[storage.EventAbort] != 0 {
		t.Fatalf("skip must write no punish events: %v", kinds)
	}
	// Счётчик попыток skip не трогает: гард стоит ДО инкремента, иначе
	// следующий честный провал этого юзера эскалировался бы на ступень раньше.
	if n, err := db.AttemptCount(ctx, testChatID, testUserID, attemptsTTL); err != nil || n != 2 {
		t.Fatalf("skip must not touch attempts (n=%d err=%v, want 2)", n, err)
	}
}

// TestPromotionDeletesEphemeralCaptcha — промоушен при ЭФЕМЕРНОЙ капче:
// сообщение сносится deleteEphemeralMessage (ephemeral id + receiver), а не
// обычным deleteMessage(0); строка уходит по msg-id guard'у — рестарт не
// воскресит серию.
func TestPromotionDeletesEphemeralCaptcha(t *testing.T) {
	ctx := context.Background()
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)

	expires := time.Now().Add(time.Minute)
	if err := db.PutPending(ctx, storage.PendingRow{
		ChatID: testChatID, UserID: testUserID, MessageID: 0,
		CorrectIdx: 2, ExpiresAt: expires, EphemeralID: 321, Stage: 1,
	}); err != nil {
		t.Fatal(err)
	}
	b.store.Put(testChatID, testUserID, 0, 2, expires, 0, 321, 1)

	from := telego.User{ID: 9, FirstName: "Аня"}
	upd := memberUpdate(testChatID, from, telego.User{ID: testUserID}, "member", "administrator")
	if err := b.handleChatMember(nil, upd); err != nil {
		t.Fatal(err)
	}

	if n := fc.callCount("deleteEphemeralMessage"); n != 1 {
		t.Fatalf("deleteEphemeralMessage calls = %d, want 1 (%v)", n, fc.callList())
	}
	if n := fc.callCount("deleteMessage"); n != 0 {
		t.Fatalf("regular deleteMessage = %d, want 0 (обычный id капчи = 0, удалять нельзя)", n)
	}
	if bodies := fc.callBodies("deleteEphemeralMessage"); len(bodies) != 1 ||
		!strings.Contains(bodies[0], `"receiver_user_id":7`) {
		t.Fatalf("deletion must target the captcha receiver: %v", bodies)
	}
	if rows := pendingRows(t, db); len(rows) != 0 {
		t.Fatalf("pending rows = %d, want 0 (restart must not resurrect)", len(rows))
	}
}

// TestPunishAttemptFailOpenOnAdminCheckError — ошибка живой проверки НЕ
// отменяет наказание (fail-open, старое поведение): счётчик на пороге — бан
// всё равно летит, компенсирующего пасса нет.
func TestPunishAttemptFailOpenOnAdminCheckError(t *testing.T) {
	ctx := context.Background()
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	b.cfg.MaxAttempts = 3
	for range 2 {
		if _, err := db.IncrementAttempt(ctx, testChatID, testUserID, attemptsTTL); err != nil {
			t.Fatal(err)
		}
	}
	fc.err["getChatMember"] = &telegoapi.Error{ErrorCode: 500, Description: "internal"}
	p := putCaptcha(b, db, testChatID, testUserID, 77)

	if err := b.onFail(ctx, p, "тест"); err != nil {
		t.Fatalf("fail-open punish must proceed: %v", err)
	}
	if n := fc.callCount("banChatMember"); n != 1 {
		t.Fatalf("banChatMember calls = %d, want 1 (%v)", n, fc.callList())
	}
	kinds := statsKinds(t, db, testChatID, testUserID)
	if kinds[storage.EventBan] != 1 || kinds[storage.EventPass] != 0 {
		t.Fatalf("want exactly one ban, no pass: %v", kinds)
	}
}

// TestReplyWaitPunishSkippedForPromotedAdmin — финал серии reply-wait на
// свежем админе: кика нет, залипший мьют снят, воронка закрыта ОДНИМ пассом,
// строка pending_replies снята (иначе рестарт воскресит наказание),
// счётчик попыток не тронут.
func TestReplyWaitPunishSkippedForPromotedAdmin(t *testing.T) {
	ctx := context.Background()
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	fc.resp["getChatMember"] = `{"status":"administrator","user":{"id":7,"is_bot":false,"first_name":"Юзер"}}`
	if err := db.PutGreeting(ctx, testChatID, testUserID, 500, time.Now()); err != nil {
		t.Fatal(err)
	}
	p := b.replies.Put(testChatID, testUserID, time.Now().Add(-time.Millisecond), 0, captchaStages, 0)
	if err := db.PutPendingReply(ctx, storage.PendingReply{
		ChatID: testChatID, UserID: testUserID, ExpiresAt: p.ExpiresAt, Stage: captchaStages,
	}); err != nil {
		t.Fatal(err)
	}

	// Финальная стадия уже истекла — цикл сразу уходит в карательный хвост.
	b.replyWaitLoop(testChatID, testUserID, p)

	if n := fc.callCount("banChatMember") + fc.callCount("unbanChatMember"); n != 0 {
		t.Fatalf("promoted admin must not be punished (calls: %v)", fc.callList())
	}
	if fc.callCount("restrictChatMember") == 0 {
		t.Fatal("stale captcha mute must be released on the skip")
	}
	kinds := statsKinds(t, db, testChatID, testUserID)
	if kinds[storage.EventPass] != 1 || kinds[storage.EventKick]+kinds[storage.EventBan] != 0 {
		t.Fatalf("skip must close the funnel with exactly one pass: %v", kinds)
	}
	if rows, err := db.LoadAllPendingReplies(ctx); err != nil || len(rows) != 0 {
		t.Fatalf("pending_replies rows = %d, err=%v; want 0", len(rows), err)
	}
	if n, err := db.AttemptCount(ctx, testChatID, testUserID, attemptsTTL); err != nil || n != 0 {
		t.Fatalf("attempts = %d, err=%v; want 0 — skip must not increment", n, err)
	}
}
