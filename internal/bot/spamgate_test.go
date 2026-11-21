package bot

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mymmrac/telego"

	"github.com/menand/AntiSpamBot/internal/storage"
)

func TestSpamGateCannotPublishOldReadAfterToggle(t *testing.T) {
	b, db, _ := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	llm := &fakeLLM{enabled: true}
	b.groqc = llm
	if err := db.SetSpamCheckEnabled(b.runCtx, testChatID, true); err != nil {
		t.Fatal(err)
	}
	read := make(chan error, 1)
	resume := make(chan struct{})
	defer func() {
		select {
		case <-resume:
		default:
			close(resume)
		}
	}()
	result := make(chan spamGate, 1)
	go func() {
		result <- b.cachedSpamGate(testChatID, func(ctx context.Context, chatID int64) (storage.ChatSettings, error) {
			settings, err := db.GetChatSettings(ctx, chatID)
			read <- err
			select {
			case <-resume:
			case <-t.Context().Done():
			}
			return settings, err
		})
	}()
	select {
	case err := <-read:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("settings read did not start")
	}
	if err := b.setSpamCheckEnabled(testChatID, false); err != nil {
		t.Fatal(err)
	}
	close(resume)
	select {
	case gate := <-result:
		if gate.enabled {
			t.Fatal("stale enabled value escaped the cache fill")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("settings read did not finish")
	}

	sendMessage := func(id int, userID int64, edited bool) {
		t.Helper()
		m := telego.Message{
			MessageID: id, Chat: telego.Chat{ID: testChatID, Type: telego.ChatTypeSupergroup},
			From: &telego.User{ID: userID}, Text: "Тестовое сообщение",
		}
		handler := b.handleGroupMessage
		if edited {
			handler = b.handleEditedGroupMessage
		}
		if err := handler(nil, m); err != nil {
			t.Fatal(err)
		}
	}
	sendMessage(101, 900, false)
	sendMessage(102, 901, true)
	b.maybeProfileCheck(testChatID, 902, 0)
	b.spamMu.Lock()
	inflight := len(b.spamInflight)
	b.spamMu.Unlock()
	if inflight != 0 || llm.callCount() != 0 {
		t.Fatal("a disabled chat admitted a check")
	}
	if err := b.setSpamCheckEnabled(testChatID, true); err != nil {
		t.Fatal(err)
	}
	sendMessage(103, 903, false)
	sendMessage(104, 904, true)
	b.maybeProfileCheck(testChatID, 905, 0)
	waitFor(t, func() bool { return llm.callCount() == 3 })
}

func TestSpamGateCancelsOldChecksAcrossReenable(t *testing.T) {
	b, db, _ := newFlowBot(t)
	serviceableChat(t, b, db, testChatID)
	llm := &fakeLLM{enabled: true}
	b.groqc = llm
	if err := b.setSpamCheckEnabled(testChatID, true); err != nil {
		t.Fatal(err)
	}
	old := b.cachedSpamGate(testChatID, db.GetChatSettings)
	if err := b.setSpamCheckEnabled(testChatID, false); err != nil {
		t.Fatal(err)
	}
	if err := b.setSpamCheckEnabled(testChatID, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.classifyVerdict(old.ctx, "test", "test", testChatID, 900); !errors.Is(err, context.Canceled) {
		t.Fatalf("old provider context stayed active: %v", err)
	}
	if llm.callCount() != 0 {
		t.Fatal("old check reached the provider")
	}
	current := b.cachedSpamGate(testChatID, db.GetChatSettings)
	if _, _, err := b.classifyVerdict(current.ctx, "test", "test", testChatID, 900); err != nil {
		t.Fatalf("current provider context is unavailable: %v", err)
	}
	if llm.callCount() != 1 {
		t.Fatal("re-enabled chat did not reach the provider")
	}
}
