package bot

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/mymmrac/telego"
)

func TestReplyWatchIgnoredEditsDoNotLogContent(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "unwatched"}[enabled], func(t *testing.T) {
			b, _, _ := newFlowBot(t)
			b.cfg.ReplyWatchEnabled = enabled
			var logs bytes.Buffer
			b.log = slog.New(slog.NewTextHandler(&logs, nil))
			b.maybeHandleReplyWatchEdit(&telego.Message{
				MessageID: 10, Chat: telego.Chat{ID: testChatID}, From: &telego.User{ID: testUserID}, Text: "private sample text",
			})
			if logs.Len() != 0 {
				t.Fatal("ignored edit was logged")
			}
		})
	}
}

func TestReplyWatchIgnoresIncompleteMessage(t *testing.T) {
	b, _, _ := newFlowBot(t)
	b.cfg.ReplyWatchEnabled = true
	b.maybeHandleReplyWatchEdit(nil)
	b.maybeHandleReplyWatchEdit(&telego.Message{MessageID: 10})
}
