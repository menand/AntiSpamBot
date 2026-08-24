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

// goldenAdminJSON — ответ getChatMember: юзер 9 — администратор чата.
const goldenAdminJSON = `{"status":"administrator","user":{"id":9,"is_bot":false,"first_name":"Админ"}}`

// assertGoldenBanned — общие проверки мгновенного исполнения репорта:
// локальный banRevoke, событие spamban, глобальная база, без плашки,
// публичное подтверждение и ни слова про гейт доверия.
func assertGoldenBanned(t *testing.T, b *Bot, db *storage.DB, fc *fakeCaller) {
	t.Helper()
	ctx := context.Background()

	if n := fc.callCount("banChatMember"); n < 1 {
		t.Fatalf("golden report must banRevoke locally, banChatMember calls = %d", n)
	}
	if banned, err := db.IsSpamBanned(ctx, 777); err != nil || !banned {
		t.Fatalf("target must land in the global spam base (banned=%v err=%v)", banned, err)
	}
	s, err := db.QueryStats(ctx, testChatID, time.Now().Add(-time.Minute), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if s.SpamBanned != 1 {
		t.Fatalf("spamban events = %d, want 1", s.SpamBanned)
	}
	if pending, err := db.HasPendingVoteForAuthor(ctx, testChatID, 777); err != nil || pending {
		t.Fatalf("golden report must not create a vote plashka (pending=%v err=%v)", pending, err)
	}
	got := strings.Join(fc.callBodies("sendMessage"), "\n")
	if !strings.Contains(got, "распознал спамера") {
		t.Fatalf("want public confirmation, got:\n%s", got)
	}
	if strings.Contains(got, "историей сообщений") {
		t.Fatalf("golden voice bypasses the trust gate, got:\n%s", got)
	}
}

// TestSpamReportGoldenOwner — /spam от владельца бота банит сразу, без
// плашки и без гейта доверия.
func TestSpamReportGoldenOwner(t *testing.T) {
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	b.cfg.OwnerIDs = map[int64]struct{}{9: {}}

	if err := b.handleSpamCommand(nil, reportCommand(9, &telego.User{ID: 777, FirstName: "Спамер"})); err != nil {
		t.Fatal(err)
	}
	assertGoldenBanned(t, b, db, fc)
}

// TestSpamReportGoldenAdmin — то же для админа чата: живая проверка
// getChatMember мимо кэша. Фейк отвечает «администратор» только за юзера 9:
// запрос про цель (777) падает ошибкой, чтобы гард цели не счёл её админом.
func TestSpamReportGoldenAdmin(t *testing.T) {
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	fc.resp["getChatMember"] = goldenAdminJSON
	fc.errWhen = func(method string, data *telegoapi.RequestData) bool {
		if method != "getChatMember" || data == nil {
			return false
		}
		return strings.Contains(string(data.BodyRaw), "777")
	}

	if err := b.handleSpamCommand(nil, reportCommand(9, &telego.User{ID: 777, FirstName: "Спамер"})); err != nil {
		t.Fatal(err)
	}
	assertGoldenBanned(t, b, db, fc)
}

// TestSpamReportGoldenFallbackNeedsCache — ошибка getChatMember при пустом
// кэше НЕ дарит золотой голос: не-админ уходит в обычный путь (отказ по
// доверию, без бана). Единственная ветка, где не-золотой мог бы получить
// мгновенный бан.
func TestSpamReportGoldenFallbackNeedsCache(t *testing.T) {
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	fc.err["getChatMember"] = &telegoapi.Error{ErrorCode: 429, Description: "Too Many Requests"}

	if err := b.handleSpamCommand(nil, reportCommand(9, &telego.User{ID: 777, FirstName: "Цель"})); err != nil {
		t.Fatal(err)
	}
	if n := fc.callCount("banChatMember"); n != 0 {
		t.Fatalf("failed live check with empty cache must not grant golden voice (banChatMember=%d)", n)
	}
	got := strings.Join(fc.callBodies("sendMessage"), "\n")
	if !strings.Contains(got, "историей сообщений") {
		t.Fatalf("non-golden reporter must hit the trust refusal, got:\n%s", got)
	}
}

// TestSpamReportGoldenTakesPendingVote — репорт админа при висящей плашке
// обычного репорта снимает её и исполняет вердикт сам.
func TestSpamReportGoldenTakesPendingVote(t *testing.T) {
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	b.cfg.OwnerIDs = map[int64]struct{}{9: {}}
	if err := db.PutSpamVote(context.Background(), storage.SpamVote{
		ChatID:      testChatID,
		BotMsgID:    999,
		TargetMsgID: 5,
		AuthorID:    777,
		InitiatorID: 3,
		Prob:        100,
		CreatedAt:   time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	if err := b.handleSpamCommand(nil, reportCommand(9, &telego.User{ID: 777, FirstName: "Спамер"})); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if pending, err := db.HasPendingVoteForAuthor(ctx, testChatID, 777); err != nil || pending {
		t.Fatalf("pending plashka must be taken by the golden report (pending=%v err=%v)", pending, err)
	}
	assertGoldenBanned(t, b, db, fc)

	var deletedPlashka bool
	for _, body := range fc.callBodies("deleteMessage") {
		if strings.Contains(body, "999") {
			deletedPlashka = true
			break
		}
	}
	if !deletedPlashka {
		t.Fatalf("taken plashka message 999 must be deleted, deleteMessage bodies:\n%s",
			strings.Join(fc.callBodies("deleteMessage"), "\n"))
	}
}

// TestFullunbanSweepsChatsAndBase — /fullunban чистит глобальную базу
// спамеров и снимает баны/попытки в остальных чатах реестра.
func TestFullunbanSweepsChatsAndBase(t *testing.T) {
	ctx := context.Background()
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	serviceableChat(t, b, db, -100200)
	if err := db.AddSpamBanned(ctx, 555, testChatID, time.Now()); err != nil {
		t.Fatal(err)
	}
	ttl := 24 * time.Hour
	if _, err := db.IncrementAttempt(ctx, testChatID, 555, ttl); err != nil {
		t.Fatal(err)
	}
	if _, err := db.IncrementAttempt(ctx, -100200, 555, ttl); err != nil {
		t.Fatal(err)
	}

	text, err := b.execUnmod("f", testChatID, 555)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "полностью разбанен") || !strings.Contains(text, "вычеркнут из базы спамеров") {
		t.Fatalf("want full-unban summary with base cleanup, got:\n%s", text)
	}
	if banned, err := db.IsSpamBanned(ctx, 555); err != nil || banned {
		t.Fatalf("target must be wiped from the spam base (banned=%v err=%v)", banned, err)
	}
	if n, err := db.AttemptCount(ctx, testChatID, 555, ttl); err != nil || n != 0 {
		t.Fatalf("attempts in origin chat must be reset (n=%d err=%v)", n, err)
	}

	// Fanout асинхронный: поллим само DB-условие (attempts второго чата
	// сброшены) — счётчик unban-вызовов инкрементится до коммита ResetAttempts,
	// по нему можно остановиться раньше готовности.
	deadline := time.Now().Add(2 * time.Second)
	for {
		n, err := db.AttemptCount(ctx, -100200, 555, ttl)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("fanout did not reset other-chat attempts in time; unbanChatMember calls = %d", fc.callCount("unbanChatMember"))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n, err := db.AttemptCount(ctx, -100200, 555, ttl); err != nil || n != 0 {
		t.Fatalf("attempts in other chat must be reset by fanout (n=%d err=%v)", n, err)
	}
}

// TestUnbanStaysLocal — обычный /unban («u») по-прежнему трогает только свой
// чат: фонового обхода у него нет, вызов детерминирован сразу после execUnmod.
func TestUnbanStaysLocal(t *testing.T) {
	b, db, fc := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	serviceableChat(t, b, db, -100200)

	text, err := b.execUnmod("u", testChatID, 555)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text, "базе спамеров") {
		t.Fatalf("plain unban must not claim base cleanup, got:\n%s", text)
	}
	if n := fc.callCount("unbanChatMember"); n != 1 {
		t.Fatalf("plain unban must touch only the current chat, calls = %d", n)
	}
}
