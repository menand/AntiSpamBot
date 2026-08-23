package bot

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"

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

// messageHasUserContent — сообщение несёт реальный ввод юзера (текст, подпись
// или вложение), а не сервисное событие (добавление участника, смена
// названия...). Ответом на приветствие считается только контент.
func messageHasUserContent(m *telego.Message) bool {
	return strings.TrimSpace(m.Text) != "" ||
		strings.TrimSpace(m.Caption) != "" ||
		attachmentKindRU(*m) != ""
}

// replyWaitSatisfied снимает ожидание, когда юзер написал сообщение.
// Вызывается из middleware (bot.go) на каждое групповое сообщение с контентом,
// ДО маршрутизации — быстрый in-memory промах для всех, у кого ожидания нет.
func (b *Bot) replyWaitSatisfied(chatID, userID int64) {
	p, ok := b.replies.Take(chatID, userID)
	if !ok {
		return
	}
	p.Cancel()
	// Строку гасим ДО записи пасса: окно между ними микросекундное, но
	// рестарт в нём воскресил бы ожидание для уже ответившего — с киком за
	// «молчание», которого не было.
	if err := b.db.DeletePendingReplyIf(b.runCtx, chatID, userID, p.ExpiresAt); err != nil {
		b.log.Warn("delete pending reply on satisfy", "err", err, "chat", chatID, "user", userID)
	}
	// Единственный победитель гонки фиксирует «прошёл»: капча уже позади,
	// ответ на приветствие — финальная проверка. При однофакторной проверке
	// (reply_check выключен) пасс записан ещё в onSuccess, и здесь Take
	// просто промахивается — лишний пасс не пишется.
	if err := b.db.RecordEvent(b.runCtx, chatID, userID, storage.EventPass, time.Now(), ""); err != nil {
		b.log.Warn("record pass event (reply)", "err", err)
	}
	b.log.Info("reply check passed", "chat", chatID, "user", userID)
}

// cancelReplyWait тихо снимает ожидание (юзер вышел/кикнут/забанен/замьючен) —
// кик за молчание ему уже не грозит. Возвращает true, если ожидание было
// активным: вызывающему надо закрыть воронку статистики событием (left/pass),
// иначе юзер навсегда зависнет в «В процессе» — пасс при reply-check пишется
// только в replyWaitSatisfied, то есть после снятия его уже никто не запишет.
func (b *Bot) cancelReplyWait(chatID, userID int64) bool {
	if p, ok := b.replies.Take(chatID, userID); ok {
		p.Cancel()
		if err := b.db.DeletePendingReplyIf(b.runCtx, chatID, userID, p.ExpiresAt); err != nil {
			b.log.Warn("delete pending reply on cancel", "err", err, "chat", chatID, "user", userID)
		}
		return true
	}
	return false
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

	// Приветствие-якорь сносим: «Добро пожаловать, X!» без X — мусор.
	if msgID, ok, err := b.db.TakeGreetingMsg(ctx, p.ChatID, p.UserID); err == nil && ok {
		if err := b.deleteMessage(ctx, p.ChatID, msgID); err != nil {
			b.log.Debug("delete greeting of silent user", "err", err, "chat", p.ChatID)
		}
	}

	// Та же лестница наказаний, что у капчи (общий punishAttempt): счётчик
	// attempts с гвардией «ошибка счётчика запрещает бан», порог
	// effectiveMaxAttempts, дальше бан. Событие и уведомление — ПОСЛЕ
	// успешного действия (бан, которого не было, не должен попадать в
	// статистику), а pending_replies удаляем только после успеха: при провале
	// рестарт восстановит ожидание и повторит наказание, иначе мьют остался
	// бы навсегда.
	if err := b.punishAttempt(ctx, p.ChatID, p.UserID, storage.ReasonNoReply, "молчание",
		func(event storage.EventKind, _ int) {
			b.notifyModAction(p.ChatID, p.UserID, event, storage.ReasonNoReply)
		}); err != nil {
		b.log.Error("punish silent user", "err", err, "chat", p.ChatID, "user", p.UserID)
		return
	}
	if err := b.db.DeletePendingReplyIf(ctx, p.ChatID, p.UserID, p.ExpiresAt); err != nil {
		// Строка — механизм повтора наказания при рестарте; потерять её
		// молча = повторный кик/бан уже наказанного.
		b.log.Warn("delete pending reply after punish", "err", err, "chat", p.ChatID, "user", p.UserID)
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
	staleChats := map[int64]struct{}{}
	restored := 0
	// Общий бюджет на все liveness-пробы истёкших строк — как в
	// restorePending: массовый простой не должен тянуть рестарт по 10 c за
	// строку. Исчерпался — оставшиеся строки наказываются по грейсу.
	lctx, lcancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer lcancel()
	for _, row := range rows {
		// Чат мог стать необслуживаемым за время простоя (ALLOWED_CHATS,
		// отклонение владельцем, кик бота): таймеры наказаний туда стрелять
		// не должны — стартуют раньше reconcileChats, который вычистил бы
		// строки только позже.
		if !b.chatServiceable(row.ChatID) {
			staleChats[row.ChatID] = struct{}{}
			continue
		}
		expires := row.ExpiresAt
		if expires.Before(now) {
			// Истекла, пока бот лежал: юзер мог уйти офлайн (left-апдейт
			// потерян), и слепой grace-кик записал бы фантомный kick в
			// воронку. Живая проверка: ушедшему — left без наказания,
			// присутствующему — штатный грейс; ошибка API (включая
			// исчерпанный общий бюджет проб) — старое поведение.
			if b.restoredReplyUserDeparted(lctx, row) {
				continue
			}
			// Грейс синхронизируем и в БД: строка после наказания удаляется
			// guard'ом по expires_at, и со старым дедлайном удаление
			// промахнулось бы — кик повторялся бы на каждом рестарте.
			expires = now.Add(1 * time.Second)
			if err := b.db.PutPendingReply(ctx, storage.PendingReply{
				ChatID: row.ChatID, UserID: row.UserID, ExpiresAt: expires,
			}); err != nil {
				b.log.Warn("sync restored reply deadline", "err", err,
					"chat", row.ChatID, "user", row.UserID)
			}
		}
		p := b.replies.Put(row.ChatID, row.UserID, expires)
		b.goSafe("waitReplyTimeout", func() { b.waitReplyTimeout(p) })
		restored++
	}
	for chatID := range staleChats {
		if err := b.db.DeletePendingRepliesChat(ctx, chatID); err != nil {
			b.log.Warn("delete pending replies of unserviceable chat",
				"err", err, "chat", chatID)
		}
	}
	if restored > 0 || len(staleChats) > 0 {
		b.log.Info("restored pending replies", "count", restored,
			"total", len(rows), "skipped_chats", len(staleChats))
	}
	return restored, nil
}

// restoredReplyUserDeparted закрывает истёкшее за простой ожидание юзера,
// который за это время вышел/был убран (лево-апдейт потерян): left вместо
// фантомного кика за молчание. true — исход решён здесь. Ошибка API — false
// (наказываем по грейсу, как раньше).
func (b *Bot) restoredReplyUserDeparted(ctx context.Context, row storage.PendingReply) bool {
	m, err := b.api.GetChatMember(ctx, &telego.GetChatMemberParams{
		ChatID: tu.ID(row.ChatID),
		UserID: row.UserID,
	})
	if err != nil {
		b.log.Debug("restored reply liveness", "err", err,
			"chat", row.ChatID, "user", row.UserID)
		return false
	}
	if s := m.MemberStatus(); s != "left" && s != "kicked" {
		return false
	}
	// Удаляем по исходному дедлайну — строка ещё с ним лежит.
	if err := b.db.DeletePendingReplyIf(ctx, row.ChatID, row.UserID, row.ExpiresAt); err != nil {
		b.log.Warn("delete expired pending reply of departed user", "err", err)
	}
	b.recordLeftEvent(ctx, row.ChatID, row.UserID, "left while offline")
	b.log.Info("expired reply wait closed — user left while offline",
		"chat", row.ChatID, "user", row.UserID)
	return true
}

// replyRequirementLine — строка-требование, дописываемая к приветствию при
// включённом режиме (владелец выбрал «любое сообщение», не строгий reply).
func replyRequirementLine(seconds int) string {
	return fmt.Sprintf("\n\n⏱ Напиши что-нибудь в чат в течение %d %s — иначе придётся зайти заново.",
		seconds, pluralRU(seconds, "секунды", "секунд", "секунд"))
}
