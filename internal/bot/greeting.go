package bot

import (
	"context"
	"fmt"

	"github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
	tu "github.com/mymmrac/telego/telegoutil"
)

func (b *Bot) maybeSendGreeting(ctx context.Context, chatID, userID int64) {
	enabled, err := b.db.GetGreetingEnabled(ctx, chatID)
	if err != nil {
		b.log.Warn("get greeting flag", "err", err, "chat", chatID)
		return
	}
	if !enabled {
		return
	}
	infos, err := b.db.GetUserInfos(ctx, []int64{userID})
	if err != nil {
		b.log.Warn("fetch user info for greeting", "err", err)
	}
	mention := mentionOrID(infos, userID)
	text := fmt.Sprintf("🎉 Добро пожаловать, %s!", mention)
	_, err = b.api.SendMessage(ctx, tu.Message(tu.ID(chatID), text).
		WithParseMode(telego.ModeHTML))
	if err != nil {
		b.log.Warn("send greeting", "err", err, "chat", chatID, "user", userID)
	}
}

// handleGreetingCommand is a no-op. Greeting toggles are done via the DM menu
// (/chats → pick chat → "🎉 Приветствие" button). Kept registered so that if
// someone types /greeting in a group the command is swallowed silently.
func (b *Bot) handleGreetingCommand(_ *th.Context, _ telego.Message) error {
	return nil
}
