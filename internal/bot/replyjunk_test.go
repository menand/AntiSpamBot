package bot

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegoapi"

	"github.com/menand/AntiSpamBot/internal/storage"
)

// TestReplyCountsAsAnswer — таблица фильтра осмысленности: формальные
// заглушки («+», приветствия, смайлики, стикеры, голая ссылка) ответом не
// считаются, короткие живые ответы на вопрос-вызов чатов («Уже бегал(а) с нами
// или только планируешь?») — считаются.
func TestReplyCountsAsAnswer(t *testing.T) {
	tests := []struct {
		name string
		msg  telego.Message
		want bool
	}{
		// Мусор: не засчитывается.
		{"плюс", telego.Message{Text: "+"}, false},
		{"плюсы", telego.Message{Text: "+++"}, false},
		{"точка", telego.Message{Text: "."}, false},
		{"смайлик", telego.Message{Text: "🙂"}, false},
		{"только цифры", telego.Message{Text: "12345"}, false},
		{"пусто", telego.Message{}, false},
		{"пробелы", telego.Message{Text: "   "}, false},
		{"стикер без текста", telego.Message{Sticker: &telego.Sticker{FileID: "x"}}, false},
		{"фото без подписи", telego.Message{Photo: []telego.PhotoSize{{FileID: "x"}}}, false},
		{"привет", telego.Message{Text: "привет"}, false},
		{"приветствие с восклицанием", telego.Message{Text: "Здравствуйте!"}, false},
		{"привет всем", telego.Message{Text: "привет всем"}, false},
		{"смайлик и привет", telego.Message{Text: "🙂 привет"}, false},
		{"добрый день", telego.Message{Text: "Добрый день!"}, false},
		{"доброе утро с эмодзи", telego.Message{Text: "доброе утро ☀️"}, false},
		{"как дела", telego.Message{Text: "как дела?"}, false},
		{"спасибо", telego.Message{Text: "спасибо)"}, false},
		{"ок", telego.Message{Text: "ок"}, false},
		{"да нет", telego.Message{Text: "да нет"}, false},
		{"ну ладно", telego.Message{Text: "ну ладно"}, false},
		{"здрасте — частая форма", telego.Message{Text: "здрасте"}, false},
		{"здраствуйте — опечатка", telego.Message{Text: "здраствуйте"}, false},
		{"голая ссылка", telego.Message{Text: "https://t.me/joinchat/AAAA"}, false},
		{"домен", telego.Message{Text: "vk.com/menand"}, false},
		{"упоминание", telego.Message{Text: "@ivan_ivanov"}, false},
		{"хэштег", telego.Message{Text: "#бегаю"}, false},
		{"hello", telego.Message{Text: "hello"}, false},
		{"приветствую — хвост формы", telego.Message{Text: "приветствую"}, false},
		{"спасибки", telego.Message{Text: "спасибки!!"}, false},
		{"подпись-пустышка", telego.Message{Caption: "привет всем"}, false},
		{"команда — не ответ", telego.Message{Text: "/start"}, false},
		{"мод-команда — не ответ", telego.Message{Text: "/mute"}, false},

		// Живые ответы: засчитываются.
		{"ещё не бегала", telego.Message{Text: "ещё не бегала"}, true},
		{"только планирую", telego.Message{Text: "только планирую"}, true},
		{"планирую завтра побегать", telego.Message{Text: "планирую завтра побегать"}, true},
		{"хочу присоединиться", telego.Message{Text: "хочу присоединиться"}, true},
		{"хочу к вам — ответ-намерение", telego.Message{Text: "хочу к вам"}, true},
		{"очень хочу — ответ-намерение", telego.Message{Text: "очень хочу"}, true},
		{"я не спамер — прямой ответ на вопрос бота", telego.Message{Text: "я не спамер"}, true},
		{"одно слово-ответ", telego.Message{Text: "бегаю"}, true},
		{"косая черта внутри слова", telego.Message{Text: "бегаю/бегала"}, true},
		{"живой ответ с эмодзи и скобками", telego.Message{Text: "Здравствуйте! Пока только планирую побегать с вами))"}, true},
		{"привет + смысл", telego.Message{Text: "Привет! Хочу бегать с вами"}, true},
		{"ответ с точкой", telego.Message{Text: "Бегаю."}, true},
		{"меня позвал друг", telego.Message{Text: "меня позвал друг"}, true},
		{"ответ со ссылкой", telego.Message{Text: "смотрю профиль https://t.me/x"}, true},
		{"подпись-ответ", telego.Message{Caption: "бегала с вами в мае"}, true},
		{"текст важнее подписи", telego.Message{Text: "ещё не бегала", Caption: "привет"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := replyCountsAsAnswer(&tc.msg); got != tc.want {
				t.Fatalf("replyCountsAsAnswer(text=%q, caption=%q) = %v, want %v",
					tc.msg.Text, tc.msg.Caption, got, tc.want)
			}
		})
	}
}

// TestReplyStorePostponeOncePerSeries — «+5 минут» даётся один раз за серию,
// переносится через переход стадии, не выдаётся чужому ожиданию и не тратится
// впустую, когда двигать некуда.
func TestReplyStorePostponeOncePerSeries(t *testing.T) {
	s := newReplyStore()
	p := s.Put(1, 2, time.Now().Add(time.Minute), 0, 1, 0)

	at := time.Now().Add(5 * time.Minute)
	d, ok := s.Postpone(1, 2, p, at)
	if !ok || !d.Equal(at) {
		t.Fatalf("first postpone must be granted with the new deadline: d=%v ok=%v", d, ok)
	}
	if got := s.Deadline(p); !got.Equal(at) {
		t.Fatalf("deadline = %v, want %v", got, at)
	}
	// Второй раз в той же серии — no-op: мусором нельзя продлевать бесконечно.
	if _, ok := s.Postpone(1, 2, p, at.Add(time.Hour)); ok {
		t.Fatal("second postpone must be refused (once per series)")
	}
	// Чужому ожиданию (identity-гвард) продление не положено.
	if _, ok := s.Postpone(9, 9, p, at.Add(time.Hour)); ok {
		t.Fatal("foreign wait must not be postponed")
	}
	// Назад не двигаем — и одноразовое продление при этом не тратится.
	p2 := s.Put(3, 4, time.Now().Add(time.Hour), 0, 1, 0)
	if _, ok := s.Postpone(3, 4, p2, time.Now().Add(time.Minute)); ok {
		t.Fatal("postpone must not move the deadline backwards")
	}
	if _, ok := s.Postpone(3, 4, p2, time.Now().Add(2*time.Hour)); !ok {
		t.Fatal("postpone that could not help must not burn the one-shot")
	}
	// Переход стадии переносит продление: Replace создаёт НОВЫЙ pending, и
	// предложенный более ранний срок не должен затирать уже отодвинутый —
	// max берётся под тем же мьютексом (окно «Postpone между расчётом
	// nextExpires и CAS» закрыто).
	next, ok := s.Replace(1, 2, p, time.Now().Add(time.Minute), 0, 2, 0)
	if !ok {
		t.Fatal("replace must succeed")
	}
	if got := s.Deadline(next); !got.Equal(at) {
		t.Fatalf("stage transition must keep the postponed deadline: %v, want %v", got, at)
	}
	if _, ok := s.Postpone(1, 2, next, at.Add(2*time.Hour)); ok {
		t.Fatal("postponed flag must survive the stage transition")
	}
	next2, ok := s.Replace(1, 2, next, time.Now().Add(time.Second), 0, 3, 0)
	if !ok || !s.Deadline(next2).Equal(at) {
		t.Fatalf("replace must keep the later deadline: %v", s.Deadline(next2))
	}
}

// TestReplyStoreTakeExpired — финальная кара изымает ожидание только
// атомарно-истёкшее: продлённое («+5 минут») не должно быть убито в микроокне
// между проверкой срока и Take, чужое/отсутствующее — не трогается вовсе.
func TestReplyStoreTakeExpired(t *testing.T) {
	s := newReplyStore()
	p := s.Put(1, 2, time.Now().Add(time.Minute), 0, 1, 0)
	// Срок в будущем — не изымается.
	if taken, ok := s.TakeExpired(1, 2, p, time.Now()); ok || taken != nil {
		t.Fatal("future-deadline wait must not be taken")
	}
	if _, ok := s.Get(1, 2); !ok {
		t.Fatal("wait must stay in the store")
	}
	// Чужой указатель на пару с УЖЕ истёкшим сроком: отказ обязан доказывать
	// гвард идентичности, а не проверку срока — без гварда изъятие бы прошло.
	expired := s.Put(3, 4, time.Now().Add(-time.Second), 0, 1, 0)
	foreign := s.Put(7, 8, time.Now().Add(-time.Second), 0, 1, 0)
	if _, ok := s.TakeExpired(3, 4, foreign, time.Now()); ok {
		t.Fatal("foreign pending must not be taken")
	}
	if _, ok := s.Get(3, 4); !ok {
		t.Fatal("expired wait must stay until its own pointer is presented")
	}
	// Истёкшая со своим указателем — изымается, store пуст.
	taken, ok := s.TakeExpired(3, 4, expired, time.Now())
	if !ok || taken != expired {
		t.Fatal("expired wait must be taken")
	}
	if _, ok := s.Get(3, 4); ok {
		t.Fatal("taken wait must leave the store")
	}
}

// TestReplyStoreClaimJunkHintOncePerStage — подсказка о мусоре показывается не
// чаще раза на стадию, откатывается при провале доставки и «перезаряжается» на
// следующей стадии.
func TestReplyStoreClaimJunkHintOncePerStage(t *testing.T) {
	s := newReplyStore()
	p := s.Put(1, 2, time.Now().Add(time.Minute), 0, 1, 0)
	if !s.ClaimJunkHint(1, 2, p) {
		t.Fatal("first claim of stage 1 must win")
	}
	if s.ClaimJunkHint(1, 2, p) {
		t.Fatal("second claim of the same stage must lose")
	}
	// Откат (подсказка не доставлена): следующее мусорное сообщение объяснит.
	s.ReleaseJunkHint(1, 2, p)
	if !s.ClaimJunkHint(1, 2, p) {
		t.Fatal("claim must be re-armable after release")
	}
	// Чужому ожиданию подсказка не положена.
	if s.ClaimJunkHint(9, 9, p) {
		t.Fatal("foreign wait must not claim the hint")
	}
	// Новая стадия серии — снова можно.
	next, ok := s.Replace(1, 2, p, time.Now().Add(time.Minute), 0, 2, 0)
	if !ok || !s.ClaimJunkHint(1, 2, next) {
		t.Fatal("new stage must allow the hint again")
	}
}

// TestJunkReplyDeletesHintsAndExtends — мусорный ответ: сообщение удалено,
// подсказка эфемерная и обещает фактически выданные +5 минут, дедлайн сдвинут
// и персистентен, пасс не записан, ожидание живо.
func TestJunkReplyDeletesHintsAndExtends(t *testing.T) {
	ctx := context.Background()
	b, db, fc := newFlowBot(t)

	p := b.replies.Put(testChatID, testUserID, time.Now().Add(time.Minute), 0, 1, 0)
	if err := db.PutPendingReply(ctx, storage.PendingReply{
		ChatID: testChatID, UserID: testUserID, ExpiresAt: b.replies.Deadline(p), Stage: 1,
	}); err != nil {
		t.Fatal(err)
	}

	m := &telego.Message{
		MessageID: 777,
		Chat:      telego.Chat{ID: testChatID, Type: "supergroup"},
		From:      &telego.User{ID: testUserID, FirstName: "Юзер"},
		Text:      "+",
	}
	b.handleJunkReply(m, p)

	if n := fc.callCount("deleteMessage"); n != 1 {
		t.Fatalf("deleteMessage calls = %d, want 1 (мусор удалён)", n)
	}
	bodies := fc.callBodies("sendMessage")
	if len(bodies) != 1 {
		t.Fatalf("sendMessage = %d, want 1 (подсказка)", len(bodies))
	}
	if !strings.Contains(bodies[0], `"receiver_user_id":7`) {
		t.Fatalf("подсказка должна быть эфемерной: %s", bodies[0])
	}
	if !strings.Contains(bodies[0], "не считается ответом") {
		t.Fatalf("подсказка должна объяснять правило: %s", bodies[0])
	}
	if !strings.Contains(bodies[0], "Например") {
		t.Fatalf("подсказка должна показывать пример валидного ответа: %s", bodies[0])
	}
	if !strings.Contains(bodies[0], "Сообщение удалено") {
		t.Fatalf("подсказка должна говорить об удалении, когда оно состоялось: %s", bodies[0])
	}
	if !strings.Contains(bodies[0], "ещё 5 минут") {
		t.Fatalf("подсказка должна обещать выданные +5 минут: %s", bodies[0])
	}
	if _, ok := b.replies.Get(testChatID, testUserID); !ok {
		t.Fatal("wait must stay armed after a junk answer")
	}
	if k := statsKinds(t, db, testChatID, testUserID); k[storage.EventPass] != 0 {
		t.Fatalf("passes = %d, want 0 (мусор не ответ)", k[storage.EventPass])
	}
	if d := b.replies.Deadline(p); time.Until(d) < 4*time.Minute {
		t.Fatalf("in-memory deadline = %v, want ~+5min", d)
	}
	rows, err := db.LoadAllPendingReplies(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("pending_replies = %v err=%v, want 1 row", rows, err)
	}
	if time.Until(rows[0].ExpiresAt) < 4*time.Minute {
		// Рестарт не должен воскресить старый, более ранний срок.
		t.Fatalf("persisted deadline = %v, want ~+5min", rows[0].ExpiresAt)
	}
}

// TestJunkReplySecondMessageSilent — второй мусор той же стадии: снова удалён и
// заглотан, но без подсказки и без второго продления.
func TestJunkReplySecondMessageSilent(t *testing.T) {
	ctx := context.Background()
	b, db, fc := newFlowBot(t)

	p := b.replies.Put(testChatID, testUserID, time.Now().Add(time.Minute), 0, 1, 0)
	if err := db.PutPendingReply(ctx, storage.PendingReply{
		ChatID: testChatID, UserID: testUserID, ExpiresAt: b.replies.Deadline(p), Stage: 1,
	}); err != nil {
		t.Fatal(err)
	}
	msg := func(id int, text string) *telego.Message {
		return &telego.Message{
			MessageID: id,
			Chat:      telego.Chat{ID: testChatID, Type: "supergroup"},
			From:      &telego.User{ID: testUserID, FirstName: "Юзер"},
			Text:      text,
		}
	}

	b.handleJunkReply(msg(1, "+"), p)
	d1 := b.replies.Deadline(p)
	b.handleJunkReply(msg(2, "🙂"), p)

	if n := fc.callCount("deleteMessage"); n != 2 {
		t.Fatalf("deleteMessage calls = %d, want 2 (оба мусора удалены)", n)
	}
	if bodies := fc.callBodies("sendMessage"); len(bodies) != 1 {
		t.Fatalf("подсказка должна быть одна на стадию: %v", bodies)
	}
	if d2 := b.replies.Deadline(p); !d2.Equal(d1) {
		t.Fatalf("второе продление не должно выдаваться: %v → %v", d1, d2)
	}
}

// TestJunkReplyResolvedWaitNotResurrected — гонка: middleware увидел ожидание,
// но к моменту junk-ветки победитель его уже разрешил (ответ/выход/кара).
// Мусор всё ещё удаляется, но «+5» не выдаётся, подсказки нет (claim промах
// по чужому указателю), а строка в БД не воскрешается продлением.
func TestJunkReplyResolvedWaitNotResurrected(t *testing.T) {
	ctx := context.Background()
	b, db, fc := newFlowBot(t)

	p := b.replies.Put(testChatID, testUserID, time.Now().Add(time.Minute), 0, 1, 0)
	if err := db.PutPendingReply(ctx, storage.PendingReply{
		ChatID: testChatID, UserID: testUserID, ExpiresAt: b.replies.Deadline(p), Stage: 1,
	}); err != nil {
		t.Fatal(err)
	}
	// Победитель: изъятие из стора + guard-удаление строки (как в
	// replyWaitSatisfied / cancelReplyWait).
	if taken, ok := b.replies.Take(testChatID, testUserID); !ok || taken != p {
		t.Fatal("winner must take the wait")
	}
	if err := db.DeletePendingReplyIf(ctx, testChatID, testUserID, 1); err != nil {
		t.Fatal(err)
	}

	m := &telego.Message{
		MessageID: 778,
		Chat:      telego.Chat{ID: testChatID, Type: "supergroup"},
		From:      &telego.User{ID: testUserID, FirstName: "Юзер"},
		Text:      "+",
	}
	b.handleJunkReply(m, p)

	if n := fc.callCount("deleteMessage"); n != 1 {
		t.Fatalf("deleteMessage calls = %d, want 1 (мусор удаляется независимо от гонки)", n)
	}
	if bodies := fc.callBodies("sendMessage"); len(bodies) != 0 {
		t.Fatalf("разрешённому ожиданию подсказка не положена: %v", bodies)
	}
	if _, ok := b.replies.Get(testChatID, testUserID); ok {
		t.Fatal("junk reply must not re-arm a resolved wait")
	}
	if rows, err := db.LoadAllPendingReplies(ctx); err != nil || len(rows) != 0 {
		t.Fatalf("продление не должно воскрешать разрешённую строку: %v err=%v", rows, err)
	}
}

// TestJunkReplyHintTextCombinations — честность подсказки: каждое обещание
// («Сообщение удалено», «Даю ещё N минут») появляется ровно по фактическому
// флагу, а объяснение правила есть всегда.
func TestJunkReplyHintTextCombinations(t *testing.T) {
	cases := []struct {
		name              string
		extended, deleted bool
		wantDeleted       bool
		wantExtended      bool
	}{
		{"ни удаления, ни продления", false, false, false, false},
		{"только удалено", false, true, true, false},
		{"только продление", true, false, false, true},
		{"удалено и продлено", true, true, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text := junkReplyHintText(tc.extended, tc.deleted)
			if got := strings.Contains(text, "Сообщение удалено"); got != tc.wantDeleted {
				t.Fatalf("deleted=%v: «Сообщение удалено» = %v, want %v: %s",
					tc.deleted, got, tc.wantDeleted, text)
			}
			if got := strings.Contains(text, "Даю ещё"); got != tc.wantExtended {
				t.Fatalf("extended=%v: «Даю ещё» = %v, want %v: %s",
					tc.extended, got, tc.wantExtended, text)
			}
			if !strings.Contains(text, "не считается ответом") {
				t.Fatalf("подсказка обязана объяснять правило: %s", text)
			}
		})
	}
}

// TestJunkReplyZeroMessageIDNoDeleteLie — теоретический нулевой message_id:
// удалять нечего, попытки не было — deleteMessage не вызывается, а подсказка
// не смеет утверждать «Сообщение удалено.».
func TestJunkReplyZeroMessageIDNoDeleteLie(t *testing.T) {
	ctx := context.Background()
	b, db, fc := newFlowBot(t)

	p := b.replies.Put(testChatID, testUserID, time.Now().Add(time.Minute), 0, 1, 0)
	if err := db.PutPendingReply(ctx, storage.PendingReply{
		ChatID: testChatID, UserID: testUserID, ExpiresAt: b.replies.Deadline(p), Stage: 1,
	}); err != nil {
		t.Fatal(err)
	}

	m := &telego.Message{
		MessageID: 0,
		Chat:      telego.Chat{ID: testChatID, Type: "supergroup"},
		From:      &telego.User{ID: testUserID, FirstName: "Юзер"},
		Text:      "+",
	}
	b.handleJunkReply(m, p)

	if n := fc.callCount("deleteMessage"); n != 0 {
		t.Fatalf("deleteMessage calls = %d, want 0 (удалять нечего)", n)
	}
	bodies := fc.callBodies("sendMessage")
	if len(bodies) != 1 {
		t.Fatalf("sendMessage = %d, want 1 (подсказка)", len(bodies))
	}
	if strings.Contains(bodies[0], "Сообщение удалено") {
		t.Fatalf("без попытки удаления подсказка не должна говорить об удалении: %s", bodies[0])
	}
}

// TestJunkReplyHintSendFailureReleasesClaim — подсказка не доставлена (сетевой
// сбой): claim откатывается, следующее мусорное сообщение объясняет заново, но
// «+5 минут» второй раз не обещается — оно уже выдано первой попыткой.
func TestJunkReplyHintSendFailureReleasesClaim(t *testing.T) {
	ctx := context.Background()
	b, db, fc := newFlowBot(t)

	p := b.replies.Put(testChatID, testUserID, time.Now().Add(time.Minute), 0, 1, 0)
	if err := db.PutPendingReply(ctx, storage.PendingReply{
		ChatID: testChatID, UserID: testUserID, ExpiresAt: b.replies.Deadline(p), Stage: 1,
	}); err != nil {
		t.Fatal(err)
	}
	msg := func(id int) *telego.Message {
		return &telego.Message{
			MessageID: id,
			Chat:      telego.Chat{ID: testChatID, Type: "supergroup"},
			From:      &telego.User{ID: testUserID, FirstName: "Юзер"},
			Text:      "+",
		}
	}

	fc.errWhen = func(method string, _ *telegoapi.RequestData) bool {
		return method == "sendMessage"
	}
	b.handleJunkReply(msg(1), p) // подсказка провалилась, +5 всё равно выдано
	fc.errWhen = nil

	if d := b.replies.Deadline(p); time.Until(d) < 4*time.Minute {
		t.Fatalf("+5 должно выдаваться независимо от доставки подсказки: %v", d)
	}
	b.handleJunkReply(msg(2), p)
	bodies := fc.callBodies("sendMessage")
	// Первая подсказка (провалившаяся) тоже записала тело — ждём две попытки,
	// и повторная не должна обещать вторые +5.
	if len(bodies) != 2 {
		t.Fatalf("подсказка должна быть переотправлена после отката claim: %v", bodies)
	}
	if strings.Contains(bodies[1], "ещё 5 минут") {
		t.Fatalf("повторная подсказка не должна обещать вторые +5: %s", bodies[1])
	}
}

// TestReplyWaitLoopHonorsJunkExtension — «+5 минут» материально: цикл серии,
// проснувшись на СТАРОМ дедлайне, не двигает стадию и не карает, если срок был
// отодвинут мусорным ответом; серия продолжается уже по новому сроку.
func TestReplyWaitLoopHonorsJunkExtension(t *testing.T) {
	ctx := context.Background()
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	b.cfg.CaptchaStageInterval = 120 * time.Millisecond
	if err := db.SetReplyCheckEnabled(ctx, testChatID, true); err != nil {
		t.Fatal(err)
	}

	s := storage.ChatSettings{GreetingEnabled: true, ReplyCheckEnabled: true}
	if _, ok := b.sendGreetingAnchor(ctx, s, welcomeReply, testChatID, testUserID, 0, 1); !ok {
		t.Fatal("anchor send failed")
	}
	b.maybeArmReplyWait(s, testChatID, testUserID, 0)
	p, ok := b.replies.Get(testChatID, testUserID)
	if !ok {
		t.Fatal("wait must be armed")
	}
	// Гигиена: серия живёт независимым таймером — гасим её после теста, иначе
	// она продолжит стрелять по store/БД уже после завершения теста.
	t.Cleanup(func() {
		if cur, ok := b.replies.Take(testChatID, testUserID); ok {
			cur.Cancel()
		}
		for st := 1; st <= captchaStages; st++ {
			if err := db.DeletePendingReplyIf(context.Background(), testChatID, testUserID, st); err != nil {
				t.Errorf("cleanup pending reply stage %d: %v", st, err)
			}
		}
	})

	// Продлеваем задолго до старого дедлайна: цикл обязан проснуться на старом
	// сроке, увидеть будущий и просто перезавести сон.
	if _, ok := b.replies.Postpone(testChatID, testUserID, p, time.Now().Add(1500*time.Millisecond)); !ok {
		t.Fatal("postpone must be granted")
	}
	time.Sleep(500 * time.Millisecond)
	if rows, err := db.LoadAllPendingReplies(ctx); err != nil || len(rows) != 1 || rows[0].Stage != 1 {
		t.Fatalf("стадия уехала до продлённого срока: %v err=%v", rows, err)
	}
	if n := fc.callCount("sendMessage"); n != 1 {
		t.Fatalf("sendMessage = %d, want 1 (якорь, без напоминания и кары)", n)
	}
	// А после продлённого срока серия продолжается штатно.
	waitForWithin(t, 3*time.Second, func() bool {
		rows, err := db.LoadAllPendingReplies(ctx)
		return err == nil && len(rows) == 1 && rows[0].Stage == 2
	})
	// Серию гасим сразу: «продление уважается» уже доказано выше (стадия 1
	// на 500-й мс при старом сроке 120 мс, напоминания нет, стадия 2 пришла
	// не раньше продлённого срока), а живой серии под -race на загруженной
	// машине хватило бы пары интервалов до кары — тест ловил бы ложный фейл.
	if cur, ok := b.replies.Take(testChatID, testUserID); ok {
		cur.Cancel()
	}
	k := statsKinds(t, db, testChatID, testUserID)
	if k[storage.EventKick]+k[storage.EventBan] != 0 {
		t.Fatalf("продление истекло в кару: %v", k)
	}
}

// TestEditedMessageCountsAsReply — дописанный правкой ответ снимает ожидание
// (раньше правки не снимали его вообще, и юзер получал кик за «молчание» при
// формально написанном ответе); мусорная правка — не ответ и ничего не ломает.
func TestEditedMessageCountsAsReply(t *testing.T) {
	ctx := context.Background()

	setup := func(t *testing.T) (*Bot, *storage.DB) {
		t.Helper()
		b, db, _ := newFlowBot(t)
		serviceableChat(t, b, db, testChatID)
		p := b.replies.Put(testChatID, testUserID, time.Now().Add(time.Minute), 0, 1, 0)
		if err := db.PutPendingReply(ctx, storage.PendingReply{
			ChatID: testChatID, UserID: testUserID, ExpiresAt: b.replies.Deadline(p), Stage: 1,
		}); err != nil {
			t.Fatal(err)
		}
		return b, db
	}
	edit := func(text string) telego.Message {
		return telego.Message{
			MessageID: 321,
			Chat:      telego.Chat{ID: testChatID, Type: "supergroup"},
			From:      &telego.User{ID: testUserID, FirstName: "Юзер"},
			Text:      text,
		}
	}

	t.Run("осмысленная правка — пасс", func(t *testing.T) {
		b, db := setup(t)
		if err := b.handleEditedGroupMessage(nil, edit("ещё не бегала")); err != nil {
			t.Fatal(err)
		}
		if _, ok := b.replies.Get(testChatID, testUserID); ok {
			t.Fatal("wait must be consumed by a meaningful edit")
		}
		if k := statsKinds(t, db, testChatID, testUserID); k[storage.EventPass] != 1 {
			t.Fatalf("passes = %d, want 1", k[storage.EventPass])
		}
	})

	t.Run("мусорная правка — ожидание живо", func(t *testing.T) {
		b, db := setup(t)
		if err := b.handleEditedGroupMessage(nil, edit("+")); err != nil {
			t.Fatal(err)
		}
		if _, ok := b.replies.Get(testChatID, testUserID); !ok {
			t.Fatal("junk edit must not satisfy the wait")
		}
		if k := statsKinds(t, db, testChatID, testUserID); k[storage.EventPass] != 0 {
			t.Fatalf("passes = %d, want 0", k[storage.EventPass])
		}
	})
}

// TestReplyWaitMiddleware — обработка сообщений с взведённым ожиданием в
// миддлваре: настоящий ответ снимает ожидание (и НЕ глотается), мусор
// глотается и удаляется, команды — ни то ни другое (уходят в свои хендлеры,
// ожидание остаётся взведённым); без ожидания и в необслуживаемом чате метод
// не трогает ничего.
func TestReplyWaitMiddleware(t *testing.T) {
	armWait := func(t *testing.T, b *Bot, db *storage.DB) {
		t.Helper()
		p := b.replies.Put(testChatID, testUserID, time.Now().Add(time.Minute), 0, 1, 0)
		if err := db.PutPendingReply(context.Background(), storage.PendingReply{
			ChatID: testChatID, UserID: testUserID, ExpiresAt: b.replies.Deadline(p), Stage: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	msg := func(text string) *telego.Message {
		return &telego.Message{
			MessageID: 555,
			Chat:      telego.Chat{ID: testChatID, Type: "supergroup"},
			From:      &telego.User{ID: testUserID, FirstName: "Юзер"},
			Text:      text,
		}
	}

	t.Run("ответ — пасс, не глотается", func(t *testing.T) {
		b, db, fc := newFlowBot(t)
		serviceableChat(t, b, db, testChatID)
		armWait(t, b, db)
		if swallowed := b.replyWaitMiddleware(msg("ещё не бегала")); swallowed {
			t.Fatal("meaningful answer must not be swallowed")
		}
		if _, ok := b.replies.Get(testChatID, testUserID); ok {
			t.Fatal("wait must be consumed by the answer")
		}
		if k := statsKinds(t, db, testChatID, testUserID); k[storage.EventPass] != 1 {
			t.Fatalf("passes = %d, want 1", k[storage.EventPass])
		}
		if n := fc.callCount("deleteMessage"); n != 0 {
			t.Fatalf("deleteMessage = %d, want 0 (ответ не удаляем)", n)
		}
	})

	t.Run("мусор — глотается, удаляется, ожидание живо", func(t *testing.T) {
		b, db, fc := newFlowBot(t)
		serviceableChat(t, b, db, testChatID)
		armWait(t, b, db)
		if swallowed := b.replyWaitMiddleware(msg("+")); !swallowed {
			t.Fatal("junk must be swallowed")
		}
		// Удаление идёт в goSafe — дожидаемся асинхронного хвоста.
		waitFor(t, func() bool { return fc.callCount("deleteMessage") == 1 })
		if _, ok := b.replies.Get(testChatID, testUserID); !ok {
			t.Fatal("wait must stay armed after junk")
		}
		if k := statsKinds(t, db, testChatID, testUserID); k[storage.EventPass] != 0 {
			t.Fatalf("passes = %d, want 0 (мусор не ответ)", k[storage.EventPass])
		}
	})

	t.Run("команда — не ответ и не мусор", func(t *testing.T) {
		b, db, fc := newFlowBot(t)
		serviceableChat(t, b, db, testChatID)
		armWait(t, b, db)
		if swallowed := b.replyWaitMiddleware(msg("/ban")); swallowed {
			t.Fatal("command must not be swallowed")
		}
		if _, ok := b.replies.Get(testChatID, testUserID); !ok {
			t.Fatal("command must not satisfy the wait")
		}
		// goSafe-ветка мусора не должна даже стартовать: пауза на её след.
		time.Sleep(50 * time.Millisecond)
		if n := fc.callCount("deleteMessage"); n != 0 {
			t.Fatalf("deleteMessage = %d, want 0 (команду не удаляем)", n)
		}
	})

	t.Run("без ожидания — no-op", func(t *testing.T) {
		b, db, fc := newFlowBot(t)
		serviceableChat(t, b, db, testChatID)
		if swallowed := b.replyWaitMiddleware(msg("+")); swallowed {
			t.Fatal("no wait — nothing to do")
		}
		time.Sleep(50 * time.Millisecond)
		if n := fc.callCount("deleteMessage"); n != 0 {
			t.Fatalf("deleteMessage = %d, want 0", n)
		}
	})

	t.Run("необслуживаемый чат — мусор не трогаем", func(t *testing.T) {
		b, _, fc := newFlowBot(t)
		// Строки реестра нет → chatServiceable=false (fail-closed апрув):
		// ожидание взведено, но удалять сообщение в брошенном чате нельзя.
		b.replies.Put(testChatID, testUserID, time.Now().Add(time.Minute), 0, 1, 0)
		if swallowed := b.replyWaitMiddleware(msg("+")); swallowed {
			t.Fatal("unserviceable chat must not swallow")
		}
		time.Sleep(50 * time.Millisecond)
		if n := fc.callCount("deleteMessage"); n != 0 {
			t.Fatalf("deleteMessage = %d, want 0 (чат не обслуживаем)", n)
		}
	})
}
