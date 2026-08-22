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

// Тесты-регрессии по итогам полного ревью: гейт владельца на переспрос
// аппрувов через /start, чужой ручной кик посреди reply-wait и гейт
// глобальной базы спамеров на успех локального бана.

func TestStartReAskGate(t *testing.T) {
	seedPending := func(t *testing.T, b *Bot, db *storage.DB) {
		t.Helper()
		b.rememberChat(context.Background(), storage.ChatInfo{
			ChatID: -100555, Title: "Секретный чат", Type: "supergroup",
		})
		if err := db.SetChatApproval(context.Background(), -100555, storage.ChatPending); err != nil {
			t.Fatal(err)
		}
	}
	startMsg := func(fromID int64) telego.Message {
		return telego.Message{
			Chat: telego.Chat{ID: fromID, Type: "private"},
			From: &telego.User{ID: fromID, FirstName: "Юзер"},
		}
	}
	sentPrompt := func(fc *fakeCaller) bool {
		for _, body := range fc.callBodies("sendMessage") {
			if strings.Contains(body, "appr:") || strings.Contains(body, "Секретный чат") {
				return true
			}
		}
		return false
	}

	t.Run("чужой /start не переспрашивает и не видит титулы", func(t *testing.T) {
		b, db, fc := newFlowBot(t)
		b.cfg.OwnerIDs = map[int64]struct{}{999: {}}
		seedPending(t, b, db)

		if err := b.handlePrivateStart(nil, startMsg(777)); err != nil {
			t.Fatal(err)
		}
		if sentPrompt(fc) {
			t.Fatal("foreign /start must not re-send approval prompts nor leak pending titles")
		}
	})

	t.Run("владелец получает переспрос", func(t *testing.T) {
		b, db, fc := newFlowBot(t)
		b.cfg.OwnerIDs = map[int64]struct{}{999: {}}
		seedPending(t, b, db)

		if err := b.handlePrivateStart(nil, startMsg(999)); err != nil {
			t.Fatal(err)
		}
		if !sentPrompt(fc) {
			t.Fatal("owner /start must re-ask all pending chats")
		}
	})
}

func TestForeignKickDuringReplyWait(t *testing.T) {
	armWait := func(t *testing.T, b *Bot, db *storage.DB) {
		t.Helper()
		exp := time.Now().Add(time.Minute)
		b.replies.Put(testChatID, testUserID, exp)
		if err := db.PutPendingReply(context.Background(), storage.PendingReply{
			ChatID: testChatID, UserID: testUserID, ExpiresAt: exp,
		}); err != nil {
			t.Fatal(err)
		}
	}
	waitCleared := func(t *testing.T, b *Bot, db *storage.DB) {
		t.Helper()
		if _, ok := b.replies.Take(testChatID, testUserID); ok {
			t.Fatal("reply wait must be cancelled")
		}
		rows, err := db.LoadAllPendingReplies(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 0 {
			t.Fatalf("pending_replies rows = %d, want 0", len(rows))
		}
	}

	t.Run("ручной кик админа закрывает воронку left", func(t *testing.T) {
		b, db, _ := newFlowBot(t)
		serviceableChat(t, b, db, testChatID)
		armWait(t, b, db)

		upd := memberUpdate(testChatID, leftActor,
			telego.User{ID: testUserID}, "member", "kicked")
		if err := b.handleChatMember(nil, upd); err != nil {
			t.Fatal(err)
		}

		kinds := statsKinds(t, db, testChatID, testUserID)
		if kinds[storage.EventLeft] != 1 {
			t.Fatalf("kinds = %v, want exactly one left (user must not hang in «В процессе»)", kinds)
		}
		waitCleared(t, b, db)
	})

	t.Run("свой бот-кик событий не дублирует", func(t *testing.T) {
		b, db, _ := newFlowBot(t)
		serviceableChat(t, b, db, testChatID)
		armWait(t, b, db)

		upd := memberUpdate(testChatID, telego.User{ID: 42},
			telego.User{ID: testUserID}, "member", "kicked")
		if err := b.handleChatMember(nil, upd); err != nil {
			t.Fatal(err)
		}

		kinds := statsKinds(t, db, testChatID, testUserID)
		for _, k := range []storage.EventKind{storage.EventLeft, storage.EventKick, storage.EventBan} {
			if kinds[k] != 0 {
				t.Fatalf("bot-origin kick recorded %v (all: %v) — initiator writes its own event", k, kinds)
			}
		}
		waitCleared(t, b, db)
	})
}

func TestResolveSpamVoteGatesGlobalBaseOnBanSuccess(t *testing.T) {
	vote := storage.SpamVote{
		ChatID:      testChatID,
		BotMsgID:    321,
		TargetMsgID: 322,
		AuthorID:    testUserID,
		CreatedAt:   time.Now(),
	}
	run := func(t *testing.T, breakBan bool) bool {
		t.Helper()
		ctx := context.Background()
		b, db, fc := newFlowBot(t)
		serviceableChat(t, b, db, testChatID)
		if err := db.PutSpamVote(ctx, vote); err != nil {
			t.Fatal(err)
		}
		if breakBan {
			fc.err["banChatMember"] = &telegoapi.Error{ErrorCode: 403, Description: "not enough rights"}
		}
		if !b.resolveSpamVote(vote, true, "тест") {
			t.Fatal("resolver must win the take")
		}
		spamBanned, err := db.IsSpamBanned(ctx, testUserID)
		if err != nil {
			t.Fatal(err)
		}
		return spamBanned
	}

	t.Run("несостоявшийся бан глобальную базу не пишет", func(t *testing.T) {
		if run(t, true) {
			t.Fatal("unenforceable verdict must not feed the global spammer base")
		}
	})
	t.Run("удавшийся бан пишет базу", func(t *testing.T) {
		if !run(t, false) {
			t.Fatal("successful ban must add the user to the global spammer base")
		}
	})
}
