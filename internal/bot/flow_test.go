package bot

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegoapi"

	"github.com/menand/AntiSpamBot/internal/captcha"
	"github.com/menand/AntiSpamBot/internal/config"
	"github.com/menand/AntiSpamBot/internal/gemini"
	"github.com/menand/AntiSpamBot/internal/gigachat"
	"github.com/menand/AntiSpamBot/internal/groq"
	"github.com/menand/AntiSpamBot/internal/storage"
)

// fakeCaller — подставной telegoapi.Caller: без сети, ответы/ошибки задаются
// по имени метода (парсится из URL). По умолчанию каждый метод отвечает
// ok:true/result:true. calls копятся под мьютом — telego дергает API из
// горутин (goSafe).
type fakeCaller struct {
	mu     sync.Mutex
	calls  []string
	bodies []string          // тела запросов, параллельно с calls (индексы совпадают)
	resp   map[string]string // method → raw JSON result
	// respSeq — method → очередь ответов: вызов берёт следующую запись,
	// последняя повторяется. Для проверок «каждой стадии серии свой ответ»
	// (например, свой ephemeral_message_id).
	respSeq map[string][]string
	err     map[string]*telegoapi.Error // method → ошибка
	// errWhen — точечный сбой: метод проходит, если fn(data)==false.
	errWhen func(method string, data *telegoapi.RequestData) bool
}

func (f *fakeCaller) Call(_ context.Context, url string, data *telegoapi.RequestData) (*telegoapi.Response, error) {
	method := url[strings.LastIndexByte(url, '/')+1:]
	body := ""
	if data != nil && data.BodyRaw != nil {
		body = string(data.BodyRaw)
	}
	f.mu.Lock()
	f.calls = append(f.calls, method)
	f.bodies = append(f.bodies, body)
	apiErr := f.err[method]
	failWhen := f.errWhen != nil && f.errWhen(method, data)
	want := f.resp[method]
	if seq := f.respSeq[method]; len(seq) > 0 {
		want = seq[0]
		if len(seq) > 1 {
			f.respSeq[method] = seq[1:]
		}
	}
	f.mu.Unlock()
	if apiErr != nil || failWhen {
		if apiErr == nil {
			apiErr = &telegoapi.Error{ErrorCode: 400, Description: "Bad Request: injected"}
		}
		return &telegoapi.Response{Ok: false, Error: apiErr}, apiErr
	}
	result := json.RawMessage("true")
	if want != "" {
		result = json.RawMessage(want)
	}
	return &telegoapi.Response{Ok: true, Result: result}, nil
}

func (f *fakeCaller) callList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	copy(out, f.calls)
	return out
}

func (f *fakeCaller) callCount(method string) int {
	n := 0
	for _, m := range f.callList() {
		if m == method {
			n++
		}
	}
	return n
}

// callBodies возвращает тела всех вызовов данного метода (в порядке отправки).
func (f *fakeCaller) callBodies(method string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for i, m := range f.calls {
		if m == method && i < len(f.bodies) {
			out = append(out, f.bodies[i])
		}
	}
	return out
}

// newFlowBot — бот с fake-API для сквозных тестов карательного пути.
const (
	testChatID = -100100
	testUserID = 7
)

func newFlowBot(t *testing.T) (*Bot, *storage.DB, *fakeCaller) {
	t.Helper()
	ctx := context.Background()
	db, err := storage.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	fc := &fakeCaller{
		resp: map[string]string{
			// Объектные ответы: голый "true" не анмаршаллится в структуру и
			// telego ретраит вызов (лишние 7 c на лестницу).
			"getChat": "{}",
			"getMe":   `{"id":42,"is_bot":true,"first_name":"Test","username":"antispam_bot"}`,
			"sendMessage": `{"message_id":555,"date":1700000000,
				"chat":{"id":-100100,"type":"supergroup"}}`,
		},
		respSeq: map[string][]string{},
		err:     map[string]*telegoapi.Error{},
	}
	api, err := telego.NewBot("110201543:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsawX", telego.WithAPICaller(fc), telego.WithDiscardLogger())
	if err != nil {
		t.Fatalf("new telego bot: %v", err)
	}
	logOut := io.Writer(io.Discard)
	if os.Getenv("BOT_TEST_LOG") != "" {
		logOut = os.Stderr
	}
	b := &Bot{
		api:     api,
		cfg:     &config.Config{MaxAttempts: 3, CaptchaStageInterval: 30 * time.Second},
		store:   captcha.NewStore(),
		db:      db,
		log:     slog.New(slog.NewTextHandler(logOut, nil)),
		me:      &telego.User{ID: 42, IsBot: true, Username: "antispam_bot"},
		runCtx:  context.Background(),
		replies: newReplyStore(),

		// Пустые ключи = провайдеры выключены (как без env в проде);
		// тесты ИИ-цепочки подменяют поля фейками llmClassifier.
		groqc:         groq.New("", ""),
		gemic:         gemini.New("", ""),
		gigac:         gigachat.New("", "", "", groq.SystemPrompt),
		chatCache:     map[int64]storage.ChatInfo{},
		userCache:     map[int64]storage.UserInfo{},
		greetInput:    map[int64]greetInputState{},
		adminCache:    map[chatUser]adminCacheEntry{},
		approvalCache: map[int64]bool{},
		spamGateCache: map[int64]bool{},
	}
	return b, db, fc
}

// serviceableChat регистрирует чат в реестре (rememberChat пишет строку с
// DEFAULT 'approved') и кэширует статус — гейт chatServiceable открыт.
func serviceableChat(t *testing.T, b *Bot, db *storage.DB, chatID int64) {
	t.Helper()
	b.rememberChat(context.Background(), storage.ChatInfo{
		ChatID: chatID, Title: "Тест", Type: "supergroup",
	})
	if err := db.SetChatApproval(context.Background(), chatID, storage.ChatApproved); err != nil {
		t.Fatalf("approve chat: %v", err)
	}
}

func statsKinds(t *testing.T, db *storage.DB, chatID, userID int64) map[storage.EventKind]int {
	t.Helper()
	s, err := db.QueryStats(context.Background(), chatID,
		time.Unix(0, 0), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("query stats: %v", err)
	}
	return map[storage.EventKind]int{
		storage.EventJoin: s.Joined, storage.EventPass: s.Passed,
		storage.EventKick: s.Kicked, storage.EventBan: s.Banned,
		storage.EventLeft: s.Left, storage.EventSpamBan: s.SpamBanned,
		storage.EventAbort: s.Aborted,
	}
}

func pendingRows(t *testing.T, db *storage.DB) []storage.PendingRow {
	t.Helper()
	rows, err := db.LoadAllPending(context.Background())
	if err != nil {
		t.Fatalf("load pending: %v", err)
	}
	return rows
}

// memberUpdate собирает chat_member-апдейт.
func memberUpdate(chatID int64, from, user telego.User, oldStatus, newStatus string) telego.Update {
	member := func(status string) telego.ChatMember {
		u := user
		switch status {
		case "member":
			return &telego.ChatMemberMember{User: u}
		case "restricted":
			return &telego.ChatMemberRestricted{User: u, IsMember: true}
		case "kicked":
			return &telego.ChatMemberBanned{User: u}
		default:
			return &telego.ChatMemberLeft{User: u}
		}
	}
	return telego.Update{ChatMember: &telego.ChatMemberUpdated{
		From:          from,
		Chat:          telego.Chat{ID: chatID, Type: "supergroup"},
		OldChatMember: member(oldStatus),
		NewChatMember: member(newStatus),
	}}
}

var (
	leftActor = telego.User{ID: 9, FirstName: "Аня"}
)

// putCaptcha заводит капчу в store и в БД.
func putCaptcha(b *Bot, db *storage.DB, chatID, userID int64, msgID int) *captcha.Pending {
	expires := time.Now().Add(time.Minute)
	_ = db.PutPending(context.Background(), storage.PendingRow{
		ChatID: chatID, UserID: userID, MessageID: msgID,
		CorrectIdx: 2, ExpiresAt: expires, Stage: 1,
	})
	return b.store.Put(chatID, userID, msgID, 2, expires, 0, 0, 1)
}

func TestOnFailLadder(t *testing.T) {
	tests := []struct {
		name        string
		preAttempts int
		maxAttempts int
		breakMethod string
		wantKind    storage.EventKind // "" — событий быть не должно
	}{
		{"первый провал — тихий кик", 0, 3, "", storage.EventKick},
		{"последний провал — бан", 2, 3, "", storage.EventBan},
		{"пер-чатовый override max=1", 0, 0, "", storage.EventBan},
		{"упавший бан события не пишет", 2, 3, "banChatMember", ""},
		{"упавший unban кика события не пишет", 0, 3, "unbanChatMember", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			b, db, fc := newFlowBot(t)
			if tc.maxAttempts > 0 {
				b.cfg.MaxAttempts = tc.maxAttempts
			} else {
				// Настоящий пер-чатовый override: max_attempts = 1 в настройках.
				one := 1
				if err := db.SetMaxAttempts(ctx, testChatID, &one); err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < tc.preAttempts; i++ {
				if _, err := db.IncrementAttempt(ctx, testChatID, testUserID, attemptsTTL); err != nil {
					t.Fatal(err)
				}
			}
			if tc.breakMethod != "" {
				fc.err[tc.breakMethod] = &telegoapi.Error{ErrorCode: 400, Description: "no rights"}
			}
			p := putCaptcha(b, db, testChatID, testUserID, 77)

			err := b.onFail(ctx, p, "тест")
			if tc.breakMethod != "" && err == nil {
				t.Fatal("expected error from broken API method")
			}
			kinds := statsKinds(t, db, testChatID, testUserID)
			if tc.wantKind == "" {
				if kinds[storage.EventKick]+kinds[storage.EventBan] != 0 {
					t.Fatalf("events after failed punishment: %v, want none", kinds)
				}
				// Pending-строка переживает провал — это механизм рестарт-повтора.
				if rows := pendingRows(t, db); len(rows) != 1 {
					t.Fatalf("pending rows = %d, want 1 (restart must retry)", len(rows))
				}
			} else {
				if err != nil {
					t.Fatalf("onFail: %v", err)
				}
				if got := kinds[tc.wantKind]; got != 1 {
					t.Fatalf("event %s count = %d, want 1 (all: %v)", tc.wantKind, got, kinds)
				}
				for _, k := range []storage.EventKind{storage.EventKick, storage.EventBan} {
					if k != tc.wantKind && kinds[k] != 0 {
						t.Fatalf("unexpected %s event (all: %v)", k, kinds)
					}
				}
			}
		})
	}
}

func TestUserLeftMidCaptcha(t *testing.T) {
	t.Run("сам вышел — left, без кика", func(t *testing.T) {
		b, db, fc := newFlowBot(t)
		serviceableChat(t, b, db, testChatID)
		putCaptcha(b, db, testChatID, testUserID, 50)

		upd := memberUpdate(testChatID, leftActor, telego.User{ID: testUserID}, "member", "left")
		if err := b.handleChatMember(nil, upd); err != nil {
			t.Fatal(err)
		}
		kinds := statsKinds(t, db, testChatID, testUserID)
		if kinds[storage.EventLeft] != 1 || kinds[storage.EventKick] != 0 {
			t.Fatalf("kinds = %v, want exactly one left and no kick", kinds)
		}
		if _, ok := b.store.Get(testChatID, testUserID); ok {
			t.Fatal("pending must be cancelled")
		}
		if rows := pendingRows(t, db); len(rows) != 0 {
			t.Fatalf("pending rows = %d, want 0", len(rows))
		}
		if n := fc.callCount("deleteMessage"); n != 1 {
			t.Fatalf("deleteMessage calls = %d, want 1", n)
		}
	})

	t.Run("кикнут админом через UI — left закрывает воронку", func(t *testing.T) {
		b, db, _ := newFlowBot(t)
		serviceableChat(t, b, db, testChatID)
		putCaptcha(b, db, testChatID, testUserID, 50)

		upd := memberUpdate(testChatID, leftActor, telego.User{ID: testUserID}, "member", "kicked")
		if err := b.handleChatMember(nil, upd); err != nil {
			t.Fatal(err)
		}
		kinds := statsKinds(t, db, testChatID, testUserID)
		if kinds[storage.EventLeft] != 1 {
			t.Fatalf("UI removal must close the funnel with left, got %v", kinds)
		}
	})

	t.Run("наше собственное наказание — лево-ветка молчит", func(t *testing.T) {
		b, db, _ := newFlowBot(t)
		serviceableChat(t, b, db, testChatID)
		putCaptcha(b, db, testChatID, testUserID, 50)

		// Инициатор (/kick, вердикт) уже погасил капчу тихо; апдейт от бота
		// не должен записать второе событие.
		b.cancelCaptchaSilent(testChatID, testUserID)
		upd := memberUpdate(testChatID, *b.me, telego.User{ID: testUserID}, "member", "kicked")
		if err := b.handleChatMember(nil, upd); err != nil {
			t.Fatal(err)
		}
		kinds := statsKinds(t, db, testChatID, testUserID)
		total := kinds[storage.EventLeft] + kinds[storage.EventKick] + kinds[storage.EventBan]
		if total != 0 {
			t.Fatalf("bot-initiated update recorded %v events", kinds)
		}
	})

	t.Run("апдейт от бота — чистка без событий", func(t *testing.T) {
		b, db, _ := newFlowBot(t)
		serviceableChat(t, b, db, testChatID)
		putCaptcha(b, db, testChatID, testUserID, 50)

		// Инициатор в ДРУГОМ чате (например, banEverywhere) событие уже
		// записал у себя; лево-ветка обязана погасить сиротскую капчу этого
		// чата (иначе таймаут снял бы кросс-бан через unban), но не писать
		// поверх инициатора ни left, ни kick.
		upd := memberUpdate(testChatID, *b.me, telego.User{ID: testUserID}, "member", "kicked")
		if err := b.handleChatMember(nil, upd); err != nil {
			t.Fatal(err)
		}
		if _, ok := b.store.Get(testChatID, testUserID); ok {
			t.Fatal("bot-origin update must cancel the orphaned captcha")
		}
		if rows := pendingRows(t, db); len(rows) != 0 {
			t.Fatalf("pending rows = %d, want 0", len(rows))
		}
		kinds := statsKinds(t, db, testChatID, testUserID)
		total := kinds[storage.EventLeft] + kinds[storage.EventKick] + kinds[storage.EventBan]
		if total != 0 {
			t.Fatalf("bot-origin cleanup recorded events: %v", kinds)
		}
	})
}

func TestCancelCaptchaSilentStopsTimeoutPunishment(t *testing.T) {
	b, db, _ := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	p := putCaptcha(b, db, testChatID, testUserID, 50)

	b.cancelCaptchaSilent(testChatID, testUserID)

	select {
	case <-p.Done():
	case <-time.After(time.Second):
		t.Fatal("silent cancel must stop the stage timer via Cancel")
	}
	// Повторный вызов безопасен (Take промахивается).
	b.cancelCaptchaSilent(testChatID, testUserID)
}

// putReplyWait заводит ожидание ответа в store и БД (стадия 1).
func putReplyWait(b *Bot, db *storage.DB, chatID, userID int64) *replyPending {
	expires := time.Now().Add(time.Minute)
	_ = db.PutPendingReply(context.Background(), storage.PendingReply{
		ChatID: chatID, UserID: userID, ExpiresAt: expires, Stage: 1,
	})
	return b.replies.Put(chatID, userID, expires, 0, 1, 0)
}

func TestWaitReplyTimeoutLadder(t *testing.T) {
	tests := []struct {
		name        string
		preAttempts int
		breakMethod string
		wantKind    storage.EventKind
	}{
		{"молчание — тихий кик", 0, "", storage.EventKick},
		{"повторное молчание — бан", 2, "", storage.EventBan},
		{"упавший бан держит строку", 2, "banChatMember", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			b, db, fc := newFlowBot(t)
			for i := 0; i < tc.preAttempts; i++ {
				if _, err := db.IncrementAttempt(ctx, testChatID, testUserID, attemptsTTL); err != nil {
					t.Fatal(err)
				}
			}
			if tc.breakMethod != "" {
				fc.err[tc.breakMethod] = &telegoapi.Error{ErrorCode: 400, Description: "no rights"}
			}
			if err := db.PutGreeting(ctx, testChatID, testUserID, 500, time.Now()); err != nil {
				t.Fatal(err)
			}
			if err := db.PutPendingReply(ctx, storage.PendingReply{
				ChatID: testChatID, UserID: testUserID,
				ExpiresAt: time.Now().Add(-time.Millisecond),
				Stage:     captchaStages,
			}); err != nil {
				t.Fatal(err)
			}
			p := b.replies.Put(testChatID, testUserID, time.Now().Add(-time.Millisecond), 0, captchaStages, 0)

			b.replyWaitLoop(testChatID, testUserID, p)

			kinds := statsKinds(t, db, testChatID, testUserID)
			if tc.wantKind == "" {
				if kinds[storage.EventBan]+kinds[storage.EventKick] != 0 {
					t.Fatalf("failed punishment wrote events: %v", kinds)
				}
				rows, err := db.LoadAllPendingReplies(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if len(rows) != 1 {
					t.Fatalf("pending_replies rows = %d, want 1 (restart must retry)", len(rows))
				}
				return
			}
			if got := kinds[tc.wantKind]; got != 1 {
				t.Fatalf("event %s = %d, want 1 (%v)", tc.wantKind, got, kinds)
			}
			// Якорь-приветствие снесён, строка ожидания удалена.
			if _, ok, err := db.TakeGreetingMsg(ctx, testChatID, testUserID); err != nil || ok {
				t.Fatalf("greeting anchor must be deleted (ok=%v err=%v)", ok, err)
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

func TestHandleCallbackTakeMatch(t *testing.T) {
	t.Run("клик по устаревшей клавиатуре не решает живую капчу", func(t *testing.T) {
		b, db, fc := newFlowBot(t)
		serviceableChat(t, b, db, testChatID)
		live := putCaptcha(b, db, testChatID, testUserID, 100)

		query := telego.CallbackQuery{
			ID:   "q1",
			From: telego.User{ID: testUserID, FirstName: "Юзер"},
			Data: "cap:" + strconv.FormatInt(testUserID, 10) + ":2", // верный индекс — но клавиатура чужая
			Message: &telego.Message{
				MessageID: 999,
				Chat:      telego.Chat{ID: testChatID, Type: "supergroup"},
			},
		}
		if err := b.handleCallback(nil, query); err != nil {
			t.Fatal(err)
		}
		if liveP, ok := b.store.Get(testChatID, testUserID); !ok || liveP != live {
			t.Fatal("stale click must leave the live captcha in place")
		}
		if fc.callCount("restrictChatMember") != 0 {
			t.Fatal("stale click must not release the user")
		}
		if rows := pendingRows(t, db); len(rows) != 1 {
			t.Fatalf("pending rows = %d, want 1", len(rows))
		}
	})

	t.Run("клик по живой клавиатуре проходит капчу", func(t *testing.T) {
		b, db, fc := newFlowBot(t)
		serviceableChat(t, b, db, testChatID)
		putCaptcha(b, db, testChatID, testUserID, 100)

		query := telego.CallbackQuery{
			ID:   "q2",
			From: telego.User{ID: testUserID, FirstName: "Юзер"},
			Data: "cap:" + strconv.FormatInt(testUserID, 10) + ":2",
			Message: &telego.Message{
				MessageID: 100,
				Chat:      telego.Chat{ID: testChatID, Type: "supergroup"},
			},
		}
		if err := b.handleCallback(nil, query); err != nil {
			t.Fatal(err)
		}
		if _, ok := b.store.Get(testChatID, testUserID); ok {
			t.Fatal("live click must take the captcha")
		}
		if fc.callCount("restrictChatMember") == 0 {
			t.Fatal("release must restrict the user back to default perms")
		}
		kinds := statsKinds(t, db, testChatID, testUserID)
		if kinds[storage.EventPass] != 1 {
			t.Fatalf("pass events = %d, want 1 (%v)", kinds[storage.EventPass], kinds)
		}
	})

	t.Run("провальный клик при упавшем бане оставляет pending-строку", func(t *testing.T) {
		b, db, fc := newFlowBot(t)
		serviceableChat(t, b, db, testChatID)
		putCaptcha(b, db, testChatID, testUserID, 100)
		fc.err["banChatMember"] = &telegoapi.Error{ErrorCode: 400, Description: "no rights"}

		query := telego.CallbackQuery{
			ID:   "q3",
			From: telego.User{ID: testUserID, FirstName: "Юзер"},
			Data: "cap:" + strconv.FormatInt(testUserID, 10) + ":0", // неверно
			Message: &telego.Message{
				MessageID: 100,
				Chat:      telego.Chat{ID: testChatID, Type: "supergroup"},
			},
		}
		if err := b.handleCallback(nil, query); err != nil {
			t.Fatal(err)
		}
		rows := pendingRows(t, db)
		if len(rows) != 1 {
			t.Fatalf("pending rows = %d, want 1 (restart must retry the ban)", len(rows))
		}
	})
}

func captchaQuery(id string, userID int64, idx int, msgID int64) telego.CallbackQuery {
	return telego.CallbackQuery{
		ID:   id,
		From: telego.User{ID: userID, FirstName: "Юзер"},
		Data: "cap:" + strconv.FormatInt(userID, 10) + ":" + strconv.Itoa(idx),
		Message: &telego.Message{
			MessageID: int(msgID),
			Chat:      telego.Chat{ID: testChatID, Type: "supergroup"},
		},
	}
}
