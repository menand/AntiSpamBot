package bot

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

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

	// postponed — серия уже получила разовые «+5 минут» за мусорный ответ;
	// hintedStage — стадия, на которой автору уже показана подсказка о мусоре.
	// Оба поля — под мьютексом стора (Postpone/ClaimJunkHint), переживают
	// переходы стадий через Replace; при рестарте теряются (лишний раз щедро —
	// не несправедливо: после рестарта юзер максимум ещё раз получит +5).
	postponed   bool
	hintedStage int

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

// Deadline — текущий дедлайн ожидания под мьютексом стора: Postpone может
// отодвинуть его в любой момент, читать поле напрямую нельзя (гонка с
// таймером цикла серии).
func (s *replyStore) Deadline(p *replyPending) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return p.ExpiresAt
}

// Postpone отодвигает дедлайн ожидания до at (если тот позже текущего) — «+5
// минут» за мусорный ответ — и только ОДИН РАЗ за серию: повторный вызов —
// no-op, иначе мусорными сообщениями можно было бы продлевать проверку
// бесконечно. Ожидание НЕ изымается: гонок с ответом/выходом/карой нет, кто
// первым решил, тот и разобрался с ним (identity-гвард под мьютексом).
// Возвращает (новый дедлайн, выдано ли продление этим вызовом); false также
// когда двигать некуда (at не позже текущего срока — продление не тратится).
func (s *replyStore) Postpone(chatID, userID int64, p *replyPending, at time.Time) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.items[chatUser{chatID, userID}]; !ok || cur != p {
		return time.Time{}, false
	}
	if p.postponed || !at.After(p.ExpiresAt) {
		return p.ExpiresAt, false
	}
	p.postponed = true
	p.ExpiresAt = at
	return p.ExpiresAt, true
}

// ClaimJunkHint — атомарный claim разовой (на стадию) подсказки автору
// мусорного ответа: параллельные мусорные сообщения не должны поднять шторм
// эфемерных отправок (прецедент ClaimWarned у карантина). На новой стадии
// серии подсказка снова разрешена: старое приветствие к тому времени удалено,
// прежнее объяснение в чате не висит.
func (s *replyStore) ClaimJunkHint(chatID, userID int64, p *replyPending) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.items[chatUser{chatID, userID}]; !ok || cur != p {
		return false
	}
	if p.hintedStage >= p.Stage {
		return false
	}
	p.hintedStage = p.Stage
	return true
}

// ReleaseJunkHint — откат claim'а, если подсказку не удалось доставить:
// следующее мусорное сообщение объяснит заново (тот же трейд-офф, что у
// ClearWarned карантина: лучше повторное объяснение, чем подсказка, которую
// никто не увидел).
func (s *replyStore) ReleaseJunkHint(chatID, userID int64, p *replyPending) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.items[chatUser{chatID, userID}]; ok && cur == p && p.hintedStage == p.Stage {
		p.hintedStage = p.Stage - 1
	}
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

// TakeExpired — Take с проверкой срока под ОДНИМ мьютексом: финальная кара не
// должна изъять ожидание, которому только что выдали «+5 минут» (Postpone мог
// приземлиться между отдельной проверкой дедлайна в цикле и Take — иначе кара
// убила бы юзера, которому подсказка уже пообещала время). Проигравший
// возвращает false НЕ трогая store: либо ожидание уже разрешил кто-то другой
// (identity не сошлась), либо срок ещё не истёк (продлён — цикл перезаведёт сон).
func (s *replyStore) TakeExpired(chatID, userID int64, p *replyPending, now time.Time) (*replyPending, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := chatUser{chatID, userID}
	cur, ok := s.items[k]
	if !ok || cur != p || cur.ExpiresAt.After(now) {
		return nil, false
	}
	delete(s.items, k)
	return cur, true
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
	// «+5 минут» могло приземлиться в окно перехода (Postpone правит
	// old.ExpiresAt под этим же мьютексом): срок следующей стадии — max
	// предложенного и текущего, иначе обещание подсказки потерялось бы, а
	// одноразовое продление сгорело. Обратный порядок (Postpone после Replace)
	// гвардится identity-проверкой Postpone и сюда не доходит.
	installed := expiresAt
	if old.ExpiresAt.After(installed) {
		installed = old.ExpiresAt
	}
	next := &replyPending{ChatID: chatID, UserID: userID, ExpiresAt: installed,
		ThreadID: threadID, Stage: stage, GreetingMsgID: greetingMsgID,
		// «+5 минут» — свойство СЕРИИ, а не стадии: переход не должен его
		// терять, иначе вторая порция мусора снова продлевала бы проверку.
		// hintedStage сбрасывается: подсказка на новой стадии снова уместна.
		postponed: old.postponed,
		cancelCh:  make(chan struct{})}
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

// maybeArmReplyWait — хук после приветствия в onSuccess: при режиме «с
// ответом» взводит серию напоминаний (стадия 1). Якорь шлёт onSuccess ДО
// этого вызова — см. комментарий там про порядок release→arm. Возвращает
// true, если ожидание реально взведено: false (режим без проверки или провал
// персистентности) говорит onSuccess не рисовать в якоре требование, которое
// никто не проверяет.
func (b *Bot) maybeArmReplyWait(s storage.ChatSettings, chatID, userID int64, threadID int) bool {
	if effectiveWelcomeMode(s) != welcomeReply {
		return false
	}
	// Снапшот s прочитан до размьюта (секунды ретраев release внутри
	// onSuccess): админ мог увести чат из «с ответом», пока капча дозревала.
	// Свежая проверка сужает окно гонки, но не закрывает его (смена режима
	// может пройти между ней и Put) — добивает страховка в replyWaitLoop:
	// серия в чате без режима не напомнит и не накажет, а снимется пассом.
	// Ошибка чтения — не повод решать за чат: армируем как раньше.
	// Уверенное «режим ушёл» — серию не взводим И закрываем воронку
	// компенсирующим пассом: капча пройдена, требование отменено админом, а
	// в reply-режиме пасс пишет только ответ — без этого юзер навсегда завис
	// бы в «В процессе» (ожидания нет, и ни disarmReplyWaits, ни цикл его не
	// увидят).
	if fresh, err := b.db.GetChatSettings(b.runCtx, chatID); err == nil && effectiveWelcomeMode(fresh) != welcomeReply {
		if rerr := b.db.RecordEvent(b.runCtx, chatID, userID, storage.EventPass, time.Now(), ""); rerr != nil {
			b.log.Warn("record compensating pass (mode left reply before arm)", "err", rerr,
				"chat", chatID, "user", userID)
		}
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
	// Пост-персист identity-чек (гонка «решение прилетело между Put в память
	// и PutPendingReply»): ответ юзера, промоушен или /mute могли изъять
	// ожидание за миллисекунды персиста — тогда только что записанная строка
	// сирота, и рестарт воскресил бы серию на уже решённого юзера (промоушен-
	// бэкстоп закрыл бы воронку вторым пассом, а замьюченный получил бы кик
	// за неответ поверх решения админа). Guard по стадии 1 — не тронем чужой.
	if cur, ok := b.replies.Get(chatID, userID); !ok || cur != p {
		// Guard-delete — на liveCtx (как в sendCaptchaStage): SIGTERM в
		// микроокне персиста не должен оставить сироту, которую рестарт
		// воскресит для уже решённого юзера.
		dctx, dcancel := b.liveCtx()
		if derr := b.db.DeletePendingReplyIf(dctx, chatID, userID, 1); derr != nil {
			b.log.Warn("delete orphan pending reply (resolved mid-persist)", "err", derr,
				"chat", chatID, "user", userID)
		}
		dcancel()
		// Ожидание уже снято: требование-строку в приветствии рисовать
		// нельзя (enforce'ить её нечем), серию не запускаем.
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

// junkReplyExtend — сколько добавляется к текущей стадии за первый мусорный
// ответ серии (один раз за серию: дальше мусор только тихо удаляется).
const junkReplyExtend = 5 * time.Minute

// contentWordMinRunes — минимум букв в слове, которое может считаться
// содержательным. Порог калиброван по живому вопросу-вызову чатов («Привет! …
// а ты точно не спамер? Уже бегал(а) с нами или только планируешь?»):
// «бегаю», «планирую», «присоединиться» — ответы; «бег», «иду» — нет
// (подсказка и требование честно просят пару слов по делу).
const contentWordMinRunes = 4

// emptyReplyWords — стоп-список «пустышек»: приветствия, вежливость, реакции
// и служебные слова, которыми формальная заглушка маскируется под ответ.
// Слово из списка НИКОГДА не содержательное, сколько бы в нём ни было букв
// («здравствуйте» — 11 букв, но ответом не является). Список умышленно
// пополняемый, но безопасный: он лишь понижает слово до «пустышки», а тонуть
// в нём может только сообщение ЦЕЛИКОМ из пустышек — слова живых ответов
// («бегала», «планирую», «присоединиться», «завтра»...) здесь не должны
// появляться никогда. Ключи — :lower + ё→е; слова короче contentWordMinRunes
// можно не перечислять (они и так не содержательные).
var emptyReplyWords = map[string]struct{}{
	// русские приветствия (хвосты форм добивают префиксы ниже; «здрасте»/
	// «здраствуйте» — частые опечатки, их префиксы не ловят, поэтому точные)
	"привет": {}, "приветик": {}, "приветики": {}, "превед": {}, "прив": {},
	"здрасти": {}, "здрасьте": {}, "здрасте": {}, "здравствуй": {}, "здравствуйте": {},
	"здраствуйте": {}, "здарова": {}, "здаров": {}, "здоровенько": {}, "хелло": {},
	// пожелания дня
	"добро": {}, "доброе": {}, "добрый": {}, "доброго": {}, "пожаловать": {},
	"утро": {}, "утра": {}, "утречка": {}, "день": {}, "денек": {}, "денечек": {},
	"вечер": {}, "вечера": {}, "ночи": {}, "ночь": {},
	// коллективные обращения («все» вне списка: 3 буквы — и так не слова)
	"всем": {}, "всех": {}, "ребята": {}, "ребят": {}, "ребятки": {},
	"друзья": {}, "други": {}, "народ": {}, "люди": {}, "камрады": {}, "коллеги": {},
	"товарищи": {}, "господа": {}, "бегуны": {}, "бегунов": {}, "бегунам": {},
	// вежливость
	"спасибо": {}, "спасибки": {}, "спасибочки": {}, "пасиб": {}, "пасиба": {},
	"благодарю": {}, "пожалуйста": {}, "пжлст": {}, "плиз": {},
	// согласие и реакции
	"окей": {}, "ладно": {}, "давай": {}, "давайте": {},
	"отлично": {}, "классно": {}, "круто": {}, "супер": {}, "клево": {},
	"здорово": {}, "зачет": {}, "молодцы": {}, "молодец": {},
	// служебные и филлеры
	"меня": {}, "тебе": {}, "тебя": {}, "вами": {}, "нами": {},
	"свой": {}, "свои": {}, "своя": {}, "свое": {}, "наше": {}, "ваши": {},
	"этот": {}, "такой": {}, "такая": {}, "такое": {}, "тута": {}, "здесь": {},
	"пока": {}, "только": {}, "очень": {}, "просто": {}, "вообще": {}, "короче": {},
	"тоже": {}, "также": {}, "вроде": {}, "типа": {}, "кажется": {}, "наверное": {},
	"когда": {}, "почему": {}, "зачем": {}, "откуда": {}, "какой": {}, "какая": {},
	"какие": {}, "куда": {}, "чего": {},
	"дела": {}, "дело": {}, "делишки": {}, "настроение": {}, "жизнь": {},
	// «хочу/хотел(а)/спамер/спам/тест/капча» умышленно ВНЕ списка: «очень
	// хочу», «хочу к вам», «я не спамер» — живые ответы-намерения, а не
	// заглушки; цена ложного мусора (удаление + серия кары) выше цены
	// пропущенного «хочу» (реакция продукт-ревью).
	"поздравляю": {}, "поздравляем": {},
	// иностранные приветствия/вежливость
	"hello": {}, "hallo": {}, "hola": {}, "ciao": {}, "bonjour": {}, "salut": {},
	"okay": {}, "welcome": {}, "thanks": {}, "thank": {}, "please": {}, "merci": {},
	"guys": {}, "everyone": {}, "everybody": {}, "good": {}, "nice": {}, "cool": {},
	"morning": {}, "evening": {}, "night": {},
}

// emptyReplyWordPrefixes — корни-пустышки, у которых форм больше, чем хочется
// перечислять поимённо («привет»/«приветики»/«приветствую», «здравствуй»/
// «здравствуйте», «спасибо»/«спасибки»/«спасибочки»): слово с таким началом
// не содержательное.
var emptyReplyWordPrefixes = []string{"привет", "здравств", "здаров", "спасиб", "благодар"}

// isCommandText — сообщение похоже на команду: первый непробельный символ «/».
// Telegram помечает такие первым словом как bot_command, но строковой проверки
// достаточно: она не зависит от наличия entities и одинакова для «/ban» и
// «/help@OtherBot».
func isCommandText(m *telego.Message) bool {
	return strings.HasPrefix(strings.TrimSpace(m.Text), "/")
}

// replyCountsAsAnswer — проходит ли сообщение ступень осмысленности: ответом
// на приветствие-проверку считается только текст/подпись хотя бы с одним
// содержательным словом. Задача фильтра — отсечь формальные заглушки («+»,
// «привет», смайлик, стикер, голая ссылка), НЕ задевая короткие живые ответы
// на вопрос-вызов («ещё не бегала», «хочу присоединиться»). Лексика, без LLM:
// детерминированно, не жжёт квоты и одинаково для всех чатов.
func replyCountsAsAnswer(m *telego.Message) bool {
	if isCommandText(m) {
		// Команда — не ответ на вопрос-вызов: «/start» не должен закрывать
		// проверку, и «/ban»/«/del» — тем более (мод-команды уходят в свои
		// хендлеры, ожидание остаётся взведённым: молчаливое «/help» — не
		// ответ на «уже бегал(а)?»).
		return false
	}
	text := m.Text
	if strings.TrimSpace(text) == "" {
		// Подпись медиа: фото с осмысленной подписью — полноценный ответ;
		// голый стикер/кружок/фото без текста — нет.
		text = m.Caption
	}
	return textHasContentWord(text)
}

// textHasContentWord — есть ли в тексте хоть одно содержательное слово.
// Токен со служебным знаком (ссылка, домен, @упоминание, #хэштег, время) и
// цифры/символы словами не считаются: их «буквы» осмысленности не доказывают.
// Маркеры середины токена — только «.» и «:»: «t.me/x» и «http://…» ловятся
// ими, а слэш умышленно НЕ маркер — «бегаю/бегала» остаётся живой фразой
// (команды вида «/ban» отсекает isCommandText выше, до лексики).
func textHasContentWord(text string) bool {
	for _, raw := range strings.Fields(text) {
		// @упоминания и #хэштеги — не слова, даже после срезания краевых
		// знаков; проверяем маркеры на СЫРОМ токене: TrimFunc ниже съел бы
		// ведущие «@»/«#» и превратил «@ivan_ivanov» в «ivan_ivanov».
		if strings.ContainsAny(raw, "@#") {
			continue
		}
		// Знаки по краям (точка, скобка, восклицание) — обычная пунктуация:
		// «Бегаю.» — слово, «t.me/x» — ссылка. Смотрим только середину токена.
		tok := strings.TrimFunc(raw, func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r)
		})
		if tok == "" || strings.ContainsAny(tok, ".:") {
			continue
		}
		for _, w := range strings.FieldsFunc(tok, func(r rune) bool { return !unicode.IsLetter(r) }) {
			norm := strings.ReplaceAll(strings.ToLower(w), "ё", "е")
			if utf8.RuneCountInString(norm) < contentWordMinRunes {
				continue
			}
			if _, empty := emptyReplyWords[norm]; empty {
				continue
			}
			if hasEmptyReplyPrefix(norm) {
				continue
			}
			return true
		}
	}
	return false
}

// hasEmptyReplyPrefix — слово с корнем-пустышкой («приветствующий»,
// «здравствуйте», «спасибки», «благодарствую»).
func hasEmptyReplyPrefix(w string) bool {
	for _, pre := range emptyReplyWordPrefixes {
		if strings.HasPrefix(w, pre) {
			return true
		}
	}
	return false
}

// replyWaitSatisfied снимает ожидание, когда юзер написал сообщение.
// Вызывается из middleware (bot.go) на каждое групповое сообщение с контентом,
// ДО маршрутизации (быстрый in-memory промах для всех, у кого ожидания нет),
// и из handleEditedGroupMessage: дописанный правкой ответ тоже засчитывается.
// Возвращает изъятое ожидание (nil, если его не было / не наш — обычный юзер
// без reply-check): middleware использует не-nil как признак «прошёл 2-й
// уровень» и шлёт владельцам форвард ответа (notifyReplyAnswered).
func (b *Bot) replyWaitSatisfied(chatID, userID int64) *replyPending {
	p, ok := b.replies.Take(chatID, userID)
	if !ok {
		return nil
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
	return p
}

// handleJunkReply — мусорный ответ на приветствие-проверку: сообщение
// удаляется (в чате чисто; апдейт уже заглотан middleware — в счётчики,
// архив и ИИ-чек удалённый мусор не идёт), автору уходит разовая (на стадию)
// эфемерная подсказка, а серия один раз за серию получает +5 минут. Человеку,
// машинально написавшему «+», даётся шанс ответить нормально, но формальная
// заглушка проверку НЕ проходит — молчание в конце серии карается как раньше.
// Вызывается из goSafe: сетевые вызовы не должны тормозить цепочку хендлеров,
// а мутации — умереть с ctx апдейта (liveCtx).
func (b *Bot) handleJunkReply(m *telego.Message, p *replyPending) {
	ctx, cancel := b.liveCtx()
	defer cancel()
	chatID, userID := m.Chat.ID, m.From.ID

	// +5 минут — один раз за серию; персистим новый дедлайн CAS'ом по стадии:
	// рестарт не должен воскресить старый, более ранний срок (переход стадии
	// персистится ДО исполнения, так что чужую строку N+1 гвард не тронет).
	deadline, granted := b.replies.Postpone(chatID, userID, p, time.Now().Add(junkReplyExtend))
	if granted {
		if err := b.db.ExtendPendingReplyIf(ctx, chatID, userID, p.Stage, deadline); err != nil {
			b.log.Warn("junk reply: persist extended deadline", "err", err,
				"chat", chatID, "user", userID)
		}
	}

	// Удаляем через короткую лестницу (прецедент цензора карантина): мусор
	// не должен пережить один сетевой чих/429. Флаг честности — подсказка не
	// должна врать «Сообщение удалено», если удалить не удалось.
	// Флаг заводим от «удаление вообще возможно»: при нулевом id (теоретический
	// случай) попытки не было, и «Сообщение удалено.» стало бы ложью — гейт
	// честности не должен пропускать этот случай.
	deleted := m.MessageID != 0
	if deleted {
		if err := retryWith(ctx, kickUnbanBackoffs, func() error {
			return b.deleteMessage(ctx, chatID, m.MessageID)
		}); err != nil {
			deleted = false
			b.log.Warn("junk reply: delete message", "err", err, "chat", chatID, "user", userID)
		}
	}

	// Подсказка — один раз на стадию (claim под мьютом стора): флуд мусором не
	// поднимает шторм эфемерных отправок. Второй мусор стадии — тихое удаление.
	if !b.replies.ClaimJunkHint(chatID, userID, p) {
		b.log.Info("reply check: junk answer", "chat", chatID, "user", userID,
			"stage", p.Stage, "extended", granted, "deleted", deleted)
		return
	}
	if err := b.sendJunkReplyHint(ctx, m, granted, deleted); err != nil {
		// Подсказка не доставлена — откатываем claim: следующее мусорное
		// сообщение объяснит заново (прецедент ClearWarned у карантина).
		b.replies.ReleaseJunkHint(chatID, userID, p)
		b.log.Warn("junk reply: send hint", "err", err, "chat", chatID, "user", userID)
		return
	}
	b.log.Info("reply check: junk answer", "chat", chatID, "user", userID,
		"stage", p.Stage, "extended", granted, "deleted", deleted)
}

// junkReplyHintText — текст эфемерной подсказки автору мусорного ответа.
// extended говорит, что серия РЕАЛЬНО получила +5 минут: обещать время,
// которого не будет (продление уже тратилось или двигать было некуда), нельзя.
// deleted — что сообщение действительно удалили (сбой удаления честнее не
// упоминать вовсе). Тексты просят «пару слов», фильтр принимает и одно
// содержательное слово — запас в щедрую сторону: пропустить слабый ответ
// дешевле, чем удалить настоящий. Пример в тексте — чтобы правило было
// исполнимым: человек, машинально написавший «+», должен увидеть, как
// выглядит валидный ответ.
func junkReplyHintText(extended, deleted bool) string {
	text := "🙈 Это не считается ответом: нужен текст с парой слов по делу — «+», смайлики, стикеры и медиа без подписи не засчитываются. Например: «планирую бегать» или «ещё не бегала»."
	if deleted {
		text += " Сообщение удалено."
	}
	if extended {
		text += fmt.Sprintf(" ⏱ Даю ещё %d минут.", int(junkReplyExtend/time.Minute))
	}
	return text
}

// sendJunkReplyHint шлёт подсказку эфемерно (видит только адресат; прецедент
// цензора карантина — без привязки к эфемерному режиму чата) и в тот же
// форум-топик, где было сообщение. Best effort: публичного фолбэка нет —
// удалённый мусор не повод шуметь на весь чат.
func (b *Bot) sendJunkReplyHint(ctx context.Context, m *telego.Message, extended, deleted bool) error {
	notice := tu.Message(tu.ID(m.Chat.ID), junkReplyHintText(extended, deleted)).
		WithEphemeralMessageParameters(&telego.EphemeralMessageParameters{ReceiverUserID: int(m.From.ID)})
	if m.MessageThreadID != 0 {
		notice = notice.WithMessageThreadID(m.MessageThreadID)
	}
	_, err := b.api.SendMessage(ctx, notice)
	return err
}

// replyWaitMiddleware — обработка входящего группового сообщения с контентом в
// middleware (ДО маршрутизации): настоящий ответ снимает ожидание, мусор
// удаляется. Возвращает true, если апдейт надо заглотить (мусор: удалён и в
// счётчики/архив/ИИ-чек не идёт); false — сообщение уходит дальше по цепочке.
// Вынесено из замыкания middleware ради тестируемости (сам middleware лишь
// вызывает метод — см. bot.go).
func (b *Bot) replyWaitMiddleware(m *telego.Message) bool {
	p, ok := b.replies.Get(m.Chat.ID, m.From.ID)
	if !ok {
		return false
	}
	if replyCountsAsAnswer(m) {
		// не-nil — юзер реально прошёл 2-й уровень (ответил на приветствие):
		// подписчикам mod_notify уходит форвард его сообщения + карточка
		// (goSafe — не тормозим хендлер).
		if taken := b.replyWaitSatisfied(m.Chat.ID, m.From.ID); taken != nil {
			b.goSafe("notifyReplyAnswered", func() { b.notifyReplyAnswered(m, taken) })
		}
		return false
	}
	// Команда — не ответ и не мусор: «/ban», «/del», «/start» короче порога
	// фильтра, но удалять и глотать их нельзя — они уходят в свои хендлеры
	// (ожидание при этом остаётся взведённым: replyCountsAsAnswer команды не
	// засчитывает, напоминание/кара по сроку идут как шли). Гейт сервисности —
	// defense-in-depth: арм уже требует обслуживаемого чата, но удалять
	// сообщение в брошенном чате бот не должен.
	if isCommandText(m) || !b.chatServiceable(m.Chat.ID) {
		return false
	}
	// Мусорный ответ: удаление + подсказка + разовые +5 минут — в фоне
	// (сетевые лестницы ретраев не должны тормозить цепочку хендлеров).
	b.goSafe("junkReply", func() { b.handleJunkReply(m, p) })
	return true
}

// cancelReplyWait тихо снимает ожидание (юзер вышел/кикнут/забанен/замьючен) —
// кик за молчание ему уже не грозит. Возвращает true, если ожидание было
// активным: вызывающему надо закрыть воронку статистики событием (left/pass),
// иначе юзер навсегда зависнет в «В процессе» — пасс при reply-check пишется
// только в replyWaitSatisfied, то есть после снятия его уже никто не запишет.
func (b *Bot) cancelReplyWait(chatID, userID int64) bool {
	if p, ok := b.replies.Take(chatID, userID); ok {
		p.Cancel()
		// liveCtx: при SIGTERM строка не должна пережить отмену — рестарт
		// воскресил бы серию на юзера, которого команда уже разобрала.
		ctx, cancel := b.liveCtx()
		defer cancel()
		if err := b.db.DeletePendingReplyIf(ctx, chatID, userID, p.Stage); err != nil {
			b.log.Warn("delete pending reply on cancel", "err", err, "chat", chatID, "user", userID)
		}
		return true
	}
	return false
}

// disarmReplyWaits снимает ВСЕ живые ожидания ответа чата разом (смена режима
// «после капчи» уводит чат из reply): таймеры отменяются, строки удаляются,
// воронка каждого юзера закрывается компенсирующим пассом — требование
// исчезло не по его вине (прецеденты: /mute, провал отправки якоря, промоушен
// в админы). Без пасса эти юзеры навсегда зависли бы в «В процессе»: в режиме
// reply пасс пишет только replyWaitSatisfied. Крэш между записью режима и этим
// снятием подхватывает backstop в restorePendingReplies (режим != reply →
// строка не восстанавливается, пасс пишется там).
func (b *Bot) disarmReplyWaits(chatID int64) {
	waits := b.replies.TakeChat(chatID)
	if len(waits) == 0 {
		return
	}
	// Свежий дедлайн: операция идёт из хендлера колбэка, чей ctx сейчас умрёт,
	// а событие и удаление строк обязаны доехать до БД.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, p := range waits {
		p.Cancel()
		if err := b.db.RecordEvent(ctx, chatID, p.UserID, storage.EventPass, time.Now(), ""); err != nil {
			b.log.Warn("record pass on reply disarm", "err", err,
				"chat", chatID, "user", p.UserID)
		}
	}
	if err := b.db.DeletePendingRepliesChat(ctx, chatID); err != nil {
		b.log.Warn("delete pending replies on disarm", "err", err, "chat", chatID)
	}
	b.log.Info("reply waits disarmed by mode switch", "chat", chatID, "count", len(waits))
}

// disarmReplyWait снимает ОДНО ожидание, если оно всё ещё наше (Take-гвард):
// компенсирующий пасс пишется только победителем — проигравший гонку делает
// no-op (терминальный исход уже записал другой: ответ, выход, /mute). Вызывается
// страховкой режима в replyWaitLoop для серии, пережившей смену режима.
// Строка гасится по СВОЕЙ стадии: предзаписанную N+1 добивает сам цикл после
// перехода, а сюда попадаем только на исходе стадии.
func (b *Bot) disarmReplyWait(ctx context.Context, chatID, userID int64, p *replyPending) {
	taken, ok := b.replies.Take(chatID, userID)
	if !ok || taken != p {
		return
	}
	taken.Cancel()
	if err := b.db.RecordEvent(ctx, chatID, userID, storage.EventPass, time.Now(), ""); err != nil {
		b.log.Warn("record pass on reply disarm", "err", err, "chat", chatID, "user", userID)
	}
	if err := b.db.DeletePendingReplyIf(ctx, chatID, userID, p.Stage); err != nil {
		// Выжившая строка воскресила бы серию при рестарте — но backstop
		// режима в restorePendingReplies снимет её (режим != reply), ценой
		// возможного двойного пасса; это шум против кика, принимаем.
		b.log.Warn("delete pending reply on disarm", "err", err, "chat", chatID, "user", userID)
	}
	b.log.Info("reply wait disarmed — mode left reply",
		"chat", chatID, "user", userID, "stage", p.Stage)
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
		// Дедлайн читается методом стора (под мьютексом): мусорный ответ
		// мог отодвинуть его (+5 минут) в любой момент — прямое чтение поля
		// гонялось бы с Postpone.
		timer := time.NewTimer(time.Until(b.replies.Deadline(p)))
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

		// Дедлайн мог отодвинуться (+5 минут за мусорный ответ) ровно в
		// момент срабатывания старого таймера: стадия та же — не напоминаем и
		// не караем, просто перезаводим сон на новый срок. Гонки с Postpone
		// нет: срок читается под тем же мьютексом стора.
		if b.replies.Deadline(p).After(time.Now()) {
			continue
		}

		// Страховка от устаревшего арма и гонок со сменой режима: серия могла
		// взвестись по снапшоту настроек, пока чат уже уводили из «с ответом»
		// (disarmReplyWaits снял только прежние ожидания, а maybeArmReplyWait
		// добавил новую пару уже после его TakeChat). Чат больше не обещает
		// требование — не напоминаем и НЕ караем: снимаем ожидание
		// компенсирующим пассом (прецедент /mute). Стоит ДО финальной кары,
		// поэтому и «фантомная» серия никого не кикнет. Настройки читаются
		// один раз на стадию — переход ниже переиспользует тот же снапшот;
		// ошибка чтения — не повод решать за чат (та же дисциплина, что у
		// backstop в restorePendingReplies и у свежей проверки в
		// maybeArmReplyWait): серия продолжается как раньше.
		s, serr := b.db.GetChatSettings(ctx, chatID)
		if serr == nil && effectiveWelcomeMode(s) != welcomeReply {
			b.disarmReplyWait(ctx, chatID, userID, p)
			return
		}

		// Серия исчерпана: та же лестница наказаний, что у капчи (общий
		// punishAttempt): счётчик attempts с гвардией «ошибка счётчика
		// запрещает бан», порог effectiveMaxAttempts, дальше бан. Cleanup на
		// detached-контексте, чтобы shutdown не оборвал кик на полпути;
		// pending_replies удаляем только после успеха — рестарт повторит
		// наказание иначе.
		if p.Stage >= captchaStages {
			// TakeExpired — изъятие и проверка срока под ОДНИМ мьютексом:
			// мусорный ответ мог продлить серию (обещание «+5 минут» в
			// подсказке) в микроокне между проверкой дедлайна выше и Take —
			// кара не должна убить того, кому подсказка только что пообещала
			// время.
			if _, ok := b.replies.TakeExpired(chatID, userID, p, time.Now()); !ok {
				// Либо ожидание уже разрешил кто-то другой (выход, ответ,
				// /mute — он и записал исход), либо его продлили («+5») —
				// тогда просто перезаводим сон на новый срок.
				if _, mine := b.replies.Get(chatID, userID); mine {
					continue
				}
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
		nextExpires := time.Now().Add(b.effectiveStageInterval(s))
		// «+5 минут» могло приземлиться между проверкой дедлайна выше и этой
		// строкой: переносим его в следующую стадию — обещание подсказки не
		// должно потеряться на переходе.
		if d := b.replies.Deadline(p); d.After(nextExpires) {
			nextExpires = d
		}
		if err := b.db.PutPendingReply(ctx, storage.PendingReply{
			ChatID: chatID, UserID: userID, ExpiresAt: nextExpires,
			Stage: p.Stage + 1, ThreadID: p.ThreadID,
		}); err != nil {
			b.log.Warn("persist reply stage transition", "err", err,
				"chat", chatID, "user", userID, "stage", p.Stage+1)
		}

		// Якорь текущей стадии сносим: «напиши пару слов по делу» без
		// сообщения, к которому оно прикреплено, — мусор в ленте.
		b.deleteReplyAnchor(ctx, chatID, userID)

		msgID, sent := b.sendGreetingAnchor(ctx, s, welcomeReply, chatID, userID, p.ThreadID, p.Stage+1)
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
		// Replace под тем же мьютексом мог поднять срок выше предложенного
		// («+5 минут» приземлилось в окно перехода — см. Replace). Строку
		// N+1 выше писали ДО этого момента, с предложенным сроком: досылаем
		// фактический, иначе рестарт откатил бы продление к более раннему.
		// Guard-UPDATE по стадии, НЕ upsert (прецедент ExtendPendingReplyIf
		// мусорной ветки): победитель (ответ/выход/кара), приземлившийся
		// между чтением Deadline и этой записью, уже снёс строку N+1 — upsert
		// воскресил бы её, и рестарт зарядил бы фантомную серию ответившему.
		// 0 строк (строки уже нет / стадия ушла дальше) — норма.
		if d := b.replies.Deadline(next); d.After(nextExpires) {
			if err := b.db.ExtendPendingReplyIf(ctx, chatID, userID, next.Stage, d); err != nil {
				b.log.Warn("persist extended reply deadline after transition", "err", err,
					"chat", chatID, "user", userID, "stage", next.Stage)
			}
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
	disarmedChats := map[int64]struct{}{}
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
		// Режим могли увести из «с ответом», пока бот лежал (или крэш в окне
		// между записью режима и снятием серий — см. disarmReplyWaits):
		// требование больше не обещано, серию не восстанавливаем, воронку
		// закрываем пассом. Ошибка чтения настроек — не повод решать за чат:
		// восстанавливаем как раньше. Редкий двойной пасс (живое снятие
		// успело записать пасс, но упало на удалении строки, а этот backstop
		// записал второй) принимаем как шум против возможного кика.
		if s, serr := b.db.GetChatSettings(ctx, row.ChatID); serr == nil && effectiveWelcomeMode(s) != welcomeReply {
			if rerr := b.db.RecordEvent(ctx, row.ChatID, row.UserID, storage.EventPass, time.Now(), ""); rerr != nil {
				b.log.Warn("record pass on restore disarm", "err", rerr,
					"chat", row.ChatID, "user", row.UserID)
			}
			disarmedChats[row.ChatID] = struct{}{}
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
	for chatID := range disarmedChats {
		if err := b.db.DeletePendingRepliesChat(ctx, chatID); err != nil {
			b.log.Warn("delete pending replies of disarmed chat",
				"err", err, "chat", chatID)
		}
	}
	if restored > 0 || len(staleChats) > 0 || len(disarmedChats) > 0 {
		b.log.Info("restored pending replies", "count", restored,
			"total", len(rows), "skipped_chats", len(staleChats),
			"disarmed_chats", len(disarmedChats))
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

// replyRequirementLine — строка-требование для стадии stage серии. Стадия 1 —
// нейтральная, 2 — напоминание, 3 — последнее предупреждение. Текст честно
// называет фильтр осмысленности (см. replyCountsAsAnswer): «+», смайлик или
// стикер ответом не считаются, а требование без оговорки обманывало бы: юзер
// «выполнил» его формальной заглушкой, а серия продолжала бы идти. minutesStr —
// интервал в родительном падеже («в течение 2 минут»), готовит minutesGen.
func replyRequirementLine(stage int, minutesStr string) string {
	switch stage {
	case 2:
		return fmt.Sprintf("\n\n⏳ Напоминание: напиши пару слов по делу в течение %s — иначе придётся зайти заново. «+», смайлики, стикеры и медиа без подписи не засчитываются.",
			minutesStr)
	case 3:
		return fmt.Sprintf("\n\n⚠️ ПОСЛЕДНЕЕ ПРЕДУПРЕЖДЕНИЕ: напиши пару слов по делу в течение %s — иначе тебя исключат из чата. «+», смайлики, стикеры и медиа без подписи не засчитываются.",
			minutesStr)
	default:
		return fmt.Sprintf("\n\n⏱ Напиши пару слов по делу в течение %s — иначе придётся зайти заново. «+», смайлики, стикеры и медиа без подписи не засчитываются.",
			minutesStr)
	}
}
