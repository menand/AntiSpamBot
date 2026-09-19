package bot

import (
	"context"
	"regexp"
	"sync"
	"time"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"

	"github.com/menand/AntiSpamBot/internal/storage"
)

// Порог эскалации цензора: N нарушений (ссылки/форварды) за короткое окно —
// рид-онли мьют на час вместо вечной борьбы делитов с флудом. Счётчик
// in-memory — рестарт сбрасывает, но и серверный текст-рестрикт пережил бы
// его всё равно; эскалация и так вторична (делит убирал нарушение и до неё).
const (
	censorEscalateAfter  = 5
	censorEscalateWindow = 10 * time.Minute
	censorEscalateMute   = time.Hour
	// quarantineReleaseMaxPasses — сколько итераций «список→отпуск» догоняет
	// гонку капча-проход/выключение тоггла; с re-check'ом арма обычно хватает
	// одного прохода.
	quarantineReleaseMaxPasses = 3
)

// quarantineEntry — активный карантин (chat, user) в памяти: зеркало таблицы
// chat_quarantine для горячего пути цензора (каждое сообщение новичка).
type quarantineEntry struct {
	Until  time.Time // Telegram снимет рестрикт сам, серверно
	Warned bool      // эфемерное пояснение уже показано
	// Счётчик нарушений для эскалации: in-memory, окно-привязка ленивая
	// (bump сбрасывает счётчик, если последнее нарушение старше окна).
	Violations    int
	LastViolation time.Time
}

// quarantineStore — in-memory зеркало активных карантинов. Авторитет — БД
// (рестарт-безопасность); на старте зеркало сидится заново. Истёкшие записи
// выпадают лениво при чтении — таймерный свип памяти не нужен.
type quarantineStore struct {
	mu    sync.Mutex
	items map[chatUser]*quarantineEntry
}

func newQuarantineStore() *quarantineStore {
	return &quarantineStore{items: make(map[chatUser]*quarantineEntry)}
}

func (s *quarantineStore) Set(chatID, userID int64, until time.Time, warned bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[chatUser{chatID, userID}] = &quarantineEntry{Until: until, Warned: warned}
}

// Get возвращает активный карантин, лениво выбрасывая истёкшие. ok=false —
// карантина нет (не начинался, кончился или уже снят).
func (s *quarantineStore) Get(chatID, userID int64) (quarantineEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := chatUser{chatID, userID}
	e, ok := s.items[k]
	if !ok {
		return quarantineEntry{}, false
	}
	if time.Now().After(e.Until) {
		delete(s.items, k)
		return quarantineEntry{}, false
	}
	return *e, true
}

// ClaimWarned атомарно взводит флаг разового пояснения. Возвращает true
// ТОЛЬКО переключившему false→true: параллельные нарушения в один момент
// оба видели бы снапшот Warned=false и оба ушли бы в SendMessage — этим
// claim'ом победитель ровно один, проигравшие молчат.
func (s *quarantineStore) ClaimWarned(chatID, userID int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.items[chatUser{chatID, userID}]
	if !ok || e.Warned {
		return false
	}
	e.Warned = true
	return true
}

// ClearWarned откатывает claim: пояснение не доставлено (отсылка упала) — юзер
// заслуживает второго шанса увидеть его, а не тихую цензуру до конца карантина.
func (s *quarantineStore) ClearWarned(chatID, userID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.items[chatUser{chatID, userID}]; ok {
		e.Warned = false
	}
}

// BumpViolations инкрементит счётчик нарушений и возвращает новое значение.
// Счётчик сбрасывается, если предыдущее нарушение старше окна эскалации
// (долгий тихий юзер не должен уткнуться в мьют на пятой ссылке за день).
func (s *quarantineStore) BumpViolations(chatID, userID int64) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.items[chatUser{chatID, userID}]
	if !ok {
		return 0
	}
	now := time.Now()
	if now.Sub(e.LastViolation) > censorEscalateWindow {
		e.Violations = 0
	}
	e.Violations++
	e.LastViolation = now
	return e.Violations
}

// Rekey переносит зеркало при миграции basic-группа → супергруппа: Telegram
// переносит рестрикт вместе с участником в новый chat_id, и цензор должен
// смотреть туда же. Истёкшие за время миграции не переезжают (отпускать их —
// работа Telegram). Вызывается только ПОСЛЕ успешного MigrateChat.
func (s *quarantineStore) Rekey(oldID, newID int64) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for k, e := range s.items {
		if k.chatID != oldID {
			continue
		}
		delete(s.items, k)
		if time.Now().Before(e.Until) {
			s.items[chatUser{newID, k.userID}] = e
			n++
		}
	}
	return n
}

func (s *quarantineStore) Remove(chatID, userID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.items, chatUser{chatID, userID})
}

func (s *quarantineStore) RemoveChat(chatID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.items {
		if k.chatID == chatID {
			delete(s.items, k)
		}
	}
}

// quarantineLinkRe — ссылки, которые карантин не пропускает в тексте: явный
// протокол, www без протокола, t.me-ссылки и deep-линки tg://. Сугубо
// быстрая строковая подстраховка — entity-проверка (hasLinkEntity) ловит и
// схемы, которых этот regex не знает.
var quarantineLinkRe = regexp.MustCompile(`(?i)(https?://|\bwww\.|t\.me/|tg://)`)

// hasLinkEntity — ссылка, спрятанная в entity (Telegram всегда размечает
// кликабельные URL в тексте и подписях): text_link («невинный текст →
// ссылка-обманка», URL лежит в поле URL) или url («текст сам по себе
// ссылка» — поля URL не несёт). Любая из них — нарушение независимо от
// схемы (magnet:, tg://, голый IP), которую regex по строке не видит.
func hasLinkEntity(entities []telego.MessageEntity) bool {
	for _, e := range entities {
		switch e.Type {
		case telego.EntityTypeTextLink:
			if e.URL != "" {
				return true
			}
		case telego.EntityTypeURL:
			return true
		}
	}
	return false
}

// quarantineViolates — сообщение карантинного юзера, которое цензор удаляет:
// форвард любого вида (серверная грануляция прав текст-форварды не
// блокирует), текст/подпись со ссылкой (regex + entity-проверка) и любое
// вложение (подстраховка на случай, если гранулярные права по какой-то
// причине не перекрыли). Чистый текст — разрешён и проходит в
// reply-wait-логику (может быть «ответом на приветствие»).
func quarantineViolates(m *telego.Message) bool {
	if m.ForwardOrigin != nil {
		return true
	}
	if quarantineLinkRe.MatchString(m.Text) || quarantineLinkRe.MatchString(m.Caption) {
		return true
	}
	// Entities И CaptionEntities: для медиа-подписи Telegram кладёт entity в
	// отдельное поле — ссылка в подписи не должна пройти только потому, что
	// никто не посмотрел в него (та же скрытность, что лечит
	// ИИ-перепроверка правок).
	if hasLinkEntity(m.Entities) || hasLinkEntity(m.CaptionEntities) {
		return true
	}
	return attachmentKindRU(*m) != ""
}

// restoreQuarantine сидит зеркало цензора из БД после рестарта: рестрикт на
// Telegram переживает рестарт серверно, а без сида цензор не знал бы, кого
// ограничивать и кого предупредить. Строки чатов, где карантин сейчас
// ВЫКЛЮЧЕН, не сидятся: крэш в окне между тогглом и отпуском всех юзеров
// (quarantineReleaseChat) иначе пережил бы рестрикт до старого until_date —
// досрочный release здесь же.
func (b *Bot) restoreQuarantine(ctx context.Context) {
	rows, err := b.db.RestoreQuarantines(ctx, time.Now().Unix())
	if err != nil {
		b.log.Error("restore quarantines", "err", err)
		return
	}
	var drain []storage.QuarantineRow
	for _, r := range rows {
		s, err := b.db.GetChatSettings(ctx, r.ChatID)
		if err != nil {
			// Настройки нечитаемы — дрейн небезопасен: молчаливый сброс
			// тоггла высвободил бы рестрикт чата, где карантин НА САМОМ
			// ДЕЛЕ включён (de-факто выключение фичи DB-ошибкой). Fail-closed:
			// сидим зеркало, страховой reconcileQuarantines позже разберётся,
			// если тоггл действительно выключен.
			b.log.Warn("restore quarantine: settings unreadable, seeding",
				"err", err, "chat", r.ChatID)
			b.quar.Set(r.ChatID, r.UserID, r.UntilAt, r.Warned)
			continue
		}
		if s.QuarantineEnabled {
			b.quar.Set(r.ChatID, r.UserID, r.UntilAt, r.Warned)
		} else {
			drain = append(drain, r)
		}
	}
	// Дрейн выключенных чатов — на detached-бюджете, как restorePending:
	// старт может идти через охлаждающийся API, а каждый отпуск — это
	// getChat + restrict с лестницей ретраев; без общего лимита массовый
	// джойн перед падением тянул бы рестарт по 10 c за строку.
	dctx, dcancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer dcancel()
	for _, r := range drain {
		b.quar.Remove(r.ChatID, r.UserID)
		if err := b.db.DeleteQuarantine(dctx, r.ChatID, r.UserID); err != nil {
			b.log.Warn("restore: drop disabled-chat quarantine", "err", err,
				"chat", r.ChatID, "user", r.UserID)
		}
		// Release — даже если удаление строки упало: серверный рестрикт
		// держится до until_date, и пропущенный release оставил бы «только
		// текст» выключенному чату (строку потом добьёт reconcile-свип).
		if err := b.release(dctx, r.ChatID, r.UserID); err != nil {
			b.log.Warn("restore: release disabled-chat quarantine", "err", err,
				"chat", r.ChatID, "user", r.UserID)
		}
	}
}

// armQuarantine ставит карантинную строку после успешного рестрикта (вызывает
// onSuccess): зеркало первым (горячий путь цензора), БД — рестарт-безопасность.
// Провал записи в БД не откатывает рестрикт — Telegram держит его и так.
func (b *Bot) armQuarantine(ctx context.Context, chatID, userID int64, until time.Time) {
	b.quar.Set(chatID, userID, until, false)
	if err := b.db.PutQuarantine(ctx, chatID, userID, until, false); err != nil {
		b.log.Warn("persist quarantine", "err", err, "chat", chatID, "user", userID)
	}
}

// censorQuarantineMessage удаляет нарушение карантина и, первым разом, шлёт
// юзеру эфемерное пояснение (видно только адресату — без привязки к
// эфемерному режиму чата). Дальше только тихие удаления. Повторяющийся флуд
// (порог нарушений за короткое окно) эскалируется в рид-онли мьют.
func (b *Bot) censorQuarantineMessage(m *telego.Message, e quarantineEntry) {
	chatID, userID := m.Chat.ID, m.From.ID
	// Удаляем до пояснения: если юзер видит текст, он уже знает, что было.
	// Одиночный delete на сетевом чихе/429 оставлял ссылку висеть — а она и
	// есть нарушение; короткая лестница (как у kick) + честный retry_after.
	if m.MessageID != 0 {
		if err := retryWith(b.runCtx, kickUnbanBackoffs, func() error {
			return b.deleteMessage(b.runCtx, chatID, m.MessageID)
		}); err != nil {
			b.log.Warn("quarantine: delete violation",
				"err", err, "chat", chatID, "user", userID)
		}
	}
	// Разовое пояснение срезается атомарным claim'ом: снапшот e устаревает
	// между middleware и этой горутиной, и два параллельных нарушения оба
	// видели бы Warned=false → детерминированный дубль SendMessage.
	// ClaimWarned пропускает всех, кроме единственного победителя.
	if !e.Warned && b.quar.ClaimWarned(chatID, userID) {
		notice := tu.Message(tu.ID(chatID),
			"🧫 Ты в карантине: пока можно только писать текст — без ссылок и пересылок.\nСообщение удалено.").
			WithEphemeralMessageParameters(&telego.EphemeralMessageParameters{ReceiverUserID: int(userID)})
		// В топике (форум-супергруппа) пояснение обязано лечь в тот же топик,
		// где было нарушение, — иначе оно уйдёт в General.
		if m.MessageThreadID != 0 {
			notice = notice.WithMessageThreadID(m.MessageThreadID)
		}
		if _, err := b.api.SendMessage(b.runCtx, notice); err != nil {
			b.log.Warn("quarantine: send notice", "err", err, "chat", chatID, "user", userID)
			// Пояснение не доставлено (сетевой чих, юзер вне доступа) — даём
			// второй шанс при следующем нарушении, а не тихую цензуру до срока.
			b.quar.ClearWarned(chatID, userID)
			return
		}
		if err := b.db.SetQuarantineWarned(b.runCtx, chatID, userID); err != nil {
			b.log.Warn("quarantine: persist warned", "err", err, "chat", chatID, "user", userID)
		}
	}
	// Эскалация: флуд ссылок не успевает за делитами и жжёт чатовый
	// delete-бюджет — после N нарушений за окно убираем и право текста.
	if v := b.quar.BumpViolations(chatID, userID); v == censorEscalateAfter {
		b.escalateQuarantine(chatID, userID)
	}
}

// escalateQuarantine — повторяющийся нарушитель карантина: рид-онли мьют на
// час вместо вечной борьбы цензора с его делит-бюджетом. /unmute отпускает
// (событие mute питает его «10 последних»). Карантин при этом снимается:
// мьют полностью заменяет прежний текст-онли рестрикт, и по истечении срока
// юзер вернётся к дефолтным правам чата, а не к «только текст».
func (b *Bot) escalateQuarantine(chatID, userID int64) {
	if err := b.mute(b.runCtx, chatID, userID, censorEscalateMute); err != nil {
		b.log.Warn("quarantine escalation mute", "err", err, "chat", chatID, "user", userID)
		return
	}
	// Замьюченный физически не может выполнить «напиши что-нибудь» —
	// снимаем ожидание реплая тихо (прецедент /mute в modcmd.go). Иначе
	// сиротский replyWaitLoop добавил бы свой kick/noreply поверх эскалации,
	// а неписанный «прошёл» (reply-check) оставил бы юзера в «В процессе».
	if b.cancelReplyWait(chatID, userID) {
		if err := b.db.RecordEvent(b.runCtx, chatID, userID, storage.EventPass, time.Now(), ""); err != nil {
			b.log.Warn("record pass event (quarantine escalation)", "err", err)
		}
	}
	// Состояние карантина снимаем БЕЗ серверного release: дефолтные права
	// чата отменили бы только что наложенный мьют.
	b.dropQuarantineState(chatID, userID)
	if err := b.db.RecordEvent(b.runCtx, chatID, userID, storage.EventMute, time.Now(),
		storage.ReasonQuarantine); err != nil {
		b.log.Warn("quarantine escalation: record mute", "err", err, "chat", chatID, "user", userID)
	}
	b.log.Info("quarantine escalated to read-only",
		"chat", chatID, "user", userID)
}

// dropQuarantineState — снятие карантинного состояния (зеркало + БД-строка)
// БЕЗ серверного release: для путей, где прежний текст-онли рестрикт уже
// заменён другим ограничением (эскалация в рид-онли мьют, команда /mute).
// release здесь был бы вреден — возврат дефолтных прав чата отменил бы
// свежее ограничение. Probe зеркала/строки fail-open; true — карантин был
// и снят.
func (b *Bot) dropQuarantineState(chatID, userID int64) bool {
	_, inMirror := b.quar.Get(chatID, userID)
	has, err := b.db.HasQuarantine(b.runCtx, chatID, userID, time.Now().Unix())
	if err != nil {
		b.log.Warn("quarantine: has row", "err", err, "chat", chatID, "user", userID)
	}
	if !inMirror && err == nil && !has {
		return false
	}
	b.quar.Remove(chatID, userID)
	if err := b.db.DeleteQuarantine(b.runCtx, chatID, userID); err != nil {
		b.log.Warn("quarantine: delete row", "err", err, "chat", chatID, "user", userID)
	}
	return true
}

// releaseQuarantineUser снимает карантин с конкретного юзера: обратный
// рестрикт (дефолтные права чата) + чистка зеркала и БД-строки. false —
// карантина нет (не начинался, истёк или уже снят), release не вызывался —
// дёргать API на каждого админа при промоушене незачем. Общий путь для
// /trust, промоушена в админы и reconcile-свипа выключенных чатов.
func (b *Bot) releaseQuarantineUser(chatID, userID int64) bool {
	_, inMirror := b.quar.Get(chatID, userID)
	has, err := b.db.HasQuarantine(b.runCtx, chatID, userID, time.Now().Unix())
	if err != nil {
		b.log.Warn("quarantine: has row", "err", err, "chat", chatID, "user", userID)
	}
	// Fail-open: выход только по ПОДТВЕРЖДЁННОМУ «нет карантина». Error —
	// не основание молчать: release идемпотентен, а невызванный release
	// оставил бы серверный текст-рестрикт висеть (свежий карантин в зеркале
	// или нечитаемой строке всё равно надо отпускать).
	if !inMirror && err == nil && !has {
		return false
	}
	b.quar.Remove(chatID, userID)
	if err := b.db.DeleteQuarantine(b.runCtx, chatID, userID); err != nil {
		b.log.Warn("quarantine: delete row", "err", err, "chat", chatID, "user", userID)
	}
	if err := b.release(b.runCtx, chatID, userID); err != nil {
		b.log.Warn("quarantine: release", "err", err, "chat", chatID, "user", userID)
	}
	return true
}

// quarantineReleaseChat отпускает всех активных карантинных юзеров чата.
// Вызывается при ВЫКЛЮЧЕНИИ карантина: Telegram держит рестрикт до старого
// until_date, и без этого лимит «только текст» пережил бы выключение до конца
// срока. Владелец выключил — вернули дефолтные права чата каждому.
// «Список → отпуск → повтор» ловит гонку с капча-проходом: onSuccess мог
// решить арм до тоггла, а записать строку после нашего первого списка —
// следующий проход её увидит (арм после коммита тоггла сам отказывается,
// см. re-check в onSuccess).
func (b *Bot) quarantineReleaseChat(chatID int64) {
	for pass := 0; pass < quarantineReleaseMaxPasses; pass++ {
		rows, err := b.db.AllQuarantines(b.runCtx, chatID, time.Now().Unix())
		if err != nil {
			b.log.Warn("quarantine release: list active", "err", err, "chat", chatID)
			return
		}
		if len(rows) == 0 {
			return
		}
		for _, r := range rows {
			b.quar.Remove(r.ChatID, r.UserID)
			if err := b.db.DeleteQuarantine(b.runCtx, r.ChatID, r.UserID); err != nil {
				b.log.Warn("quarantine release: delete row",
					"err", err, "chat", r.ChatID, "user", r.UserID)
			}
			// Release — даже если удаление строки упало: серверный рестрикт
			// держится до until_date, и без отпуска выключенный чат остался
			// бы «только текст» до конца срока.
			if err := b.release(b.runCtx, r.ChatID, r.UserID); err != nil {
				b.log.Warn("quarantine release",
					"err", err, "chat", r.ChatID, "user", r.UserID)
			}
		}
	}
	b.log.Warn("quarantine release: still rows after budget", "chat", chatID)
}

// reconcileQuarantines — страхующий свип карантина: активные строки в чатах,
// где фича выключена (незавершённый отпуск после крэша, БД-правка), глубоко
// не могут удерживать серверный рестрикт. Запуск на старте после
// reconcileChats и на том же полусуточном свипе, что чистит истёкшие строки.
func (b *Bot) reconcileQuarantines(ctx context.Context) {
	rows, err := b.db.DisabledChatQuarantines(ctx, time.Now().Unix())
	if err != nil {
		b.log.Warn("reconcile quarantines", "err", err)
		return
	}
	for _, r := range rows {
		if b.releaseQuarantineUser(r.ChatID, r.UserID) {
			b.log.Info("quarantine released — feature disabled",
				"chat", r.ChatID, "user", r.UserID)
		}
	}
}
