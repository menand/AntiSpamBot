package bot

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/menand/AntiSpamBot/internal/storage"
)

// defaultReplyCheckSeconds — сколько ждать первого сообщения новичка при
// включённом режиме «требовать ответа» (переопределяется per chat).
const defaultReplyCheckSeconds = 60

// effectiveReplyCheckSeconds резолвит срок ожидания ответа.
func effectiveReplyCheckSeconds(s storage.ChatSettings) int {
	if s.ReplyCheckSeconds.Valid && s.ReplyCheckSeconds.Int64 > 0 {
		return int(s.ReplyCheckSeconds.Int64)
	}
	return defaultReplyCheckSeconds
}

// replyPending — одно активное ожидание «ответь на приветствие».
// Тот же паттерн гонок, что у captcha.Pending: единственный победитель
// забирает его через Take, Cancel идемпотентен через sync.Once.
type replyPending struct {
	ChatID    int64
	UserID    int64
	ExpiresAt time.Time

	cancelOnce sync.Once
	cancelCh   chan struct{}
}

func (p *replyPending) Cancel()               { p.cancelOnce.Do(func() { close(p.cancelCh) }) }
func (p *replyPending) Done() <-chan struct{} { return p.cancelCh }

// replyStore — in-memory карта активных ожиданий (persist — pending_replies).
type replyStore struct {
	mu    sync.Mutex
	items map[chatUser]*replyPending
}

func newReplyStore() *replyStore {
	return &replyStore{items: make(map[chatUser]*replyPending)}
}

// Put взводит ожидание; уже висящее для той же пары отменяется (перезаход).
func (s *replyStore) Put(chatID, userID int64, expiresAt time.Time) *replyPending {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := chatUser{chatID, userID}
	if old, ok := s.items[k]; ok {
		old.Cancel()
	}
	p := &replyPending{ChatID: chatID, UserID: userID, ExpiresAt: expiresAt,
		cancelCh: make(chan struct{})}
	s.items[k] = p
	return p
}

// Take атомарно забирает ожидание: победитель (сообщение юзера, его выход,
// таймаут) разбирается с ним, проигравшие гонку получают ok=false.
func (s *replyStore) Take(chatID, userID int64) (*replyPending, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := chatUser{chatID, userID}
	p, ok := s.items[k]
	if ok {
		delete(s.items, k)
	}
	return p, ok
}

// TakeChat забирает все ожидания чата (бот покинул чат).
func (s *replyStore) TakeChat(chatID int64) []*replyPending {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*replyPending
	for k, p := range s.items {
		if k.chatID == chatID {
			delete(s.items, k)
			out = append(out, p)
		}
	}
	return out
}

// maybeArmReplyWait — хук после приветствия в onSuccess: при включённом
// режиме взводит ожидание первого сообщения новичка.
func (b *Bot) maybeArmReplyWait(s storage.ChatSettings, chatID, userID int64) {
	if !s.ReplyCheckEnabled {
		return
	}
	deadline := time.Now().Add(time.Duration(effectiveReplyCheckSeconds(s)) * time.Second)
	p := b.replies.Put(chatID, userID, deadline)
	if err := b.db.PutPendingReply(b.runCtx, storage.PendingReply{
		ChatID: chatID, UserID: userID, ExpiresAt: deadline,
	}); err != nil {
		b.log.Warn("persist pending reply", "err", err, "chat", chatID, "user", userID)
	}
	b.goSafe("waitReplyTimeout", func() { b.waitReplyTimeout(p) })
}

// replyWaitSatisfied снимает ожидание, когда юзер написал сообщение.
// Вызывается из handleGroupMessage на КАЖДОЕ сообщение — быстрый in-memory
// промах для всех, у кого ожидания нет.
func (b *Bot) replyWaitSatisfied(chatID, userID int64) {
	p, ok := b.replies.Take(chatID, userID)
	if !ok {
		return
	}
	p.Cancel()
	_ = b.db.DeletePendingReply(b.runCtx, chatID, userID)
	b.log.Info("reply check passed", "chat", chatID, "user", userID)
}

// cancelReplyWait тихо снимает ожидание (юзер вышел/кикнут/забанен) — кик за
// молчание ему уже не грозит, событий не пишем.
func (b *Bot) cancelReplyWait(chatID, userID int64) {
	if p, ok := b.replies.Take(chatID, userID); ok {
		p.Cancel()
		_ = b.db.DeletePendingReply(b.runCtx, chatID, userID)
	}
}

// waitReplyTimeout ждёт дедлайн ожидания. Молчание = провал второго шага
// проверки: та же лестница наказаний, что у капчи, — общий счётчик attempts,
// кик до effectiveMaxAttempts, дальше бан. Cleanup на detached-контексте,
// чтобы shutdown не оборвал кик на полпути (образец waitTimeout).
func (b *Bot) waitReplyTimeout(p *replyPending) {
	timer := time.NewTimer(time.Until(p.ExpiresAt))
	defer timer.Stop()

	select {
	case <-p.Done():
		return
	case <-b.runCtx.Done():
		return
	case <-timer.C:
	}

	existing, ok := b.replies.Take(p.ChatID, p.UserID)
	if !ok || existing != p {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = b.db.DeletePendingReply(ctx, p.ChatID, p.UserID)

	// Приветствие-якорь сносим: «Добро пожаловать, X!» без X — мусор.
	if msgID, ok, err := b.db.TakeGreetingMsg(ctx, p.ChatID, p.UserID); err == nil && ok {
		if err := b.deleteMessage(ctx, p.ChatID, msgID); err != nil {
			b.log.Debug("delete greeting of silent user", "err", err, "chat", p.ChatID)
		}
	}

	count, err := b.db.IncrementAttempt(ctx, p.ChatID, p.UserID, attemptsTTL)
	if err != nil {
		b.log.Warn("increment attempt (reply)", "err", err)
		count = 1 // считаем первой попыткой и едем дальше
	}
	if count >= b.effectiveMaxAttempts(b.chatSettings(ctx, p.ChatID)) {
		b.log.Info("banning silent user", "chat", p.ChatID, "user", p.UserID, "attempts", count)
		_ = b.db.RecordEvent(ctx, p.ChatID, p.UserID, storage.EventBan, time.Now(), storage.ReasonNoReply)
		b.notifyModAction(p.ChatID, p.UserID, storage.EventBan, storage.ReasonNoReply)
		if err := b.ban(ctx, p.ChatID, p.UserID); err != nil {
			b.log.Error("ban silent user", "err", err, "chat", p.ChatID, "user", p.UserID)
		}
		return
	}
	b.log.Info("kicking silent user", "chat", p.ChatID, "user", p.UserID, "attempts", count)
	_ = b.db.RecordEvent(ctx, p.ChatID, p.UserID, storage.EventKick, time.Now(), storage.ReasonNoReply)
	b.notifyModAction(p.ChatID, p.UserID, storage.EventKick, storage.ReasonNoReply)
	if err := b.kick(ctx, p.ChatID, p.UserID); err != nil {
		b.log.Error("kick silent user", "err", err, "chat", p.ChatID, "user", p.UserID)
	}
}

// restorePendingReplies поднимает ожидания ответа после рестарта с исходными
// дедлайнами; истёкшие за время простоя получают грейс в 1 секунду.
func (b *Bot) restorePendingReplies(ctx context.Context) (int, error) {
	rows, err := b.db.LoadAllPendingReplies(ctx)
	if err != nil {
		return 0, err
	}
	now := time.Now()
	for _, row := range rows {
		expires := row.ExpiresAt
		if expires.Before(now) {
			expires = now.Add(1 * time.Second)
		}
		p := b.replies.Put(row.ChatID, row.UserID, expires)
		b.goSafe("waitReplyTimeout", func() { b.waitReplyTimeout(p) })
	}
	if len(rows) > 0 {
		b.log.Info("restored pending replies", "count", len(rows))
	}
	return len(rows), nil
}

// replyRequirementLine — строка-требование, дописываемая к приветствию при
// включённом режиме (владелец выбрал «любое сообщение», не строгий reply).
func replyRequirementLine(seconds int) string {
	return fmt.Sprintf("\n\n⏱ Напиши что-нибудь в чат в течение %d %s — иначе придётся зайти заново.",
		seconds, pluralRU(seconds, "секунды", "секунд", "секунд"))
}
