package bot

import (
	"context"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
	tu "github.com/mymmrac/telego/telegoutil"
)

// Custom greeting templates are capped well below Telegram's 4096-char
// message limit so the rendered text (template + mention markup) always fits.
const maxGreetingRunes = 500

// greetInputTTL bounds how long an armed "send me the greeting text" prompt
// stays live. Without it, an admin who tapped ✏️ and walked away would have
// an unrelated private message days later silently become the greeting.
const greetInputTTL = 15 * time.Minute

// greetInputState is the armed prompt: which chat the text is for and when
// the admin armed it.
type greetInputState struct {
	chatID  int64
	armedAt time.Time
}

func (b *Bot) maybeSendGreeting(ctx context.Context, chatID, userID int64, threadID int) {
	s := b.chatSettings(ctx, chatID)
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
	sent, err := b.api.SendMessage(ctx, params)
	if err != nil {
		b.log.Warn("send greeting", "err", err, "chat", chatID, "user", userID)
		return
	}
	// Помним id приветствия: при спам-бане юзера revoke стирает только его
	// сообщения, «Добро пожаловать» бота сносим сами по этой записи.
	if err := b.db.PutGreeting(ctx, chatID, userID, sent.MessageID, time.Now()); err != nil {
		b.log.Warn("remember greeting msg", "err", err, "chat", chatID, "user", userID)
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
	b.greetInput[userID] = greetInputState{chatID: chatID, armedAt: time.Now()}
}

// takeGreetingInput consumes the pending greeting-input state. expired=true
// means the prompt existed but sat armed longer than greetInputTTL — the
// caller should tell the admin their text was NOT saved, not stay silent.
func (b *Bot) takeGreetingInput(userID int64) (chatID int64, ok, expired bool) {
	b.greetMu.Lock()
	defer b.greetMu.Unlock()
	st, found := b.greetInput[userID]
	if !found {
		return 0, false, false
	}
	delete(b.greetInput, userID)
	if time.Since(st.armedAt) > greetInputTTL {
		return 0, false, true
	}
	return st.chatID, true, false
}

// handlePrivateText receives non-command private messages. Its only job is
// the greeting-text input flow; anything else is ignored (same silence as
// before this flow existed).
func (b *Bot) handlePrivateText(ctx *th.Context, message telego.Message) error {
	if message.From == nil {
		return nil
	}
	chatID, ok, expired := b.takeGreetingInput(message.From.ID)

	reply := func(text string) {
		_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(message.Chat.ID), text).
			WithParseMode(telego.ModeHTML))
	}

	if expired {
		// Единственный abort-путь без ответа был бы здесь — а именно тут
		// админ мог 20 минут сочинять текст. Честно скажем, что не сохранили.
		reply("⌛ Запрос на текст приветствия устарел (лимит 15 минут) — текст не сохранён. Нажми ✏️ в настройках ещё раз.")
		return nil
	}
	if !ok {
		return nil
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
