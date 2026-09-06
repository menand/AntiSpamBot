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
	ChatID        int64
	UserID        int64
	ExpiresAt     time.Time
	ThreadID      int // топик форума для повторных отправок якоря; 0 = без топика
	Stage         int // стадия серии напоминаний (1..captchaStages)
	GreetingMsgID int // message_id текущего якоря-приветствия (для stale-guard кнопки «Впустить»)

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
func (s *replyStore) Put(chatID, userID int64, expiresAt time.Time, threadID, stage, greetingMsgID int) *replyPending {
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
		ThreadID: threadID, Stage: stage, GreetingMsgID: greetingMsgID,
		cancelCh: make(chan struct{})}
	s.items[k] = p
	return p
}

// Get подсматривает активное ожидание, не изымая его — identity-проверка
// дедлайна цикла без передачи права разбираться с ним (забрать может только
// Take: ответ юзера, его выход, финальная кара).
func (s *replyStore) Get(chatID, userID int64) (*replyPending, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.items[chatUser{chatID, userID}]
	return p, ok
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

// SetGreetingMsgID обновляет message_id текущего якоря-приветствия в
// активном ожидании. Вызывается после sendGreetingAnchor, когда pending
// уже взведён (arm стоит ДО отправки приветствия — см. onSuccess).
func (s *replyStore) SetGreetingMsgID(chatID, userID int64, msgID int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.items[chatUser{chatID, userID}]; ok {
		p.GreetingMsgID = msgID
	}
}

// Replace — compare-and-swap перехода стадии: под тем же мьютексом меняет
// старый pending на следующий ТОЛЬКО если текущий всё ещё old. Закрывает окно
// между раздельными Take и Put: ответ юзера, проскочивший между ними, раньше
// промахивался бы мимо store и не записывал свой pass — серия продолжала бы
// наказывать молчанием написавшего. false = ожидание уже разрешил другой
// (изъятие состоялось), перевзводить нечего.
func (s *replyStore) Replace(chatID, userID int64, old *replyPending,
	expiresAt time.Time, threadID, stage, greetingMsgID int) (*replyPending, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := chatUser{chatID, userID}
	if cur, ok := s.items[k]; !ok || cur != old {
		return nil, false
	}
	if stage < 1 {
		stage = 1
	}
	next := &replyPending{ChatID: chatID, UserID: userID, ExpiresAt: expiresAt,
		ThreadID: threadID, Stage: stage, GreetingMsgID: greetingMsgID,
		cancelCh: make(chan struct{})}
	s.items[k] = next
	return next, true
}

// TakeMatch изымает ожидание только при подтверждённой match идентичности
// (клик по той же клавиатуре) — проверка под тем же мьютексом, что и
// изъятие. Не совпало (или пусто) — store не трогается, ok=false.
func (s *replyStore) TakeMatch(chatID, userID int64, match func(p *replyPending) bool) (*replyPending, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := chatUser{chatID, userID}
	p, ok := s.items[k]
	if !ok || !match(p) {
		return nil, false
	}
	delete(s.items, k)
	return p, true
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
// Возвращает true, если ожидание реально взведено: false (режим выключен
// или провал персистентности) говорит onSuccess не рисовать в приветствии
// требование, которое никто не проверяет.
func (b *Bot) maybeArmReplyWait(s storage.ChatSettings, chatID, userID int64, threadID int) bool {
	if !s.ReplyCheckEnabled {
		return false
	}
	interval := b.effectiveStageInterval(s)
	deadline := time.Now().Add(interval)
	p := b.replies.Put(chatID, userID, deadline, threadID, 1, 0)
	if err := b.db.PutPendingReply(b.runCtx, storage.PendingReply{
		ChatID: chatID, UserID: userID, ExpiresAt: deadline,
		Stage: 1, ThreadID: threadID,
	}); err != nil {
		// Прецедент капчи (её pre-persist abort): сбой БД коррелирует с
		// близким рестартом, а строка pending_replies — единственный механизм
		// повтора наказания после него. Молчаливая деградация «живём по
		// памяти» означала бы: рестарт тихо терял ожидание, а живой процесс
		// карал за молчание, которого после рестарта никто бы не повторил.
		// Снимаем ожидание с компенсирующим пассом (якорь-то ещё не отправлен:
		// arming стоит ДО приветствия — юзер останется с обычным welcome),
		// серию не запускаем.
		b.log.Error("persist pending reply — disarming wait", "err", err,
			"chat", chatID, "user", userID)
		if armed, ok := b.replies.Take(chatID, userID); ok && armed == p {
			armed.Cancel()
			if err := b.db.RecordEvent(b.runCtx, chatID, userID, storage.EventPass, time.Now(), ""); err != nil {
				b.log.Warn("record compensating pass (reply arm failed)", "err", err,
					"chat", chatID, "user", userID)
			}
		}
		return false
	}
	b.goSafe("replyWaitLoop", func() { b.replyWaitLoop(chatID, userID, p) })
	return true
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
	// «молчание», которого не было. Guard по стадии: переход к следующей
	// персистится ДО отправки напоминания, так что ответ, прилетевший в окно
	// пересылки, стирает СВОЮ строку и не трогает уже записанную следующую.
	if err := b.db.DeletePendingReplyIf(b.runCtx, chatID, userID, p.Stage); err != nil {
		b.log.Warn("delete pending reply on satisfy", "err", err, "chat", chatID, "user", userID)
	}
	// Стадия 1: приветствие остаётся в чате, но кнопка «Впустить» больше
	// неактуальна — снимаем клавиатуру. Строку greetings НЕ удаляем: она
	// нужна cleanupTargetTraces для удаления приветствия при спам-бане.
	if p.Stage == 1 {
		if msgID, ok, err := b.db.GetGreetingMsg(b.runCtx, chatID, userID); err == nil && ok {
			if _, err := b.api.EditMessageReplyMarkup(b.runCtx, &telego.EditMessageReplyMarkupParams{
				ChatID:    tu.ID(chatID),
				MessageID: msgID,
			}); err != nil {
				b.log.Debug("remove reply-approve button", "err", err, "chat", chatID, "msg", msgID)
			}
		}
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
		if err := b.db.DeletePendingReplyIf(b.runCtx, chatID, userID, p.Stage); err != nil {
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

		// Identity-проверка БЕЗ изъятия: забрать ожидание вправе только
		// настоящий победитель (ответ юзера, его выход, финальная кара ниже).
		if cur, ok := b.replies.Get(chatID, userID); !ok || cur != p {
			return
		}

		// Серия исчерпана: та же лестница наказаний, что у капчи (общий
		// punishAttempt): счётчик attempts с гвардией «ошибка счётчика
		// запрещает бан», порог effectiveMaxAttempts, дальше бан. Cleanup на
		// detached-контексте, чтобы shutdown не оборвал кик на полпути;
		// pending_replies удаляем только после успеха — рестарт повторит
		// наказание иначе.
		if p.Stage >= captchaStages {
			taken, ok := b.replies.Take(chatID, userID)
			if !ok || taken != p {
				return
			}
			b.deleteReplyAnchor(ctx, chatID, userID)
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel() //nolint:gocritic // deferInLoop: intentional — each iteration has its own context for cleanup
			if err := b.punishAttempt(cleanupCtx, chatID, userID, storage.ReasonNoReply, "молчание",
				func(event storage.EventKind, _ int) {
					b.notifyModAction(chatID, userID, event, storage.ReasonNoReply)
				}); err != nil {
				b.log.Error("punish silent user", "err", err, "chat", chatID, "user", userID)
				return
			}
			if err := b.db.DeletePendingReplyIf(cleanupCtx, chatID, userID, p.Stage); err != nil {
				// Строка — механизм повтора наказания при рестарте; потерять её
				// молча = повторный кик/бан уже наказанного.
				b.log.Warn("delete pending reply after punish", "err", err, "chat", chatID, "user", userID)
			}
			return
		}

		// Промежуточная стадия. Порядок принципиален:
		// 1) переход персистится ДО исполнения — иначе крэш в окне пересылки
		//    оставил бы в БД истёкшую стадию, и рестарт клампил бы серию к
		//    финалу с грейс-киком юзера, у которого оставались стадии;
		// 2) ожидание изымается ПОСЛЕ отправки напоминания: ответ юзера,
		//    прилетевший в секунды ретраев отправки, обязан застать pending
		//    в store (replyWaitSatisfied), иначе серия дожала бы написавшего.
		settings := b.chatSettings(ctx, chatID)
		nextExpires := time.Now().Add(b.effectiveStageInterval(settings))
		if err := b.db.PutPendingReply(ctx, storage.PendingReply{
			ChatID: chatID, UserID: userID, ExpiresAt: nextExpires,
			Stage: p.Stage + 1, ThreadID: p.ThreadID,
		}); err != nil {
			b.log.Warn("persist reply stage transition", "err", err,
				"chat", chatID, "user", userID, "stage", p.Stage+1)
		}

		// Якорь текущей стадии сносим: «напиши что-нибудь» без сообщения,
		// к которому оно прикреплено, — мусор в ленте.
		b.deleteReplyAnchor(ctx, chatID, userID)

		msgID, sent := b.sendGreetingAnchor(ctx, settings, chatID, userID, p.ThreadID, p.Stage+1)
		if !sent {
			// Напоминание не ушло даже после ретраев (429-шторм, сеть), а
			// предыдущий якорь мы только что удалили: требование «напиши
			// что-нибудь» юзер выполнить не может — он его не видит. Снимаем
			// ожидание: невозможность ответить — не вина юзера (прецедент
			// onSuccess / замьюченного /mute).
			taken, took := b.replies.Take(chatID, userID)
			// Строку стадии N+1 гасим всегда: у satisfier'а и cancelReplyWait
			// гвард идёт по ИХ стадии (p.Stage), предзаписанную следующую
			// она не достаёт — без этой очистки рестарт воскресил бы фантомную
			// стадию с киком за «молчание».
			if err := b.db.DeletePendingReplyIf(ctx, chatID, userID, p.Stage+1); err != nil {
				b.log.Warn("delete pending reply on disarmed wait", "err", err, "chat", chatID, "user", userID)
			}
			if !took || taken != p {
				// Пока напоминание уходило в ретраях, ожидание уже разрешил
				// другой (ответ юзера, выход, /mute) — терминальное событие
				// записал он. Проигравший гонку — no-op по событиям: второй
				// пасс поверх чужого исхода ломал бы воронку.
				b.log.Info("reply wait resolved by winner — reminder send failed",
					"chat", chatID, "user", userID, "stage", p.Stage+1)
				return
			}
			taken.Cancel()
			// Компенсирующий пасс — только когда серия была ещё у нас и
			// другого исхода у неё не появилось.
			if err := b.db.RecordEvent(ctx, chatID, userID, storage.EventPass, time.Now(), ""); err != nil {
				b.log.Warn("record pass event (disarmed reply wait)", "err", err)
			}
			b.log.Warn("reply wait disarmed — reminder send failed",
				"chat", chatID, "user", userID, "stage", p.Stage+1)
			return
		}

		next, advanced := b.replies.Replace(chatID, userID, p, nextExpires, p.ThreadID, p.Stage+1, msgID)
		if !advanced {
			// Юзер ответил (или ожидание снято), пока напоминание уходило:
			// пасс уже записан satisfier'ом, а только что отправленное
			// напоминание — лишнее, сносим. CAS не перевзводил стадию, так
			// что предзаписанную строку N+1 чистим здесь — satisfier
			// гасил СВОЮ (старую) стадию.
			if msgID != 0 {
				if derr := b.deleteMessage(ctx, chatID, msgID); derr != nil {
					b.log.Debug("delete reminder of already-satisfied wait",
						"err", derr, "chat", chatID, "msg", msgID)
				}
			}
			if err := b.db.DeletePendingReplyIf(ctx, chatID, userID, p.Stage+1); err != nil {
				// Выжившая строка N+1 воскресила бы фантомное ожидание при
				// рестарте — с грейс-киком ответившего юзера. Не глушим.
				b.log.Warn("delete transitioned row of satisfied wait",
					"err", err, "chat", chatID, "user", userID)
			}
			return
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
			// БД (guard удаления идёт по СТАДИИ — кламп обязан доехать до
			// строки) заставляют цикл наказать, а не слать очередное
			// напоминание.
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
		p := b.replies.Put(row.ChatID, row.UserID, expires, row.ThreadID, stage, 0)
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
	// Удаляем по исходной стадии — строка ещё с ней лежит.
	if err := b.db.DeletePendingReplyIf(ctx, row.ChatID, row.UserID, row.Stage); err != nil {
		b.log.Warn("delete expired pending reply of departed user", "err", err)
	}
	b.recordLeftEvent(ctx, row.ChatID, row.UserID, "left while offline")
	b.log.Info("expired reply wait closed — user left while offline",
		"chat", row.ChatID, "user", row.UserID)
	return true
}

// replyRequirementLine — строка-требование для стадии stage серии (владелец
// выбрал «любое сообщение», не строгий reply). Стадия 1 — нейтральная, 2 —
// напоминание, 3 — последнее предупреждение. minutesStr — интервал в родительном
// падеже («в течение 2 минут»), готовит minutesGen.
func replyRequirementLine(stage int, minutesStr string) string {
	switch stage {
	case 2:
		return fmt.Sprintf("\n\n⏳ Напоминание: напиши что-нибудь в чат в течение %s — иначе придётся зайти заново.",
			minutesStr)
	case 3:
		return fmt.Sprintf("\n\n⚠️ ПОСЛЕДНЕЕ ПРЕДУПРЕЖДЕНИЕ: напиши что-нибудь в чат в течение %s — иначе тебя исключат из чата.",
			minutesStr)
	default:
		return fmt.Sprintf("\n\n⏱ Напиши что-нибудь в чат в течение %s — иначе придётся зайти заново.",
			minutesStr)
	}
}
