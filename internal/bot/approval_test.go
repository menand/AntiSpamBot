package bot

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/menand/AntiSpamBot/internal/config"
	"github.com/menand/AntiSpamBot/internal/storage"
)

func approvalTestBot(t *testing.T) *Bot {
	t.Helper()
	ctx := context.Background()
	db, err := storage.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &Bot{
		cfg:           &config.Config{},
		db:            db,
		log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		runCtx:        ctx,
		approvalCache: make(map[int64]bool),
	}
}

func TestParseApprovalCallback(t *testing.T) {
	tests := []struct {
		name        string
		data        string
		wantApprove bool
		wantChat    int64
		wantOK      bool
	}{
		{"approve", "appr:y:-100123", true, -100123, true},
		{"reject", "appr:n:-100123", false, -100123, true},
		{"positive id", "appr:y:123", true, 123, true},
		{"wrong prefix", "menu:y:123", false, 0, false},
		{"too few parts", "appr:y", false, 0, false},
		{"too many parts", "appr:y:1:2", false, 0, false},
		{"unknown action", "appr:x:-100", false, 0, false},
		{"bad chat id", "appr:y:abc", false, 0, false},
		{"empty", "", false, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			approve, chatID, ok := parseApprovalCallback(tc.data)
			if ok != tc.wantOK || approve != tc.wantApprove || chatID != tc.wantChat {
				t.Fatalf("parseApprovalCallback(%q) = (%v, %d, %v), want (%v, %d, %v)",
					tc.data, approve, chatID, ok, tc.wantApprove, tc.wantChat, tc.wantOK)
			}
		})
	}
}

func TestChatApprovedAndServiceable(t *testing.T) {
	b := approvalTestBot(t)
	chatID := int64(-100123)

	// Незарегистрированный чат = новый = не подтверждён, не обслуживается.
	if b.chatApproved(chatID) {
		t.Fatal("unregistered chat must not be approved")
	}
	if b.chatServiceable(chatID) {
		t.Fatal("unregistered chat must not be serviceable")
	}

	// Легаси-путь (RememberChat, как до введения подтверждения) → approved.
	if err := b.db.RememberChat(b.runCtx, storage.ChatInfo{ChatID: chatID, Title: "X", Type: "group"}); err != nil {
		t.Fatal(err)
	}
	if !b.chatApproved(chatID) {
		t.Fatal("registered legacy chat must be approved")
	}
	if !b.chatServiceable(chatID) {
		t.Fatal("registered legacy chat must be serviceable")
	}

	// pending — чат инертен (симуляция пути askOwnerApproval: БД + кэш).
	_ = b.db.SetChatApproval(b.runCtx, chatID, storage.ChatPending)
	b.setApprovalCache(chatID, false)
	if b.chatApproved(chatID) {
		t.Fatal("pending chat must not be approved")
	}
	if b.chatServiceable(chatID) {
		t.Fatal("pending chat must not be serviceable")
	}

	// approved — работает как раньше.
	_ = b.db.SetChatApproval(b.runCtx, chatID, storage.ChatApproved)
	b.setApprovalCache(chatID, true)
	if !b.chatServiceable(chatID) {
		t.Fatal("approved chat must be serviceable")
	}

	// rejected — инертен.
	_ = b.db.SetChatApproval(b.runCtx, chatID, storage.ChatRejected)
	b.setApprovalCache(chatID, false)
	if b.chatServiceable(chatID) {
		t.Fatal("rejected chat must not be serviceable")
	}

	// Кэш не вечен: сброс (delApprovalCache, как в dropChat) → перечитывание БД.
	_ = b.db.SetChatApproval(b.runCtx, chatID, storage.ChatApproved)
	b.delApprovalCache(chatID)
	if !b.chatApproved(chatID) {
		t.Fatal("cache miss must re-read the DB")
	}

	// ALLOWED_CHATS-фильтр работает поверх статуса подтверждения.
	b.cfg.AllowedChats = map[int64]struct{}{12345: {}}
	if b.chatServiceable(chatID) {
		t.Fatal("chat outside ALLOWED_CHATS must not be serviceable")
	}
	b.cfg.AllowedChats[chatID] = struct{}{}
	if !b.chatServiceable(chatID) {
		t.Fatal("chat inside ALLOWED_CHATS must be serviceable")
	}
}

func TestCarryApprovalOnMigrate(t *testing.T) {
	b := approvalTestBot(t) // OWNER_IDS не настроены (len == 0)

	// approved → переносится как есть, кэш старого id чистится.
	_ = b.db.SetChatApproval(b.runCtx, 1, storage.ChatApproved)
	b.setApprovalCache(1, true)
	b.carryApprovalOnMigrate(b.runCtx, 1, 2)
	if got, exists, _ := b.db.GetChatApproval(b.runCtx, 2); !exists || got != storage.ChatApproved {
		t.Fatalf("migrated approved chat = (%q, %v), want (approved, true)", got, exists)
	}
	if !b.chatApproved(2) {
		t.Fatal("migrated approved chat must be serviceable")
	}
	if b.approvalCache[1] {
		t.Fatal("old chat id must be dropped from the approval cache")
	}

	// pending без владельцев → авто-апрув на новом id (иначе чат завис бы
	// инертным навсегда, а старые кнопки указывали бы на мёртвый id).
	_ = b.db.SetChatApproval(b.runCtx, 3, storage.ChatPending)
	b.carryApprovalOnMigrate(b.runCtx, 3, 4)
	if got, exists, _ := b.db.GetChatApproval(b.runCtx, 4); !exists || got != storage.ChatApproved {
		t.Fatalf("migrated pending chat (no owners) = (%q, %v), want (approved, true)", got, exists)
	}

	// Незарегистрированный старый чат — новый не трогается.
	b.carryApprovalOnMigrate(b.runCtx, 99, 5)
	if _, exists, _ := b.db.GetChatApproval(b.runCtx, 5); exists {
		t.Fatal("carry from unknown chat must not create a new row")
	}
}
