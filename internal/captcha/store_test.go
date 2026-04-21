package captcha

import (
	"sync"
	"testing"
	"time"
)

func TestStorePutAndTake(t *testing.T) {
	s := NewStore()
	p := s.Put(1, 2, 100, 3, time.Minute)
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
	first := s.Put(1, 2, 100, 0, time.Minute)
	_ = s.Put(1, 2, 200, 0, time.Minute) // overwrites

	select {
	case <-first.Done():
		// good — old one was cancelled
	case <-time.After(time.Second):
		t.Fatal("first pending was not cancelled when second Put replaced it")
	}
}

func TestPendingCancelIsIdempotent(t *testing.T) {
	s := NewStore()
	p := s.Put(1, 2, 0, 0, time.Minute)
	p.Cancel()
	p.Cancel() // must not panic
	p.Cancel()
}

func TestStoreConcurrentTake(t *testing.T) {
	s := NewStore()
	s.Put(1, 2, 0, 0, time.Minute)

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
