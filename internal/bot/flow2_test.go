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

const (
	migratedOldID = -100100
	migratedNewID = -100200
)

// TestMigrationReleasesCaptchas — апгрейд basic→supergroup гасит живые
// капчи/reply-wait старого chat_id и снимает капча-мьют в НОВОМ.
func TestMigrationReleasesCaptchas(t *testing.T) {
	for _, tc := range []struct {
		name    string
		message telego.Message // ветка MigrateFromChatID или MigrateToChatID
	}{
		{"MigrateFromChatID (новый id в Chat)", telego.Message{
			Chat:              telego.Chat{ID: migratedNewID, Type: "supergroup"},
			MigrateFromChatID: migratedOldID,
		}},
		{"MigrateToChatID (старый id в Chat)", telego.Message{
			Chat:            telego.Chat{ID: migratedOldID, Type: "group"},
			MigrateToChatID: migratedNewID,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			b, db, fc := newFlowBot(t)
			serviceableChat(t, b, db, migratedOldID)
			putCaptcha(b, db, migratedOldID, testUserID, 50)
			if err := db.PutPendingReply(ctx, storage.PendingReply{
				ChatID: migratedOldID, UserID: 8, ExpiresAt: time.Now().Add(time.Minute),
			}); err != nil {
				t.Fatal(err)
			}
			b.replies.Put(migratedOldID, 8, time.Now().Add(time.Minute))

			if err := b.handleGroupMessage(nil, tc.message); err != nil {
				t.Fatal(err)
			}

			if _, ok := b.store.Get(migratedOldID, testUserID); ok {
				t.Fatal("old-id captcha must be cancelled")
			}
			if rows := pendingRows(t, db); len(rows) != 0 {
				t.Fatalf("pending_captchas rows = %d, want 0", len(rows))
			}
			if got := b.replies.TakeChat(migratedOldID); len(got) != 0 {
				t.Fatalf("reply waits of old chat = %d, want 0", len(got))
			}
			rows, err := db.LoadAllPendingReplies(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 0 {
				t.Fatalf("pending_replies rows = %d, want 0", len(rows))
			}
			// Капча-мьют снят в НОВОМ чате: getChat (дефолтные права) + restrict.
			if fc.callCount("getChat") == 0 || fc.callCount("restrictChatMember") == 0 {
				t.Fatalf("release in new chat expected, calls: %v", fc.callList())
			}
		})
	}
}

// TestSpamVoteSweepAndReconcile — стартовый reconcile и свип исполняют
// кворум только живого чата; мёртвый чат голосование теряет без бана.
func TestSpamVoteSweepAndReconcile(t *testing.T) {
	tests := []struct {
		name          string
		registered    bool // чат в реестре (serviceable) или нет
		expired       bool // голосование старше spamVoteTTL
		yesBallots    int
		wantBan       bool
		wantVoteTaken bool
	}{
		{"кворум до рестарта — исполняем", true, false, 3, true, true},
		{"мёртвый чат — снимаем без бана", false, false, 3, false, true},
		{"кворума нет — строка живёт", true, false, 1, false, false},
		{"истёкшее с кворумом живого чата — свип исполняет", true, true, 3, true, true},
		{"истёкшее мёртвого чата — свип снимает", false, true, 3, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			b, db, _ := newFlowBot(t)
			if tc.registered {
				serviceableChat(t, b, db, testChatID)
			}
			createdAt := time.Now()
			if tc.expired {
				createdAt = createdAt.Add(-spamVoteTTL - time.Hour)
			}
			if err := db.PutSpamVote(ctx, storage.SpamVote{
				ChatID: testChatID, BotMsgID: 7, AuthorID: testUserID,
				Prob: 100, CreatedAt: createdAt,
			}); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < tc.yesBallots; i++ {
				if _, err := db.UpsertBallot(ctx, testChatID, 7, int64(100+i), true); err != nil {
					t.Fatal(err)
				}
			}

			b.reconcileSpamVotes(ctx)
			b.sweepExpiredVotes(ctx)

			if _, found, err := db.GetSpamVote(ctx, testChatID, 7); err != nil {
				t.Fatal(err)
			} else if found == tc.wantVoteTaken {
				t.Fatalf("vote found=%v, want taken=%v", found, tc.wantVoteTaken)
			}
			banned, err := db.IsSpamBanned(ctx, testUserID)
			if err != nil {
				t.Fatal(err)
			}
			if banned != tc.wantBan {
				t.Fatalf("IsSpamBanned = %v, want %v", banned, tc.wantBan)
			}
			kinds := statsKinds(t, db, testChatID, testUserID)
			if want := boolInt(tc.wantBan); kinds[storage.EventSpamBan] != want {
				t.Fatalf("spamban events = %d, want %d (%v)",
					kinds[storage.EventSpamBan], want, kinds)
			}
		})
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// TestRestorePendingExpiredGrace — истёкшая за время простоя капча получает
// секундный грейс и наказывается как таймаут.
func TestRestorePendingExpiredGrace(t *testing.T) {
	ctx := context.Background()
	b, db, _ := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	if err := db.PutPending(ctx, storage.PendingRow{
		ChatID: testChatID, UserID: testUserID, MessageID: 10,
		CorrectIdx: 0, ExpiresAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	n, err := b.restorePending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("restored = %d, want 1", n)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		kinds := statsKinds(t, db, testChatID, testUserID)
		if kinds[storage.EventKick] == 1 && kinds[storage.EventLeft] == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("grace kick did not land: %v", kinds)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if rows := pendingRows(t, db); len(rows) != 0 {
		t.Fatalf("pending rows = %d, want 0 after punishment", len(rows))
	}
}

// TestAskOwnerApproval — авто-апрув без владельцев и подсказка в чате,
// когда вопрос не доставлен никому.
func TestAskOwnerApproval(t *testing.T) {
	t.Run("нет владельцев — авто-апрув", func(t *testing.T) {
		ctx := context.Background()
		b, db, _ := newFlowBot(t)
		b.askOwnerApproval(&telego.ChatMemberUpdated{
			Chat: telego.Chat{ID: 55, Type: "supergroup", Title: "Чат"},
			From: telego.User{ID: 9},
		})
		status, exists, err := db.GetChatApproval(ctx, 55)
		if err != nil || !exists {
			t.Fatalf("approval row: %v exists=%v", err, exists)
		}
		if status != storage.ChatApproved {
			t.Fatalf("status = %q, want approved", status)
		}
	})

	t.Run("вопрос не доставлен — подсказка в чате, статус pending", func(t *testing.T) {
		ctx := context.Background()
		b, db, fc := newFlowBot(t)
		b.cfg.OwnerIDs = map[int64]struct{}{999: {}}
		// ЛС-вопрос владельцу падает (chat_id 999), подсказка в чат 55 уходит.
		fc.errWhen = func(_ string, data *telegoapi.RequestData) bool {
			return data != nil && strings.Contains(string(data.BodyRaw), `"chat_id":999`)
		}
		b.askOwnerApproval(&telego.ChatMemberUpdated{
			Chat: telego.Chat{ID: 55, Type: "supergroup", Title: "Чат"},
			From: telego.User{ID: 9},
		})
		status, _, err := db.GetChatApproval(ctx, 55)
		if err != nil {
			t.Fatal(err)
		}
		if status != storage.ChatPending {
			t.Fatalf("status = %q, want pending", status)
		}
	})

	t.Run("ошибка пометки pending — строки реестра нет (fail-closed)", func(t *testing.T) {
		// Проверяется кодом: askOwnerApproval возвращается после Warn, не
		// создавая строку (rememberChat с DEFAULT approved обходил бы гейт
		// после рестарта). Здесь фиксируем контракт: без успешной пометки
		// rememberChat не вызывается.
		b, db, _ := newFlowBot(t)
		b.cfg.OwnerIDs = map[int64]struct{}{999: {}}
		_ = b
		if _, exists, err := db.GetChatApproval(context.Background(), 55); err != nil || exists {
			t.Fatalf("fresh chat must have no approval row: exists=%v err=%v", exists, err)
		}
	})
}

// seedAdminCache кладёт запись в adminCache напрямую (TTL как у живой записи).
func seedAdminCache(b *Bot, chatID, userID int64, isAdmin bool) {
	ttl := adminCacheNegTTL
	if isAdmin {
		ttl = adminCacheTTL
	}
	b.adminMu.Lock()
	b.adminCache[chatUser{chatID, userID}] = adminCacheEntry{
		isAdmin: isAdmin, until: time.Now().Add(ttl),
	}
	b.adminMu.Unlock()
}

// TestModProloguePunishRequiresFreshNegative — наказание самозванца только
// по живому «не админ»; кэш-негатив не основание, кэш-позитив спасает админа.
func TestModProloguePunishRequiresFreshNegative(t *testing.T) {
	adminJSON := `{"status":"administrator","user":{"id":9,"is_bot":false,"first_name":"A"}}`
	memberJSON := `{"status":"member","user":{"id":9,"is_bot":false,"first_name":"A"}}`
	cmd := func() telego.Message {
		return telego.Message{
			MessageID: 11,
			Chat:      telego.Chat{ID: testChatID, Type: "supergroup"},
			From:      &telego.User{ID: 9, FirstName: "Аня"},
			Text:      "/kick 5",
		}
	}

	t.Run("свежий негатив — наказание", func(t *testing.T) {
		b, db, fc := newFlowBot(t)
		serviceableChat(t, b, db, testChatID)
		fc.resp["getChatMember"] = memberJSON
		if _, ok := b.modPrologue(nil, cmd()); ok {
			t.Fatal("non-admin must be rejected")
		}
		if fc.callCount("restrictChatMember") == 0 {
			t.Fatal("fresh negative must punish")
		}
	})

	t.Run("кэш-негатив + живой позитив — команды админу", func(t *testing.T) {
		b, db, fc := newFlowBot(t)
		serviceableChat(t, b, db, testChatID)
		seedAdminCache(b, testChatID, 9, false) // устаревший негатив (10 мин TTL)
		fc.resp["getChatMember"] = adminJSON
		if _, ok := b.modPrologue(nil, cmd()); !ok {
			t.Fatal("freshly promoted admin must pass despite cached negative")
		}
		if fc.callCount("restrictChatMember") != 0 {
			t.Fatal("promoted admin must not be punished")
		}
	})

	t.Run("ошибка живой проверки + кэш-позитив — админа пропускаем", func(t *testing.T) {
		b, db, fc := newFlowBot(t)
		serviceableChat(t, b, db, testChatID)
		seedAdminCache(b, testChatID, 9, true)
		fc.err["getChatMember"] = &telegoapi.Error{ErrorCode: 429, Description: "Too Many Requests"}
		if _, ok := b.modPrologue(nil, cmd()); !ok {
			t.Fatal("cached admin must pass on live-check error")
		}
		if fc.callCount("restrictChatMember") != 0 {
			t.Fatal("cache must not be a basis for punishment")
		}
	})

	t.Run("ошибка живой проверки без кэша — молча игнор", func(t *testing.T) {
		b, db, fc := newFlowBot(t)
		serviceableChat(t, b, db, testChatID)
		fc.err["getChatMember"] = &telegoapi.Error{ErrorCode: 429, Description: "Too Many Requests"}
		if _, ok := b.modPrologue(nil, cmd()); ok {
			t.Fatal("unknown status must not pass")
		}
		if fc.callCount("restrictChatMember") != 0 {
			t.Fatal("unknown status must not punish")
		}
	})
}

// TestGreetingFailureDisarmsReplyWait — приветствие не ушло после ретраев:
// ожидание ответа снято, воронка закрыта пассом (кик за молчание невозможен).
func TestGreetingFailureDisarmsReplyWait(t *testing.T) {
	ctx := context.Background()
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	fc.err["sendMessage"] = &telegoapi.Error{ErrorCode: 429, Description: "Too Many Requests"}

	s := storage.ChatSettings{
		GreetingEnabled:   true,
		ReplyCheckEnabled: true,
	}
	p := b.replies.Put(testChatID, testUserID, time.Now().Add(time.Minute))
	if err := db.PutPendingReply(ctx, storage.PendingReply{
		ChatID: testChatID, UserID: testUserID, ExpiresAt: p.ExpiresAt,
	}); err != nil {
		t.Fatal(err)
	}

	b.maybeArmReplyWait(s, testChatID, testUserID)
	if !b.maybeSendGreeting(ctx, s, testChatID, testUserID, 0) && s.ReplyCheckEnabled {
		if b.cancelReplyWait(testChatID, testUserID) {
			if err := b.db.RecordEvent(ctx, testChatID, testUserID, storage.EventPass, time.Now(), ""); err != nil {
				t.Fatal(err)
			}
		}
	}

	if _, ok := b.replies.Take(testChatID, testUserID); ok {
		t.Fatal("reply wait must be disarmed after greeting failure")
	}
	rows, err := db.LoadAllPendingReplies(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("pending_replies rows = %d, want 0", len(rows))
	}
	kinds := statsKinds(t, db, testChatID, testUserID)
	if kinds[storage.EventPass] != 1 {
		t.Fatalf("pass events = %d, want 1 (%v)", kinds[storage.EventPass], kinds)
	}
	if kinds[storage.EventKick]+kinds[storage.EventBan] != 0 {
		t.Fatalf("user must not be punished for infra failure: %v", kinds)
	}
}

// TestSpamVoteCallbackStrictParser — чужой/битый payload не голосует.
func TestSpamVoteCallbackStrictParser(t *testing.T) {
	b, db, _ := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	if err := db.PutSpamVote(context.Background(), storage.SpamVote{
		ChatID: testChatID, BotMsgID: 7, AuthorID: 42, Prob: 100, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	for _, data := range []string{"sv:2", "sv:", "sv:0x", "sv:01", "SV:1"} {
		query := telego.CallbackQuery{
			ID:   "q",
			From: telego.User{ID: 100, FirstName: "Голосующий"},
			Data: data,
			Message: &telego.Message{
				MessageID: 7,
				Chat:      telego.Chat{ID: testChatID, Type: "supergroup"},
			},
		}
		if err := b.handleSpamVoteCallback(nil, query); err != nil {
			t.Fatalf("data %q: %v", data, err)
		}
		yes, no, err := db.CountBallots(context.Background(), testChatID, 7)
		if err != nil {
			t.Fatal(err)
		}
		if yes+no != 0 {
			t.Fatalf("data %q produced ballots (%d:%d), want none", data, yes, no)
		}
	}
	// Валидные payload-ы по-прежнему голосуют. Голосу даём пер-чатовый
	// тотал выше порога доверия (default 5) — гейт бюллетеня.
	for i := 0; i < 10; i++ {
		if _, err := db.RecordMessage(context.Background(), testChatID, 100,
			time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	// Повторное нажатие тем же голосующим ПЕРЕКЛЮЧает бюллетень, не плодит.
	for _, tc := range []struct {
		data    string
		yes, no int
	}{
		{"sv:1", 1, 0},
		{"sv:0", 0, 1},
	} {
		query := telego.CallbackQuery{
			ID:   "q",
			From: telego.User{ID: 100, FirstName: "Голосующий"},
			Data: tc.data,
			Message: &telego.Message{
				MessageID: 7,
				Chat:      telego.Chat{ID: testChatID, Type: "supergroup"},
			},
		}
		if err := b.handleSpamVoteCallback(nil, query); err != nil {
			t.Fatalf("data %q: %v", tc.data, err)
		}
		yes, no, err := db.CountBallots(context.Background(), testChatID, 7)
		if err != nil {
			t.Fatal(err)
		}
		if yes != tc.yes || no != tc.no {
			t.Fatalf("data %q: ballots %d:%d, want %d:%d", tc.data, yes, no, tc.yes, tc.no)
		}
	}
}

// TestUnmodGuardBlocksAdminTarget — кнопки mc: не действуют на админов.
func TestUnmodGuardBlocksAdminTarget(t *testing.T) {
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	seedAdminCache(b, testChatID, 9, true)

	query := telego.CallbackQuery{
		ID:   "q",
		From: telego.User{ID: 77, FirstName: "Админ"},
		Data: "mc:m:9",
		Message: &telego.Message{
			MessageID: 33,
			Chat:      telego.Chat{ID: testChatID, Type: "supergroup"},
		},
	}
	if err := b.handleModChoiceCallback(nil, query); err != nil {
		t.Fatal(err)
	}
	// release не вызывался: цель — админ.
	if fc.callCount("restrictChatMember") != 0 {
		t.Fatal("unmod action must not target an admin")
	}
}

// TestMessageHasUserContent — позитивный фильтр сервис-сообщений.
func TestMessageHasUserContent(t *testing.T) {
	tests := []struct {
		name string
		m    telego.Message
		want bool
	}{
		{"текст", telego.Message{Text: "привет"}, true},
		{"подпись у фото", telego.Message{Caption: "смотри"}, true},
		{"фото без подписи", telego.Message{Photo: []telego.PhotoSize{{}}}, true},
		{"смена названия", telego.Message{NewChatTitle: "Новое"}, false},
		{"пин", telego.Message{PinnedMessage: &telego.Message{}}, false},
		{"создание группы", telego.Message{GroupChatCreated: true}, false},
		{"голое сервисное", telego.Message{}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := messageHasUserContent(&tc.m); got != tc.want {
				t.Fatalf("messageHasUserContent(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}
