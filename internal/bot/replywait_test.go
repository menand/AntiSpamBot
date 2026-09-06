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

	b.replyWaitSatisfied(1, 2)

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

	// Повторный вызов (ожидания уже нет) пасс не дублирует.
	b.replyWaitSatisfied(1, 2)
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
