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

// replyPending — одно активное ожидание «ответь на приветствие».
// Тот же паттерн гонок, что у captcha.Pending: единственный победитель
// забирает его через Take, Cancel идемпотентен через sync.Once.
type replyPending struct {
	ChatID    int64
	UserID    int64
	ExpiresAt time.Time
	ThreadID  int // топик форума для повторных отправок якоря; 0 = без топика
	Stage     int // стадия серии напоминаний (1..captchaStages)

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
func (s *replyStore) Put(chatID, userID int64, expiresAt time.Time, threadID, stage int) *replyPending {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := chatUser{chatID, userID}
	if old, ok := s.items[k]; ok {
		old.Cancel()
	}
	if stage < 1 {
		stage = 1
	}
	p := &replyPending{ChatID: chatID, UserID: userID, ExpiresAt: expiresAt,
		ThreadID: threadID, Stage: stage,
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
// режиме взводит серию напоминаний (стадия 1). Само приветствие-якорь шлёт
// onSuccess ДО этого вызова — см. комментарий там про порядок release→arm.
func (b *Bot) maybeArmReplyWait(s storage.ChatSettings, chatID, userID int64, threadID int) {
	if !s.ReplyCheckEnabled {
		return
	}
	interval := b.effectiveStageInterval(s)
	deadline := time.Now().Add(interval)
	p := b.replies.Put(chatID, userID, deadline, threadID, 1)
	if err := b.db.PutPendingReply(b.runCtx, storage.PendingReply{
		ChatID: chatID, UserID: userID, ExpiresAt: deadline,
		Stage: 1, ThreadID: threadID,
	}); err != nil {
		b.log.Warn("persist pending reply", "err", err, "chat", chatID, "user", userID)
	}
	b.goSafe("replyWaitLoop", func() { b.replyWaitLoop(chatID, userID, p) })
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
	// Поздний ответ (стадии 2+): исходное приветствие уже удалено при смене
	// стадии, живой якорь — напоминание. Прошёл — сносим и его: пинать
	// ответившего больше незачем. Ответивший на первой стадии сохраняет
	// обычное приветствие.
	if p.Stage >= 2 {
		b.deleteReplyAnchor(b.runCtx, chatID, userID)
	}
	// Единственный победитель гонки фиксирует «прошёл»: капча уже позади,
	// ответ на приветствие — финальная проверка. При однофакторной проверке
	// (reply_check выключен) пасс записан ещё в onSuccess, и здесь Take
	// просто промахивается — лишний пасс не пишется.
	if err := b.db.RecordEvent(b.runCtx, chatID, userID, storage.EventPass, time.Now(), ""); err != nil {
		b.log.Warn("record pass event (reply)", "err", err)
	}
	b.log.Info("reply check passed", "chat", chatID, "user", userID, "stage", p.Stage)
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

// replyWaitLoop ведёт серию напоминаний «ответь на приветствие» — ту же схему
// из трёх сообщений, что у капчи (captchaStages), на том же интервале:
// якорь стадии истёк → удаляем его, шлём следующий (напоминание, затем
// последнее предупреждение); исчерпана вся серия → штатная лестница наказаний
// за молчание (одна попытка на серию, как у капчи). Решившие раньше срока
// (сообщение юзера, выход, /mute) изымают pending через Take и гасят Cancel'ом.
func (b *Bot) replyWaitLoop(chatID, userID int64, p *replyPending) {
	ctx := b.runCtx
	for {
		timer := time.NewTimer(time.Until(p.ExpiresAt))
		select {
		case <-p.Done():
			timer.Stop()
			return
		case <-ctx.Done():
			// Shutdown: строка pending_replies персистентна (со стадией и
			// дедлайном) — рестарт продолжит серию с этого места.
			timer.Stop()
			return
		case <-timer.C:
		}

		// Единственный победитель дедлайна: identity-проверка отсекает гонку
		// «ответ юзера успел изъять ожидание между таймером и Take».
		taken, ok := b.replies.Take(chatID, userID)
		if !ok || taken != p {
			return
		}

		// Якорь текущей стадии сносим: «напиши что-нибудь» без сообщения,
		// к которому оно прикреплено, — мусор в ленте. (Тот же порядок, что
		// у прежнего waitReplyTimeout: якорь удаляется до наказания.)
		b.deleteReplyAnchor(ctx, chatID, userID)

		// Серия исчерпана: та же лестница наказаний, что у капчи (общий
		// punishAttempt): счётчик attempts с гвардией «ошибка счётчика
		// запрещает бан», порог effectiveMaxAttempts, дальше бан. Cleanup на
		// detached-контексте, чтобы shutdown не оборвал кик на полпути;
		// pending_replies удаляем только после успеха — рестарт повторит
		// наказание иначе.
		if p.Stage >= captchaStages {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := b.punishAttempt(cleanupCtx, chatID, userID, storage.ReasonNoReply, "молчание",
				func(event storage.EventKind, _ int) {
					b.notifyModAction(chatID, userID, event, storage.ReasonNoReply)
				}); err != nil {
				b.log.Error("punish silent user", "err", err, "chat", chatID, "user", userID)
				return
			}
			if err := b.db.DeletePendingReplyIf(cleanupCtx, chatID, userID, p.ExpiresAt); err != nil {
				// Строка — механизм повтора наказания при рестарте; потерять её
				// молча = повторный кик/бан уже наказанного.
				b.log.Warn("delete pending reply after punish", "err", err, "chat", chatID, "user", userID)
			}
			return
		}

		// Следующая стадия: настройки перечитываем, чтобы правки интервала из
		// меню подхватывались следующими стадиями (редкое событие — минуты).
		s := b.chatSettings(ctx, chatID)
		if !b.sendGreetingAnchor(ctx, s, chatID, userID, p.ThreadID, p.Stage+1) {
			// Напоминание не ушло даже после ретраев (429-шторм, сеть), а
			// предыдущий якорь мы только что удалили: требование «напиши
			// что-нибудь» юзер выполнить не может — он его не видит. Снимаем
			// ожидание и закрываем воронку пассом: невозможность ответить —
			// не вина юзера (прецедент onSuccess / замьюченного /mute).
			if err := b.db.DeletePendingReplyIf(ctx, chatID, userID, p.ExpiresAt); err != nil {
				b.log.Warn("delete pending reply on disarmed wait", "err", err, "chat", chatID, "user", userID)
			}
			if err := b.db.RecordEvent(ctx, chatID, userID, storage.EventPass, time.Now(), ""); err != nil {
				b.log.Warn("record pass event (disarmed reply wait)", "err", err)
			}
			b.log.Warn("reply wait disarmed — reminder send failed",
				"chat", chatID, "user", userID, "stage", p.Stage+1)
			return
		}
		expires := time.Now().Add(b.effectiveStageInterval(s))
		next := b.replies.Put(chatID, userID, expires, p.ThreadID, p.Stage+1)
		if err := b.db.PutPendingReply(ctx, storage.PendingReply{
			ChatID: chatID, UserID: userID, ExpiresAt: expires,
			Stage: next.Stage, ThreadID: next.ThreadID,
		}); err != nil {
			b.log.Warn("persist pending reply (next stage)", "err", err, "chat", chatID, "user", userID)
		}
		p = next
	}
}

// deleteReplyAnchor удаляет текущий якорь-приветствие по записи из таблицы
// greetings (Take читает и стирает строку). Отсутствие записи (уже удалён /
// не отправлялся) — норма.
func (b *Bot) deleteReplyAnchor(ctx context.Context, chatID, userID int64) {
	if msgID, ok, err := b.db.TakeGreetingMsg(ctx, chatID, userID); err == nil && ok {
		if err := b.deleteMessage(ctx, chatID, msgID); err != nil {
			b.log.Debug("delete reply anchor", "err", err, "chat", chatID, "msg", msgID)
		}
	} else if err != nil {
		b.log.Warn("take reply anchor", "err", err, "chat", chatID, "user", userID)
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
		stage := row.Stage
		if expires.Before(now) {
			// Истекла, пока бот лежал: юзер мог уйти офлайн (left-апдейт
			// потерян), и слепой grace-кик записал бы фантомный kick в
			// воронку. Живая проверка: ушедшему — left без наказания,
			// присутствующему — штатный грейс; ошибка API (включая
			// исчерпанный общий бюджет проб) — старое поведение.
			if b.restoredReplyUserDeparted(lctx, row) {
				continue
			}
			// Простой съел серию: рестарт карает по грейсу, как прежний
			// одиночный таймаут. Кламп к финальной стадии + синхронизация в
			// БД (guard удаления идёт по expires_at) заставляют цикл наказать,
			// а не слать очередное напоминание.
			expires = now.Add(1 * time.Second)
			stage = captchaStages
			if err := b.db.PutPendingReply(ctx, storage.PendingReply{
				ChatID: row.ChatID, UserID: row.UserID, ExpiresAt: expires,
				Stage: stage, ThreadID: row.ThreadID,
			}); err != nil {
				b.log.Warn("sync restored reply deadline", "err", err,
					"chat", row.ChatID, "user", row.UserID)
			}
		}
		p := b.replies.Put(row.ChatID, row.UserID, expires, row.ThreadID, stage)
		b.goSafe("replyWaitLoop", func() { b.replyWaitLoop(row.ChatID, row.UserID, p) })
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

// replyRequirementLine — строка-требование для стадии stage серии (владелец
// выбрал «любое сообщение», не строгий reply). Стадия 1 — нейтральная, 2 —
// напоминание, 3 — последнее предупреждение.
func replyRequirementLine(stage, minutes int) string {
	mins := pluralRU(minutes, "минуту", "минуты", "минут")
	switch stage {
	case 2:
		return fmt.Sprintf("\n\n⏳ Напоминание: напиши что-нибудь в чат в течение %d %s — иначе придётся зайти заново.",
			minutes, mins)
	case 3:
		return fmt.Sprintf("\n\n⚠️ ПОСЛЕДНЕЕ ПРЕДУПРЕЖДЕНИЕ: напиши что-нибудь в чат в течение %d %s — иначе тебя исключат из чата.",
			minutes, mins)
	default:
		return fmt.Sprintf("\n\n⏱ Напиши что-нибудь в чат в течение %d %s — иначе придётся зайти заново.",
			minutes, mins)
	}
}
