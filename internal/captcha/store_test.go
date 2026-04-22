package captcha

import (
	"sync"
	"testing"
	"time"
)

func TestStorePutAndTake(t *testing.T) {
	s := NewStore()
	p := s.Put(1, 2, 100, 3, time.Now().Add(time.Minute))
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
	first := s.Put(1, 2, 100, 0, time.Now().Add(time.Minute))
	_ = s.Put(1, 2, 200, 0, time.Now().Add(time.Minute)) // overwrites

	select {
	case <-first.Done():
		// good — old one was cancelled
	case <-time.After(time.Second):
		t.Fatal("first pending was not cancelled when second Put replaced it")
	}
}

func TestPendingCancelIsIdempotent(t *testing.T) {
	s := NewStore()
	p := s.Put(1, 2, 0, 0, time.Now().Add(time.Minute))
	p.Cancel()
	p.Cancel() // must not panic
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
				// simulate some setup work before either Put or cleanup
				time.Sleep(time.Millisecond)
				s.FinishKickoff(1, 2)
			}
		}()
	}
	wg.Wait()
	if won != 1 {
		t.Fatalf("expected exactly 1 kickoff to win, got %d", won)
	}

	// After everyone finished, next kickoff should succeed
	if !s.BeginKickoff(1, 2) {
		t.Fatal("kickoff should succeed after all previous ones finished")
	}
	s.FinishKickoff(1, 2)
}

func TestBeginKickoffBlockedByActiveCaptcha(t *testing.T) {
	s := NewStore()
	s.Put(1, 2, 100, 0, time.Now().Add(time.Minute))

	if s.BeginKickoff(1, 2) {
		t.Fatal("kickoff should fail when a captcha is already active")
	}

	s.Take(1, 2)
	if !s.BeginKickoff(1, 2) {
		t.Fatal("kickoff should succeed after Take cleared the captcha")
	}
	s.FinishKickoff(1, 2)
}

func TestStoreConcurrentTake(t *testing.T) {
	s := NewStore()
	s.Put(1, 2, 0, 0, time.Now().Add(time.Minute))

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
