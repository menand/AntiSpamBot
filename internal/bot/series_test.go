package bot

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/mymmrac/telego"

	"github.com/menand/AntiSpamBot/internal/storage"
)

// TestCaptchaSeriesThreeStages — молчание проходит все три стадии серии и
// только потом наказывается ОДИН раз: два промежуточных таймаута не пишут
// событий, не двигают attempts и каждый раз шлют новое сообщение капчи,
// удаляя предыдущее.
func TestCaptchaSeriesThreeStages(t *testing.T) {
	ctx := context.Background()
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	b.cfg.CaptchaStageInterval = 40 * time.Millisecond

	b.runCaptcha(testChatID, telego.User{ID: testUserID, FirstName: "Юзер"}, 0)

	if rows := pendingRows(t, db); len(rows) != 1 || rows[0].Stage != 1 {
		t.Fatalf("stage 1 must be armed: %+v", rows)
	}

	waitFor(t, func() bool {
		rows := pendingRows(t, db)
		return len(rows) == 1 && rows[0].Stage == 2
	})
	if n := fc.callCount("sendMessage"); n != 2 {
		t.Fatalf("sendMessage calls = %d, want 2 (стадия 2 отправлена)", n)
	}
	kinds := statsKinds(t, db, testChatID, testUserID)
	for k, n := range kinds {
		if n != 0 {
			t.Fatalf("промежуточный таймаут записал событие %s (%v)", k, kinds)
		}
	}
	if n, err := db.AttemptCount(ctx, testChatID, testUserID, attemptsTTL); err != nil || n != 0 {
		t.Fatalf("attempts после стадии 2 = %d (err %v), want 0 — серия ещё не провалена", n, err)
	}

	waitFor(t, func() bool {
		rows := pendingRows(t, db)
		return len(rows) == 1 && rows[0].Stage == 3
	})

	waitFor(t, func() bool {
		k := statsKinds(t, db, testChatID, testUserID)
		return k[storage.EventKick] == 1 && k[storage.EventBan] == 0
	})
	if n := fc.callCount("sendMessage"); n != 3 {
		t.Fatalf("sendMessage calls = %d, want 3 (три сообщения серии)", n)
	}
	// Два перехода между стадиями + удаление в onFail.
	if n := fc.callCount("deleteMessage"); n != 3 {
		t.Fatalf("deleteMessage calls = %d, want 3", n)
	}
	if n, err := db.AttemptCount(ctx, testChatID, testUserID, attemptsTTL); err != nil || n != 1 {
		t.Fatalf("attempts после серии = %d (err %v), want 1 — вся серия одна попытка", n, err)
	}
	if rows := pendingRows(t, db); len(rows) != 0 {
		t.Fatalf("pending rows = %d, want 0 после наказания", len(rows))
	}
}

// TestCaptchaSeriesWrongClickKicksImmediately — активная неверная кнопка
// проваливает капчу сразу, серия напоминаний для промолчавших её не касается.
func TestCaptchaSeriesWrongClickKicksImmediately(t *testing.T) {
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	putCaptcha(b, db, testChatID, testUserID, 100)

	query := telego.CallbackQuery{
		ID:   "q",
		From: telego.User{ID: testUserID, FirstName: "Юзер"},
		Data: "cap:" + strconv.FormatInt(testUserID, 10) + ":0", // неверно (верный 2)
		Message: &telego.Message{
			MessageID: 100,
			Chat:      telego.Chat{ID: testChatID, Type: "supergroup"},
		},
	}
	if err := b.handleCallback(nil, query); err != nil {
		t.Fatal(err)
	}

	waitFor(t, func() bool {
		k := statsKinds(t, db, testChatID, testUserID)
		return k[storage.EventKick] == 1
	})
	if n := fc.callCount("sendMessage"); n != 0 {
		t.Fatalf("неверный клик не должен продолжать серию, sendMessage = %d", n)
	}
	if _, ok := b.store.Get(testChatID, testUserID); ok {
		t.Fatal("captcha must be taken by the failing click")
	}
}

// TestRestoreResumesRemainingSeriesStages — рестарт на середине серии
// доигрывает остаток: стадия 2 по истечении переходит в 3, финал наказывает.
func TestRestoreResumesRemainingSeriesStages(t *testing.T) {
	ctx := context.Background()
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	b.cfg.CaptchaStageInterval = 40 * time.Millisecond

	if err := db.PutPending(ctx, storage.PendingRow{
		ChatID: testChatID, UserID: testUserID, MessageID: 10,
		CorrectIdx: 1,
		// expires_at хранится в секундах: дедлайн короче ~1 c усёкся бы в
		// «истёкший за простой» с клампом к финальной стадии.
		ExpiresAt: time.Now().Add(1500 * time.Millisecond),
		Stage:     2,
	}); err != nil {
		t.Fatal(err)
	}

	n, err := b.restorePending(ctx)
	if err != nil || n != 1 {
		t.Fatalf("restored = %d err=%v, want 1/nil", n, err)
	}

	waitFor(t, func() bool {
		rows := pendingRows(t, db)
		return len(rows) == 1 && rows[0].Stage == 3
	})
	if n := fc.callCount("sendMessage"); n != 1 {
		t.Fatalf("sendMessage calls = %d, want 1 (только стадия 3)", n)
	}

	waitFor(t, func() bool {
		k := statsKinds(t, db, testChatID, testUserID)
		return k[storage.EventKick] == 1
	})
	if rows := pendingRows(t, db); len(rows) != 0 {
		t.Fatalf("pending rows = %d, want 0", len(rows))
	}
}

// TestReplyWaitLatePassDeletesReminder — поздний ответ (стадия 2+) сносит
// напоминание-якорь и пишет пасс; ответ на первой стадии приветствие хранит.
func TestReplyWaitLatePassDeletesReminder(t *testing.T) {
	ctx := context.Background()

	t.Run("поздний ответ — напоминание удалено", func(t *testing.T) {
		b, db, fc := newFlowBot(t)
		if err := db.PutGreeting(ctx, testChatID, testUserID, 500, time.Now()); err != nil {
			t.Fatal(err)
		}
		p := b.replies.Put(testChatID, testUserID, time.Now().Add(time.Minute), 0, 2)
		if err := db.PutPendingReply(ctx, storage.PendingReply{
			ChatID: testChatID, UserID: testUserID, ExpiresAt: p.ExpiresAt, Stage: 2,
		}); err != nil {
			t.Fatal(err)
		}

		b.replyWaitSatisfied(testChatID, testUserID)

		if n := fc.callCount("deleteMessage"); n != 1 {
			t.Fatalf("deleteMessage calls = %d, want 1 (якорь стадии 2)", n)
		}
		if _, ok, err := db.TakeGreetingMsg(ctx, testChatID, testUserID); err != nil || ok {
			t.Fatalf("greeting row must be consumed (ok=%v err=%v)", ok, err)
		}
		kinds := statsKinds(t, db, testChatID, testUserID)
		if kinds[storage.EventPass] != 1 {
			t.Fatalf("passes = %d, want 1", kinds[storage.EventPass])
		}
		if rows, err := db.LoadAllPendingReplies(ctx); err != nil || len(rows) != 0 {
			t.Fatalf("pending_replies = %v err=%v, want empty", rows, err)
		}
	})

	t.Run("ответ на первой стадии — приветствие остаётся", func(t *testing.T) {
		b, db, fc := newFlowBot(t)
		if err := db.PutGreeting(ctx, testChatID, testUserID, 500, time.Now()); err != nil {
			t.Fatal(err)
		}
		p := b.replies.Put(testChatID, testUserID, time.Now().Add(time.Minute), 0, 1)
		if err := db.PutPendingReply(ctx, storage.PendingReply{
			ChatID: testChatID, UserID: testUserID, ExpiresAt: p.ExpiresAt, Stage: 1,
		}); err != nil {
			t.Fatal(err)
		}

		b.replyWaitSatisfied(testChatID, testUserID)

		if n := fc.callCount("deleteMessage"); n != 0 {
			t.Fatalf("deleteMessage calls = %d, want 0 (обычное приветствие)", n)
		}
	})
}
