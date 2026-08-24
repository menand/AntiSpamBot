package bot

// Тесты закрытия находок полного ревью (2026-08):
//  1. проигравший Take disarm-ветки reply-wait не пишет второй пасс;
//  2. проигранный kickoff-лок при переходе стадии капчи гасит осиротевший
//     pre-persist строки;
//  3. /unmute вне супергруппы отказывается через refuseAndDelete.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegoapi"

	"github.com/menand/AntiSpamBot/internal/storage"
)

// TestReplyWaitReminderFailLoserNoSecondPass — напоминание не ушло после
// ретраев, но пока оно уходило, ожидание разрешил победитель (ответ юзера,
// прилетевший в окно ретраев): терминальное событие пишет только он. Второй
// пасс поверх чужого исхода ломал бы воронку. Осиротевшая pre-persist строка
// стадии N+1 при этом гасится в любом случае — гвард satisfier'а идёт по ЕГО
// стадии и её не достаёт.
func TestReplyWaitReminderFailLoserNoSecondPass(t *testing.T) {
	ctx := context.Background()
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	b.cfg.CaptchaStageInterval = 40 * time.Millisecond
	if err := db.SetReplyCheckEnabled(ctx, testChatID, true); err != nil {
		t.Fatal(err)
	}

	// Победитель срабатывает внутри ПЕРВОЙ попытки отправки напоминания —
	// синхронно в окне ретраев, детерминированно раньше финального Take цикла.
	var winnerOnce sync.Once
	fc.errWhen = func(_ string, data *telegoapi.RequestData) bool {
		if data != nil && strings.Contains(string(data.BodyRaw), "Напоминание") {
			winnerOnce.Do(func() { b.replyWaitSatisfied(testChatID, testUserID) })
			return true
		}
		return false
	}

	s := storage.ChatSettings{GreetingEnabled: true, ReplyCheckEnabled: true}
	if _, ok := b.sendGreetingAnchor(ctx, s, testChatID, testUserID, 0, 1); !ok {
		t.Fatal("anchor send failed")
	}
	b.maybeArmReplyWait(s, testChatID, testUserID, 0)

	// Лестница ретраев 400 (0+1+2+4 c) длиннее стандартных пяти секунд.
	waitForWithin(t, 20*time.Second, func() bool {
		rows, err := db.LoadAllPendingReplies(ctx)
		return err == nil && len(rows) == 0
	})

	k := statsKinds(t, db, testChatID, testUserID)
	if k[storage.EventPass] != 1 {
		t.Fatalf("pass events = %d, want 1 — второй пасс поверх satisfier'а: %v",
			k[storage.EventPass], k)
	}
	if k[storage.EventKick]+k[storage.EventBan] != 0 {
		t.Fatalf("юзер наказан за недоставленное требование: %v", k)
	}
	if _, ok := b.replies.Take(testChatID, testUserID); ok {
		t.Fatal("reply wait must be gone")
	}
}

// TestCaptchaTransitionKickoffLostCleansOrphanRow — переход стадии проиграл
// гонку за kickoff-лок (дубль-дelivery держит его своей серией): pre-persist
// выше успел записать стадию N+1 под СТАРЫМ message_id, и без очистки здесь
// рестарт поднял бы призрачную капчу, если дубль-серия оборвётся до своего
// persist. События не пишутся, следующая стадия не отправляется.
func TestCaptchaTransitionKickoffLostCleansOrphanRow(t *testing.T) {
	ctx := context.Background()
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)

	if err := db.PutPending(ctx, storage.PendingRow{
		ChatID: testChatID, UserID: testUserID, MessageID: 10,
		CorrectIdx: 2, ExpiresAt: time.Now().Add(1500 * time.Millisecond), Stage: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if n, err := b.restorePending(ctx); err != nil || n != 1 {
		t.Fatalf("restored = %d err=%v, want 1/nil", n, err)
	}

	// «Дубль-доставка» захватывает kickoff-лок строго в окне между Take'ом
	// истёкшей стадии и попыткой цикла: синхронно внутри вызова удаления
	// старого сообщения, который в цикле стоит ровно между ними.
	var dupOnce sync.Once
	fc.errWhen = func(method string, _ *telegoapi.RequestData) bool {
		if method == "deleteMessage" {
			dupOnce.Do(func() {
				if !b.store.BeginKickoff(testChatID, testUserID) {
					t.Error("duplicate must win the kickoff lock in this window")
				}
			})
		}
		return false
	}
	defer b.store.FinishKickoff(testChatID, testUserID)

	waitFor(t, func() bool { return len(pendingRows(t, db)) == 0 })
	if _, ok := b.store.Get(testChatID, testUserID); ok {
		t.Fatal("captcha must be taken by the loop")
	}
	k := statsKinds(t, db, testChatID, testUserID)
	for kind, n := range k {
		if n != 0 {
			t.Fatalf("проигранный лок не должен писать события (%s): %v", kind, k)
		}
	}
	if n := fc.callCount("sendMessage"); n != 0 {
		t.Fatalf("sendMessage = %d, want 0 — серия продолжит дубль", n)
	}
}

// TestUnmuteNonSupergroupRefusalDeletesCommand — отказ /unmute в обычной
// группе симметричен /mute: реплей якорится на команду, затем команда
// удаляется — служебной команде нечего висеть в чате.
func TestUnmuteNonSupergroupRefusalDeletesCommand(t *testing.T) {
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	b.cfg.OwnerIDs = map[int64]struct{}{999: {}}

	msg := telego.Message{
		MessageID: 11,
		Chat:      telego.Chat{ID: testChatID, Type: "group"},
		From:      &telego.User{ID: 999, FirstName: "Хозяин"},
		Text:      "/unmute",
	}
	if err := b.handleUnmuteCommand(nil, msg); err != nil {
		t.Fatal(err)
	}

	bodies := fc.callBodies("sendMessage")
	if len(bodies) != 1 || !strings.Contains(bodies[0], "супергруппах") {
		t.Fatalf("отказ не отправлен или текст не тот: %v", bodies)
	}
	if n := fc.callCount("deleteMessage"); n != 1 {
		t.Fatalf("deleteMessage = %d, want 1 — команда не должна висеть в чате", n)
	}
}
