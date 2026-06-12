package bot

import (
	"context"
	"fmt"
	"html"
	"strings"

	"github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
	tu "github.com/mymmrac/telego/telegoutil"
)

// Custom greeting templates are capped well below Telegram's 4096-char
// message limit so the rendered text (template + mention markup) always fits.
const maxGreetingRunes = 500

func (b *Bot) maybeSendGreeting(ctx context.Context, chatID, userID int64, threadID int) {
	s, err := b.db.GetChatSettings(ctx, chatID)
	if err != nil {
		b.log.Warn("get chat settings for greeting", "err", err, "chat", chatID)
		return
	}
	if !s.GreetingEnabled {
		return
	}
	infos, err := b.db.GetUserInfos(ctx, []int64{userID})
	if err != nil {
		b.log.Warn("fetch user info for greeting", "err", err)
	}
	mention := mentionOrID(infos, userID)
	text := renderGreeting(s.GreetingText.String, mention)

	params := tu.Message(tu.ID(chatID), text).WithParseMode(telego.ModeHTML)
	if threadID != 0 {
		params = params.WithMessageThreadID(threadID)
	}
	if _, err = b.api.SendMessage(ctx, params); err != nil {
		b.log.Warn("send greeting", "err", err, "chat", chatID, "user", userID)
	}
}

// renderGreeting builds the greeting text. A non-empty custom template is
// HTML-escaped (admins type plain text; unescaped input would break or abuse
// our ModeHTML send) and its {name} placeholders are replaced with the
// mention markup. Empty template = built-in default.
func renderGreeting(template, mention string) string {
	tpl := strings.TrimSpace(template)
	if tpl == "" {
		return fmt.Sprintf("🎉 Добро пожаловать, %s!", mention)
	}
	return strings.ReplaceAll(html.EscapeString(tpl), "{name}", mention)
}

// handleGreetingCommand is a no-op. Greeting toggles are done via the DM menu
// (/chats → pick chat → "🎉 Приветствие" button). Kept registered so that if
// someone types /greeting in a group the command is swallowed silently.
func (b *Bot) handleGreetingCommand(_ *th.Context, _ telego.Message) error {
	return nil
}

// setGreetingInputPending arms the "next private message is the new greeting
// text for chatID" state for a user.
func (b *Bot) setGreetingInputPending(userID, chatID int64) {
	b.greetMu.Lock()
	defer b.greetMu.Unlock()
	b.greetInput[userID] = chatID
}

// takeGreetingInput consumes the pending greeting-input state, if armed.
func (b *Bot) takeGreetingInput(userID int64) (int64, bool) {
	b.greetMu.Lock()
	defer b.greetMu.Unlock()
	chatID, ok := b.greetInput[userID]
	if ok {
		delete(b.greetInput, userID)
	}
	return chatID, ok
}

// handlePrivateText receives non-command private messages. Its only job is
// the greeting-text input flow; anything else is ignored (same silence as
// before this flow existed).
func (b *Bot) handlePrivateText(ctx *th.Context, message telego.Message) error {
	if message.From == nil {
		return nil
	}
	chatID, ok := b.takeGreetingInput(message.From.ID)
	if !ok {
		return nil
	}

	reply := func(text string) {
		_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(message.Chat.ID), text).
			WithParseMode(telego.ModeHTML))
	}

	// Re-check: rights could have been revoked between the button tap and
	// this message.
	if !b.canManageChat(ctx, message.From.ID, chatID) {
		reply("У тебя больше нет прав на этот чат — текст не сохранён.")
		return nil
	}

	text := strings.TrimSpace(message.Text)
	switch {
	case text == "":
		// Media/sticker/empty — keep waiting for an actual text message.
		b.setGreetingInputPending(message.From.ID, chatID)
		reply("Нужно обычное текстовое сообщение. Пришли текст приветствия, «-» для сброса или /cancel для отмены.")
		return nil
	case strings.HasPrefix(text, "/"):
		// Any command (incl. /cancel) aborts the input flow.
		reply("Ок, ввод текста приветствия отменён.")
		return nil
	case text == "-":
		if err := b.db.SetGreetingText(b.runCtx, chatID, nil); err != nil {
			b.log.Warn("reset greeting text", "err", err, "chat", chatID)
			reply("Не получилось сохранить, попробуй ещё раз.")
			return nil
		}
		reply("Вернул стандартное приветствие:\n\n" + renderGreeting("", b.previewMention(message.From)))
		return nil
	}

	if len([]rune(text)) > maxGreetingRunes {
		b.setGreetingInputPending(message.From.ID, chatID)
		reply(fmt.Sprintf("Слишком длинно (больше %d символов). Сократи и пришли ещё раз.", maxGreetingRunes))
		return nil
	}

	if err := b.db.SetGreetingText(b.runCtx, chatID, &text); err != nil {
		b.log.Warn("set greeting text", "err", err, "chat", chatID)
		reply("Не получилось сохранить, попробуй ещё раз.")
		return nil
	}
	reply("Сохранил! Так будет выглядеть приветствие:\n\n" + renderGreeting(text, b.previewMention(message.From)))
	return nil
}

// previewMention renders the asking admin's own mention — used to preview the
// greeting exactly as a new member would see it.
func (b *Bot) previewMention(u *telego.User) string {
	if u == nil {
		return "id0"
	}
	return mentionHTML(*u)
}
