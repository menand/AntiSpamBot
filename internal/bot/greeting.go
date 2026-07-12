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

// Кастомные шаблоны приветствия ограничены сильно ниже телеграмного лимита
// 4096 символов, чтобы итоговый текст (шаблон + разметка mention) влезал
// всегда.
const maxGreetingRunes = 500

// greetInputTTL ограничивает жизнь взведённого запроса «пришли мне текст
// приветствия». Без него админ, нажавший ✏️ и ушедший, через несколько дней
// молча превратил бы случайное личное сообщение в приветствие чата.
const greetInputTTL = 15 * time.Minute

// greetInputState — взведённый запрос: для какого чата текст и когда админ
// его взвёл.
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

// renderGreeting собирает текст приветствия. Непустой кастомный шаблон
// HTML-экранируется (админы пишут обычный текст; неэкранированный ввод
// сломал бы или заабьюзил нашу отправку в ModeHTML), затем плейсхолдеры
// {name} заменяются разметкой mention. Пустой шаблон = встроенный дефолт.
func renderGreeting(template, mention string) string {
	tpl := strings.TrimSpace(template)
	if tpl == "" {
		return fmt.Sprintf("🎉 Добро пожаловать, %s!", mention)
	}
	return strings.ReplaceAll(html.EscapeString(tpl), "{name}", mention)
}

// handleGreetingCommand — no-op. Приветствие тогглится через DM-меню
// (/chats → выбрать чат → кнопка «🎉 Приветствие»). Хендлер зарегистрирован,
// чтобы /greeting в группе молча проглатывался.
func (b *Bot) handleGreetingCommand(_ *th.Context, _ telego.Message) error {
	return nil
}

// setGreetingInputPending взводит для юзера состояние «следующее личное
// сообщение — новый текст приветствия чата chatID».
func (b *Bot) setGreetingInputPending(userID, chatID int64) {
	b.greetMu.Lock()
	defer b.greetMu.Unlock()
	b.greetInput[userID] = greetInputState{chatID: chatID, armedAt: time.Now()}
}

// takeGreetingInput забирает взведённое состояние ввода приветствия.
// expired=true значит, что запрос был, но провисел дольше greetInputTTL —
// вызывающий обязан сказать админу, что текст НЕ сохранён, а не промолчать.
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

// handlePrivateText получает некомандные личные сообщения. Его единственная
// работа — флоу ввода текста приветствия; всё остальное игнорируется (та же
// тишина, что была до появления этого флоу).
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

	// Перепроверка: права могли отозвать между нажатием кнопки и этим
	// сообщением.
	if !b.canManageChat(ctx, message.From.ID, chatID) {
		reply("У тебя больше нет прав на этот чат — текст не сохранён.")
		return nil
	}

	text := strings.TrimSpace(message.Text)
	switch {
	case text == "":
		// Медиа/стикер/пусто — продолжаем ждать настоящий текст.
		b.setGreetingInputPending(message.From.ID, chatID)
		reply("Нужно обычное текстовое сообщение. Пришли текст приветствия, «-» для сброса или /cancel для отмены.")
		return nil
	case strings.HasPrefix(text, "/"):
		// Любая команда (включая /cancel) отменяет флоу ввода.
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

// previewMention рендерит mention самого админа — предпросмотр приветствия
// ровно таким, каким его увидит новичок.
func (b *Bot) previewMention(u *telego.User) string {
	if u == nil {
		return "id0"
	}
	return mentionHTML(*u)
}
