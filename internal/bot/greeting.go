package bot

import (
	"context"
	"fmt"
	"strings"

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

func (b *Bot) handleGreetingCommand(ctx *th.Context, message telego.Message) error {
	if message.Chat.Type != "group" && message.Chat.Type != "supergroup" {
		return nil
	}
	if !b.chatAllowed(message.Chat.ID) {
		return nil
	}
	if message.From == nil {
		return nil
	}
	if !b.isOwner(message.From.ID) {
		isAdmin, err := b.isChatAdmin(ctx, message.Chat.ID, message.From.ID)
		if err != nil {
			b.log.Warn("check admin", "err", err)
			return nil
		}
		if !isAdmin {
			_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(message.Chat.ID),
				"Команда доступна только администраторам чата.").
				WithReplyParameters(&telego.ReplyParameters{MessageID: message.MessageID}))
			return nil
		}
	}

	fields := strings.Fields(message.Text)
	if len(fields) < 2 {
		enabled, _ := b.db.GetGreetingEnabled(ctx, message.Chat.ID)
		status := "включено ✅"
		if !enabled {
			status = "выключено ❌"
		}
		_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(message.Chat.ID),
			fmt.Sprintf(
				"Приветствие новых участников после капчи: <b>%s</b>\n\n"+
					"Чтобы поменять: <code>/greeting on</code> или <code>/greeting off</code>",
				status)).
			WithParseMode(telego.ModeHTML).
			WithReplyParameters(&telego.ReplyParameters{MessageID: message.MessageID}))
		return nil
	}

	var newVal bool
	switch strings.ToLower(fields[1]) {
	case "on", "вкл", "включить", "включи":
		newVal = true
	case "off", "выкл", "выключить", "выключи":
		newVal = false
	default:
		_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(message.Chat.ID),
			"Аргумент должен быть <code>on</code> или <code>off</code>.").
			WithParseMode(telego.ModeHTML).
			WithReplyParameters(&telego.ReplyParameters{MessageID: message.MessageID}))
		return nil
	}

	if err := b.db.SetGreetingEnabled(ctx, message.Chat.ID, newVal); err != nil {
		b.log.Warn("set greeting", "err", err)
		return nil
	}
	msg := "Приветствие <b>включено</b> для этого чата."
	if !newVal {
		msg = "Приветствие <b>выключено</b> для этого чата."
	}
	_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(message.Chat.ID), msg).
		WithParseMode(telego.ModeHTML).
		WithReplyParameters(&telego.ReplyParameters{MessageID: message.MessageID}))
	return nil
}
