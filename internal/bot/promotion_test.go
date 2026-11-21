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

func TestPunishmentWaitsForConfirmedRole(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		want         storage.EventKind
	}{
		{"after error user became admin", "administrator", storage.EventPass},
		{"after error user is still member", "member", storage.EventBan},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "deferred.db")
			b, _, fc := newFlowBot(t)
			b.db = openPendingRestoreDB(t, ctx, path)
			serviceableChat(t, b, b.db, testChatID)
			for range 2 {
				if _, err := b.db.IncrementAttempt(ctx, testChatID, testUserID, attemptsTTL); err != nil {
					t.Fatal(err)
				}
			}
			fc.err["getChatMember"] = &telegoapi.Error{ErrorCode: 500, Description: "internal"}
			expirePunishmentCheck(t, b, storage.ReasonCaptcha)()
			decision, exists, err := b.db.GetPunishment(ctx, testChatID, testUserID)
			if err != nil || !exists || decision.Kind != "" || decision.Attempt != 0 {
				t.Fatalf("failure did not preserve an uncounted decision: %+v, %v, %v", decision, exists, err)
			}
			if n, err := b.db.AttemptCount(ctx, testChatID, testUserID, attemptsTTL); err != nil || n != 2 ||
				fc.callCount("banChatMember") != 0 {
				t.Fatalf("uncertain role counted or punished: %d, %v, %v", n, err, fc.callList())
			}
			if err := b.db.Close(); err != nil {
				t.Fatal(err)
			}
			restarted, _, retry := newFlowBot(t)
			restarted.db = openPendingRestoreDB(t, ctx, path)
			retry.resp["getChatMember"] = fmt.Sprintf(`{"status":%q,"user":{"id":7}}`, tc.status)
			restarted.reconcilePunishments(ctx)
			restarted.reconcilePunishments(ctx)
			expect := 2
			if tc.want == storage.EventBan {
				expect++
			}
			if n, err := restarted.db.AttemptCount(ctx, testChatID, testUserID, attemptsTTL); err != nil || n != expect {
				t.Fatalf("attempts after recovery: %d, %v; want %d", n, err, expect)
			}
			if events := statsKinds(t, restarted.db, testChatID, testUserID); events[tc.want] != 1 {
				t.Fatalf("want one %s: %v", tc.want, events)
			}
			if tc.want == storage.EventPass && retry.callCount("banChatMember") != 0 {
				t.Fatalf("admin was punished: %v", retry.callList())
			}
		})
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
	// Цикл сверяет режим перед карой: серия обязана быть в «с ответом»,
	// иначе тест проверял бы disarm, а не promotion-guard карательного хвоста.
	if err := db.SetWelcomeMode(ctx, testChatID, string(welcomeReply)); err != nil {
		t.Fatal(err)
	}
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

func TestRestrictedMembershipChangesWithoutStatusChange(t *testing.T) {
	b, db, fc := newFlowBot(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.runCtx = ctx
	serviceableChat(t, b, db, testChatID)
	putCaptcha(b, db, testChatID, testUserID, 77)
	user := telego.User{ID: testUserID}
	upd := memberUpdate(testChatID, leftActor, user, "restricted", "restricted")
	upd.ChatMember.OldChatMember = &telego.ChatMemberRestricted{Status: "restricted", User: user, IsMember: true}
	upd.ChatMember.NewChatMember = &telego.ChatMemberRestricted{Status: "restricted", User: user, IsMember: false}
	if err := b.handleChatMember(nil, upd); err != nil {
		t.Fatal(err)
	}
	if b.store.IsCaptchaActive(testChatID, testUserID) || len(pendingRows(t, db)) != 0 {
		t.Fatal("departed restricted user kept a timer")
	}
	if kinds := statsKinds(t, db, testChatID, testUserID); kinds[storage.EventLeft] != 1 {
		t.Fatalf("want one departure: %v", kinds)
	}

	upd.ChatMember.OldChatMember, upd.ChatMember.NewChatMember = upd.ChatMember.NewChatMember, upd.ChatMember.OldChatMember
	if err := b.handleChatMember(nil, upd); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		p, ok := b.store.Get(testChatID, testUserID)
		return ok && p.MessageID != 77
	})
	if kinds := statsKinds(t, db, testChatID, testUserID); kinds[storage.EventJoin] != 1 || kinds[storage.EventKick] != 0 ||
		kinds[storage.EventBan] != 0 || fc.callCount("unbanChatMember") != 0 {
		t.Fatalf("return did not start a fresh check: %v, %v", kinds, fc.callList())
	}
}

func TestRestoreSkipsAbsentRestrictedMembers(t *testing.T) {
	for _, reason := range []string{storage.ReasonCaptcha, storage.ReasonNoReply} {
		t.Run(reason, func(t *testing.T) {
			b, db, fc := newFlowBot(t)
			ctx := b.runCtx
			serviceableChat(t, b, db, testChatID)
			fc.resp["getChatMember"] = `{"status":"restricted","is_member":false,"user":{"id":7}}`
			expires := time.Now().Add(time.Minute)
			if reason == storage.ReasonCaptcha {
				if err := db.PutPending(ctx, storage.PendingRow{ChatID: testChatID, UserID: testUserID, MessageID: 77, ExpiresAt: expires}); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := db.SetWelcomeMode(ctx, testChatID, string(welcomeReply)); err != nil {
					t.Fatal(err)
				}
				if err := db.PutPendingReply(ctx, storage.PendingReply{ChatID: testChatID, UserID: testUserID, ExpiresAt: expires}); err != nil {
					t.Fatal(err)
				}
			}
			if n, err := b.restorePending(ctx); n != 0 || err != nil {
				t.Fatalf("absent user got a captcha timer: %d, %v", n, err)
			}
			if n, err := b.restorePendingReplies(ctx); n != 0 || err != nil {
				t.Fatalf("absent user got a reply timer: %d, %v", n, err)
			}
			if kinds := statsKinds(t, db, testChatID, testUserID); kinds[storage.EventLeft] != 1 ||
				fc.callCount("banChatMember") != 0 || fc.callCount("unbanChatMember") != 0 {
				t.Fatalf("unexpected effect of restoring an absent member: %v, %v", kinds, fc.callList())
			}
		})
	}
}
