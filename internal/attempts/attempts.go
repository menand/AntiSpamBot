package attempts

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type record struct {
	count     int
	updatedAt time.Time
}

type Tracker struct {
	mu   sync.Mutex
	data map[string]*record
	ttl  time.Duration
}

func NewTracker(ttl time.Duration) *Tracker {
	return &Tracker{
		data: make(map[string]*record),
		ttl:  ttl,
	}
}

func key(chatID, userID int64) string {
	return fmt.Sprintf("%d:%d", chatID, userID)
}

func (t *Tracker) Increment(chatID, userID int64) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	k := key(chatID, userID)
	r, ok := t.data[k]
	if !ok || time.Since(r.updatedAt) > t.ttl {
		r = &record{}
		t.data[k] = r
	}
	r.count++
	r.updatedAt = time.Now()
	return r.count
}

func (t *Tracker) Reset(chatID, userID int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.data, key(chatID, userID))
}

func (t *Tracker) Run(ctx context.Context) {
	ticker := time.NewTicker(t.ttl / 2)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.sweep()
		}
	}
}

func (t *Tracker) sweep() {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	for k, r := range t.data {
		if now.Sub(r.updatedAt) > t.ttl {
			delete(t.data, k)
		}
	}
}
