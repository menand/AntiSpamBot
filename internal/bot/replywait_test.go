package bot

import (
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/menand/AntiSpamBot/internal/storage"
)

func TestReplyStoreTakeSingleWinner(t *testing.T) {
	s := newReplyStore()
	s.Put(1, 2, time.Now().Add(time.Minute))

	// Гонка «сообщение vs таймаут vs выход»: Take выигрывает ровно один.
	var wins int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
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
	old := s.Put(1, 2, time.Now().Add(time.Minute))
	s.Put(1, 2, time.Now().Add(2*time.Minute)) // перезаход перевзводит

	select {
	case <-old.Done():
		// старое ожидание отменено — его waitReplyTimeout выйдет тихо
	case <-time.After(time.Second):
		t.Fatal("old pending must be cancelled on re-Put")
	}
	if p, ok := s.Take(1, 2); !ok || p == old {
		t.Fatalf("Take must return the new pending, got ok=%v same=%v", ok, p == old)
	}
}

func TestReplyStoreTakeChat(t *testing.T) {
	s := newReplyStore()
	s.Put(1, 10, time.Now().Add(time.Minute))
	s.Put(1, 11, time.Now().Add(time.Minute))
	s.Put(2, 12, time.Now().Add(time.Minute))

	got := s.TakeChat(1)
	if len(got) != 2 {
		t.Fatalf("TakeChat(1) = %d pendings, want 2", len(got))
	}
	if _, ok := s.Take(2, 12); !ok {
		t.Fatal("chat 2 pending must survive TakeChat(1)")
	}
}

func TestEffectiveReplyCheckSeconds(t *testing.T) {
	var s storage.ChatSettings
	if got := effectiveReplyCheckSeconds(s); got != defaultReplyCheckSeconds {
		t.Errorf("default: got %d, want %d", got, defaultReplyCheckSeconds)
	}
	s.ReplyCheckSeconds = sql.NullInt64{Int64: 90, Valid: true}
	if got := effectiveReplyCheckSeconds(s); got != 90 {
		t.Errorf("override: got %d, want 90", got)
	}
}
