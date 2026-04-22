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
	mu       sync.Mutex
	items    map[string]*Pending
	inflight map[string]bool // kickoffs currently in setup (pre-Put)
}

func NewStore() *Store {
	return &Store{
		items:    make(map[string]*Pending),
		inflight: make(map[string]bool),
	}
}

// BeginKickoff marks (chatID, userID) as being set up for a captcha. Returns
// true if we won the race and the caller is responsible for calling
// FinishKickoff when done (regardless of whether Put was reached). Returns
// false if another captcha is already active or another kickoff is already
// in progress — the caller should bail out silently.
func (s *Store) BeginKickoff(chatID, userID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key(chatID, userID)
	if _, ok := s.items[k]; ok {
		return false
	}
	if s.inflight[k] {
		return false
	}
	s.inflight[k] = true
	return true
}

// FinishKickoff clears the in-flight flag. Safe to call multiple times.
// Must be called by the same caller that got `true` from BeginKickoff.
func (s *Store) FinishKickoff(chatID, userID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.inflight, key(chatID, userID))
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
