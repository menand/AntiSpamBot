package bot

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegoapi"

	"github.com/menand/AntiSpamBot/internal/storage"
)

func TestQuarantineStoreLazyExpiry(t *testing.T) {
	s := newQuarantineStore()
	s.Set(1, 2, time.Now().Add(time.Hour), false)
	s.Set(1, 3, time.Now().Add(-time.Minute), true) // истёкший

	if _, ok := s.Get(1, 2); !ok {
		t.Fatal("active quarantine must be visible")
	}
	if _, ok := s.Get(1, 3); ok {
		t.Fatal("expired quarantine must lazily drop")
	}
	if _, ok := s.Get(1, 9); ok {
		t.Fatal("unknown user must not be quarantined")
	}

	if !s.ClaimWarned(1, 2) {
		t.Fatal("ClaimWarned must claim a fresh un-warned entry")
	}
	if e, _ := s.Get(1, 2); !e.Warned {
		t.Fatal("ClaimWarned must stick")
	}
	if s.ClaimWarned(1, 2) {
		t.Fatal("ClaimWarned must be first-claim-only")
	}
	s.ClearWarned(1, 2)
	if e, _ := s.Get(1, 2); e.Warned {
		t.Fatal("ClearWarned must roll the claim back")
	}
	s.Remove(1, 2)
	if _, ok := s.Get(1, 2); ok {
		t.Fatal("Remove must clear the entry")
	}
}

func TestQuarantineViolates(t *testing.T) {
	tests := []struct {
		name string
		msg  telego.Message
		want bool
	}{
		{"чистый текст", telego.Message{Text: "привет, как дела?"}, false},
		{"пустое сообщение", telego.Message{}, false},
		{"явный протокол", telego.Message{Text: "смотри http://spam.ru/x"}, true},
		{"https", telego.Message{Text: "https://example.com"}, true},
		{"www без протокола", telego.Message{Text: "зайди на www.spam.com"}, true},
		{"t.me", telego.Message{Text: "канал t.me/spam"}, true},
		{"deep-link tg://", telego.Message{Text: "tg://resolve?domain=x"}, true},
		{"ссылка только в подписи", telego.Message{
			Caption: "вот ссылка www.x.ru", Photo: []telego.PhotoSize{{FileID: "p"}},
		}, true},
		{"чистый текст с фото — вложение", telego.Message{
			Text: "вот фото", Photo: []telego.PhotoSize{{FileID: "p"}},
		}, true},
		{"стикер", telego.Message{Text: "", Sticker: &telego.Sticker{FileID: "s"}}, true},
		{"видимая ссылка в entities", telego.Message{Text: "скидки",
			Entities: []telego.MessageEntity{{Type: telego.EntityTypeTextLink, URL: "http://spam.ru"}}}, true},
		{"text_link без URL", telego.Message{Text: "скидки",
			Entities: []telego.MessageEntity{{Type: telego.EntityTypeTextLink}}}, false},
		{"tg:// в entities", telego.Message{Text: "жми",
			Entities: []telego.MessageEntity{{Type: telego.EntityTypeTextLink, URL: "tg://resolve?domain=x"}}}, true},
		{"magnet в entities", telego.Message{Text: "торрент",
			Entities: []telego.MessageEntity{{Type: telego.EntityTypeTextLink, URL: "magnet:?xt=urn:btih:abc"}}}, true},
		{"url-entity в entities (текст сам по себе ссылка)", telego.Message{Text: "https://site.ru",
			Entities: []telego.MessageEntity{{Type: telego.EntityTypeURL}}}, true},
		{"text_link в caption-entities (ссылка в подписи)", telego.Message{
			Caption:         "скидки",
			Photo:           []telego.PhotoSize{{FileID: "p"}},
			CaptionEntities: []telego.MessageEntity{{Type: telego.EntityTypeTextLink, URL: "magnet:?xt=urn:btih:abc"}},
		}, true},
		{"url-entity в caption-entities", telego.Message{
			Caption:         "site.ru",
			Photo:           []telego.PhotoSize{{FileID: "p"}},
			CaptionEntities: []telego.MessageEntity{{Type: telego.EntityTypeURL}},
		}, true},
		{"caption text_link без URL — не нарушение", telego.Message{
			Caption:         "просто текст",
			CaptionEntities: []telego.MessageEntity{{Type: telego.EntityTypeTextLink}},
		}, false},
		{"беневольная caption-entity при фото — фото всё равно нарушение", telego.Message{
			Caption:         "фото",
			Photo:           []telego.PhotoSize{{FileID: "p"}},
			CaptionEntities: []telego.MessageEntity{{Type: telego.EntityTypeCode}},
		}, true},
		{"просто болд, без ссылки", telego.Message{Text: "важно",
			Entities: []telego.MessageEntity{{Type: telego.EntityTypeBold}}}, false},
		{"форвард", telego.Message{Text: "пересланное",
			ForwardOrigin: &telego.MessageOriginChat{}}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := quarantineViolates(&tc.msg); got != tc.want {
				t.Fatalf("quarantineViolates() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestOnSuccessArmsQuarantine — на успешной капче при включённом карантине
// идёт рестрикт «только текст» (НЕ обычный release), ставится зеркало и БД-
// строка; release при этом не вызывается вовсе.
func TestOnSuccessArmsQuarantine(t *testing.T) {
	ctx := context.Background()
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	if err := db.SetQuarantineEnabled(ctx, testChatID, true); err != nil {
		t.Fatal(err)
	}

	pend := putCaptcha(b, db, testChatID, testUserID, 77)
	if err := b.onSuccess(ctx, pend, ""); err != nil {
		t.Fatal(err)
	}

	bodies := fc.callBodies("restrictChatMember")
	if len(bodies) != 1 {
		t.Fatalf("restrictChatMember calls = %d, want exactly 1 (только карантинный рестрикт)", len(bodies))
	}
	if !strings.Contains(bodies[0], `"can_add_web_page_previews":false`) {
		t.Fatalf("quarantine restrict body must be text-only: %s", bodies[0])
	}
	if strings.Contains(bodies[0], `"can_send_media_messages"`) {
		t.Fatalf("deprecated can_send_media_messages must not be set: %s", bodies[0])
	}

	if _, ok := b.quar.Get(testChatID, testUserID); !ok {
		t.Fatal("quarantine mirror must hold the user after pass")
	}
	ok, err := db.HasQuarantine(ctx, testChatID, testUserID, time.Now().Unix())
	if err != nil || !ok {
		t.Fatalf("quarantine row must persist: ok=%v err=%v", ok, err)
	}
}

// TestOnSuccessFallbackReleaseOnQuarantineFailure — провал карантинного
// рестрикта (все попытки) откатывается на обычный release: проверенного юзера
// нельзя оставить за бессрочным капча-мьютом, и никакой «фантомной» карантинной
// строки не появляется (арм — только после успеха). Лестницу укорачиваем, чтобы
// не спать 7 секунд на провалах.
func TestOnSuccessFallbackReleaseOnQuarantineFailure(t *testing.T) {
	old := tgBackoffs
	tgBackoffs = []time.Duration{0, time.Millisecond, 2 * time.Millisecond, 4 * time.Millisecond}
	t.Cleanup(func() { tgBackoffs = old })

	ctx := context.Background()
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	if err := db.SetQuarantineEnabled(ctx, testChatID, true); err != nil {
		t.Fatal(err)
	}
	// Падает ровно «только текст» (текст-онли подпись в теле), release-вызов
	// (all-true подпись) проходит на первой попытке.
	fc.errWhen = func(_ string, data *telegoapi.RequestData) bool {
		return data != nil && data.BodyRaw != nil &&
			strings.Contains(string(data.BodyRaw), `"can_add_web_page_previews":false`)
	}

	pend := putCaptcha(b, db, testChatID, testUserID, 77)
	if err := b.onSuccess(ctx, pend, ""); err != nil {
		t.Fatalf("fallback release must succeed: %v", err)
	}

	bodies := fc.callBodies("restrictChatMember")
	var sawTextOnly, sawAllTrue bool
	for _, body := range bodies {
		if strings.Contains(body, `"can_add_web_page_previews":false`) {
			sawTextOnly = true
		}
		if strings.Contains(body, `"can_add_web_page_previews":true`) {
			sawAllTrue = true
		}
	}
	if !sawTextOnly {
		t.Fatal("quarantine restrict must be attempted first")
	}
	if !sawAllTrue {
		t.Fatalf("fallback release must run after quarantine failure, bodies: %v", bodies)
	}

	if _, ok := b.quar.Get(testChatID, testUserID); ok {
		t.Fatal("no quarantine may be armed when the restrict failed")
	}
	ok, err := db.HasQuarantine(ctx, testChatID, testUserID, time.Now().Unix())
	if err != nil || ok {
		t.Fatalf("no quarantine row may persist on failure: ok=%v err=%v", ok, err)
	}
}

// TestQuarantineReleaseChatClearsRows — выключение карантина отпускает всех
// активных карантинных юзеров (release) и чистит зеркало с БД; чужие чаты
// не трогаются.
func TestQuarantineReleaseChatClearsRows(t *testing.T) {
	ctx := context.Background()
	b, db, _ := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)

	// Две строки в тестовом чате и одна в соседнем.
	b.armQuarantine(ctx, testChatID, 101, time.Now().Add(time.Hour))
	b.armQuarantine(ctx, testChatID, 102, time.Now().Add(2*time.Hour))
	b.armQuarantine(ctx, -200200, 103, time.Now().Add(time.Hour))

	b.quarantineReleaseChat(testChatID)

	if _, ok := b.quar.Get(testChatID, 101); ok {
		t.Fatal("chat-1 quarantine must be removed from mirror")
	}
	if _, ok := b.quar.Get(testChatID, 102); ok {
		t.Fatal("chat-1 quarantine must be removed from mirror")
	}
	ok, err := db.HasQuarantine(ctx, testChatID, 101, time.Now().Unix())
	if err != nil || ok {
		t.Fatalf("chat-1 row must be deleted: ok=%v err=%v", ok, err)
	}
	// Соседний чат жив.
	if _, ok := b.quar.Get(-200200, 103); !ok {
		t.Fatal("other chat's quarantine must survive")
	}
	ok, err = db.HasQuarantine(ctx, -200200, 103, time.Now().Unix())
	if err != nil || !ok {
		t.Fatalf("other chat's row must survive: ok=%v err=%v", ok, err)
	}
}

// TestCensorQuarantineMessage — первое нарушение удаляется + уходит разовое
// эфемерное пояснение; второе — только тихое удаление. warned персистится.
func TestCensorQuarantineMessage(t *testing.T) {
	ctx := context.Background()
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	b.armQuarantine(ctx, testChatID, testUserID, time.Now().Add(time.Hour))

	m := telego.Message{
		MessageID: 123,
		Chat:      telego.Chat{ID: testChatID, Type: "supergroup"},
		From:      &telego.User{ID: testUserID, IsBot: false},
		Text:      "смотри http://spam.ru",
	}
	e, _ := b.quar.Get(testChatID, testUserID)
	b.censorQuarantineMessage(&m, e)

	if fc.callCount("deleteMessage") != 1 {
		t.Fatalf("deleteMessage calls = %d, want 1", fc.callCount("deleteMessage"))
	}
	if fc.callCount("sendMessage") != 1 {
		t.Fatalf("sendMessage calls = %d, want 1 (разовое пояснение)", fc.callCount("sendMessage"))
	}
	msgs := fc.callBodies("sendMessage")
	if len(msgs) != 1 || !strings.Contains(msgs[0], "🧫") ||
		!strings.Contains(msgs[0], fmt.Sprintf(`"receiver_user_id":%d`, testUserID)) {
		t.Fatalf("notice must be ephemeral to the offender: %v", msgs)
	}

	// Второе нарушение — тихо (warned уже true и в зеркале, и в БД; цензор в
	// проде берёт свежее значение из Get на каждое сообщение).
	fresh, _ := b.quar.Get(testChatID, testUserID)
	if !fresh.Warned {
		t.Fatal("mirror must reflect the sent notice immediately")
	}
	b.censorQuarantineMessage(&m, fresh)
	if fc.callCount("sendMessage") != 1 {
		t.Fatalf("second violation must not re-notify, sendMessage = %d", fc.callCount("sendMessage"))
	}

	rows, err := db.AllQuarantines(ctx, testChatID, time.Now().Unix())
	if err != nil || len(rows) != 1 || !rows[0].Warned {
		t.Fatalf("warned must persist: rows=%+v err=%v", rows, err)
	}
}

// TestRestoreQuarantineSeedsMirror — рестарт поднимает активные строки в
// зеркало цензора, истёкшие — нет, а строки чатов с ВЫКЛЮЧЕННЫМ карантином
// вместо сида отпускаются (крэш между тогглом и отпуском не переживает
// рестарт) и сносятся.
func TestRestoreQuarantineSeedsMirror(t *testing.T) {
	ctx := context.Background()
	b, db, fc := newFlowBot(t)

	// Чат 1 — карантин включён; чат 2 — выключен (leave-default строки настроек).
	if err := db.SetQuarantineEnabled(ctx, 1, true); err != nil {
		t.Fatal(err)
	}
	if err := db.PutQuarantine(ctx, 1, 10, time.Now().Add(time.Hour), true); err != nil {
		t.Fatal(err)
	}
	if err := db.PutQuarantine(ctx, 1, 11, time.Now().Add(-time.Minute), false); err != nil {
		t.Fatal(err)
	}
	if err := db.PutQuarantine(ctx, 2, 20, time.Now().Add(time.Hour), false); err != nil {
		t.Fatal(err)
	}
	b.restoreQuarantine(ctx)

	if e, ok := b.quar.Get(1, 10); !ok || !e.Warned {
		t.Fatalf("active row of enabled chat must be seeded: e=%+v ok=%v", e, ok)
	}
	if _, ok := b.quar.Get(1, 11); ok {
		t.Fatal("expired row must not be seeded")
	}
	if _, ok := b.quar.Get(2, 20); ok {
		t.Fatal("disabled-chat row must not be seeded")
	}
	ok, err := db.HasQuarantine(ctx, 2, 20, time.Now().Unix())
	if err != nil || ok {
		t.Fatalf("disabled-chat row must be dropped: ok=%v err=%v", ok, err)
	}
	if fc.callCount("restrictChatMember") == 0 {
		t.Fatal("disabled-chat quarantine must be released server-side on restore")
	}
}

// TestQuarantineRekey — миграция чата переносит зеркальные записи на новый
// chat_id, истёкшие при этом роняет (крэш между BeginKickoff-дедупом и сменой
// id не переживает рестарт), чужие чаты не трогает.
func TestQuarantineRekey(t *testing.T) {
	s := newQuarantineStore()
	s.Set(1, 10, time.Now().Add(time.Hour), false)
	s.Set(1, 11, time.Now().Add(-time.Minute), true) // истёкший — долой
	s.Set(2, 20, time.Now().Add(time.Hour), false)

	if n := s.Rekey(1, 3); n != 1 {
		t.Fatalf("Rekey moved %d entries, want 1", n)
	}
	if _, ok := s.Get(3, 10); !ok {
		t.Fatal("active entry must move to the new chat")
	}
	if _, ok := s.Get(1, 10); ok {
		t.Fatal("old chat must not hold the moved entry")
	}
	if _, ok := s.Get(3, 11); ok {
		t.Fatal("expired entry must be dropped, not moved")
	}
	if _, ok := s.Get(2, 20); !ok {
		t.Fatal("other chat must be untouched")
	}
	if _, ok := s.Get(1, 11); ok {
		t.Fatal("expired entry must not linger on the source chat")
	}
}

// TestCensorConcurrentSingleNotice — два параллельных цензора одного юзера:
// удаляются оба сообщения, но эфемерное пояснение уходит ровно ОДНО (гонку
// снапшот-warned ↔ ClaimWarned решает атомарный клейм, а не чтение).
func TestCensorConcurrentSingleNotice(t *testing.T) {
	ctx := context.Background()
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	b.armQuarantine(ctx, testChatID, testUserID, time.Now().Add(time.Hour))

	msgs := []*telego.Message{
		{MessageID: 1, Chat: telego.Chat{ID: testChatID, Type: "supergroup"},
			From: &telego.User{ID: testUserID}, Text: "http://a.ru"},
		{MessageID: 2, Chat: telego.Chat{ID: testChatID, Type: "supergroup"},
			From: &telego.User{ID: testUserID}, Text: "http://b.ru"},
	}
	var wg sync.WaitGroup
	for _, m := range msgs {
		wg.Add(1)
		go func(m *telego.Message) {
			defer wg.Done()
			e, _ := b.quar.Get(testChatID, testUserID)
			b.censorQuarantineMessage(m, e)
		}(m)
	}
	wg.Wait()

	if fc.callCount("deleteMessage") != 2 {
		t.Fatalf("deleteMessage calls = %d, want 2 (оба сообщения)", fc.callCount("deleteMessage"))
	}
	if n := fc.callCount("sendMessage"); n != 1 {
		t.Fatalf("sendMessage calls = %d, want 1 (один победитель клейма)", n)
	}
}

// TestQuarantineEscalationMutesFlood — N-е нарушение в окне не просто удаляет,
// а эскалирует: рид-онли мьют, снятие карантинной строки и зеркала, событие
// mute (кормит /unmute last-10).
func TestQuarantineEscalationMutesFlood(t *testing.T) {
	ctx := context.Background()
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	b.armQuarantine(ctx, testChatID, testUserID, time.Now().Add(time.Hour))

	for i := 1; i <= censorEscalateAfter; i++ {
		m := telego.Message{
			MessageID: i, Chat: telego.Chat{ID: testChatID, Type: "supergroup"},
			From: &telego.User{ID: testUserID}, Text: "http://spam.ru",
		}
		e, _ := b.quar.Get(testChatID, testUserID)
		b.censorQuarantineMessage(&m, e)
	}

	ok, err := db.HasQuarantine(ctx, testChatID, testUserID, time.Now().Unix())
	if err != nil || ok {
		t.Fatalf("quarantine row must be dropped on escalation: ok=%v err=%v", ok, err)
	}
	if _, ok := b.quar.Get(testChatID, testUserID); ok {
		t.Fatal("escalation must clear the mirror")
	}
	if fc.callCount("restrictChatMember") != 1 {
		t.Fatalf("escalation mute calls = %d, want 1", fc.callCount("restrictChatMember"))
	}
	recent, err := db.RecentEventUsers(ctx, testChatID, 10, []storage.EventKind{storage.EventMute}, nil)
	if err != nil || len(recent) != 1 || recent[0].UserID != testUserID {
		t.Fatalf("escalation mute event expected: recent=%+v err=%v", recent, err)
	}
}

// TestReconcileQuarantinesReleasesDisabledChats — сверилка отпускает «фантомные»
// строки чатов с ВЫКЛЮЧЕННЫМ карантином (тоггл во время карантина), не трогая
// остальные чаты.
func TestReconcileQuarantinesReleasesDisabledChats(t *testing.T) {
	ctx := context.Background()
	b, db, _ := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)

	// Карантинный юзер; потом тоггл OFF — строка превращается в «фантомную».
	b.armQuarantine(ctx, testChatID, testUserID, time.Now().Add(time.Hour))
	if err := db.SetQuarantineEnabled(ctx, testChatID, false); err != nil {
		t.Fatal(err)
	}
	// Второй чат: карантин включён, строка должна пережить сверилку.
	b.armQuarantine(ctx, -200200, 103, time.Now().Add(time.Hour))
	if err := db.SetQuarantineEnabled(ctx, -200200, true); err != nil {
		t.Fatal(err)
	}

	b.reconcileQuarantines(ctx)

	ok, err := db.HasQuarantine(ctx, testChatID, testUserID, time.Now().Unix())
	if err != nil || ok {
		t.Fatalf("disabled-chat row must be released: ok=%v err=%v", ok, err)
	}
	if _, ok := b.quar.Get(testChatID, testUserID); ok {
		t.Fatal("disabled-chat mirror must be cleared")
	}
	if _, ok := b.quar.Get(-200200, 103); !ok {
		t.Fatal("enabled chat's quarantine must survive reconcile")
	}
}

// TestPromotionReleasesQuarantine — назначение админом мгновенно снимает
// карантин (и зеркало, и БД): новому админу цензор не нужен.
func TestPromotionReleasesQuarantine(t *testing.T) {
	ctx := context.Background()
	b, db, _ := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	b.armQuarantine(ctx, testChatID, testUserID, time.Now().Add(time.Hour))

	from := telego.User{ID: 42, IsBot: true, Username: "antispam_bot"}
	upd := memberUpdate(testChatID, from, telego.User{ID: testUserID}, "member", "administrator")
	if err := b.handleChatMember(nil, upd); err != nil {
		t.Fatal(err)
	}

	if _, ok := b.quar.Get(testChatID, testUserID); ok {
		t.Fatal("promotion must clear the quarantine mirror")
	}
	ok, err := db.HasQuarantine(ctx, testChatID, testUserID, time.Now().Unix())
	if err != nil || ok {
		t.Fatalf("promotion must drop the quarantine row: ok=%v err=%v", ok, err)
	}
	recent, err := db.RecentEventUsers(ctx, testChatID, 10, []storage.EventKind{storage.EventPass}, nil)
	if err == nil && len(recent) != 0 {
		t.Fatalf("promotion must not record a pass event: recent=%+v", recent)
	}
}

// TestTrustCommandReleasesQuarantine — /trust админа на карантинного юзера
// (реплаем) снимает карантин и подтверждает; повторный вызов без активного
// карантина честно отвечает «нет активного карантина». Наказания и защита
// целей — это общий mod-код (покрыт в flow2_test).
func TestTrustCommandReleasesQuarantine(t *testing.T) {
	ctx := context.Background()
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	b.armQuarantine(ctx, testChatID, testUserID, time.Now().Add(time.Hour))

	// Живая проверка админа в этом харнессе отвечает некорректно (голый true),
	// поэтому админ и жертва сидят в кэше: админ — положительно, жертва —
	// отрицательно (guardModTarget не должен принять админа за цель).
	seedAdminCache(b, testChatID, 9, true)
	seedAdminCache(b, testChatID, testUserID, false)

	trust := func() telego.Message {
		return telego.Message{
			MessageID: 22,
			Chat:      telego.Chat{ID: testChatID, Type: "supergroup"},
			From:      &telego.User{ID: 9, FirstName: "Аня"},
			Text:      "/trust",
			ReplyToMessage: &telego.Message{
				MessageID: 33,
				Chat:      telego.Chat{ID: testChatID, Type: "supergroup"},
				From:      &telego.User{ID: testUserID, FirstName: "Новичок"},
			},
		}
	}

	if err := b.handleTrustCommand(nil, trust()); err != nil {
		t.Fatal(err)
	}
	ok, err := db.HasQuarantine(ctx, testChatID, testUserID, time.Now().Unix())
	if err != nil || ok {
		t.Fatalf("trust must drop the quarantine row: ok=%v err=%v", ok, err)
	}
	if _, ok := b.quar.Get(testChatID, testUserID); ok {
		t.Fatal("trust must clear the mirror")
	}
	conf := strings.Join(fc.callBodies("sendMessage"), " ")
	if !strings.Contains(conf, "Карантин снят") {
		t.Fatalf("confirmation expected, got: %s", conf)
	}

	// Повторный вызов — карантина уже нет: честный ответ вместо порицания.
	// Смотрим ТОЛЬКО сообщения, появившиеся после первого вызова — конкатенация
	// всей истории после первого «Карантин снят» давала бы ложный позитив
	// (пустой ответ всё равно «содержал» бы фразу из первого раза).
	before := fc.callCount("sendMessage")
	if err := b.handleTrustCommand(nil, trust()); err != nil {
		t.Fatal(err)
	}
	later := fc.callBodies("sendMessage")
	if len(later) <= before {
		t.Fatal("expected a reply to the second /trust")
	}
	conf = strings.Join(later[before:], " ")
	if !strings.Contains(conf, "нет активного карантина") {
		t.Fatalf("no-quarantine reply expected, got: %s", conf)
	}
	if strings.Contains(conf, "Карантин снят") {
		t.Fatalf("second call must not claim a release: %s", conf)
	}
}

// TestEscalationDisarmsReplyWait — эскалация в рид-онли мьют (порог
// нарушений цензора) снимает активный reply-wait с компенсирующим pass
// (замьюченный физически не сможет ответить на приветствие) и убирает
// карантинное состояние БЕЗ серверного release (дефолтные права чата
// отменили бы только что наложенный мьют).
func TestEscalationDisarmsReplyWait(t *testing.T) {
	ctx := context.Background()
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	b.armQuarantine(ctx, testChatID, testUserID, time.Now().Add(time.Hour))
	b.replies.Put(testChatID, testUserID, time.Now().Add(time.Minute), 0, 1, 0)

	b.escalateQuarantine(testChatID, testUserID)

	// Ровно один рестрикт — сам мьют. Серверный release (второй
	// restrictChatMember с дефолтными правами) НЕ вызывался.
	if n := fc.callCount("restrictChatMember"); n != 1 {
		t.Fatalf("restrictChatMember calls = %d, want 1 (mute only, no release)", n)
	}
	if _, ok := b.replies.Get(testChatID, testUserID); ok {
		t.Fatal("escalation must cancel the reply-wait")
	}
	passed, err := db.RecentEventUsers(ctx, testChatID, 10,
		[]storage.EventKind{storage.EventPass}, nil)
	if err != nil || len(passed) != 1 || passed[0].UserID != testUserID {
		t.Fatalf("escalation must record a compensating pass: %+v err=%v", passed, err)
	}
	mutes, err := db.RecentEventUsers(ctx, testChatID, 10,
		[]storage.EventKind{storage.EventMute}, []string{storage.ReasonQuarantine})
	if err != nil || len(mutes) != 1 || mutes[0].UserID != testUserID {
		t.Fatalf("escalation must record a quarantine mute: %+v err=%v", mutes, err)
	}
	has, err := db.HasQuarantine(ctx, testChatID, testUserID, time.Now().Unix())
	if err != nil || has {
		t.Fatalf("escalation must drop the quarantine row: has=%v err=%v", has, err)
	}
	if _, ok := b.quar.Get(testChatID, testUserID); ok {
		t.Fatal("escalation must clear the quarantine mirror")
	}
}

// TestPromotionForgivesSpamBan — промоушен разбаненного вручную спамера
// (kicked → administrator, рука человека) успевает И простить глобальный
// флаг (хук прощения стоит ДО ветки промоушена), И снять карантин: ранний
// return в промоушене не должен «перекрыть» forgiveness-хук.
func TestPromotionForgivesSpamBan(t *testing.T) {
	ctx := context.Background()
	b, db, _ := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	if err := db.AddSpamBanned(ctx, testUserID, testChatID, time.Now()); err != nil {
		t.Fatal(err)
	}
	b.armQuarantine(ctx, testChatID, testUserID, time.Now().Add(time.Hour))

	from := telego.User{ID: 9, FirstName: "Админ"} // человек, не бот (и не сама жертва)
	upd := memberUpdate(testChatID, from, telego.User{ID: testUserID}, "kicked", "administrator")
	if err := b.handleChatMember(nil, upd); err != nil {
		t.Fatal(err)
	}

	banned, err := db.IsSpamBanned(ctx, testUserID)
	if err != nil || banned {
		t.Fatalf("promotion of unbanned spammer must forgive the global flag: banned=%v err=%v", banned, err)
	}
	has, err := db.HasQuarantine(ctx, testChatID, testUserID, time.Now().Unix())
	if err != nil || has {
		t.Fatalf("promotion must drop the quarantine row: has=%v err=%v", has, err)
	}
	if _, ok := b.quar.Get(testChatID, testUserID); ok {
		t.Fatal("promotion must clear the quarantine mirror")
	}
}

// TestCensorNoticeThreaded — разовое пояснение нарушителя карантина в
// форум-топике уходит в тот же топик (message_thread_id) — не в General.
func TestCensorNoticeThreaded(t *testing.T) {
	ctx := context.Background()
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	b.armQuarantine(ctx, testChatID, testUserID, time.Now().Add(time.Hour))

	m := telego.Message{
		MessageID:       123,
		MessageThreadID: 55,
		Chat:            telego.Chat{ID: testChatID, Type: "supergroup"},
		From:            &telego.User{ID: testUserID},
		Text:            "смотри http://spam.ru",
	}
	e, _ := b.quar.Get(testChatID, testUserID)
	b.censorQuarantineMessage(&m, e)

	msgs := fc.callBodies("sendMessage")
	if len(msgs) != 1 || !strings.Contains(msgs[0], `"message_thread_id":55`) {
		t.Fatalf("notice must go to the offending topic: %v", msgs)
	}
}
