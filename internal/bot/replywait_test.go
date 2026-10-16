package bot

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/menand/AntiSpamBot/internal/storage"
)

func TestReplyStoreTakeSingleWinner(t *testing.T) {
	s := newReplyStore()
	s.Put(1, 2, time.Now().Add(time.Minute), 0, 1, 0)

	// Гонка «сообщение vs таймаут vs выход»: Take выигрывает ровно один.
	var wins int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok := s.Take(1, 2); ok {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("Take winners = %d, want exactly 1", wins)
	}
}

func TestReplyStorePutReplacesAndCancels(t *testing.T) {
	s := newReplyStore()
	old := s.Put(1, 2, time.Now().Add(time.Minute), 0, 1, 0)
	s.Put(1, 2, time.Now().Add(2*time.Minute), 0, 1, 0) // перезаход перевзводит

	select {
	case <-old.Done():
		// старое ожидание отменено — его replyWaitLoop выйдет тихо
	case <-time.After(time.Second):
		t.Fatal("old pending must be cancelled on re-Put")
	}
	if p, ok := s.Take(1, 2); !ok || p == old {
		t.Fatalf("Take must return the new pending, got ok=%v same=%v", ok, p == old)
	}
}

func TestReplyStoreTakeChat(t *testing.T) {
	s := newReplyStore()
	s.Put(1, 10, time.Now().Add(time.Minute), 0, 1, 0)
	s.Put(1, 11, time.Now().Add(time.Minute), 0, 1, 0)
	s.Put(2, 12, time.Now().Add(time.Minute), 0, 1, 0)

	got := s.TakeChat(1)
	if len(got) != 2 {
		t.Fatalf("TakeChat(1) = %d pendings, want 2", len(got))
	}
	if _, ok := s.Take(2, 12); !ok {
		t.Fatal("chat 2 pending must survive TakeChat(1)")
	}
}

func newTestBotWithDB(t *testing.T) (*Bot, *storage.DB) {
	t.Helper()
	ctx := context.Background()
	db, err := storage.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &Bot{
		db:      db,
		replies: newReplyStore(),
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		runCtx:  context.Background(),
	}, db
}

func TestReplyWaitSatisfiedRecordsPass(t *testing.T) {
	ctx := context.Background()
	b, db := newTestBotWithDB(t)
	b.replies.Put(1, 2, time.Now().Add(time.Minute), 0, 1, 0)
	_ = db.PutPendingReply(ctx, storage.PendingReply{
		ChatID: 1, UserID: 2, ExpiresAt: time.Now().Add(time.Minute),
	})

	if p := b.replyWaitSatisfied(1, 2); p == nil {
		t.Fatal("replyWaitSatisfied must return the taken pending")
	}

	// Ожидание снято и «прошёл» записан ровно один раз.
	if _, ok := b.replies.Take(1, 2); ok {
		t.Fatal("pending reply must be taken by replyWaitSatisfied")
	}
	if rows, err := db.LoadAllPendingReplies(ctx); err != nil {
		t.Fatal(err)
	} else if len(rows) != 0 {
		t.Fatalf("pending_replies rows = %d, want 0", len(rows))
	}
	s, err := db.QueryStats(ctx, 1, time.Now().Add(-time.Minute), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if s.Passed != 1 {
		t.Fatalf("passes = %d, want 1", s.Passed)
	}

	// Повторный вызов (ожидания уже нет) пасс не дублирует и возвращает nil.
	if p := b.replyWaitSatisfied(1, 2); p != nil {
		t.Fatal("replyWaitSatisfied on an idle wait must return nil")
	}
	s, _ = db.QueryStats(ctx, 1, time.Now().Add(-time.Minute), time.Now().Add(time.Minute))
	if s.Passed != 1 {
		t.Fatalf("passes after duplicate = %d, want 1", s.Passed)
	}
}

func TestCancelReplyWaitSignalsCancelled(t *testing.T) {
	ctx := context.Background()
	b, db := newTestBotWithDB(t)
	b.replies.Put(1, 2, time.Now().Add(time.Minute), 0, 1, 0)
	_ = db.PutPendingReply(ctx, storage.PendingReply{
		ChatID: 1, UserID: 2, ExpiresAt: time.Now().Add(time.Minute),
	})

	if !b.cancelReplyWait(1, 2) {
		t.Fatal("cancelReplyWait must report an active wait was cancelled")
	}
	if rows, err := db.LoadAllPendingReplies(ctx); err != nil {
		t.Fatal(err)
	} else if len(rows) != 0 {
		t.Fatalf("pending_replies rows = %d, want 0", len(rows))
	}
	if b.cancelReplyWait(1, 2) {
		t.Fatal("cancelReplyWait on an idle wait must report false")
	}
}

// TestDisarmReplyWaits — смена режима «после капчи» уводит чат из reply:
// живые серии снимаются разом, каждая воронка закрывается компенсирующим
// пассом (в режиме reply пасс пишет только ответ — без пасса юзеры навсегда
// зависли бы в «В процессе»), строки pending_replies удаляются, таймеры
// отменяются: дорешать или наказать после смены режима некому.
func TestDisarmReplyWaits(t *testing.T) {
	ctx := context.Background()
	b, db := newTestBotWithDB(t)

	var cancels []*replyPending
	for _, uid := range []int64{10, 11} {
		cancels = append(cancels, b.replies.Put(1, uid, time.Now().Add(time.Minute), 0, 1, 0))
		if err := db.PutPendingReply(ctx, storage.PendingReply{
			ChatID: 1, UserID: uid, ExpiresAt: time.Now().Add(time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Чужой чат снятие не трогает — ни в сторе, ни в БД.
	b.replies.Put(2, 20, time.Now().Add(time.Minute), 0, 1, 0)
	if err := db.PutPendingReply(ctx, storage.PendingReply{
		ChatID: 2, UserID: 20, ExpiresAt: time.Now().Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	b.disarmReplyWaits(1)

	for i, p := range cancels {
		select {
		case <-p.Done():
		default:
			t.Fatalf("pending %d must be cancelled", i)
		}
	}
	if _, ok := b.replies.Take(1, 10); ok {
		t.Fatal("disarmed wait must leave the store")
	}
	if _, ok := b.replies.Take(2, 20); !ok {
		t.Fatal("wait of another chat must survive the disarm")
	}
	rows, err := db.LoadAllPendingReplies(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ChatID != 2 {
		t.Fatalf("pending_replies after disarm = %+v, want only chat 2", rows)
	}
	s, err := db.QueryStats(ctx, 1, time.Now().Add(-time.Minute), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if s.Passed != 2 {
		t.Fatalf("compensating passes = %d, want 2", s.Passed)
	}
}

// TestRestorePendingRepliesDisarmsNonReplyMode — крэш в окне между записью
// режима и живым снятием серий (или ручная правка настроек): строка чата,
// ушедшего из «с ответом», не восстанавливается — воронка закрывается
// пассом, иначе юзер получил бы кик за требование, которое уже не обещано.
func TestRestorePendingRepliesDisarmsNonReplyMode(t *testing.T) {
	ctx := context.Background()
	b, db, _ := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	if err := db.SetWelcomeMode(ctx, testChatID, string(welcomeGreet)); err != nil {
		t.Fatal(err)
	}
	if err := db.PutPendingReply(ctx, storage.PendingReply{
		ChatID: testChatID, UserID: testUserID,
		ExpiresAt: time.Now().Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	n, err := b.restorePendingReplies(ctx)
	if err != nil || n != 0 {
		t.Fatalf("restored = %d err=%v, want 0/nil", n, err)
	}
	if _, ok := b.replies.Take(testChatID, testUserID); ok {
		t.Fatal("wait of a non-reply chat must not be armed")
	}
	rows, err := db.LoadAllPendingReplies(ctx)
	if err != nil || len(rows) != 0 {
		t.Fatalf("pending_replies rows = %d, err=%v; want 0", len(rows), err)
	}
	if k := statsKinds(t, db, testChatID, testUserID); k[storage.EventPass] != 1 {
		t.Fatalf("compensating pass = %d, want 1 (%v)", k[storage.EventPass], k)
	}
}

// TestMaybeArmReplyWaitModeLeftRecordsPass — пока капча дозревала, админ увёл
// чат из «с ответом» (устаревший снапшот s): серия не взводится, но воронка
// закрывается компенсирующим пассом — иначе прошедший капчу юзер навсегда
// завис бы в «В процессе» (в reply-режиме пасс пишет только ответ, а ожидания
// нет — ни disarmReplyWaits, ни цикл его не увидят).
func TestMaybeArmReplyWaitModeLeftRecordsPass(t *testing.T) {
	ctx := context.Background()
	b, db, _ := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	if err := db.SetWelcomeMode(ctx, testChatID, string(welcomeGreet)); err != nil {
		t.Fatal(err)
	}
	s := storage.ChatSettings{GreetingEnabled: true, ReplyCheckEnabled: true} // снимок ДО смены режима
	if b.maybeArmReplyWait(s, testChatID, testUserID, 0) {
		t.Fatal("stale snapshot must not arm against the DB mode")
	}
	if _, ok := b.replies.Take(testChatID, testUserID); ok {
		t.Fatal("no wait may be armed")
	}
	rows, err := db.LoadAllPendingReplies(ctx)
	if err != nil || len(rows) != 0 {
		t.Fatalf("pending_replies rows = %d, err=%v; want 0", len(rows), err)
	}
	if k := statsKinds(t, db, testChatID, testUserID); k[storage.EventPass] != 1 {
		t.Fatalf("compensating pass = %d, want 1 (%v)", k[storage.EventPass], k)
	}
}

// TestReplyWaitLoopDisarmsWhenModeLeft — страховка режима в цикле: серия,
// пережившая смену режима (устаревший арм / гонка со снятием), на первой же
// стадии снимается компенсирующим пассом — без напоминаний и без кика за
// «молчание», которое чат больше не требует.
func TestReplyWaitLoopDisarmsWhenModeLeft(t *testing.T) {
	ctx := context.Background()
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	if err := db.SetWelcomeMode(ctx, testChatID, string(welcomeGreet)); err != nil {
		t.Fatal(err)
	}
	if err := db.PutPendingReply(ctx, storage.PendingReply{
		ChatID: testChatID, UserID: testUserID, ExpiresAt: time.Now().Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	// Просроченный дедлайн — цикл проверяет режим сразу, без ожидания стадии.
	p := b.replies.Put(testChatID, testUserID, time.Now().Add(-time.Second), 0, 1, 0)

	b.replyWaitLoop(testChatID, testUserID, p) // синхронно: страховка возвращает управление

	if _, ok := b.replies.Take(testChatID, testUserID); ok {
		t.Fatal("wait must be taken by the mode guard")
	}
	rows, err := db.LoadAllPendingReplies(ctx)
	if err != nil || len(rows) != 0 {
		t.Fatalf("pending_replies rows = %d, err=%v; want 0", len(rows), err)
	}
	k := statsKinds(t, db, testChatID, testUserID)
	if k[storage.EventPass] != 1 || k[storage.EventKick]+k[storage.EventBan] != 0 {
		t.Fatalf("want one compensating pass and no punish, got %v", k)
	}
	if n := fc.callCount("sendMessage"); n != 0 {
		t.Fatalf("no reminder may be sent after the mode left reply, sends = %d", n)
	}
}

// TestDisarmReplyWaitLoserNoSecondPass — disarm проигравшего гонку не пишет
// второй пасс: ожидание уже изъял настоящий победитель (ответ юзера и т.п.).
func TestDisarmReplyWaitLoserNoSecondPass(t *testing.T) {
	ctx := context.Background()
	b, db := newTestBotWithDB(t)
	p := b.replies.Put(1, 2, time.Now().Add(time.Minute), 0, 1, 0)
	if _, ok := b.replies.Take(1, 2); !ok {
		t.Fatal("winner must take the wait")
	}
	b.disarmReplyWait(ctx, 1, 2, p)

	s, err := db.QueryStats(ctx, 1, time.Now().Add(-time.Minute), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if s.Passed != 0 {
		t.Fatalf("loser must not record a pass, got %d", s.Passed)
	}
}
