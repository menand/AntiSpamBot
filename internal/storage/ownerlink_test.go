package storage

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

const testLinkBotID = 42

func TestOwnerDMLinkRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	now := time.Now()

	if err := db.PutOwnerDMLink(ctx, testLinkBotID, 7, 900, -1001, 55, 500, true, now); err != nil {
		t.Fatalf("put link: %v", err)
	}
	link, ok, err := db.OwnerDMLink(ctx, testLinkBotID, 7, 900)
	if err != nil || !ok {
		t.Fatalf("owner dm link: ok=%v err=%v", ok, err)
	}
	if link.ChatID != -1001 || link.ThreadID != 55 || link.SourceMsgID != 500 || !link.IsForward {
		t.Fatalf("link = %+v, want chat -1001 thread 55 source 500 forward", link)
	}

	// Повторная запись по той же паре — upsert, а не дубль.
	if err := db.PutOwnerDMLink(ctx, testLinkBotID, 7, 900, -1002, 0, 600, false, now); err != nil {
		t.Fatalf("upsert link: %v", err)
	}
	link, ok, _ = db.OwnerDMLink(ctx, testLinkBotID, 7, 900)
	if !ok || link.ChatID != -1002 || link.ThreadID != 0 || link.SourceMsgID != 600 || link.IsForward {
		t.Fatalf("upserted link = %+v", link)
	}

	// Чужой владелец и чужой id сообщения линка не видят.
	if _, ok, _ := db.OwnerDMLink(ctx, testLinkBotID, 8, 900); ok {
		t.Fatalf("other owner must not see the link")
	}
	if _, ok, _ := db.OwnerDMLink(ctx, testLinkBotID, 7, 901); ok {
		t.Fatalf("unknown dm message must not resolve")
	}

	// Нулевые id — «связывать нечего»: строки нет, ошибки нет.
	if err := db.PutOwnerDMLink(ctx, testLinkBotID, 7, 0, -1001, 0, 500, true, now); err != nil {
		t.Fatalf("zero dm id must be a no-op, got %v", err)
	}
}

// TestOwnerDMLinkBoundToBot — ЛС-нумерация своя у каждого бота, а БД переживает
// смену BOT_TOKEN: карточка старого бота с тем же dm_message_id не должна
// находиться у нового. Иначе ответ владельца ушёл бы в чужой чат.
func TestOwnerDMLinkBoundToBot(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	now := time.Now()

	if err := db.PutOwnerDMLink(ctx, testLinkBotID, 7, 900, -1001, 0, 500, true, now); err != nil {
		t.Fatalf("put link: %v", err)
	}
	if _, ok, _ := db.OwnerDMLink(ctx, 99, 7, 900); ok {
		t.Fatalf("link of another bot must not resolve")
	}
	// Другой бот может завести свою связку с тем же (владелец, dm id).
	if err := db.PutOwnerDMLink(ctx, 99, 7, 900, -1002, 0, 600, false, now); err != nil {
		t.Fatalf("put link for another bot: %v", err)
	}
	own, ok, _ := db.OwnerDMLink(ctx, testLinkBotID, 7, 900)
	if !ok || own.ChatID != -1001 || own.SourceMsgID != 500 {
		t.Fatalf("first bot's link = %+v ok=%v", own, ok)
	}
	other, ok, _ := db.OwnerDMLink(ctx, 99, 7, 900)
	if !ok || other.ChatID != -1002 || other.SourceMsgID != 600 {
		t.Fatalf("second bot's link = %+v ok=%v", other, ok)
	}
}

func TestPruneOwnerDMLinks(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	now := time.Now()

	if err := db.PutOwnerDMLink(ctx, testLinkBotID, 7, 900, -1001, 0, 500, true, now.Add(-40*24*time.Hour)); err != nil {
		t.Fatalf("put old link: %v", err)
	}
	if err := db.PutOwnerDMLink(ctx, testLinkBotID, 7, 901, -1001, 0, 501, true, now); err != nil {
		t.Fatalf("put fresh link: %v", err)
	}
	n, err := db.PruneOwnerDMLinks(ctx, now.Add(-30*24*time.Hour))
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n != 1 {
		t.Fatalf("pruned %d links, want 1", n)
	}
	if _, ok, _ := db.OwnerDMLink(ctx, testLinkBotID, 7, 900); ok {
		t.Fatalf("old link must be pruned")
	}
	if _, ok, _ := db.OwnerDMLink(ctx, testLinkBotID, 7, 901); !ok {
		t.Fatalf("fresh link must survive")
	}
}

// TestOwnerDMLinkDroppedOnChatMigration — при апгрейде basic group → supergroup
// message id старого чата умирают, поэтому связки чата удаляются: перенесённый
// source_msg_id в новой нумерации указывает на чужое сообщение.
func TestOwnerDMLinkDroppedOnChatMigration(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	now := time.Now()

	if err := db.PutOwnerDMLink(ctx, testLinkBotID, 7, 900, -1001, 0, 500, true, now); err != nil {
		t.Fatalf("put link: %v", err)
	}
	if err := db.MigrateChat(ctx, -1001, -1002); err != nil {
		t.Fatalf("migrate chat: %v", err)
	}
	if _, ok, _ := db.OwnerDMLink(ctx, testLinkBotID, 7, 900); ok {
		t.Fatalf("link of the migrated chat must be dropped (message ids die)")
	}
}

// TestOwnerDMLinkDevTableWithoutBotID — dev-БД, где owner_dm_links успела
// появиться в прежней форме (PK без bot_id): Open обязан пройти (ALTER
// добавляет bot_id), старые строки не находятся ни у какого бота, а новая
// запись кладётся поверх старого PK.
func TestOwnerDMLinkDevTableWithoutBotID(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")

	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := raw.ExecContext(ctx, `
		CREATE TABLE owner_dm_links (
			owner_id      INTEGER NOT NULL,
			dm_message_id INTEGER NOT NULL,
			chat_id       INTEGER NOT NULL,
			thread_id     INTEGER NOT NULL DEFAULT 0,
			source_msg_id INTEGER NOT NULL,
			is_forward    INTEGER NOT NULL DEFAULT 0,
			at            INTEGER NOT NULL,
			PRIMARY KEY (owner_id, dm_message_id)
		);
		INSERT INTO owner_dm_links VALUES (7, 900, -1001, 0, 500, 1, 0);
	`); err != nil {
		t.Fatalf("seed legacy table: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open with legacy owner_dm_links: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// bot_id = 0 у старых строк: мост не должен находить их ни у какого бота.
	if link, ok, err := db.OwnerDMLink(ctx, testLinkBotID, 7, 900); err != nil || ok {
		t.Fatalf("legacy row must not resolve: ok=%v err=%v link=%+v", ok, err, link)
	}
	// Новая запись по той же паре заменяет легаси-строку (старый PK).
	if err := db.PutOwnerDMLink(ctx, testLinkBotID, 7, 900, -1002, 0, 600, false, time.Now()); err != nil {
		t.Fatalf("put link over legacy row: %v", err)
	}
	link, ok, err := db.OwnerDMLink(ctx, testLinkBotID, 7, 900)
	if err != nil || !ok {
		t.Fatalf("new row must resolve: ok=%v err=%v", ok, err)
	}
	if link.ChatID != -1002 || link.SourceMsgID != 600 || link.IsForward {
		t.Fatalf("new row = %+v, want chat -1002 source 600 card", link)
	}
}

// TestPutOwnerDMLinkIncompleteKeys — любое незаполненное поле = «связывать
// нечего»: ни строки, ни ошибки (форвард/карточка могли не отправиться).
func TestPutOwnerDMLinkIncompleteKeys(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	now := time.Now()

	cases := []struct {
		name   string
		bot    int64
		owner  int64
		dm     int
		chat   int64
		source int
	}{
		{name: "нет бота", bot: 0, owner: 7, dm: 900, chat: -1001, source: 500},
		{name: "нет владельца", bot: testLinkBotID, owner: 0, dm: 900, chat: -1001, source: 500},
		{name: "нет ЛС-сообщения", bot: testLinkBotID, owner: 7, dm: 0, chat: -1001, source: 500},
		{name: "нет чата", bot: testLinkBotID, owner: 7, dm: 900, chat: 0, source: 500},
		{name: "нет исходника", bot: testLinkBotID, owner: 7, dm: 900, chat: -1001, source: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := db.PutOwnerDMLink(ctx, tc.bot, tc.owner, tc.dm, tc.chat, 0, tc.source, true, now); err != nil {
				t.Fatalf("incomplete key must be a no-op, got %v", err)
			}
			if _, ok, _ := db.OwnerDMLink(ctx, testLinkBotID, 7, 900); ok {
				t.Fatalf("row must not be written")
			}
		})
	}
}
