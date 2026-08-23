package bot

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegoapi"

	"github.com/menand/AntiSpamBot/internal/config"
	"github.com/menand/AntiSpamBot/internal/storage"
)

// waitFor крутит fn до успеха или таймаута — для goSafe-горутин (admitTrusted,
// banKnownSpammer, рестарт-очистки), чьё завершение тесту нужно дождаться.
func waitFor(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !fn() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached in time")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// fakeLLM — подставной llmClassifier: без сети, поведение задаётся полями.
type fakeLLM struct {
	enabled bool
	spam    bool
	err     error

	calls        int32 // atomic
	mu           sync.Mutex
	deadlinesSet []bool // был ли у ctx вызова дедлайн (для суб-бюджета первичного)
}

func (f *fakeLLM) Enabled() bool { return f.enabled }
func (f *fakeLLM) Model() string { return "fake" }

func (f *fakeLLM) Classify(ctx context.Context, _ string, _ string) (bool, error) {
	atomic.AddInt32(&f.calls, 1)
	_, hasDeadline := ctx.Deadline()
	f.mu.Lock()
	f.deadlinesSet = append(f.deadlinesSet, hasDeadline)
	f.mu.Unlock()
	if f.err != nil {
		return false, f.err
	}
	return f.spam, nil
}

func (f *fakeLLM) callCount() int { return int(atomic.LoadInt32(&f.calls)) }

// TestAIProvidersOrder — цепочка собирается в порядке AI_PROVIDER_ORDER,
// неизвестные провайдеры пропускаются, пустой order — дефолт.
func TestAIProvidersOrder(t *testing.T) {
	tests := []struct {
		name  string
		order []string
		want  []string
	}{
		{"пустой order — дефолт", nil, config.DefaultAIProviders},
		{"явный порядок сохраняется", []string{"gemini", "groq"}, []string{"gemini", "groq"}},
		{"неизвестный провайдер пропущен", []string{"groq", "claude", "gigachat"}, []string{"groq", "gigachat"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, _, _ := newFlowBot(t)
			b.cfg.AIProviderOrder = tc.order
			var got []string
			for _, p := range b.aiProviders() {
				got = append(got, p.name)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("aiProviders = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestClassifyVerdictFallback — первый упавший провайдер передаёт ход второму,
// все упавшие дают ошибку; суб-бюджет получает только первичный при ≥2
// включённых.
func TestClassifyVerdictFallback(t *testing.T) {
	t.Run("первичный упал — фолбек ответил", func(t *testing.T) {
		b, _, _ := newFlowBot(t)
		primary := &fakeLLM{enabled: true, err: errors.New("rate limit")}
		fallback := &fakeLLM{enabled: true, spam: true}
		b.groqc = primary
		b.gemic = fallback

		spam, provider, err := b.classifyVerdict(context.Background(), "sys", "facts", testChatID, testUserID)
		if err != nil || !spam || provider != "gemini" {
			t.Fatalf("spam=%v provider=%q err=%v, want true/gemini/nil", spam, provider, err)
		}
		if primary.callCount() != 1 || fallback.callCount() != 1 {
			t.Fatalf("calls primary=%d fallback=%d, want 1/1", primary.callCount(), fallback.callCount())
		}
	})

	t.Run("все упали — ошибка, fail-open у вызывающих", func(t *testing.T) {
		b, _, _ := newFlowBot(t)
		b.groqc = &fakeLLM{enabled: true, err: errors.New("down")}
		b.gigac = &fakeLLM{enabled: true, err: errors.New("down too")}

		spam, _, err := b.classifyVerdict(context.Background(), "sys", "facts", testChatID, testUserID)
		if err == nil || spam {
			t.Fatalf("want error and no verdict, got spam=%v err=%v", spam, err)
		}
	})

	t.Run("суб-бюджет только первичному при двух включённых", func(t *testing.T) {
		b, _, _ := newFlowBot(t)
		primary := &fakeLLM{enabled: true, err: errors.New("slow")}
		fallback := &fakeLLM{enabled: true}
		b.groqc = primary
		b.gemic = fallback

		_, _, _ = b.classifyVerdict(context.Background(), "sys", "facts", testChatID, testUserID)
		primary.mu.Lock()
		defer primary.mu.Unlock()
		if len(primary.deadlinesSet) != 1 || !primary.deadlinesSet[0] {
			t.Fatalf("primary must run under a sub-budget deadline, got %v", primary.deadlinesSet)
		}
		fallback.mu.Lock()
		defer fallback.mu.Unlock()
		if len(fallback.deadlinesSet) != 1 || fallback.deadlinesSet[0] {
			t.Fatalf("fallback must inherit the full check ctx, got %v", fallback.deadlinesSet)
		}
	})

	t.Run("один провайдер — полный бюджет", func(t *testing.T) {
		b, _, _ := newFlowBot(t)
		solo := &fakeLLM{enabled: true}
		b.groqc = solo

		_, _, _ = b.classifyVerdict(context.Background(), "sys", "facts", testChatID, testUserID)
		solo.mu.Lock()
		defer solo.mu.Unlock()
		if len(solo.deadlinesSet) != 1 || solo.deadlinesSet[0] {
			t.Fatalf("solo provider must get the full ctx, got %v", solo.deadlinesSet)
		}
	})
}

// TestManualUnbanForgivesSpamBase — ручной разбан чужой рукой стирает
// глобальный флаг спамера; свои события и чужие боты — нет (кроме
// GroupAnonymousBot — рука человека-анонимного админа).
func TestManualUnbanForgivesSpamBase(t *testing.T) {
	tests := []struct {
		name    string
		from    telego.User
		forgive bool
	}{
		{"чужая рука человека — прощение", telego.User{ID: 9, FirstName: "Админ"}, true},
		{"анонимный админ через GroupAnonymousBot — прощение",
			telego.User{ID: groupAnonymousBotID, IsBot: true, FirstName: "Аноним"}, true},
		{"сам юзер перезашёл — флаг остаётся", telego.User{ID: testUserID}, false},
		{"чужой бот — флаг остаётся",
			telego.User{ID: 123456, IsBot: true, FirstName: "Бот"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			b, db, _ := newFlowBot(t)
			serviceableChat(t, b, db, testChatID)
			if err := db.AddSpamBanned(ctx, testUserID, testChatID, time.Now()); err != nil {
				t.Fatal(err)
			}

			upd := memberUpdate(testChatID, tc.from,
				telego.User{ID: testUserID, FirstName: "Юзер"}, "kicked", "member")
			if err := b.handleChatMember(nil, upd); err != nil {
				t.Fatal(err)
			}
			banned, err := db.IsSpamBanned(ctx, testUserID)
			if err != nil {
				t.Fatal(err)
			}
			if banned == tc.forgive {
				t.Fatalf("IsSpamBanned=%v, want forgive=%v", banned, tc.forgive)
			}
		})
	}
}

// TestOnFailBrokenCounterNeverBans — ошибка счётчика попыток принудительно
// ведёт на кик (ban+unban), пермабан по «угаданному» счёту запрещён.
func TestOnFailBrokenCounterNeverBans(t *testing.T) {
	tests := []struct {
		name       string
		breakDB    bool // сломать IncrementAttempt (закрыть БД перед onFail)
		wantUnbans int  // kick = ban+unban; permaban = banShort без unban
	}{
		{"рабочий счётчик, последняя попытка — бан", false, 0},
		{"сломанный счётчик — принудительный кик", true, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			b, db, fc := newFlowBot(t)
			serviceableChat(t, b, db, testChatID)
			b.cfg.MaxAttempts = 3
			for i := 0; i < 2; i++ { // две попытки уже были
				if _, err := db.IncrementAttempt(ctx, testChatID, testUserID, attemptsTTL); err != nil {
					t.Fatal(err)
				}
			}
			p := putCaptcha(b, db, testChatID, testUserID, 77)
			if tc.breakDB {
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
			}

			if err := b.onFail(ctx, p, "тест"); err != nil {
				t.Fatalf("onFail must not fail the punish itself: %v", err)
			}
			if got := fc.callCount("unbanChatMember"); got != tc.wantUnbans {
				t.Fatalf("unban calls = %d, want %d (calls: %v)", got, tc.wantUnbans, fc.callList())
			}
			if n := fc.callCount("banChatMember"); n != 1 {
				t.Fatalf("ban calls = %d, want exactly one punish", n)
			}
		})
	}
}

// TestRunCaptchaShutdownInDelay — shutdown внутри окна CaptchaDelay: мьют
// снят, воронка закрыта abort'ом (join уже записан), капча не осталась.
func TestRunCaptchaShutdownInDelay(t *testing.T) {
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	b.cfg.CaptchaDelay = 80 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	b.runCtx = ctx
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	b.runCaptcha(testChatID, telego.User{ID: testUserID, FirstName: "Юзер"}, 0)

	kinds := statsKinds(t, db, testChatID, testUserID)
	if kinds[storage.EventAbort] != 1 || kinds[storage.EventLeft] != 0 {
		t.Fatalf("kinds = %v, want funnel closed with abort", kinds)
	}
	if _, ok := b.store.Get(testChatID, testUserID); ok || len(pendingRows(t, db)) != 0 {
		t.Fatal("shutdown must not leave a live captcha")
	}
	if n := fc.callCount("restrictChatMember"); n < 2 {
		t.Fatalf("restrict + release expected, restrict calls = %d (%v)",
			n, fc.callList())
	}
}

// TestRestoreServiceabilityAndLiveness — рестарт не армирует таймеры для
// необслуживаемых чатов и не карает ушедших офлайн.
func TestRestoreServiceabilityAndLiveness(t *testing.T) {
	ctx := context.Background()

	t.Run("rejected чат — строки снесены, таймеров нет", func(t *testing.T) {
		b, db, _ := newFlowBot(t)
		b.rememberChat(ctx, storage.ChatInfo{ChatID: -100999, Title: "Мёртвый", Type: "supergroup"})
		if err := db.SetChatApproval(ctx, -100999, storage.ChatRejected); err != nil {
			t.Fatal(err)
		}
		if err := db.PutPending(ctx, storage.PendingRow{
			ChatID: -100999, UserID: testUserID, MessageID: 10,
			CorrectIdx: 0, ExpiresAt: time.Now().Add(-time.Hour),
		}); err != nil {
			t.Fatal(err)
		}

		n, err := b.restorePending(ctx)
		if err != nil || n != 0 {
			t.Fatalf("restored = %d err=%v, want 0/nil", n, err)
		}
		if rows := pendingRows(t, db); len(rows) != 0 {
			t.Fatalf("pending rows of rejected chat must be dropped, got %d", len(rows))
		}
	})

	t.Run("истёкшая, юзер на месте — грейс-кик как раньше", func(t *testing.T) {
		b, db, fc := newFlowBot(t)
		serviceableChat(t, b, db, testChatID)
		fc.resp["getChatMember"] =
			`{"status":"member","user":{"id":7,"is_bot":false,"first_name":"Юзер"}}`
		if err := db.PutPending(ctx, storage.PendingRow{
			ChatID: testChatID, UserID: testUserID, MessageID: 10,
			CorrectIdx: 0, ExpiresAt: time.Now().Add(-time.Hour),
		}); err != nil {
			t.Fatal(err)
		}

		n, err := b.restorePending(ctx)
		if err != nil || n != 1 {
			t.Fatalf("restored = %d err=%v, want 1/nil", n, err)
		}
		waitFor(t, func() bool {
			k := statsKinds(t, db, testChatID, testUserID)
			return k[storage.EventKick] == 1 && k[storage.EventLeft] == 0
		})
		if rows := pendingRows(t, db); len(rows) != 0 {
			t.Fatalf("pending rows after punish = %d, want 0", len(rows))
		}
	})

	t.Run("истёкшая, юзер вышел офлайн — left и размьют без кика", func(t *testing.T) {
		b, db, fc := newFlowBot(t)
		serviceableChat(t, b, db, testChatID)
		fc.resp["getChatMember"] =
			`{"status":"left","user":{"id":7,"is_bot":false,"first_name":"Юзер"}}`
		if err := db.PutPending(ctx, storage.PendingRow{
			ChatID: testChatID, UserID: testUserID, MessageID: 10,
			CorrectIdx: 0, ExpiresAt: time.Now().Add(-time.Hour),
		}); err != nil {
			t.Fatal(err)
		}

		n, err := b.restorePending(ctx)
		if err != nil || n != 0 {
			t.Fatalf("departed user must not be armed, restored = %d err=%v", n, err)
		}
		waitFor(t, func() bool { return fc.callCount("restrictChatMember") >= 1 })
		k := statsKinds(t, db, testChatID, testUserID)
		if k[storage.EventLeft] != 1 || k[storage.EventKick]+k[storage.EventBan] != 0 {
			t.Fatalf("departed user punished after restore: %v", k)
		}
		if rows := pendingRows(t, db); len(rows) != 0 {
			t.Fatalf("pending rows = %d, want 0", len(rows))
		}
	})
}

// TestRestoreRepliesServiceabilityAndLiveness — то же для ожиданий ответа:
// мёртвые чаты чистятся, ушедший офлайн не получает кик за молчание.
func TestRestoreRepliesServiceabilityAndLiveness(t *testing.T) {
	ctx := context.Background()

	t.Run("rejected чат — строки снесены", func(t *testing.T) {
		b, db, _ := newFlowBot(t)
		b.rememberChat(ctx, storage.ChatInfo{ChatID: -100888, Title: "Мёртвый", Type: "supergroup"})
		if err := db.SetChatApproval(ctx, -100888, storage.ChatRejected); err != nil {
			t.Fatal(err)
		}
		if err := db.PutPendingReply(ctx, storage.PendingReply{
			ChatID: -100888, UserID: testUserID, ExpiresAt: time.Now().Add(time.Hour),
		}); err != nil {
			t.Fatal(err)
		}

		n, err := b.restorePendingReplies(ctx)
		if err != nil || n != 0 {
			t.Fatalf("restored = %d err=%v, want 0/nil", n, err)
		}
		rows, err := db.LoadAllPendingReplies(ctx)
		if err != nil || len(rows) != 0 {
			t.Fatalf("replies of dead chat must be dropped: %v %v", rows, err)
		}
	})

	t.Run("истёкшее, юзер вышел офлайн — left без наказания", func(t *testing.T) {
		b, db, fc := newFlowBot(t)
		serviceableChat(t, b, db, testChatID)
		fc.resp["getChatMember"] =
			`{"status":"left","user":{"id":7,"is_bot":false,"first_name":"Юзер"}}`
		if err := db.PutPendingReply(ctx, storage.PendingReply{
			ChatID: testChatID, UserID: testUserID, ExpiresAt: time.Now().Add(-time.Hour),
		}); err != nil {
			t.Fatal(err)
		}

		n, err := b.restorePendingReplies(ctx)
		if err != nil || n != 0 {
			t.Fatalf("departed user must not be armed, restored = %d err=%v", n, err)
		}
		waitFor(t, func() bool {
			k := statsKinds(t, db, testChatID, testUserID)
			return k[storage.EventLeft] == 1
		})
		k := statsKinds(t, db, testChatID, testUserID)
		if k[storage.EventKick]+k[storage.EventBan] != 0 {
			t.Fatalf("silent-user punish for departed user: %v", k)
		}
		rows, err := db.LoadAllPendingReplies(ctx)
		if err != nil || len(rows) != 0 {
			t.Fatalf("reply rows = %v err=%v, want empty", rows, err)
		}
	})
}

// TestOnUserJoinedTrustedAndSpammer — специальные входы: доверенный без капчи
// с дедупом дубль-доставки, известный спамер с мгновенным banRevoke, спамер
// при упавшем бане — обычная капча.
func TestOnUserJoinedTrustedAndSpammer(t *testing.T) {
	user := telego.User{ID: testUserID, FirstName: "Юзер"}
	chat := telego.Chat{ID: testChatID, Type: "supergroup"}

	t.Run("доверенный — join+pass без капчи и рестрикта", func(t *testing.T) {
		b, db, fc := newFlowBot(t)
		serviceableChat(t, b, db, testChatID)
		if err := db.AddTrusted(context.Background(), testChatID, testUserID, time.Now()); err != nil {
			t.Fatal(err)
		}

		b.onUserJoined(chat, user, 0)
		waitFor(t, func() bool {
			k := statsKinds(t, db, testChatID, testUserID)
			return k[storage.EventJoin] == 1 && k[storage.EventPass] == 1
		})
		if _, ok := b.store.Get(testChatID, testUserID); ok {
			t.Fatal("trusted user must not get a captcha")
		}
		if fc.callCount("restrictChatMember") != 0 {
			t.Fatal("trusted user must not be restricted")
		}

		// Дубль-доставка в течение минуты — событий больше нет.
		b.onUserJoined(chat, user, 0)
		k := statsKinds(t, db, testChatID, testUserID)
		if k[storage.EventJoin] != 1 || k[storage.EventPass] != 1 {
			t.Fatalf("duplicate delivery double-counted: %v", k)
		}
	})

	t.Run("известный спамер — мгновенный banRevoke + spamban", func(t *testing.T) {
		b, db, fc := newFlowBot(t)
		serviceableChat(t, b, db, testChatID)
		if err := db.AddSpamBanned(context.Background(), testUserID, testChatID, time.Now()); err != nil {
			t.Fatal(err)
		}

		b.onUserJoined(chat, user, 0)
		waitFor(t, func() bool {
			return statsKinds(t, db, testChatID, testUserID)[storage.EventSpamBan] == 1
		})
		bodies := fc.callBodies("banChatMember")
		if len(bodies) != 1 || !strings.Contains(bodies[0], `"revoke_messages":true`) {
			t.Fatalf("banChatMember bodies = %v, want one with revoke_messages:true", bodies)
		}
		if n := fc.callCount("unbanChatMember"); n != 0 {
			t.Fatalf("spammer ban must be permanent, unban calls = %d", n)
		}
		k := statsKinds(t, db, testChatID, testUserID)
		if k[storage.EventKick]+k[storage.EventBan] != 0 {
			t.Fatalf("captcha punish events leaked into instant ban: %v", k)
		}
	})

	t.Run("спамер и неудавшийся бан — фолбэк на капчу", func(t *testing.T) {
		b, db, fc := newFlowBot(t)
		serviceableChat(t, b, db, testChatID)
		if err := db.AddSpamBanned(context.Background(), testUserID, testChatID, time.Now()); err != nil {
			t.Fatal(err)
		}
		fc.err["banChatMember"] = &telegoapi.Error{ErrorCode: 400, Description: "no rights"}

		b.onUserJoined(chat, user, 0)
		// Фолбэк стартует только после полной лестницы ретраев banRevoke
		// (~7 c) — ждём дольше дефолтного waitFor.
		deadline := time.Now().Add(15 * time.Second)
		for len(pendingRows(t, db)) != 1 {
			if time.Now().After(deadline) {
				t.Fatal("fallback captcha never armed")
			}
			time.Sleep(200 * time.Millisecond)
		}
		if _, ok := b.store.Get(testChatID, testUserID); !ok {
			t.Fatal("fallback captcha must be live in the store")
		}
		k := statsKinds(t, db, testChatID, testUserID)
		if k[storage.EventJoin] != 1 || k[storage.EventSpamBan] != 0 {
			t.Fatalf("failed-ban fallback must record plain join, got %v", k)
		}
		b.cancelCaptchaSilent(testChatID, testUserID) // прибираем waitTimeout
	})
}

// TestCaptchaCallbacksServiceabilityGate — капча в необслуживаемом чате не
// решается и не наказывает (стартовое окно после рестарта).
func TestCaptchaCallbacksServiceabilityGate(t *testing.T) {
	ctx := context.Background()
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	putCaptcha(b, db, testChatID, testUserID, 100)
	if err := db.SetChatApproval(ctx, testChatID, storage.ChatRejected); err != nil {
		t.Fatal(err)
	}
	b.delApprovalCache(testChatID)

	if err := b.handleCallback(nil, captchaQuery("q10", testUserID, 2, 100)); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.store.Get(testChatID, testUserID); !ok {
		t.Fatal("captcha in unserviceable chat must not be resolvable")
	}
	if n := fc.callCount("restrictChatMember"); n != 0 {
		t.Fatalf("unserviceable chat must not trigger release/punish, calls: %v", fc.callList())
	}

	// «Впустить» — тот же гейт, до живой проверки админства.
	b.cfg.OwnerIDs = map[int64]struct{}{9: {}}
	approve := telego.CallbackQuery{
		ID:   "q11",
		From: telego.User{ID: 9, FirstName: "Владелец"},
		Data: "capok:" + itoa64(testUserID),
		Message: &telego.Message{
			MessageID: 100,
			Chat:      telego.Chat{ID: testChatID, Type: "supergroup"},
		},
	}
	before := len(fc.callList())
	if err := b.handleApproveCallback(nil, approve); err != nil {
		t.Fatal(err)
	}
	for _, m := range fc.callList()[before:] {
		if m == "restrictChatMember" || m == "getChatMember" {
			t.Fatalf("approve in dead chat must not call %s", m)
		}
	}
	if _, ok := b.store.Get(testChatID, testUserID); !ok {
		t.Fatal("captcha in unserviceable chat must not be approvable")
	}
}

// TestHandleCallbackInaccessibleMessageStale — недоступное сообщение
// (InaccessibleMessage) считается устаревшей клавиатурой: живую капчу оно
// не решает.
func TestHandleCallbackInaccessibleMessageStale(t *testing.T) {
	t.Run("ответ юзера", func(t *testing.T) {
		b, db, fc := newFlowBot(t)
		serviceableChat(t, b, db, testChatID)
		live := putCaptcha(b, db, testChatID, testUserID, 100)

		query := telego.CallbackQuery{
			ID:   "q12",
			From: telego.User{ID: testUserID, FirstName: "Юзер"},
			Data: "cap:" + itoa64(testUserID) + ":2", // верный индекс, но сообщение недоступно
			Message: &telego.InaccessibleMessage{
				Chat:      telego.Chat{ID: testChatID, Type: "supergroup"},
				MessageID: 999,
			},
		}
		if err := b.handleCallback(nil, query); err != nil {
			t.Fatal(err)
		}
		if p, ok := b.store.Get(testChatID, testUserID); !ok || p != live {
			t.Fatal("inaccessible message must not resolve the live captcha")
		}
		if n := fc.callCount("restrictChatMember"); n != 0 {
			t.Fatalf("stale click must not release the user, calls: %v", fc.callList())
		}
	})

	t.Run("кнопка админа", func(t *testing.T) {
		b, db, _ := newFlowBot(t)
		serviceableChat(t, b, db, testChatID)
		live := putCaptcha(b, db, testChatID, testUserID, 100)
		b.cfg.OwnerIDs = map[int64]struct{}{9: {}}

		query := telego.CallbackQuery{
			ID:   "q13",
			From: telego.User{ID: 9, FirstName: "Владелец"},
			Data: "capok:" + itoa64(testUserID),
			Message: &telego.InaccessibleMessage{
				Chat:      telego.Chat{ID: testChatID, Type: "supergroup"},
				MessageID: 999,
			},
		}
		if err := b.handleApproveCallback(nil, query); err != nil {
			t.Fatal(err)
		}
		if p, ok := b.store.Get(testChatID, testUserID); !ok || p != live {
			t.Fatal("inaccessible message must not approve the live captcha")
		}
	})
}

// TestDisarmGreetInputOnCommand — ЛС-команда разряжает взведённый ввод
// приветствия с честным подтверждением; некомандные сообщения не трогает.
func TestDisarmGreetInputOnCommand(t *testing.T) {
	privateMsg := func(text string) telego.Message {
		return telego.Message{
			Chat: telego.Chat{ID: testUserID, Type: "private"},
			From: &telego.User{ID: testUserID, FirstName: "Админ"},
			Text: text,
		}
	}

	t.Run("/help отменяет ввод", func(t *testing.T) {
		b, _, fc := newFlowBot(t)
		b.setGreetingInputPending(testUserID, testChatID)

		b.cancelGreetingInputOnCommand(context.Background(), privateMsg("/help"))

		if len(b.greetInput) != 0 {
			t.Fatal("command must disarm the greeting input")
		}
		bodies := fc.callBodies("sendMessage")
		if len(bodies) != 1 || !strings.Contains(bodies[0], "отменён") {
			t.Fatalf("confirmation expected, bodies = %v", bodies)
		}
	})

	t.Run("обычный текст не трогает состояние", func(t *testing.T) {
		b, _, fc := newFlowBot(t)
		b.setGreetingInputPending(testUserID, testChatID)

		b.cancelGreetingInputOnCommand(context.Background(), privateMsg("привет"))

		if len(b.greetInput) != 1 {
			t.Fatal("plain text must keep the input armed")
		}
		if n := fc.callCount("sendMessage"); n != 0 {
			t.Fatalf("no confirmation expected, sends = %d", n)
		}
	})

	t.Run("истёкший запрос — честное «устарел»", func(t *testing.T) {
		b, _, fc := newFlowBot(t)
		b.greetInput[testUserID] = greetInputState{
			chatID: testChatID, armedAt: time.Now().Add(-greetInputTTL - time.Minute),
		}

		b.cancelGreetingInputOnCommand(context.Background(), privateMsg("/stats"))

		if len(b.greetInput) != 0 {
			t.Fatal("expired state must be consumed")
		}
		bodies := fc.callBodies("sendMessage")
		if len(bodies) != 1 || !strings.Contains(bodies[0], "устарел") {
			t.Fatalf("expiry notice expected, bodies = %v", bodies)
		}
	})
}

// TestPunishNonAdminTexts — обещание мьюта только когда мьют реально прошёл.
func TestPunishNonAdminTexts(t *testing.T) {
	msg := func() telego.Message {
		return telego.Message{
			Chat: telego.Chat{ID: testChatID, Type: "supergroup"},
			From: &telego.User{ID: 777, FirstName: "Балуй"},
		}
	}

	t.Run("мьют прошёл — предупреждение о мьюте", func(t *testing.T) {
		b, _, fc := newFlowBot(t)
		b.punishNonAdmin(nil, msg())
		bodies := fc.callBodies("sendMessage")
		found := false
		for _, body := range bodies {
			if strings.Contains(body, "Мьют на 1 минуту") {
				found = true
			}
		}
		if !found {
			t.Fatalf("mute promise expected, bodies = %v", bodies)
		}
	})

	t.Run("рестрикт упал — без обещания мьюта", func(t *testing.T) {
		b, _, fc := newFlowBot(t)
		fc.err["restrictChatMember"] = &telegoapi.Error{ErrorCode: 400, Description: "no rights"}
		b.punishNonAdmin(nil, msg())
		bodies := fc.callBodies("sendMessage")
		foundMute, foundReply := false, false
		for _, body := range bodies {
			if strings.Contains(body, "Мьют на 1 минуту") {
				foundMute = true
			}
			if strings.Contains(body, "не балуйся") {
				foundReply = true
			}
		}
		if foundMute || !foundReply {
			t.Fatalf("honest reply without mute promise expected, bodies = %v", bodies)
		}
	})
}

// TestResolveModTargetHygiene — автофорварды/сервисные отправители/боты целью
// модкоманд не становятся (как у /spam).
func TestResolveModTargetHygiene(t *testing.T) {
	tests := []struct {
		name string
		r    *telego.Message
		want bool
	}{
		{"автофорвард канала — отказ", &telego.Message{
			IsAutomaticForward: true,
			From:               &telego.User{ID: telegramServiceUserID, FirstName: "Telegram"},
		}, false},
		{"сервисный 777000 — отказ", &telego.Message{
			From: &telego.User{ID: telegramServiceUserID, FirstName: "Telegram"},
		}, false},
		{"бот — отказ", &telego.Message{
			From: &telego.User{ID: 555, IsBot: true, FirstName: "Бот"},
		}, false},
		{"обычный юзер — цель", &telego.Message{
			From:      &telego.User{ID: 555, FirstName: "Юзер"},
			MessageID: 42,
		}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, _, _ := newFlowBot(t)
			message := telego.Message{ReplyToMessage: tc.r}
			id, msgID, ok := b.resolveModTarget(message)
			if ok != tc.want {
				t.Fatalf("ok = %v, want %v (id=%d msgID=%d)", ok, tc.want, id, msgID)
			}
			if ok && (id != 555 || msgID != 42) {
				t.Fatalf("target = (%d,%d), want (555,42)", id, msgID)
			}
		})
	}
}

// TestSpamGateCacheInvalidation — кэш тумблера антиспама переживает закрытие
// БД (нет походов в базу на каждое сообщение) и сбрасывается тогглом.
func TestSpamGateCacheInvalidation(t *testing.T) {
	ctx := context.Background()

	t.Run("негатив кэшируется", func(t *testing.T) {
		b, db, _ := newFlowBot(t)
		serviceableChat(t, b, db, testChatID)
		b.cfg.GroqAPIKey = "key" // ИИ доступен — гейт реально стоит на пути

		if b.spamCheckEnabledCached(testChatID) {
			t.Fatal("default chat must have the check disabled")
		}
		if _, cached := b.spamGateCache[testChatID]; !cached {
			t.Fatal("negative result must be cached")
		}
		msg := telego.Message{Chat: telego.Chat{ID: testChatID}, From: &telego.User{ID: 5}}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		b.maybeSpamCheck(msg) // должен выйти по кэшу без похода в БД и паник
	})

	t.Run("позитив кэшируется, сброс возвращает чтение", func(t *testing.T) {
		b, db, _ := newFlowBot(t)
		serviceableChat(t, b, db, testChatID)
		if err := db.SetSpamCheckEnabled(ctx, testChatID, true); err != nil {
			t.Fatal(err)
		}
		if !b.spamCheckEnabledCached(testChatID) {
			t.Fatal("enabled flag must be read and cached")
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		if !b.spamCheckEnabledCached(testChatID) {
			t.Fatal("cached positive must survive a closed DB")
		}
		b.delSpamGateCache(testChatID)
		if b.spamCheckEnabledCached(testChatID) {
			t.Fatal("after invalidation a closed DB must fail closed")
		}
	})
}

func itoa64(v int64) string {
	return strconv.FormatInt(v, 10)
}

// reportCommand — /spam-команда реплаем на сообщение цели.
func reportCommand(fromID int64, target *telego.User) telego.Message {
	return telego.Message{
		MessageID: 10,
		Chat:      telego.Chat{ID: testChatID, Type: "supergroup"},
		From:      &telego.User{ID: fromID, FirstName: "Репортёр"},
		Text:      "/spam",
		ReplyToMessage: &telego.Message{
			MessageID: 5,
			From:      target,
		},
	}
}

// seedTrusted накручивает порог истории сообщений (дефолт whitelist = 5).
func seedTrusted(t *testing.T, db *storage.DB, userID int64) {
	t.Helper()
	for i := 0; i < 6; i++ {
		if _, err := db.RecordMessage(context.Background(), testChatID, userID,
			time.Now().Add(-time.Duration(6-i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
}

// TestSpamReportTrustGateFailClosed — ошибка чтения счётчика НЕ пропускает
// репорт: fail-closed, как у гейта бюллетеней.
func TestSpamReportTrustGateFailClosed(t *testing.T) {
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	cmd := reportCommand(9, &telego.User{ID: 42, FirstName: "Цель"})

	_ = db.Close() // sql.ErrConnDone на UserMessageTotal
	if err := b.handleSpamCommand(nil, cmd); err != nil {
		t.Fatal(err)
	}
	if n := fc.callCount("sendMessage"); n != 0 {
		t.Fatalf("trust-gate DB error must fail CLOSED — плашка не создаётся (sendMessage=%d)", n)
	}
}

// TestSpamReportBelowTrustRefused — новичок без истории репортить не может.
func TestSpamReportBelowTrustRefused(t *testing.T) {
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	if err := b.handleSpamCommand(nil, reportCommand(9, &telego.User{ID: 42})); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(fc.callBodies("sendMessage"), "\n")
	if !strings.Contains(got, "историей сообщений") {
		t.Fatalf("want trust refusal, got:\n%s", got)
	}
}

// TestSpamVoteInitiatorExcludedBeforeGolden — инициатор не голосует в своём
// репорте даже золотым голосом админа.
func TestSpamVoteInitiatorExcludedBeforeGolden(t *testing.T) {
	ctx := context.Background()
	voteQuery := func(voter int64) telego.CallbackQuery {
		return telego.CallbackQuery{
			ID:   "q",
			From: telego.User{ID: voter, FirstName: "Инициатор"},
			Data: "sv:1",
			Message: &telego.Message{MessageID: 7,
				Chat: telego.Chat{ID: testChatID, Type: "supergroup"}},
		}
	}

	for _, tc := range []struct {
		name       string
		adminVoter bool
	}{
		{"простой инициатор", false},
		{"админ-инициатор без золотого голоса", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, db, fc := newFlowBot(t)
			serviceableChat(t, b, db, testChatID)
			if tc.adminVoter {
				fc.resp["getChatMember"] =
					`{"status":"administrator","user":{"id":7,"is_bot":false,"first_name":"И"}}`
			}
			if err := db.PutSpamVote(ctx, storage.SpamVote{
				ChatID: testChatID, BotMsgID: 7, TargetMsgID: 555,
				AuthorID: 42, InitiatorID: 7, Prob: 100, CreatedAt: time.Now(),
			}); err != nil {
				t.Fatal(err)
			}
			if err := b.handleSpamVoteCallback(nil, voteQuery(7)); err != nil {
				t.Fatal(err)
			}
			if n := fc.callCount("banChatMember"); n != 0 {
				t.Fatalf("инициатор не должен резолвить вердикт (banChatMember=%d)", n)
			}
			if _, found, _ := db.GetSpamVote(ctx, testChatID, 7); !found {
				t.Fatal("голосование должно пережить клик инициатора")
			}
		})
	}
}

// TestMigrateChatStateCarriesBotAddedAt — дата появления бота переживает
// апгрейд basic group → supergroup; легаси-чат дату не изобретает.
func TestMigrateChatStateCarriesBotAddedAt(t *testing.T) {
	ctx := context.Background()
	oldID, newID := int64(-5000), int64(-100001)
	at := time.Now().Add(-72 * time.Hour).Truncate(time.Second)

	t.Run("дата переносится write-once", func(t *testing.T) {
		b, db, _ := newFlowBot(t)
		if err := db.RememberChat(ctx, storage.ChatInfo{ChatID: oldID, Title: "Old", Type: "group"}); err != nil {
			t.Fatal(err)
		}
		if err := db.SetChatBotAddedAtIfEmpty(ctx, oldID, at); err != nil {
			t.Fatal(err)
		}
		b.migrateChatState(oldID, newID)
		got, ok, err := db.GetChatBotAddedAt(ctx, newID)
		if err != nil || !ok || !got.Equal(at) {
			t.Fatalf("bot_added_at not carried: got %v ok=%v err=%v", got, ok, err)
		}
		if _, ok, _ := db.GetChatBotAddedAt(ctx, oldID); ok {
			t.Error("old chat row must be gone")
		}
	})

	t.Run("легаси-чат остаётся NULL", func(t *testing.T) {
		b, db, _ := newFlowBot(t)
		if err := db.RememberChat(ctx, storage.ChatInfo{ChatID: oldID, Title: "Old", Type: "group"}); err != nil {
			t.Fatal(err)
		}
		b.migrateChatState(oldID, newID)
		if _, ok, _ := db.GetChatBotAddedAt(ctx, newID); ok {
			t.Error("legacy chat must not invent an added-at date")
		}
	})
}

// TestResolveReportTarget — только реплай: приветствие/сообщение ок,
// боты и чужие сообщения бота — нет.
func TestResolveReportTarget(t *testing.T) {
	ctx := context.Background()
	b, db, _ := newFlowBot(t)

	cases := []struct {
		name   string
		r      *telego.Message
		putGrt bool
		wantID int64
		wantOK bool
	}{
		{"реплай на обычное сообщение", &telego.Message{MessageID: 1, From: &telego.User{ID: 8}}, false, 8, true},
		{"реплай на приветствие бота", &telego.Message{MessageID: 77, From: &telego.User{ID: 42, IsBot: true}}, true, 8, true},
		{"реплай на чужую плашку бота", &telego.Message{MessageID: 78, From: &telego.User{ID: 42, IsBot: true}}, false, 0, false},
		{"бот-цель отклонён", &telego.Message{MessageID: 2, From: &telego.User{ID: 99, IsBot: true}}, false, 0, false},
		{"сервисный Telegram отклонён", &telego.Message{MessageID: 3, From: &telego.User{ID: 777000}}, false, 0, false},
		{"канал-цель (отрицательный id) отклонён", &telego.Message{MessageID: 4, From: &telego.User{ID: -100500}}, false, 0, false},
		{"без From", &telego.Message{MessageID: 6}, false, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.putGrt {
				if err := db.PutGreeting(ctx, testChatID, 8, 77, time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			gotID, ok := b.resolveReportTarget(testChatID, tc.r)
			if ok != tc.wantOK || gotID != tc.wantID {
				t.Fatalf("got (%d,%v), want (%d,%v)", gotID, ok, tc.wantID, tc.wantOK)
			}
		})
	}
}

// TestJoinedLinePrecedence — цепочка фолбэков строки входа в карточке.
func TestJoinedLinePrecedence(t *testing.T) {
	ctx := context.Background()
	const chat = int64(-300)
	const user = int64(42)
	old := time.Now().Add(-30 * 24 * time.Hour)

	cases := []struct {
		name    string
		setup   func(db *storage.DB)
		wantHas string
		wantNot string
	}{
		{"строка members главнее всего", func(db *storage.DB) {
			db.UpsertMember(ctx, chat, user, old)
		}, "Последний вход", "видимо"},
		{"bot_added_at второй", func(db *storage.DB) {
			db.RememberChat(ctx, storage.ChatInfo{ChatID: chat, Title: "C", Type: "supergroup"})
			db.SetChatBotAddedAtIfEmpty(ctx, chat, old)
		}, "дата моего добавления", ""},
		{"раннее событие третий", func(db *storage.DB) {
			db.RememberChat(ctx, storage.ChatInfo{ChatID: chat, Title: "C", Type: "supergroup"})
			db.RecordEvent(ctx, chat, user, storage.EventJoin, old, "")
		}, "самое раннее событие", ""},
		{"ничего не известно — последний", func(*storage.DB) {}, "Вход: неизвестно", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, db, _ := newFlowBot(t)
			tc.setup(db)
			got := b.joinedLine(chat, user)
			if !strings.Contains(got, tc.wantHas) {
				t.Errorf("line %q must contain %q", got, tc.wantHas)
			}
			if tc.wantNot != "" && strings.Contains(got, tc.wantNot) {
				t.Errorf("line %q must NOT contain %q", got, tc.wantNot)
			}
		})
	}
}
