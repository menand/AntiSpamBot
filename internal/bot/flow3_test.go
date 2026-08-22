package bot

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegoapi"

	"github.com/menand/AntiSpamBot/internal/storage"
)

// TestPunishReasonColumns — наказания пишут машиночитаемый reason в events:
// капча — «captcha», молчание — «noreply». По этим значениям /unmute и
// /whitelist строят списки «10 последних» (RecentEventUsers с фильтром
// причин), потеря значения ломает их молча.
func TestPunishReasonColumns(t *testing.T) {
	tests := []struct {
		name        string
		noreply     bool // true — путь waitReplyTimeout, false — onFail
		preAttempts int
		wantKind    storage.EventKind
	}{
		{"капча: тихий кик", false, 0, storage.EventKick},
		{"капча: бан", false, 2, storage.EventBan},
		{"молчание: тихий кик", true, 0, storage.EventKick},
		{"молчание: бан", true, 2, storage.EventBan},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			b, db, _ := newFlowBot(t)
			for i := 0; i < tc.preAttempts; i++ {
				if _, err := db.IncrementAttempt(ctx, testChatID, testUserID, attemptsTTL); err != nil {
					t.Fatal(err)
				}
			}
			wantReason := storage.ReasonCaptcha
			if tc.noreply {
				wantReason = storage.ReasonNoReply
				if err := db.PutGreeting(ctx, testChatID, testUserID, 500, time.Now()); err != nil {
					t.Fatal(err)
				}
				b.waitReplyTimeout(b.replies.Put(testChatID, testUserID,
					time.Now().Add(-time.Millisecond)))
			} else if err := b.onFail(ctx, putCaptcha(b, db, testChatID, testUserID, 77), "таймаут"); err != nil {
				t.Fatal(err)
			}

			list, err := db.RecentEventUsers(ctx, testChatID, 10,
				[]storage.EventKind{tc.wantKind}, []string{wantReason})
			if err != nil || len(list) != 1 || list[0].UserID != testUserID {
				t.Fatalf("event %s/%s not recorded: list=%+v err=%v",
					tc.wantKind, wantReason, list, err)
			}
			wrong := storage.ReasonNoReply
			if tc.noreply {
				wrong = storage.ReasonCaptcha
			}
			if list, _ := db.RecentEventUsers(ctx, testChatID, 10,
				[]storage.EventKind{tc.wantKind}, []string{wrong}); len(list) != 0 {
				t.Fatalf("event carried wrong reason %q: %+v", wrong, list)
			}
		})
	}
}

// TestRestorePendingRepliesGrace — рестарт поднимает ожидания ответа:
// истёкшие за время простоя получают секундный грейс и наказываются как
// таймаут (noreply, якорь снесён), живые ждут своего дедлайна.
func TestRestorePendingRepliesGrace(t *testing.T) {
	ctx := context.Background()

	t.Run("истёкшая за простой — грейс и noreply-кик", func(t *testing.T) {
		b, db, fc := newFlowBot(t)
		serviceableChat(t, b, db, testChatID)
		if err := db.PutGreeting(ctx, testChatID, testUserID, 500, time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := db.PutPendingReply(ctx, storage.PendingReply{
			ChatID: testChatID, UserID: testUserID,
			ExpiresAt: time.Now().Add(-time.Hour),
		}); err != nil {
			t.Fatal(err)
		}

		n, err := b.restorePendingReplies(ctx)
		if err != nil || n != 1 {
			t.Fatalf("restored = %d, err = %v; want 1, nil", n, err)
		}

		deadline := time.Now().Add(5 * time.Second)
		for {
			kinds := statsKinds(t, db, testChatID, testUserID)
			if kinds[storage.EventKick] == 1 && kinds[storage.EventBan] == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("grace noreply-kick did not land: %v", kinds)
			}
			time.Sleep(100 * time.Millisecond)
		}
		if fc.callCount("deleteMessage") == 0 {
			t.Fatal("greeting anchor must be deleted")
		}
		rows, err := db.LoadAllPendingReplies(ctx)
		if err != nil || len(rows) != 0 {
			t.Fatalf("pending_replies rows = %d, err=%v; want 0", len(rows), err)
		}
	})

	t.Run("живая — восстановлена с исходным дедлайном", func(t *testing.T) {
		b, db, _ := newFlowBot(t)
		serviceableChat(t, b, db, testChatID)
		if err := db.PutPendingReply(ctx, storage.PendingReply{
			ChatID: testChatID, UserID: testUserID,
			ExpiresAt: time.Now().Add(time.Minute),
		}); err != nil {
			t.Fatal(err)
		}

		if n, err := b.restorePendingReplies(ctx); err != nil || n != 1 {
			t.Fatalf("restored = %d, err = %v; want 1, nil", n, err)
		}
		p, ok := b.replies.Take(testChatID, testUserID)
		if !ok {
			t.Fatal("live wait must be restored into the store")
		}
		if d := time.Until(p.ExpiresAt); d <= 30*time.Second {
			t.Fatalf("restored deadline drifted: %v, want ~1m (не грейс)", d)
		}
		p.Cancel()
	})
}

// TestOnSuccessRealDisarm — прогон через НАСТОЯЩИЙ onSuccess: двухфакторный
// режим откладывает пасс до ответа; упавшее приветствие снимает ожидание с
// компенсирующим пассом; однофакторный пишет пасс сразу. Заодно фиксируем
// порядок «ожидание взводится ДО отправки приветствия».
func TestOnSuccessRealDisarm(t *testing.T) {
	tests := []struct {
		name          string
		replyCheck    bool
		breakGreeting bool
		wantPasses    int
	}{
		{"двухфакторный: приветствие ушло — пасс ждёт ответа", true, false, 0},
		{"двухфакторный: приветствие упало — disarm + компенсация", true, true, 1},
		{"однофакторный: пасс сразу", false, false, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			b, db, fc := newFlowBot(t)
			serviceableChat(t, b, db, testChatID)
			if tc.breakGreeting {
				// 400 — не ретраится по смыслу, но лестница всё равно
				// прогоняется; берём ошибку, чтобы не ждать 7 секунд 429.
				fc.err["sendMessage"] = &telegoapi.Error{ErrorCode: 400, Description: "chat not found"}
			}
			if tc.replyCheck {
				if err := db.SetReplyCheckEnabled(ctx, testChatID, true); err != nil {
					t.Fatal(err)
				}
			}

			pend := putCaptcha(b, db, testChatID, testUserID, 77)
			if err := b.onSuccess(ctx, pend, ""); err != nil {
				t.Fatal(err)
			}

			kinds := statsKinds(t, db, testChatID, testUserID)
			if got := kinds[storage.EventPass]; got != tc.wantPasses {
				t.Fatalf("pass events = %d, want %d (%v)", got, tc.wantPasses, kinds)
			}
			if fc.callCount("restrictChatMember") == 0 {
				t.Fatal("release must lift the captcha mute")
			}
			if _, ok, _ := db.MemberJoinedAt(ctx, testChatID, testUserID); !ok {
				t.Fatal("member row must be upserted on pass")
			}

			p, armed := b.replies.Take(testChatID, testUserID)
			if tc.replyCheck && !tc.breakGreeting {
				if !armed {
					t.Fatal("reply wait must be armed before the greeting send")
				}
				p.Cancel() // прибираем горутину waitReplyTimeout
				return
			}
			if armed {
				t.Fatal("no reply wait may survive onSuccess here")
			}
			rows, err := db.LoadAllPendingReplies(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 0 {
				t.Fatalf("pending_replies rows = %d, want 0", len(rows))
			}
		})
	}
}

// TestRunCaptchaAborts — карательные обрывы до персистенса не оставляют
// капчи и снимают мьют; ушедший до отправки юзер получает left вместо
// фантомного кика; счастливый путь пишет pending-строку.
func TestRunCaptchaAborts(t *testing.T) {
	user := telego.User{ID: testUserID, FirstName: "Юзер"}

	t.Run("restrict упал — abort + releaseOnAbort, капчи нет", func(t *testing.T) {
		b, db, fc := newFlowBot(t)
		fc.err["restrictChatMember"] = &telegoapi.Error{ErrorCode: 400, Description: "no rights"}

		b.runCaptcha(testChatID, user, 0)

		kinds := statsKinds(t, db, testChatID, testUserID)
		if kinds[storage.EventAbort] != 1 || kinds[storage.EventLeft] != 0 ||
			kinds[storage.EventKick]+kinds[storage.EventBan] != 0 {
			t.Fatalf("kinds = %v, want exactly one abort (не «вышли сами»)", kinds)
		}
		if _, ok := b.store.Get(testChatID, testUserID); ok {
			t.Fatal("captcha must not survive a restrict failure")
		}
		if rows := pendingRows(t, db); len(rows) != 0 {
			t.Fatalf("pending rows = %d, want 0", len(rows))
		}
	})

	t.Run("отправка упала после ретраев — abort + releaseOnAbort", func(t *testing.T) {
		b, db, fc := newFlowBot(t)
		fc.err["sendMessage"] = &telegoapi.Error{ErrorCode: 400, Description: "chat not found"}

		b.runCaptcha(testChatID, user, 0)

		kinds := statsKinds(t, db, testChatID, testUserID)
		if kinds[storage.EventAbort] != 1 || kinds[storage.EventLeft] != 0 {
			t.Fatalf("kinds = %v, funnel must close with abort", kinds)
		}
		if _, ok := b.store.Get(testChatID, testUserID); ok || len(pendingRows(t, db)) != 0 {
			t.Fatal("nothing may persist after a send failure")
		}
	})

	t.Run("юзер вышел до доставки — left, без наказания", func(t *testing.T) {
		b, db, fc := newFlowBot(t)
		fc.resp["getChatMember"] = `{"status":"left","user":{"id":7,"is_bot":false,"first_name":"Юзер"}}`

		b.runCaptcha(testChatID, user, 0)

		kinds := statsKinds(t, db, testChatID, testUserID)
		if kinds[storage.EventLeft] != 1 || kinds[storage.EventKick]+kinds[storage.EventBan] != 0 {
			t.Fatalf("departed user punished: %v", kinds)
		}
		if n := fc.callCount("deleteMessage"); n == 0 {
			t.Fatal("just-sent captcha message must be deleted")
		}
		if _, ok := b.store.Get(testChatID, testUserID); ok || len(pendingRows(t, db)) != 0 {
			t.Fatal("departed user must not get a live captcha")
		}
	})

	t.Run("счастливый путь — restrict, store, pending-строка", func(t *testing.T) {
		b, db, fc := newFlowBot(t)
		serviceableChat(t, b, db, testChatID)

		b.runCaptcha(testChatID, user, 0)

		if _, ok := b.store.Get(testChatID, testUserID); !ok {
			t.Fatal("captcha must be live in the store")
		}
		rows := pendingRows(t, db)
		if len(rows) != 1 {
			t.Fatalf("pending rows = %d, want 1", len(rows))
		}
		if fc.callCount("restrictChatMember") == 0 {
			t.Fatal("user must be restricted before the keyboard goes out")
		}
		b.cancelCaptchaSilent(testChatID, testUserID) // прибираем waitTimeout
	})

	t.Run("PutPending упал (БД закрыта) — fail-open впускает", func(t *testing.T) {
		b, db, fc := newFlowBot(t)
		_ = db.Close() // все записи падают — коррелирует с близким рестартом

		b.runCaptcha(testChatID, user, 0)

		if _, ok := b.store.Get(testChatID, testUserID); ok {
			t.Fatal("unpersistable captcha must be dropped")
		}
		if fc.callCount("restrictChatMember") < 2 {
			t.Fatalf("initial restrict + releaseOnAbort expected, calls: %v", fc.callList())
		}
	})
}

// TestBanEverywhereCrossChat — кросс-бан пропускает исходный чат, не-группы,
// необслуживаемые и доверенные чаты; остальные банит одним выстрелом c
// revoke и гасит их активные проверки.
func TestBanEverywhereCrossChat(t *testing.T) {
	ctx := context.Background()
	b, db, fc := newFlowBot(t)

	origin := int64(-100100)
	target := int64(-100200)
	trusted := int64(-100300)
	pendingChat := int64(-100500)
	serviceableChat(t, b, db, origin)  // исходный — мимо
	serviceableChat(t, b, db, target)  // должен получить бан
	serviceableChat(t, b, db, trusted) // доверенный — мимо
	if err := db.AddTrusted(ctx, trusted, testUserID, time.Now()); err != nil {
		t.Fatal(err)
	}
	b.rememberChat(ctx, storage.ChatInfo{ChatID: pendingChat, Title: "Pending", Type: "supergroup"})
	if err := db.SetChatApproval(ctx, pendingChat, storage.ChatPending); err != nil {
		t.Fatal(err)
	}
	// У целевого чата активная капча: кросс-бан обязан её погасить.
	putCaptcha(b, db, target, testUserID, 60)

	var mu sync.Mutex
	var banned []struct {
		ChatID         int64 `json:"chat_id"`
		RevokeMessages bool  `json:"revoke_messages"`
	}
	fc.errWhen = func(method string, data *telegoapi.RequestData) bool {
		if method == "banChatMember" && data != nil {
			var p struct {
				ChatID         int64 `json:"chat_id"`
				RevokeMessages bool  `json:"revoke_messages"`
			}
			if json.Unmarshal(data.BodyRaw, &p) == nil {
				mu.Lock()
				banned = append(banned, p)
				mu.Unlock()
			}
		}
		return false
	}

	links := b.banEverywhere(origin, testUserID)

	mu.Lock()
	defer mu.Unlock()
	if len(banned) != 1 || banned[0].ChatID != target || !banned[0].RevokeMessages {
		t.Fatalf("banned = %+v, want ровно [%d] c revoke", banned, target)
	}
	if len(links) != 1 {
		t.Fatalf("links = %v, want one entry", links)
	}
	// Сиротская капча целевого чата погашена.
	if _, ok := b.store.Get(target, testUserID); ok {
		t.Fatal("cross-ban must cancel the target chat's captcha")
	}
}

// TestHandleApproveCallbackTakeMatch — capok: тот же stale-guard, что у cap:,
// и тот же success-путь (пасс, release, чистка pending).
func TestHandleApproveCallbackTakeMatch(t *testing.T) {
	approveQuery := func(from telego.User, msgID int) telego.CallbackQuery {
		return telego.CallbackQuery{
			ID:   "a1",
			From: from,
			Data: "capok:" + strconv.FormatInt(testUserID, 10),
			Message: &telego.Message{
				MessageID: msgID,
				Chat:      telego.Chat{ID: testChatID, Type: "supergroup"},
			},
		}
	}

	t.Run("клик по мёртвой клавиатуре не впускает живую капчу", func(t *testing.T) {
		b, db, fc := newFlowBot(t)
		serviceableChat(t, b, db, testChatID)
		live := putCaptcha(b, db, testChatID, testUserID, 100)

		if err := b.handleApproveCallback(nil, approveQuery(leftActor, 999)); err != nil {
			t.Fatal(err)
		}
		if p, ok := b.store.Get(testChatID, testUserID); !ok || p != live {
			t.Fatal("stale approve must leave the live captcha")
		}
		if fc.callCount("restrictChatMember") != 0 {
			t.Fatal("stale approve must not release")
		}
	})

	t.Run("живое «Впустить» выпускает юзера", func(t *testing.T) {
		b, db, fc := newFlowBot(t)
		serviceableChat(t, b, db, testChatID)
		fc.resp["getChatMember"] = `{"status":"administrator","user":{"id":9,"is_bot":false,"first_name":"Аня"}}`
		putCaptcha(b, db, testChatID, testUserID, 100)

		if err := b.handleApproveCallback(nil, approveQuery(leftActor, 100)); err != nil {
			t.Fatal(err)
		}
		if _, ok := b.store.Get(testChatID, testUserID); ok {
			t.Fatal("captcha must be taken by the approve")
		}
		if fc.callCount("restrictChatMember") == 0 {
			t.Fatal("approve must release the user")
		}
		if rows := pendingRows(t, db); len(rows) != 0 {
			t.Fatalf("pending rows = %d, want 0", len(rows))
		}
		kinds := statsKinds(t, db, testChatID, testUserID)
		if kinds[storage.EventPass] != 1 {
			t.Fatalf("pass events = %d, want 1 (%v)", kinds[storage.EventPass], kinds)
		}
	})
}

// TestApprovalCallbackDecisions — appr:-кнопки в ЛС владельца исполняют
// решение; чужой клик — no-op; поздние/повторные нажатия идемпотентны;
// мёртвый вопрос статус заново не создаёт.
func TestApprovalCallbackDecisions(t *testing.T) {
	const chat = int64(-100555)
	owner := telego.User{ID: 999, FirstName: "Владелец"}
	newApprovalBot := func(t *testing.T) (*Bot, *storage.DB, *fakeCaller) {
		b, db, fc := newFlowBot(t)
		b.cfg.OwnerIDs = map[int64]struct{}{owner.ID: {}}
		return b, db, fc
	}
	queryOf := func(from telego.User, action string) telego.CallbackQuery {
		return telego.CallbackQuery{ID: "q", From: from,
			Data: "appr:" + action + ":" + strconv.FormatInt(chat, 10),
			Message: &telego.Message{MessageID: 1,
				Chat: telego.Chat{ID: owner.ID, Type: "private"}}}
	}

	t.Run("чужой клик — статус не трогается", func(t *testing.T) {
		ctx := context.Background()
		b, db, _ := newApprovalBot(t)
		_ = db.SetChatApproval(ctx, chat, storage.ChatPending)
		if err := b.handleApprovalCallback(nil, queryOf(telego.User{ID: 1}, "y")); err != nil {
			t.Fatal(err)
		}
		if st, exists, _ := db.GetChatApproval(ctx, chat); st != storage.ChatPending || !exists {
			t.Fatalf("status = %q exists=%v, want pending/true", st, exists)
		}
	})

	for _, tc := range []struct {
		name      string
		action    string
		pre       string
		want      string
		wantLeave bool
	}{
		{"pending → да: включён", "y", storage.ChatPending, storage.ChatApproved, false},
		{"pending → нет: rejected и выход", "n", storage.ChatPending, storage.ChatRejected, true},
		{"нет на approved — поздним нет не выключаем", "n", storage.ChatApproved, storage.ChatApproved, false},
		{"да на rejected — реанимация", "y", storage.ChatRejected, storage.ChatApproved, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			b, db, fc := newApprovalBot(t)
			_ = db.SetChatApproval(ctx, chat, tc.pre)

			if err := b.handleApprovalCallback(nil, queryOf(owner, tc.action)); err != nil {
				t.Fatal(err)
			}

			if !tc.wantLeave {
				if st, exists, _ := db.GetChatApproval(ctx, chat); st != tc.want || !exists {
					t.Fatalf("status = %q exists=%v, want %q", st, exists, tc.want)
				}
			} else {
				// Выход асинхронный (goSafe): ждём LeaveChat, затем dropChat,
				// который удаляет строку реестра вместе со статусом.
				deadline := time.Now().Add(3 * time.Second)
				for fc.callCount("leaveChat") == 0 && time.Now().Before(deadline) {
					time.Sleep(20 * time.Millisecond)
				}
				if fc.callCount("leaveChat") == 0 {
					t.Fatal("reject must leave the chat")
				}
				for time.Now().Before(deadline) {
					if _, exists, _ := db.GetChatApproval(ctx, chat); !exists {
						break
					}
					time.Sleep(20 * time.Millisecond)
				}
				if _, exists, _ := db.GetChatApproval(ctx, chat); exists {
					t.Fatalf("successful leave must drop the registry row (want %q)", tc.want)
				}
			}
			if b.chatApproved(chat) != (tc.want == storage.ChatApproved) {
				t.Fatal("approval cache out of sync with DB")
			}
		})
	}

	t.Run("нет строки — мёртвый вопрос не воскресает", func(t *testing.T) {
		ctx := context.Background()
		b, db, _ := newApprovalBot(t)
		if err := b.handleApprovalCallback(nil, queryOf(owner, "y")); err != nil {
			t.Fatal(err)
		}
		if _, exists, _ := db.GetChatApproval(ctx, chat); exists {
			t.Fatal("dead question must not create an approval row")
		}
	})
}

// TestGoldenVoteInstantResolve — админский клик sv: исполняет вердикт без
// кворума (живая проверка админства мимо кэша); автор за себя не голосует
// даже будучи админом.
func TestGoldenVoteInstantResolve(t *testing.T) {
	adminJSON := `{"status":"administrator","user":{"id":9,"is_bot":false,"first_name":"A"}}`
	voteQuery := func(voter int64, data string) telego.CallbackQuery {
		return telego.CallbackQuery{
			ID: "q", From: telego.User{ID: voter, FirstName: "Голосующий"}, Data: data,
			Message: &telego.Message{MessageID: 7,
				Chat: telego.Chat{ID: testChatID, Type: "supergroup"}},
		}
	}
	seedVote := func(t *testing.T, db *storage.DB) {
		t.Helper()
		if err := db.PutSpamVote(context.Background(), storage.SpamVote{
			ChatID: testChatID, BotMsgID: 7, TargetMsgID: 555,
			AuthorID: 42, Prob: 100, CreatedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}

	for _, tc := range []struct {
		name        string
		data        string
		wantBans    int
		wantSpamBan bool
	}{
		{"админ: спам — banRevoke", "sv:1", 1, true},
		{"админ: не спам — только плашка", "sv:0", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			b, db, fc := newFlowBot(t)
			serviceableChat(t, b, db, testChatID)
			fc.resp["getChatMember"] = adminJSON
			seedVote(t, db)

			if err := b.handleSpamVoteCallback(nil, voteQuery(9, tc.data)); err != nil {
				t.Fatal(err)
			}
			if got := fc.callCount("banChatMember"); got != tc.wantBans {
				t.Fatalf("banChatMember calls = %d, want %d", got, tc.wantBans)
			}
			if banned, err := db.IsSpamBanned(ctx, 42); err != nil || banned != tc.wantSpamBan {
				t.Fatalf("IsSpamBanned = %v (err %v), want %v", banned, err, tc.wantSpamBan)
			}
			kinds := statsKinds(t, db, testChatID, 42)
			if kinds[storage.EventSpamBan] != boolInt(tc.wantSpamBan) {
				t.Fatalf("spamban events = %d (%v)", kinds[storage.EventSpamBan], kinds)
			}
			if _, found, err := db.GetSpamVote(ctx, testChatID, 7); err != nil || found {
				t.Fatalf("vote found=%v err=%v, want taken", found, err)
			}
		})
	}

	t.Run("автор не голосует за себя", func(t *testing.T) {
		ctx := context.Background()
		b, db, fc := newFlowBot(t)
		serviceableChat(t, b, db, testChatID)
		fc.resp["getChatMember"] = adminJSON // даже админ-автор бессилен
		seedVote(t, db)

		if err := b.handleSpamVoteCallback(nil, voteQuery(42, "sv:1")); err != nil {
			t.Fatal(err)
		}
		if fc.callCount("banChatMember") != 0 {
			t.Fatal("self-vote must never resolve")
		}
		if _, found, _ := db.GetSpamVote(ctx, testChatID, 7); !found {
			t.Fatal("vote must survive a self-vote")
		}
	})
}

// TestSpamVoteMarginResolutionHandlerLevel — кворум добывается прямо в
// хендлере тремя доверенными голосующими: до порога голосование живёт,
// третий «за» исполняет бан.
func TestSpamVoteMarginResolutionHandlerLevel(t *testing.T) {
	ctx := context.Background()
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	if err := db.PutSpamVote(ctx, storage.SpamVote{
		ChatID: testChatID, BotMsgID: 7, TargetMsgID: 555,
		AuthorID: 42, Prob: 100, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	for voter := int64(101); voter <= 103; voter++ { // доверие: тотал > дефолтных 5
		for i := 0; i < 10; i++ {
			if _, err := db.RecordMessage(ctx, testChatID, voter, time.Now()); err != nil {
				t.Fatal(err)
			}
		}
	}
	press := func(voter int64) {
		t.Helper()
		query := telego.CallbackQuery{
			ID: "q", From: telego.User{ID: voter, FirstName: "Голосующий"}, Data: "sv:1",
			Message: &telego.Message{MessageID: 7,
				Chat: telego.Chat{ID: testChatID, Type: "supergroup"}},
		}
		if err := b.handleSpamVoteCallback(nil, query); err != nil {
			t.Fatal(err)
		}
	}

	press(101)
	press(102)
	if _, found, err := db.GetSpamVote(ctx, testChatID, 7); err != nil || !found {
		t.Fatalf("vote must survive below margin (found=%v err=%v)", found, err)
	}
	if fc.callCount("banChatMember") != 0 {
		t.Fatal("must not punish below margin")
	}

	press(103)
	if fc.callCount("banChatMember") != 1 {
		t.Fatal("margin reached → ровно один banRevoke")
	}
	if banned, err := db.IsSpamBanned(ctx, 42); err != nil || !banned {
		t.Fatalf("IsSpamBanned = %v (err %v), want true", banned, err)
	}
	if kinds := statsKinds(t, db, testChatID, 42); kinds[storage.EventSpamBan] != 1 {
		t.Fatalf("spamban events = %d (%v)", kinds[storage.EventSpamBan], kinds)
	}
}

// TestMigrationReleaseClosesFunnel — миграция выпускает юзера с записью
// пасса ПОД НОВЫЙ id (события после MigrateChat), а не оставляет «В процессе».
func TestMigrationReleaseClosesFunnel(t *testing.T) {
	ctx := context.Background()
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, migratedOldID)
	putCaptcha(b, db, migratedOldID, testUserID, 50)

	msg := telego.Message{
		Chat:              telego.Chat{ID: migratedNewID, Type: "supergroup"},
		MigrateFromChatID: migratedOldID,
	}
	if err := b.handleGroupMessage(nil, msg); err != nil {
		t.Fatal(err)
	}

	// Пасс записан в новом чате (старые события переехали MigrateChat'ом).
	s, err := db.QueryStats(ctx, migratedNewID, time.Unix(0, 0), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if s.Passed != 1 {
		t.Fatalf("pass events in new chat = %d, want 1 (%+v)", s.Passed, s)
	}
	// release целится в НОВЫЙ чат, не в старый.
	for _, body := range fc.callBodies("restrictChatMember") {
		if strings.Contains(body, strconv.FormatInt(migratedOldID, 10)) {
			t.Fatalf("release targeted the dead old chat: %s", body)
		}
	}
	if len(fc.callBodies("restrictChatMember")) == 0 {
		t.Fatal("release must restrict in the new chat")
	}
}
