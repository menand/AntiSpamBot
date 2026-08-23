package bot

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegoapi"

	"github.com/menand/AntiSpamBot/internal/captcha"
	"github.com/menand/AntiSpamBot/internal/storage"
)

// TestCaptchaSeriesThreeStages — молчание проходит все три стадии серии и
// только потом наказывается ОДИН раз: два промежуточных таймаута не пишут
// событий, не двигают attempts и каждый раз шлют новое сообщение капчи,
// удаляя предыдущее.
func TestCaptchaSeriesThreeStages(t *testing.T) {
	ctx := context.Background()
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	b.cfg.CaptchaStageInterval = 40 * time.Millisecond

	b.runCaptcha(testChatID, telego.User{ID: testUserID, FirstName: "Юзер"}, 0)

	if rows := pendingRows(t, db); len(rows) != 1 || rows[0].Stage != 1 {
		t.Fatalf("stage 1 must be armed: %+v", rows)
	}

	// Переход персистится ДО отправки напоминания, так что ждём отправку,
	// а не строку стадии.
	waitFor(t, func() bool { return fc.callCount("sendMessage") >= 2 })
	waitFor(t, func() bool {
		rows := pendingRows(t, db)
		return len(rows) == 1 && rows[0].Stage == 2
	})
	if n := fc.callCount("sendMessage"); n != 2 {
		t.Fatalf("sendMessage calls = %d, want 2 (стадия 2 отправлена)", n)
	}
	// Публичная серия обязана нести ряд «Впустить» на каждом сообщении.
	for i, body := range fc.callBodies("sendMessage") {
		if !strings.Contains(body, "capok:") {
			t.Fatalf("публичная стадия %d без ряда «Впустить»: %s", i+1, body)
		}
	}
	kinds := statsKinds(t, db, testChatID, testUserID)
	for k, n := range kinds {
		if n != 0 {
			t.Fatalf("промежуточный таймаут записал событие %s (%v)", k, kinds)
		}
	}
	if n, err := db.AttemptCount(ctx, testChatID, testUserID, attemptsTTL); err != nil || n != 0 {
		t.Fatalf("attempts после стадии 2 = %d (err %v), want 0 — серия ещё не провалена", n, err)
	}

	waitFor(t, func() bool {
		rows := pendingRows(t, db)
		return len(rows) == 1 && rows[0].Stage == 3
	})

	waitFor(t, func() bool {
		k := statsKinds(t, db, testChatID, testUserID)
		return k[storage.EventKick] == 1 && k[storage.EventBan] == 0
	})
	if n := fc.callCount("sendMessage"); n != 3 {
		t.Fatalf("sendMessage calls = %d, want 3 (три сообщения серии)", n)
	}
	// Два перехода между стадиями + удаление в onFail.
	if n := fc.callCount("deleteMessage"); n != 3 {
		t.Fatalf("deleteMessage calls = %d, want 3", n)
	}
	if n, err := db.AttemptCount(ctx, testChatID, testUserID, attemptsTTL); err != nil || n != 1 {
		t.Fatalf("attempts после серии = %d (err %v), want 1 — вся серия одна попытка", n, err)
	}
	if rows := pendingRows(t, db); len(rows) != 0 {
		t.Fatalf("pending rows = %d, want 0 после наказания", len(rows))
	}
}

// TestCaptchaSeriesWrongClickKicksImmediately — активная неверная кнопка
// проваливает капчу сразу, серия напоминаний для промолчавших её не касается.
func TestCaptchaSeriesWrongClickKicksImmediately(t *testing.T) {
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	putCaptcha(b, db, testChatID, testUserID, 100)

	query := telego.CallbackQuery{
		ID:   "q",
		From: telego.User{ID: testUserID, FirstName: "Юзер"},
		Data: "cap:" + strconv.FormatInt(testUserID, 10) + ":0", // неверно (верный 2)
		Message: &telego.Message{
			MessageID: 100,
			Chat:      telego.Chat{ID: testChatID, Type: "supergroup"},
		},
	}
	if err := b.handleCallback(nil, query); err != nil {
		t.Fatal(err)
	}

	waitFor(t, func() bool {
		k := statsKinds(t, db, testChatID, testUserID)
		return k[storage.EventKick] == 1
	})
	if n := fc.callCount("sendMessage"); n != 0 {
		t.Fatalf("неверный клик не должен продолжать серию, sendMessage = %d", n)
	}
	if _, ok := b.store.Get(testChatID, testUserID); ok {
		t.Fatal("captcha must be taken by the failing click")
	}
}

// TestRestoreResumesRemainingSeriesStages — рестарт на середине серии
// доигрывает остаток: стадия 2 по истечении переходит в 3, финал наказывает.
func TestRestoreResumesRemainingSeriesStages(t *testing.T) {
	ctx := context.Background()
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	b.cfg.CaptchaStageInterval = 40 * time.Millisecond

	if err := db.PutPending(ctx, storage.PendingRow{
		ChatID: testChatID, UserID: testUserID, MessageID: 10,
		CorrectIdx: 1,
		// expires_at хранится в секундах: дедлайн короче ~1 c усёкся бы в
		// «истёкший за простой» с клампом к финальной стадии.
		ExpiresAt: time.Now().Add(1500 * time.Millisecond),
		Stage:     2,
	}); err != nil {
		t.Fatal(err)
	}

	n, err := b.restorePending(ctx)
	if err != nil || n != 1 {
		t.Fatalf("restored = %d err=%v, want 1/nil", n, err)
	}

	waitFor(t, func() bool { return fc.callCount("sendMessage") >= 1 })
	waitFor(t, func() bool {
		rows := pendingRows(t, db)
		return len(rows) == 1 && rows[0].Stage == 3
	})
	if n := fc.callCount("sendMessage"); n != 1 {
		t.Fatalf("sendMessage calls = %d, want 1 (только стадия 3)", n)
	}

	waitFor(t, func() bool {
		k := statsKinds(t, db, testChatID, testUserID)
		return k[storage.EventKick] == 1
	})
	if rows := pendingRows(t, db); len(rows) != 0 {
		t.Fatalf("pending rows = %d, want 0", len(rows))
	}
}

// waitForWithin — waitFor с нестандартным дедлайном (определён в
// review_fixes_test.go): для путей длиннее пяти секунд, например лестницы
// ретраев отправки.
func waitForWithin(t *testing.T, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached within deadline")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestReplyWaitLatePassDeletesReminder — поздний ответ (стадия 2+) сносит
// напоминание-якорь и пишет пасс; ответ на первой стадии приветствие хранит.
func TestReplyWaitLatePassDeletesReminder(t *testing.T) {
	ctx := context.Background()

	t.Run("поздний ответ — напоминание удалено", func(t *testing.T) {
		b, db, fc := newFlowBot(t)
		if err := db.PutGreeting(ctx, testChatID, testUserID, 500, time.Now()); err != nil {
			t.Fatal(err)
		}
		p := b.replies.Put(testChatID, testUserID, time.Now().Add(time.Minute), 0, 2)
		if err := db.PutPendingReply(ctx, storage.PendingReply{
			ChatID: testChatID, UserID: testUserID, ExpiresAt: p.ExpiresAt, Stage: 2,
		}); err != nil {
			t.Fatal(err)
		}

		b.replyWaitSatisfied(testChatID, testUserID)

		if n := fc.callCount("deleteMessage"); n != 1 {
			t.Fatalf("deleteMessage calls = %d, want 1 (якорь стадии 2)", n)
		}
		if _, ok, err := db.TakeGreetingMsg(ctx, testChatID, testUserID); err != nil || ok {
			t.Fatalf("greeting row must be consumed (ok=%v err=%v)", ok, err)
		}
		kinds := statsKinds(t, db, testChatID, testUserID)
		if kinds[storage.EventPass] != 1 {
			t.Fatalf("passes = %d, want 1", kinds[storage.EventPass])
		}
		if rows, err := db.LoadAllPendingReplies(ctx); err != nil || len(rows) != 0 {
			t.Fatalf("pending_replies = %v err=%v, want empty", rows, err)
		}
	})

	t.Run("ответ на первой стадии — приветствие остаётся", func(t *testing.T) {
		b, db, fc := newFlowBot(t)
		if err := db.PutGreeting(ctx, testChatID, testUserID, 500, time.Now()); err != nil {
			t.Fatal(err)
		}
		p := b.replies.Put(testChatID, testUserID, time.Now().Add(time.Minute), 0, 1)
		if err := db.PutPendingReply(ctx, storage.PendingReply{
			ChatID: testChatID, UserID: testUserID, ExpiresAt: p.ExpiresAt, Stage: 1,
		}); err != nil {
			t.Fatal(err)
		}

		b.replyWaitSatisfied(testChatID, testUserID)

		if n := fc.callCount("deleteMessage"); n != 0 {
			t.Fatalf("deleteMessage calls = %d, want 0 (обычное приветствие)", n)
		}
	})
}

// TestCaptchaSeriesDepartedMidSeries — юзер вышел между стадиями (лево-апдейт
// потерян): liveness-перепроверка перед Put закрывает воронку left'ом с
// размьютом, напоминание не остаётся в чате, наказания нет.
func TestCaptchaSeriesDepartedMidSeries(t *testing.T) {
	ctx := context.Background()
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	b.cfg.CaptchaStageInterval = 40 * time.Millisecond
	fc.resp["getChatMember"] = `{"status":"left","user":{"id":7,"is_bot":false,"first_name":"Юзер"}}`

	// Живая строка стадии 1 с вот-вот истекающим дедлайном.
	if err := db.PutPending(ctx, storage.PendingRow{
		ChatID: testChatID, UserID: testUserID, MessageID: 10,
		CorrectIdx: 2, ExpiresAt: time.Now().Add(1500 * time.Millisecond), Stage: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if n, err := b.restorePending(ctx); err != nil || n != 1 {
		t.Fatalf("restored = %d err=%v, want 1/nil", n, err)
	}

	waitFor(t, func() bool {
		k := statsKinds(t, db, testChatID, testUserID)
		return k[storage.EventLeft] == 1
	})
	k := statsKinds(t, db, testChatID, testUserID)
	if k[storage.EventKick]+k[storage.EventBan]+k[storage.EventAbort] != 0 {
		t.Fatalf("ушедший между стадиями наказан: %v", k)
	}
	// Публичная отправка ушедшему проходит успешно — liveness ловит её
	// след и удаляет только что отправленное сообщение.
	waitFor(t, func() bool { return fc.callCount("deleteMessage") >= 2 })
	waitFor(t, func() bool { return fc.callCount("restrictChatMember") >= 1 }) // releaseOnAbort
	if rows := pendingRows(t, db); len(rows) != 0 {
		t.Fatalf("pending rows = %d, want 0", len(rows))
	}
}

// TestReplyWaitSeriesThreeStages — серия напоминаний зеркалит капчу: два
// промежуточных таймаута без событий, эскалация текста, топик соблюдён,
// ровно один noreply-кик в конце.
func TestReplyWaitSeriesThreeStages(t *testing.T) {
	ctx := context.Background()
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	b.cfg.CaptchaStageInterval = 40 * time.Millisecond
	// Цикл перечитывает настройки из БД на каждой стадии: тумблер обязан
	// быть персистентным, иначе напоминания уйдут без строк-требований.
	if err := db.SetReplyCheckEnabled(ctx, testChatID, true); err != nil {
		t.Fatal(err)
	}

	s := storage.ChatSettings{GreetingEnabled: true, ReplyCheckEnabled: true}
	if _, ok := b.sendGreetingAnchor(ctx, s, testChatID, testUserID, 77, 1); !ok {
		t.Fatal("anchor send failed")
	}
	b.maybeArmReplyWait(s, testChatID, testUserID, 77)

	waitFor(t, func() bool {
		k := statsKinds(t, db, testChatID, testUserID)
		return k[storage.EventKick] == 1 && k[storage.EventBan] == 0 && k[storage.EventPass] == 0
	})
	bodies := fc.callBodies("sendMessage")
	if len(bodies) != 3 {
		t.Fatalf("sendMessage = %d, want 3 (якорь + напоминание + предупреждение)", len(bodies))
	}
	if !strings.Contains(bodies[1], "Напоминание") || !strings.Contains(bodies[2], "ПОСЛЕДНЕЕ ПРЕДУПРЕЖДЕНИЕ") {
		t.Fatalf("тексты стадий не эскалируют: %q / %q", bodies[1], bodies[2])
	}
	if !strings.Contains(bodies[1], `"message_thread_id":77`) {
		t.Fatalf("напоминание ушло мимо топика: %s", bodies[1])
	}
	// Якорь ст.1 при переходе, якорь ст.2 при переходе, якорь ст.3 перед карой.
	if n := fc.callCount("deleteMessage"); n != 3 {
		t.Fatalf("deleteMessage = %d, want 3", n)
	}
	list, err := db.RecentEventUsers(ctx, testChatID, 10,
		[]storage.EventKind{storage.EventKick}, []string{storage.ReasonNoReply})
	if err != nil || len(list) != 1 || list[0].UserID != testUserID {
		t.Fatalf("want one noreply kick, got %+v err=%v", list, err)
	}
}

// TestReplyWaitReminderFailDisarmsWithPass — упавшее напоминание не карает:
// серия снимается с компенсирующим пассом (предыдущий якорь уже удалён —
// юзер требования не видел).
func TestReplyWaitReminderFailDisarmsWithPass(t *testing.T) {
	ctx := context.Background()
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	// Ломаем только эскалации: 400 не ретраится по смыслу, но лестницу
	// прогоняет; берём его вместо 429, чтобы не ждать лишних секунд.
	fc.errWhen = func(_ string, data *telegoapi.RequestData) bool {
		return data != nil && strings.Contains(string(data.BodyRaw), "Напоминание")
	}
	s := storage.ChatSettings{GreetingEnabled: true, ReplyCheckEnabled: true}
	b.cfg.CaptchaStageInterval = 40 * time.Millisecond
	if err := db.SetReplyCheckEnabled(ctx, testChatID, true); err != nil {
		t.Fatal(err)
	}

	if _, ok := b.sendGreetingAnchor(ctx, s, testChatID, testUserID, 0, 1); !ok {
		t.Fatal("anchor send failed")
	}
	b.maybeArmReplyWait(s, testChatID, testUserID, 0)

	// Лестница ретраев 400 (0+1+2+4 c) длиннее стандартных пяти секунд.
	waitForWithin(t, 20*time.Second, func() bool {
		k := statsKinds(t, db, testChatID, testUserID)
		return k[storage.EventPass] == 1
	})
	k := statsKinds(t, db, testChatID, testUserID)
	if k[storage.EventKick]+k[storage.EventBan] != 0 {
		t.Fatalf("юзер наказан за недоставленное требование: %v", k)
	}
	rows, err := db.LoadAllPendingReplies(ctx)
	if err != nil || len(rows) != 0 {
		t.Fatalf("pending_replies = %v err=%v, want empty", rows, err)
	}
	if _, ok := b.replies.Take(testChatID, testUserID); ok {
		t.Fatal("reply wait must be disarmed")
	}
	_ = fc
}

// TestCaptchaSeriesEphemeralAllStages — эфемерный режим накрывает ВСЮ серию
// без исключений: каждое сообщение адресовано вступившему, ряд «Впустить»
// не рисуется ни на одной стадии, каждая стадия пишет в pending СВОЙ
// ephemeral id (старый не протекает), удаления идут по эфемерному пути.
// Фолбэка «со второй попытки публично» больше нет — проверяем это на юзере
// с уже существующей попыткой.
func TestCaptchaSeriesEphemeralAllStages(t *testing.T) {
	ctx := context.Background()
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	b.cfg.CaptchaStageInterval = 40 * time.Millisecond
	if err := db.SetEphemeralEnabled(ctx, testChatID, true); err != nil {
		t.Fatal(err)
	}
	// Пытка в прошлом — фолбэка на публичную капчу больше не существует.
	if _, err := db.IncrementAttempt(ctx, testChatID, testUserID, attemptsTTL); err != nil {
		t.Fatal(err)
	}
	// Эфемерка в реальности не имеет обычного message_id и несёт СВОЙ
	// ephemeral_message_id на каждое сообщение.
	fc.respSeq["sendMessage"] = []string{
		`{"ephemeral_message_id":777,"date":1700000000,"chat":{"id":-100100,"type":"supergroup"}}`,
		`{"ephemeral_message_id":778,"date":1700000000,"chat":{"id":-100100,"type":"supergroup"}}`,
		`{"ephemeral_message_id":779,"date":1700000000,"chat":{"id":-100100,"type":"supergroup"}}`,
	}

	b.runCaptcha(testChatID, telego.User{ID: testUserID, FirstName: "Юзер"}, 0)

	if p, ok := b.store.Get(testChatID, testUserID); !ok || p.EphemeralID != 777 {
		t.Fatalf("stage 1 must be live and ephemeral: %+v", p)
	}
	waitFor(t, func() bool {
		rows := pendingRows(t, db)
		return len(rows) == 1 && rows[0].Stage == 2 && rows[0].EphemeralID == 778
	})
	waitFor(t, func() bool {
		rows := pendingRows(t, db)
		return len(rows) == 1 && rows[0].Stage == 3 && rows[0].EphemeralID == 779
	})

	waitFor(t, func() bool {
		k := statsKinds(t, db, testChatID, testUserID)
		return k[storage.EventKick] == 1 && k[storage.EventBan] == 0
	})
	bodies := fc.callBodies("sendMessage")
	if len(bodies) != 3 {
		t.Fatalf("sendMessage = %d, want 3 (вся серия)", len(bodies))
	}
	for i, body := range bodies {
		if !strings.Contains(body, `"receiver_user_id":7`) {
			t.Fatalf("стадия %d ушла публично: %s", i+1, body)
		}
		if strings.Contains(body, "capok:") {
			t.Fatalf("на эфемерной стадии %d нарисован ряд «Впустить»: %s", i+1, body)
		}
	}
	// Два перехода + удаление после наказания — все по эфемерному id,
	// обычный deleteMessage не вызывается вовсе.
	if n := fc.callCount("deleteEphemeralMessage"); n != 3 {
		t.Fatalf("deleteEphemeralMessage = %d, want 3", n)
	}
	if n := fc.callCount("deleteMessage"); n != 0 {
		t.Fatalf("deleteMessage = %d, want 0 (эфемерки удаляются своим методом)", n)
	}
}

// TestCaptchaEphemeralUserNotParticipantMidSeries — ушедший между стадиями:
// эфемерка отсутствующему падает с USER_NOT_PARTICIPANT, воронка закрывается
// честным left (не abort, без наказания), осиротевший pre-persist перехода
// стёрт, капча-мьют снят.
func TestCaptchaEphemeralUserNotParticipantMidSeries(t *testing.T) {
	ctx := context.Background()
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	b.cfg.CaptchaStageInterval = 40 * time.Millisecond
	if err := db.SetEphemeralEnabled(ctx, testChatID, true); err != nil {
		t.Fatal(err)
	}
	fc.err["sendMessage"] = &telegoapi.Error{ErrorCode: 400,
		Description: "Bad Request: USER_NOT_PARTICIPANT"}
	// Liveness-проба об уходе не знает — классифицировать должен код ошибки.
	fc.resp["getChatMember"] = `{"status":"member","user":{"id":7,"is_bot":false,"first_name":"Юзер"}}`

	if err := db.PutPending(ctx, storage.PendingRow{
		ChatID: testChatID, UserID: testUserID,
		MessageID: 0, CorrectIdx: 1, EphemeralID: 55,
		ExpiresAt: time.Now().Add(1500 * time.Millisecond), Stage: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if n, err := b.restorePending(ctx); err != nil || n != 1 {
		t.Fatalf("restored = %d err=%v, want 1/nil", n, err)
	}

	waitForWithin(t, 15*time.Second, func() bool {
		k := statsKinds(t, db, testChatID, testUserID)
		return k[storage.EventLeft] == 1
	})
	k := statsKinds(t, db, testChatID, testUserID)
	if k[storage.EventAbort] != 0 || k[storage.EventKick]+k[storage.EventBan] != 0 {
		t.Fatalf("ушедший абортнут/наказан: %v", k)
	}
	if _, ok := b.store.Get(testChatID, testUserID); ok || len(pendingRows(t, db)) != 0 {
		t.Fatal("captcha must not survive USER_NOT_PARTICIPANT")
	}
	if n := fc.callCount("restrictChatMember"); n == 0 {
		t.Fatal("releaseOnAbort must lift the captcha mute")
	}
}

// TestCaptchaImageModeEphemeralAllStages — режим картинки подчиняется
// эфемерности так же: все три стадии уходят SendPhoto адресату, без ряда
// «Впустить», удаления по эфемерному id.
func TestCaptchaImageModeEphemeralAllStages(t *testing.T) {
	ctx := context.Background()
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	b.cfg.CaptchaStageInterval = 40 * time.Millisecond
	if err := db.SetEphemeralEnabled(ctx, testChatID, true); err != nil {
		t.Fatal(err)
	}
	mode := string(captcha.ModeImage)
	if err := db.SetCaptchaMode(ctx, testChatID, &mode); err != nil {
		t.Fatal(err)
	}
	fc.resp["sendPhoto"] = `{"ephemeral_message_id":888,
		"date":1700000000,"chat":{"id":-100100,"type":"supergroup"}}`

	b.runCaptcha(testChatID, telego.User{ID: testUserID, FirstName: "Юзер"}, 0)

	waitFor(t, func() bool {
		k := statsKinds(t, db, testChatID, testUserID)
		return k[storage.EventKick] == 1 && k[storage.EventBan] == 0
	})
	// Тело SendPhoto — multipart-поток, содержимое (receiver_user_id, клавиа-
	// тура) в fake не доезжает: сам факт эфемерности фотопути пиним косвенно —
	// ветки WithReceiverUserID и «capok» стоят рядом с текстовыми и управляются
	// тем же флагом, а здесь проверяем наблюдаемое: три фото без текстового
	// фолбэка и удаление строго по эфемерному id.
	bodies := fc.callBodies("sendPhoto")
	if len(bodies) != 3 {
		t.Fatalf("sendPhoto = %d, want 3 (фолбэк на текст не ожидается)", len(bodies))
	}
	if n := fc.callCount("deleteEphemeralMessage"); n != 3 {
		t.Fatalf("deleteEphemeralMessage = %d, want 3", n)
	}
	if n := fc.callCount("deleteMessage"); n != 0 {
		t.Fatalf("deleteMessage = %d, want 0 (эфемерки удаляются своим методом)", n)
	}
}
