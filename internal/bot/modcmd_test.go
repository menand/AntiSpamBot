package bot

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegoapi"
	th "github.com/mymmrac/telego/telegohandler"

	"github.com/menand/AntiSpamBot/internal/storage"
)

// TestModTailSurvivesDeadHandlerCtx — SIGTERM посреди модерации: handler-ctx
// уже мёртв, но карательный хвост исполняется на liveCtx (runCtx, пока жив;
// иначе — свежий detached-бюджет). Транзиентный сбой бана переживается
// ретраем, событие и подтверждение на месте: отмена хендлер-контекста не
// должна съедать кик/бан.
func TestModTailSurvivesDeadHandlerCtx(t *testing.T) {
	ctx := context.Background()
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	b.cfg.OwnerIDs = map[int64]struct{}{9: {}}

	// Ровно один транзиентный сбой первого бана: ретрай обязан пройти.
	first := true
	fc.errWhen = func(method string, _ *telegoapi.RequestData) bool {
		if method == "banChatMember" && first {
			first = false
			return true
		}
		return false
	}

	bh, err := th.NewBotHandler(b.api, nil)
	if err != nil {
		t.Fatal(err)
	}
	bh.HandleMessage(func(ctx *th.Context, m telego.Message) error {
		return b.handleModCommand(ctx, m, false)
	}, th.CommandEqual("kick"))

	admin := telego.User{ID: 9, FirstName: "Аня"}
	target := telego.User{ID: testUserID, FirstName: "Гость"}
	upd := telego.Update{UpdateID: 1, Message: &telego.Message{
		MessageID:      501,
		Chat:           telego.Chat{ID: testChatID, Type: "supergroup"},
		From:           &admin,
		Text:           "/kick",
		ReplyToMessage: &telego.Message{MessageID: 500, From: &target},
	}}

	// Handler-ctx мёртв к моменту хвоста (дренаж при SIGTERM).
	dead, cancel := context.WithCancel(ctx)
	cancel()
	if err := bh.BaseGroup().HandleUpdate(dead, b.api, upd); err != nil {
		t.Fatal(err)
	}

	if got := fc.callCount("banChatMember"); got != 2 {
		t.Fatalf("banChatMember calls = %d, want 2 — ретрай обязан выжить на мёртвом handler-ctx (%v)",
			got, fc.callList())
	}
	if got := fc.callCount("unbanChatMember"); got != 1 {
		t.Fatalf("unbanChatMember calls = %d, want 1 (%v)", got, fc.callList())
	}
	s, err := db.QueryStats(ctx, testChatID, time.Now().Add(-time.Minute), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if s.ModKicked != 1 {
		t.Fatalf("mod kick events = %d, want 1 (reason mod:9)", s.ModKicked)
	}
	for _, body := range fc.callBodies("sendMessage") {
		if strings.Contains(body, "Не получилось") {
			t.Fatalf("confirmation must not be a failure text: %s", body)
		}
	}
}

// TestCleanupTargetTraces — свип следов цели модерации: капча, reply-wait,
// карантин, приветствие и плашка снимаются без серверных release (прежние
// ограничения заменяет само наказание), а синтетический lock-замок
// исполняющегося репорта не воруется и сообщением не удаляется.
func TestCleanupTargetTraces(t *testing.T) {
	ctx := context.Background()

	t.Run("полный свип", func(t *testing.T) {
		b, db, fc := newFlowBot(t)
		serviceableChat(t, b, db, testChatID)

		putCaptcha(b, db, testChatID, testUserID, 77)
		p := b.replies.Put(testChatID, testUserID, time.Now().Add(time.Minute), 0, 1, 0)
		if err := db.PutPendingReply(ctx, storage.PendingReply{
			ChatID: testChatID, UserID: testUserID, ExpiresAt: p.ExpiresAt, Stage: 1,
		}); err != nil {
			t.Fatal(err)
		}
		b.armQuarantine(ctx, testChatID, testUserID, time.Now().Add(time.Hour))
		if err := db.PutGreeting(ctx, testChatID, testUserID, 500, time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := db.PutSpamVote(ctx, storage.SpamVote{
			ChatID: testChatID, BotMsgID: 55, TargetMsgID: 44,
			AuthorID: testUserID, Prob: 100, CreatedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}

		b.cleanupTargetTraces(testChatID, testUserID)

		if _, ok := b.store.Get(testChatID, testUserID); ok {
			t.Fatal("капча должна быть изъята")
		}
		if _, ok := b.replies.Get(testChatID, testUserID); ok {
			t.Fatal("reply-wait должен быть изъят")
		}
		if _, ok := b.quar.Get(testChatID, testUserID); ok {
			t.Fatal("зеркало карантина должно быть снято")
		}
		if has, err := db.HasQuarantine(ctx, testChatID, testUserID, time.Now().Unix()); err != nil || has {
			t.Fatalf("строка карантина должна быть удалена (has=%v err=%v)", has, err)
		}
		if rows := pendingRows(t, db); len(rows) != 0 {
			t.Fatalf("pending_captchas rows = %d, want 0", len(rows))
		}
		if rows, err := db.LoadAllPendingReplies(ctx); err != nil || len(rows) != 0 {
			t.Fatalf("pending_replies rows = %d, err=%v; want 0", len(rows), err)
		}
		if n := fc.callCount("restrictChatMember"); n != 0 {
			t.Fatalf("restrictChatMember = %d, want 0 — свип не делает серверных release", n)
		}
		for _, want := range []string{`"message_id":77`, `"message_id":500`, `"message_id":55`} {
			found := false
			for _, body := range fc.callBodies("deleteMessage") {
				if strings.Contains(body, want) {
					found = true
				}
			}
			if !found {
				t.Fatalf("сообщение %s должно быть удалено; bodies=%v", want, fc.callBodies("deleteMessage"))
			}
		}
	})

	t.Run("sentinel-замок не трогается", func(t *testing.T) {
		b, db, fc := newFlowBot(t)
		serviceableChat(t, b, db, testChatID)
		if ok, err := db.PutSpamVoteOnce(ctx, storage.SpamVote{
			ChatID: testChatID, BotMsgID: -int(testUserID), TargetMsgID: 44,
			AuthorID: testUserID, Prob: 100, CreatedAt: time.Now(),
		}); err != nil || !ok {
			t.Fatalf("seed lock: ok=%v err=%v", ok, err)
		}

		b.cleanupTargetTraces(testChatID, testUserID)

		if n := fc.callCount("deleteMessage"); n != 0 {
			t.Fatalf("deleteMessage calls = %d, want 0 (у замка нет сообщения)", n)
		}
		if _, found, err := db.GetSpamVote(ctx, testChatID, -int(testUserID)); err != nil || !found {
			t.Fatalf("замок обязан пережить свип (found=%v err=%v)", found, err)
		}
	})
}

func TestParseMuteDuration(t *testing.T) {
	tests := []struct {
		text string
		want time.Duration
		ok   bool
	}{
		{"/mute 5", 5 * time.Minute, true},
		{"/mute 45m", 45 * time.Minute, true},
		{"/mute 3h", 3 * time.Hour, true},
		{"/mute 5d", 5 * 24 * time.Hour, true},
		{"/mute 45 @vasya", 45 * time.Minute, true},
		{"/mute @vasya 45", 45 * time.Minute, true},
		{"/mute@TestBot 10", 10 * time.Minute, true},
		{"/mute 400d", 365 * 24 * time.Hour, true},          // кап 365 дней
		{"/mute 999999999999d", 365 * 24 * time.Hour, true}, // overflow-защита до умножения
		{"/mute 9999999999999999999m", 0, false},            // за пределами int64 — отказ Atoi
		{"/mute", 0, false},
		{"/mute abc", 0, false},
		{"/mute 0", 0, false},
		{"/mute -5", 0, false},
		{"/mute m", 0, false},
		{"/mute 5x", 0, false},
	}
	for _, tt := range tests {
		got, ok := parseMuteDuration(tt.text)
		if got != tt.want || ok != tt.ok {
			t.Errorf("parseMuteDuration(%q) = (%v, %v), want (%v, %v)",
				tt.text, got, ok, tt.want, tt.ok)
		}
	}
}

func TestMuteLabel(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{1 * time.Minute, "1 минуту"},
		{5 * time.Minute, "5 минут"},
		{90 * time.Minute, "90 минут"},
		{2 * time.Hour, "2 часа"},
		{3 * time.Hour, "3 часа"},
		{21 * time.Hour, "21 час"},
		{1 * 24 * time.Hour, "1 день"},
		{5 * 24 * time.Hour, "5 дней"},
	}
	for _, tt := range tests {
		if got := muteLabel(tt.d); got != tt.want {
			t.Errorf("muteLabel(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestCommandForUs(t *testing.T) {
	b := &Bot{me: &telego.User{Username: "TestBot"}}
	tests := []struct {
		text string
		want bool
	}{
		{"/mute 5", true},
		{"/mute@TestBot 5", true},
		{"/mute@testbot 5", true}, // регистр не важен
		{"/mute@OtherBot 5", false},
		{"/del@combot", false},
		{"", true},
	}
	for _, tt := range tests {
		if got := b.commandForUs(tt.text); got != tt.want {
			t.Errorf("commandForUs(%q) = %v, want %v", tt.text, got, tt.want)
		}
	}
	nilMe := &Bot{}
	if !nilMe.commandForUs("/mute@OtherBot 5") {
		t.Error("commandForUs with nil me should fall back to true")
	}
}
