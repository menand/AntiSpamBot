package captcha

import (
	"fmt"
	"sync"
	"time"
)

type Pending struct {
	ChatID     int64
	UserID     int64
	MessageID  int
	CorrectIdx int
	ExpiresAt  time.Time

	cancelOnce sync.Once
	cancelCh   chan struct{}
}

func (p *Pending) Cancel() {
	p.cancelOnce.Do(func() { close(p.cancelCh) })
}

func (p *Pending) Done() <-chan struct{} {
	return p.cancelCh
}

type Store struct {
	mu    sync.Mutex
	items map[string]*Pending
}

func NewStore() *Store {
	return &Store{items: make(map[string]*Pending)}
}

func key(chatID, userID int64) string {
	return fmt.Sprintf("%d:%d", chatID, userID)
}

func (s *Store) Put(chatID, userID int64, messageID, correctIdx int, expiresAt time.Time) *Pending {
	s.mu.Lock()
	defer s.mu.Unlock()

	k := key(chatID, userID)
	if old, ok := s.items[k]; ok {
		old.Cancel()
	}
	p := &Pending{
		ChatID:     chatID,
		UserID:     userID,
		MessageID:  messageID,
		CorrectIdx: correctIdx,
		ExpiresAt:  expiresAt,
		cancelCh:   make(chan struct{}),
	}
	s.items[k] = p
	return p
}

func (s *Store) Exists(chatID, userID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.items[key(chatID, userID)]
	return ok
}

func (s *Store) Take(chatID, userID int64) (*Pending, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key(chatID, userID)
	p, ok := s.items[k]
	if ok {
		delete(s.items, k)
	}
	return p, ok
}
