package captcha

import (
	"context"
	"sync"
	"time"
)

type Pending struct {
	ChatID      int64
	UserID      int64
	MessageID   int
	CorrectIdx  int
	ExpiresAt   time.Time
	ThreadID    int // топик форума, куда отправлена капча; 0 = без топика
	EphemeralID int // ≠0: капча эфемерная (видна только юзеру), удалять по этому id
	Stage       int // стадия серии (1..3): какое сообщение серии сейчас живо

	cancelOnce sync.Once
	cancelCh   chan struct{}
}

func (p *Pending) Cancel() {
	p.cancelOnce.Do(func() { close(p.cancelCh) })
}

func (p *Pending) Done() <-chan struct{} {
	return p.cancelCh
}

// capKey идентифицирует капчу парой (chat, user).
type capKey struct {
	chatID int64
	userID int64
}

type Store struct {
	mu       sync.Mutex
	items    map[capKey]*Pending
	inflight map[capKey]*kickoff // подготовка/смена стадии до Put
}

type kickoff struct {
	ctx    context.Context
	cancel context.CancelFunc
}

func NewStore() *Store {
	return &Store{
		items:    make(map[capKey]*Pending),
		inflight: make(map[capKey]*kickoff),
	}
}

// BeginKickoff помечает (chatID, userID) как капчу в стадии подготовки.
// Возвращает true, если мы выиграли гонку, — тогда вызывающий обязан по
// завершении вызвать FinishKickoff (независимо от того, дошло ли до Put).
// Возвращает false, если уже активна другая капча или уже идёт другой
// кикофф, — вызывающему следует молча выйти.
func (s *Store) BeginKickoff(chatID, userID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := capKey{chatID, userID}
	if _, ok := s.items[k]; ok {
		return false
	}
	if s.inflight[k] != nil {
		return false
	}
	s.beginKickoff(k)
	return true
}

func (s *Store) beginKickoff(k capKey) context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	s.inflight[k] = &kickoff{ctx: ctx, cancel: cancel}
	return ctx
}

func (s *Store) KickoffContext(chatID, userID int64) context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	if k := s.inflight[capKey{chatID, userID}]; k != nil {
		return k.ctx
	}
	return context.Background()
}

// CancelKickoff отменяет подготовку, сохраняя занятый слот до завершения
// владельца: старый сетевой запрос не должен пережить новую серию.
func (s *Store) CancelKickoff(chatID, userID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := s.inflight[capKey{chatID, userID}]
	if k == nil || k.ctx.Err() != nil {
		return false
	}
	k.cancel()
	return true
}

// CancelChatKickoffs останавливает ещё не опубликованные проверки чата.
// Слоты остаются занятыми до выхода владельцев сетевых запросов.
func (s *Store) CancelChatKickoffs(chatID int64) []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var users []int64
	for key, k := range s.inflight {
		if key.chatID == chatID && k.ctx.Err() == nil {
			k.cancel()
			users = append(users, key.userID)
		}
	}
	return users
}

// FinishKickoff снимает флаг in-flight. Безопасно вызывать несколько раз.
// Вызывать должен тот же, кто получил `true` от BeginKickoff.
func (s *Store) FinishKickoff(chatID, userID int64, expected ...context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := capKey{chatID, userID}
	if k := s.inflight[key]; k != nil && (len(expected) == 0 || k.ctx == expected[0]) {
		k.cancel()
		delete(s.inflight, key)
	}
}

func (s *Store) Put(chatID, userID int64, messageID, correctIdx int, expiresAt time.Time, threadID, ephemeralID, stage int) *Pending {
	s.mu.Lock()
	defer s.mu.Unlock()

	k := capKey{chatID, userID}
	if prep := s.inflight[k]; prep != nil && prep.ctx.Err() != nil {
		return nil
	}
	if old, ok := s.items[k]; ok {
		old.Cancel()
	}
	if stage < 1 {
		stage = 1
	}
	p := &Pending{
		ChatID:      chatID,
		UserID:      userID,
		MessageID:   messageID,
		CorrectIdx:  correctIdx,
		ExpiresAt:   expiresAt,
		ThreadID:    threadID,
		EphemeralID: ephemeralID,
		Stage:       stage,
		cancelCh:    make(chan struct{}),
	}
	s.items[k] = p
	return p
}

// IsCaptchaActive сообщает, находится ли пользователь в середине кикоффа
// капчи (до Put) либо у него есть активная ожидающая капча. Используется,
// чтобы решить, удалять ли сообщения, прилетающие от пользователя до того,
// как его успели ограничить.
func (s *Store) IsCaptchaActive(chatID, userID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := capKey{chatID, userID}
	if _, ok := s.items[k]; ok {
		return true
	}
	prep := s.inflight[k]
	return prep != nil && prep.ctx.Err() == nil
}

// TakeForKickoff атомарно передаёт истёкшую стадию её следующей отправке:
// между Take и BeginKickoff не остаётся окна, невидимого для отмены.
func (s *Store) TakeForKickoff(chatID, userID int64, expected *Pending) (*Pending, context.Context, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := capKey{chatID, userID}
	p, ok := s.items[key]
	if !ok || p != expected {
		return nil, nil, false
	}
	if prev := s.inflight[key]; prev != nil {
		if prev.ctx.Err() != nil {
			return nil, nil, false
		}
		prev.cancel()
	}
	delete(s.items, key)
	return p, s.beginKickoff(key), true
}

// Get подсматривает активную капчу, не изымая её. Нужен, чтобы проверить
// идентичность сообщения клика ДО Take: клик по устаревшей клавиатуре не
// должен разрешать живую капчу.
func (s *Store) Get(chatID, userID int64) (*Pending, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.items[capKey{chatID, userID}]
	return p, ok
}

func (s *Store) Take(chatID, userID int64) (*Pending, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := capKey{chatID, userID}
	p, ok := s.items[k]
	if ok {
		delete(s.items, k)
	}
	return p, ok
}

// TakeMatch изымает капчу только при подтверждённой match идентичности
// (клик по той же клавиатуре) — проверка под тем же мьютом, что и изъятие.
// Пара Get+Take этим закрытием не является: между ними могла улечься новая
// капча того же юзера, и клик по старой клавиатуре решил бы её исход.
// Не совпало (или пусто) — store не трогается, ok=false; разницу между
// «устаревший клик» и «капчи нет» вызывающий различит через Get.
func (s *Store) TakeMatch(chatID, userID int64, match func(p *Pending) bool) (*Pending, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := capKey{chatID, userID}
	p, ok := s.items[k]
	if !ok || !match(p) {
		return nil, false
	}
	delete(s.items, k)
	return p, true
}

// TakeChat изымает и возвращает все ожидающие капчи чата. Используется, когда
// бот покидает чат, — вызывающий должен вызвать Cancel у каждого возвращённого
// Pending, чтобы горутины captchaStageLoop завершились, а не стреляли kick/ban в
// чате, где бота больше нет.
func (s *Store) TakeChat(chatID int64) []*Pending {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*Pending
	for k, p := range s.items {
		if k.chatID == chatID {
			delete(s.items, k)
			out = append(out, p)
		}
	}
	return out
}

func (s *Store) HasChat(chatID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.items {
		if key.chatID == chatID {
			return true
		}
	}
	for key := range s.inflight {
		if key.chatID == chatID {
			return true
		}
	}
	return false
}
