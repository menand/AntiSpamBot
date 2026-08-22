package bot

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mymmrac/telego"

	"github.com/menand/AntiSpamBot/internal/storage"
)

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
