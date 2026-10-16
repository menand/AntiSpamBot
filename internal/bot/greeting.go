package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
	tu "github.com/mymmrac/telego/telegoutil"

	"github.com/menand/AntiSpamBot/internal/storage"
)

// Кастомные шаблоны приветствия/проверки ограничены сильно ниже телеграмного
// лимита 4096 символов, чтобы итоговый текст (шаблон + разметка mention)
// влезал всегда.
const maxGreetingRunes = 500

// Встроенные тексты стадий «после капчи»: приветствие (режим greet) и
// сообщение-проверка (режим reply — к нему replyRequirementLine добавляет
// строку-требование стадии). {name} заменяется упоминанием новичка.
const (
	defaultGreetingText = "🎉 Добро пожаловать, {name}!"
	defaultReplyText    = "привет {name}. добро пожаловать в чат. а ты точно не спамер? скажи как тебя в этот чат занесло?"
)

// quarantineNoticeLine объявляется в теле якоря: новичок сразу знает, что
// после капчи он ограничен «только текстом», а не узнаёт это первым удалённым
// сообщением. Строка идёт в тело якоря, поэтому повторяется и на напоминаниях.
const quarantineNoticeLine = "\n🧫 Карантин новичка: пока можно писать только текст — без ссылок и пересылок."

// greetInputTTL ограничивает жизнь взведённого запроса «пришли мне текст».
// Без него админ, нажавший ✏️ и ушедший, через несколько дней молча превратил
// бы случайное личное сообщение в текст чата.
const greetInputTTL = 15 * time.Minute

// greetInputState — взведённый запрос: для какого чата текст, какого рода
// (приветствие или проверка — род фиксируется в момент арма) и когда взведён.
type greetInputState struct {
	chatID  int64
	kind    welcomeMode // welcomeGreet | welcomeReply
	armedAt time.Time
}

// maybeSendGreeting шлёт якорь стадии 1 серии (или просто приветствие, когда
// второй проверки нет) и возвращает message_id и доставку. Взвод reply-ожидания
// — НЕ здесь: onSuccess делает это сразу после release, до этого сетевого
// round-trip'а, иначе юзер успел бы написать в окне release→arm и был бы кикнут
// за молчание, хотя ответил.
func (b *Bot) maybeSendGreeting(ctx context.Context, s storage.ChatSettings, mode welcomeMode, chatID, userID int64, threadID int) (int, bool) {
	msgID, sent := b.sendGreetingAnchor(ctx, s, mode, chatID, userID, threadID, 1)
	return msgID, sent
}

// sendGreetingAnchor шлёт якорное сообщение стадии stage: тело режима
// (приветствие или проверка) + строка-требование стадии (только reply).
// Напоминания (стадии 2+) повторяют то же тело с усиленной строкой — три
// сообщения, как в серии капчи. Возвращает message_id отправленного сообщения
// (0 — не отправлено) и флаг доставки. Режим none — сообщений нет вовсе
// ((0, true): «доставлять было нечего», вызывающий не считает это провалом).
func (b *Bot) sendGreetingAnchor(ctx context.Context, s storage.ChatSettings, mode welcomeMode, chatID, userID int64, threadID, stage int) (int, bool) {
	if mode == welcomeNone {
		return 0, true
	}
	infos, err := b.db.GetUserInfos(ctx, []int64{userID})
	if err != nil {
		b.log.Warn("fetch user info for greeting", "err", err)
	}
	mention := mentionOrID(infos, userID)
	text := welcomeAnchorText(s, mode, stage, int(b.effectiveStageInterval(s).Minutes()), mention)

	params := tu.Message(tu.ID(chatID), text).WithParseMode(telego.ModeHTML)
	if threadID != 0 {
		params = params.WithMessageThreadID(threadID)
	}
	// Кнопки «✅ Впустить» и «🚫 Спам» — для админов: впустить вручную
	// или пометить как спамера (с подтверждением → бан во всех чатах).
	// Нужны только в режиме проверки, где ждут ответ; приветствие всегда
	// публичное (не эфемерное), кнопки видны админам.
	if mode == welcomeReply {
		kb := tu.InlineKeyboard(
			tu.InlineKeyboardRow(
				tu.InlineKeyboardButton("✅ Впустить (для админов)").
					WithCallbackData(fmt.Sprintf("rpok:%d", userID))),
			tu.InlineKeyboardRow(
				tu.InlineKeyboardButton("🚫 Спам (для админов)").
					WithCallbackData(fmt.Sprintf("rpspam:%d", userID))))
		params = params.WithReplyMarkup(kb)
	}
	// Ретраится той же лестницей, что отправка капчи: 429 прилетает ровно во
	// время масс-джойна, а в режиме проверки провал здесь означает кик юзера
	// за «молчание» — тело с требованием он никогда не видел.
	var sent *telego.Message
	if err = retryTG(ctx, func() error {
		var e error
		sent, e = b.api.SendMessage(ctx, params)
		return e
	}); err != nil || sent == nil {
		b.log.Warn("send greeting", "err", err, "chat", chatID, "user", userID)
		return 0, false
	}
	// Помним id приветствия: при спам-бане юзера revoke стирает только его
	// сообщения, «Добро пожаловать» бота сносим сами по этой записи.
	if err := b.db.PutGreeting(ctx, chatID, userID, sent.MessageID, time.Now()); err != nil {
		b.log.Warn("remember greeting msg", "err", err, "chat", chatID, "user", userID)
	}
	return sent.MessageID, true
}

// welcomeAnchorText собирает тело якоря стадии stage так, как его увидит
// новичок: тело режима (шаблон чата или встроенный дефолт) + строка-требование
// (только reply) + объявление карантина (оба говорящих режима). Чистая
// функция — ею же рендерятся предпросмотры в ЛС администратора.
func welcomeAnchorText(s storage.ChatSettings, mode welcomeMode, stage, intervalMin int, mention string) string {
	template, entities, def := welcomeTextSource(s, mode)
	text := renderWelcome(template, entities, mention, def)
	if mode == welcomeReply {
		text += replyRequirementLine(stage, minutesGen(intervalMin))
	}
	if s.QuarantineEnabled {
		text += quarantineNoticeLine
	}
	return text
}

// welcomeTextSource — шаблон/entities/встроенный дефолт тела для режима:
// greet читает пару приветствия, reply — пару проверки.
func welcomeTextSource(s storage.ChatSettings, mode welcomeMode) (template, entitiesJSON, def string) {
	if mode == welcomeReply {
		return s.ReplyText.String, s.ReplyEntities.String, defaultReplyText
	}
	return s.GreetingText.String, s.GreetingEntities.String, defaultGreetingText
}

// renderWelcome собирает текст тела: кастомный шаблон с сохранёнными
// entities (админ форматировал жирным/курсивом) конвертируется в HTML
// конвертером entitiesToHTML — он сам экранирует пользовательский текст и
// НЕ тримит его (офсеты entities считаются от сырой строки). Плоский шаблон
// HTML-экранируется. Плейсхолдеры {name} заменяются разметкой mention ПОСЛЕ
// экранирования. Пустой шаблон = встроенный дефолт def.
func renderWelcome(template, entitiesJSON, mention, def string) string {
	if strings.TrimSpace(template) == "" {
		return strings.ReplaceAll(def, "{name}", mention)
	}
	if entitiesJSON != "" {
		var ents []telego.MessageEntity
		if err := json.Unmarshal([]byte(entitiesJSON), &ents); err == nil && len(ents) > 0 {
			return strings.ReplaceAll(entitiesToHTML(template, ents), "{name}", mention)
		}
	}
	return strings.ReplaceAll(html.EscapeString(strings.TrimSpace(template)), "{name}", mention)
}

// handleGreetingCommand — no-op. Приветствие/проверка настраиваются через
// DM-меню (/chats → выбрать чат → «✉️ После капчи»). Хендлер зарегистрирован,
// чтобы /greeting в группе молча проглатывался.
func (b *Bot) handleGreetingCommand(_ *th.Context, _ telego.Message) error {
	return nil
}

// setGreetingInputPending взводит для юзера состояние «следующее личное
// сообщение — новый текст (kind) для чата chatID».
func (b *Bot) setGreetingInputPending(userID, chatID int64, kind welcomeMode) {
	b.greetMu.Lock()
	defer b.greetMu.Unlock()
	b.greetInput[userID] = greetInputState{chatID: chatID, kind: kind, armedAt: time.Now()}
}

// takeGreetingInput забирает взведённое состояние ввода. expired=true значит,
// что запрос был, но провисел дольше greetInputTTL — вызывающий обязан
// сказать админу, что текст НЕ сохранён, а не промолчать. kind возвращается и
// на expired-пути, чтобы отказ был назван по роду текста.
func (b *Bot) takeGreetingInput(userID int64) (chatID int64, kind welcomeMode, ok, expired bool) {
	b.greetMu.Lock()
	defer b.greetMu.Unlock()
	st, found := b.greetInput[userID]
	if !found {
		return 0, "", false, false
	}
	delete(b.greetInput, userID)
	if time.Since(st.armedAt) > greetInputTTL {
		return 0, st.kind, false, true
	}
	return st.chatID, st.kind, true, false
}

// cancelGreetingInputOnCommand разряжает взведённый ввод текста, когда админ
// отправил в ЛС команду. Зарегистрированные команды (/start, /help, /stats...)
// перехватываются своими хендлерами раньше handlePrivateText и без этого
// разряда оставляли бы ввод висеть: следующее обычное сообщение молча
// превратилось бы в шаблон. Подтверждения те же, что у команд, дошедших до
// handlePrivateText («отменён» / «устарел»). Зовётся из pre-routing
// middleware; некомандные сообщения не трогает.
func (b *Bot) cancelGreetingInputOnCommand(ctx context.Context, message telego.Message) {
	if message.Chat.Type != "private" || message.From == nil || message.From.IsBot {
		return
	}
	if !strings.HasPrefix(strings.TrimSpace(message.Text), "/") {
		return
	}
	if _, kind, ok, expired := b.takeGreetingInput(message.From.ID); ok {
		_, _ = b.sendScreen(ctx, message.Chat.ID, screenPrompt,
			fmt.Sprintf("Ок, ввод текста %s отменён.", welcomeTextKindNoun(kind)), true, nil)
	} else if expired {
		_, _ = b.sendScreen(ctx, message.Chat.ID, screenPrompt,
			fmt.Sprintf("⌛ Запрос на текст %s устарел (лимит 15 минут) — текст не сохранён. Нажми ✏️ в настройках ещё раз.",
				welcomeTextKindNoun(kind)), true, nil)
	}
}

// handlePrivateText получает некомандные личные сообщения. Его единственная
// работа — флоу ввода текста «после капчи»; всё остальное игнорируется (та же
// тишина, что была до появления этого флоу).
func (b *Bot) handlePrivateText(ctx *th.Context, message telego.Message) error {
	if message.From == nil {
		return nil
	}
	chatID, kind, ok, expired := b.takeGreetingInput(message.From.ID)
	noun := welcomeTextKindNoun(kind)

	// Все сообщения флоу — один заменяемый экран screenPrompt: переспрос или
	// подтверждение гасит прошлый промпт, «пришли мне текст» не копится.
	// Ошибку отправки возвращаем вызывающим: там, где флоу продолжается,
	// ввод перевзводится только после доставленного промпта.
	reply := func(text string) error {
		_, err := b.sendScreen(ctx, message.Chat.ID, screenPrompt, text, true, nil)
		return err
	}

	// rePrompt — переспрос: ПОЛНЫЙ промпт с вводной строкой, а не короткая
	// отбивка — экран флоу один, и при переспросе не теряется контекст
	// ({name}, «текущий текст», лимит 15 минут). Настройки и заголовок читаем
	// на runCtx (та же БД-дисциплина, что у setWelcomeText), отправка — на ctx
	// хендлера. Ошибка чтения — деградация до одной вводной строки.
	rePrompt := func(lead string) error {
		s, err := b.db.GetChatSettings(b.runCtx, chatID)
		if err != nil {
			b.log.Warn("get chat settings", "err", err, "chat", chatID)
			return reply(lead)
		}
		return reply(b.welcomeInputPromptText(b.runCtx, chatID, s, kind, lead))
	}

	if expired {
		// Единственный abort-путь без ответа был бы здесь — а именно тут
		// админ мог 20 минут сочинять текст. Честно скажем, что не сохранили.
		_ = reply(fmt.Sprintf("⌛ Запрос на текст %s устарел (лимит 15 минут) — текст не сохранён. Нажми ✏️ в настройках ещё раз.", noun))
		return nil
	}
	if !ok {
		return nil
	}

	// Перепроверка: права могли отозвать между нажатием кнопки и этим
	// сообщением.
	if !b.canManageChat(ctx, message.From.ID, chatID) {
		_ = reply("У тебя больше нет прав на этот чат — текст не сохранён.")
		return nil
	}
	// Чат мог стать необслуживаемым (отклонён владельцем, выпал из
	// ALLOWED_CHATS): настройки мёртвого чата не пишем — они бы активировались
	// при его возврате (та же причина, что у гейта chatCallbackTarget).
	if !b.chatServiceable(chatID) {
		_ = reply("Чат больше не обслуживается — текст не сохранён.")
		return nil
	}

	resetLine, savedLine := welcomeTextActionNames(kind)

	text := strings.TrimSpace(message.Text)
	switch {
	case text == "":
		// Медиа/стикер/пусто — продолжаем ждать настоящий текст. Перевзвод
		// только после УДАЧНОЙ отправки переспроса (та же дисциплина, что у
		// взвода в startWelcomeTextInput): упавшая подсказка не должна
		// оставить взведённый ввод без промпта, где следующее обычное ЛС
		// молча стало бы шаблоном.
		if err := rePrompt("⚠️ Нужно именно текстовое сообщение — стикеры и медиа не подойдут."); err != nil {
			b.log.Warn("send welcome input re-prompt", "err", err, "chat", chatID)
			return nil
		}
		b.setGreetingInputPending(message.From.ID, chatID, kind)
		return nil
	case strings.HasPrefix(text, "/"):
		// Любая команда (включая /cancel) отменяет флоу ввода.
		_ = reply(fmt.Sprintf("Ок, ввод текста %s отменён.", noun))
		return nil
	case text == "-":
		if err := b.setWelcomeText(chatID, kind, nil, nil); err != nil {
			b.log.Warn("reset welcome text", "err", err, "chat", chatID, "kind", kind)
			_ = reply("Не получилось сохранить, попробуй ещё раз.")
			return nil
		}
		_ = reply(resetLine + "\n\n" + b.welcomePreview(chatID, kind, message.From))
		return nil
	}

	// При наличии форматирования (жирный/курсив...) сохраняем СЫРОЙ текст:
	// офсеты entities считаются от него, и TrimSpace их сломал бы.
	saved := text
	var entJSON *string
	if len(message.Entities) > 0 {
		saved = message.Text
		if raw, err := json.Marshal(message.Entities); err == nil {
			s := string(raw)
			entJSON = &s
		}
	}

	if len([]rune(saved)) > maxGreetingRunes {
		// Тот же условный перевзвод, что и в пустой ветке.
		lead := fmt.Sprintf("⚠️ Слишком длинно (больше %d символов) — сократи и пришли ещё раз.", maxGreetingRunes)
		if err := rePrompt(lead); err != nil {
			b.log.Warn("send welcome input re-prompt", "err", err, "chat", chatID)
			return nil
		}
		b.setGreetingInputPending(message.From.ID, chatID, kind)
		return nil
	}

	if err := b.setWelcomeText(chatID, kind, &saved, entJSON); err != nil {
		b.log.Warn("set welcome text", "err", err, "chat", chatID, "kind", kind)
		_ = reply("Не получилось сохранить, попробуй ещё раз.")
		return nil
	}
	_ = reply(savedLine + "\n\n" + b.welcomePreview(chatID, kind, message.From))
	return nil
}

// welcomeInputPromptText собирает полный текст промпта флоу «пришли мне
// текст»: правила ({name}, «-», /cancel, лимит 15 минут), сноску reply-режима
// и текущий сохранённый текст. lead — необязательная вводная строка
// (переспрос после стикера или слишком длинного текста). Настройки передаёт
// вызывающий: ошибку чтения решает он сам (startWelcomeTextInput прерывается,
// переспрос деградирует до одной вводной строки).
func (b *Bot) welcomeInputPromptText(ctx context.Context, chatID int64, s storage.ChatSettings, kind welcomeMode, lead string) string {
	current := "стандартный"
	switch kind {
	case welcomeGreet:
		if s.GreetingText.Valid && strings.TrimSpace(s.GreetingText.String) != "" {
			current = "<code>" + html.EscapeString(s.GreetingText.String) + "</code>"
		}
	case welcomeReply:
		if s.ReplyText.Valid && strings.TrimSpace(s.ReplyText.String) != "" {
			current = "<code>" + html.EscapeString(s.ReplyText.String) + "</code>"
		}
	}
	extra := ""
	if kind == welcomeReply {
		extra = "Строка «напиши что-нибудь…» и кнопки для админов добавятся сами.\n"
	}
	if lead != "" {
		lead += "\n\n"
	}
	return lead + fmt.Sprintf(
		"✏️ Пришли мне текст %s для чата <b>%s</b> одним сообщением.\n\n"+
			"Подстановка: <code>{name}</code> — имя новичка.\n"+
			extra+
			"Отправь «-», чтобы вернуть стандартный текст. /cancel — отмена.\n"+
			"Запрос действует 15 минут.\n\n"+
			"Текущий текст: %s",
		welcomeTextKindNoun(kind), html.EscapeString(b.chatTitle(ctx, chatID)), current)
}

// setWelcomeText пишет текст по роду ввода: приветствие → greeting_text,
// проверка → reply_text (со своими entities).
func (b *Bot) setWelcomeText(chatID int64, kind welcomeMode, text, entitiesJSON *string) error {
	if kind == welcomeReply {
		return b.db.SetReplyText(b.runCtx, chatID, text, entitiesJSON)
	}
	return b.db.SetGreetingText(b.runCtx, chatID, text, entitiesJSON)
}

// welcomePreview рендерит якорь стадии 1 так, как его увидит новичок —
// предпросмотр ровно того, что увидит чат (включая строку-требование и
// карантинную сноску). u — админ, чьё имя подставляется вместо {name}.
func (b *Bot) welcomePreview(chatID int64, kind welcomeMode, u *telego.User) string {
	s := b.chatSettings(b.runCtx, chatID)
	return welcomeAnchorText(s, kind, 1, int(b.effectiveStageInterval(s).Minutes()), b.previewMention(u))
}

// previewMention рендерит mention самого админа — предпросмотр приветствия
// ровно таким, каким его увидит новичок.
func (b *Bot) previewMention(u *telego.User) string {
	if u == nil {
		return "id0"
	}
	return mentionHTML(*u)
}
