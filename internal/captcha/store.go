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
	ThreadID   int // forum topic the captcha was sent to; 0 = no topic

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

func (s *Store) Put(chatID, userID int64, messageID, correctIdx int, expiresAt time.Time, threadID int) *Pending {
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
		ThreadID:   threadID,
		cancelCh:   make(chan struct{}),
	}
	s.items[k] = p
	return p
}

// IsCaptchaActive reports whether the user is either in the middle of a
// captcha kickoff (pre-Put) or has an active pending captcha. Used to decide
// whether to delete messages arriving from the user before they're restricted.
func (s *Store) IsCaptchaActive(chatID, userID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key(chatID, userID)
	if _, ok := s.items[k]; ok {
		return true
	}
	return s.inflight[k]
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

// TakeChat removes and returns all pending captchas for a chat. Used when the
// bot leaves a chat — the caller should Cancel each returned Pending so the
// waitTimeout goroutines exit instead of firing kick/ban in a chat the bot no
// longer belongs to.
func (s *Store) TakeChat(chatID int64) []*Pending {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Pending
	for k, p := range s.items {
		if p.ChatID == chatID {
			delete(s.items, k)
			out = append(out, p)
		}
	}
	return out
}
