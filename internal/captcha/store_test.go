package captcha

import (
	"sync"
	"testing"
	"time"
)

func TestStorePutAndTake(t *testing.T) {
	s := NewStore()
	p := s.Put(1, 2, 100, 3, time.Now().Add(time.Minute), 0)
	if p.ChatID != 1 || p.UserID != 2 || p.MessageID != 100 || p.CorrectIdx != 3 {
		t.Fatalf("unexpected pending: %+v", p)
	}

	got, ok := s.Take(1, 2)
	if !ok || got != p {
		t.Fatal("Take did not return the stored pending")
	}

	if _, ok := s.Take(1, 2); ok {
		t.Fatal("Take succeeded twice for the same key")
	}
}

func TestStorePutCancelsExisting(t *testing.T) {
	s := NewStore()
	first := s.Put(1, 2, 100, 0, time.Now().Add(time.Minute), 0)
	_ = s.Put(1, 2, 200, 0, time.Now().Add(time.Minute), 0) // перезаписывает

	select {
	case <-first.Done():
		// хорошо — старый отменён
	case <-time.After(time.Second):
		t.Fatal("first pending was not cancelled when second Put replaced it")
	}
}

func TestPendingCancelIsIdempotent(t *testing.T) {
	s := NewStore()
	p := s.Put(1, 2, 0, 0, time.Now().Add(time.Minute), 0)
	p.Cancel()
	p.Cancel() // не должен паниковать
	p.Cancel()
}

func TestBeginKickoffExclusive(t *testing.T) {
	s := NewStore()

	const workers = 50
	var wg sync.WaitGroup
	wg.Add(workers)
	var won int32
	var mu sync.Mutex
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			if s.BeginKickoff(1, 2) {
				mu.Lock()
				won++
				mu.Unlock()
				// имитируем подготовительную работу перед Put или очисткой
				time.Sleep(time.Millisecond)
				s.FinishKickoff(1, 2)
			}
		}()
	}
	wg.Wait()
	if won != 1 {
		t.Fatalf("expected exactly 1 kickoff to win, got %d", won)
	}

	// Когда все закончили, следующий kickoff должен пройти.
	if !s.BeginKickoff(1, 2) {
		t.Fatal("kickoff should succeed after all previous ones finished")
	}
	s.FinishKickoff(1, 2)
}

func TestBeginKickoffBlockedByActiveCaptcha(t *testing.T) {
	s := NewStore()
	s.Put(1, 2, 100, 0, time.Now().Add(time.Minute), 0)

	if s.BeginKickoff(1, 2) {
		t.Fatal("kickoff should fail when a captcha is already active")
	}

	s.Take(1, 2)
	if !s.BeginKickoff(1, 2) {
		t.Fatal("kickoff should succeed after Take cleared the captcha")
	}
	s.FinishKickoff(1, 2)
}

func TestTakeChat(t *testing.T) {
	s := NewStore()
	a := s.Put(10, 1, 0, 0, time.Now().Add(time.Minute), 0)
	b := s.Put(10, 2, 0, 0, time.Now().Add(time.Minute), 0)
	s.Put(20, 3, 0, 0, time.Now().Add(time.Minute), 0)

	got := s.TakeChat(10)
	if len(got) != 2 {
		t.Fatalf("TakeChat(10) returned %d pendings, want 2", len(got))
	}
	seen := map[*Pending]bool{got[0]: true, got[1]: true}
	if !seen[a] || !seen[b] {
		t.Fatal("TakeChat did not return the chat's pendings")
	}
	if _, ok := s.Take(10, 1); ok {
		t.Fatal("pending still in store after TakeChat")
	}
	if _, ok := s.Take(20, 3); !ok {
		t.Fatal("TakeChat removed a pending from another chat")
	}
}

func TestStoreConcurrentTake(t *testing.T) {
	s := NewStore()
	s.Put(1, 2, 0, 0, time.Now().Add(time.Minute), 0)

	const workers = 50
	var wg sync.WaitGroup
	wg.Add(workers)
	var gotCount int32
	var mu sync.Mutex
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			if _, ok := s.Take(1, 2); ok {
				mu.Lock()
				gotCount++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if gotCount != 1 {
		t.Fatalf("expected exactly 1 Take to succeed, got %d", gotCount)
	}
}
