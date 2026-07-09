package bot

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
	tu "github.com/mymmrac/telego/telegoutil"

	"github.com/menand/AntiSpamBot/internal/storage"
)

const (
	// spamVoteMargin — перевес голосов, решающий вердикт.
	spamVoteMargin = 3
	// spamVoteTTL — сколько живёт плашка без кворума. Держим сильно меньше
	// 48 ч: дольше Telegram вообще не даст боту удалить своё сообщение.
	spamVoteTTL = 24 * time.Hour

	defaultSpamThreshold = 90
	defaultSpamWhitelist = 5

	adminCacheTTL = 6 * time.Hour

	// spamFactsTextLimit — сколько рун текста уходит в Groq. Спам-простыни
	// длиннее не делают вердикт лучше, а токены жгут.
	spamFactsTextLimit = 1500
)

// effectiveSpamThreshold/effectiveSpamWhitelist — чистые резолверы поверх уже
// загруженных настроек (в отличие от effective*-хелперов access.go, которые
// ходят в БД сами): спам-чек и меню и так держат ChatSettings в руках.
func effectiveSpamThreshold(s storage.ChatSettings) int {
	if s.SpamThreshold.Valid {
		if v := int(s.SpamThreshold.Int64); v >= 1 && v <= 100 {
			return v
		}
	}
	return defaultSpamThreshold
}

func effectiveSpamWhitelist(s storage.ChatSettings) int {
	if s.SpamWhitelistMsgs.Valid && s.SpamWhitelistMsgs.Int64 > 0 {
		return int(s.SpamWhitelistMsgs.Int64)
	}
	return defaultSpamWhitelist
}

type adminCacheEntry struct {
	isAdmin bool
	until   time.Time
}

// isChatAdminCached — isChatAdmin с кэшем (TTL 6 ч + инвалидация из
// handleChatMember при смене статуса). Используется на каждом сообщении
// не-вайтлистнутых юзеров и на каждом голосе — без кэша это API-вызов.
func (b *Bot) isChatAdminCached(ctx context.Context, chatID, userID int64) bool {
	k := chatUserKey(chatID, userID)
	b.adminMu.Lock()
	e, ok := b.adminCache[k]
	b.adminMu.Unlock()
	if ok && time.Now().Before(e.until) {
		return e.isAdmin
	}
	isAdmin, err := b.isChatAdmin(ctx, chatID, userID)
	if err != nil {
		// Ошибку не кэшируем: следующий вызов попробует снова.
		return false
	}
	b.adminMu.Lock()
	b.adminCache[k] = adminCacheEntry{isAdmin: isAdmin, until: time.Now().Add(adminCacheTTL)}
	b.adminMu.Unlock()
	return isAdmin
}

func (b *Bot) invalidateAdminCache(chatID, userID int64) {
	b.adminMu.Lock()
	delete(b.adminCache, chatUserKey(chatID, userID))
	b.adminMu.Unlock()
}

func chatUserKey(chatID, userID int64) string {
	return fmt.Sprintf("%d:%d", chatID, userID)
}

// maybeSpamCheck — хук в конце handleGroupMessage. Решает, надо ли гнать
// сообщение в Groq, и если да — делает это асинхронно (LLM-запрос в хендлере
// заблокировал бы обработку апдейтов).
func (b *Bot) maybeSpamCheck(message telego.Message) {
	if !b.groqc.Enabled() || message.From == nil {
		return
	}
	chatID := message.Chat.ID
	user := *message.From

	s, err := b.db.GetChatSettings(b.runCtx, chatID)
	if err != nil || !s.SpamCheckEnabled {
		return
	}
	// Белый список, от дешёвого к дорогому. total уже включает текущее
	// сообщение (счётчики пишутся до хука): анализируются первые N сообщений.
	total, err := b.db.UserMessageTotal(b.runCtx, chatID, user.ID)
	if err != nil {
		b.log.Warn("spam check: message total", "err", err, "chat", chatID, "user", user.ID)
		return
	}
	if total > effectiveSpamWhitelist(s) {
		return
	}
	if b.isOwner(user.ID) || b.isChatAdminCached(b.runCtx, chatID, user.ID) {
		return
	}
	// Одна плашка на автора: при вердикте «спам» banRevoke снесёт все его
	// сообщения, флагать каждое нет смысла.
	if pending, err := b.db.HasPendingVoteForAuthor(b.runCtx, chatID, user.ID); err != nil || pending {
		return
	}
	// Дедуп параллельных сообщений того же автора, пока Groq думает.
	k := chatUserKey(chatID, user.ID)
	b.spamMu.Lock()
	if _, busy := b.spamInflight[k]; busy {
		b.spamMu.Unlock()
		return
	}
	b.spamInflight[k] = struct{}{}
	b.spamMu.Unlock()

	go func() {
		defer func() {
			b.spamMu.Lock()
			delete(b.spamInflight, k)
			b.spamMu.Unlock()
		}()
		b.runSpamCheck(message, s, total)
	}()
}

func (b *Bot) runSpamCheck(message telego.Message, s storage.ChatSettings, msgTotal int) {
	chatID := message.Chat.ID
	user := *message.From

	memberFor := ""
	if joinedAt, ok, err := b.db.MemberJoinedAt(b.runCtx, chatID, user.ID); err == nil && ok {
		memberFor = humanDurationRU(time.Since(joinedAt))
	}
	facts := buildSpamFacts(message, memberFor, msgTotal)

	ctx, cancel := context.WithTimeout(b.runCtx, 20*time.Second)
	defer cancel()
	prob, err := b.groqc.SpamProbability(ctx, facts)
	if err != nil {
		// Fail-open: сбой Groq не трогает сообщение.
		b.log.Warn("groq spam check failed (fail-open)", "err", err, "chat", chatID, "user", user.ID)
		return
	}
	threshold := effectiveSpamThreshold(s)
	b.log.Info("spam check verdict", "chat", chatID, "user", user.ID,
		"prob", prob, "threshold", threshold, "msg_total", msgTotal)
	if prob < threshold {
		return
	}

	sent, err := b.api.SendMessage(b.runCtx,
		tu.Message(tu.ID(chatID), spamVoteText(prob, 0, 0)).
			WithParseMode(telego.ModeHTML).
			WithReplyParameters(&telego.ReplyParameters{MessageID: message.MessageID}).
			WithReplyMarkup(spamVoteKeyboard()))
	if err != nil {
		b.log.Warn("send spam vote message", "err", err, "chat", chatID)
		return
	}
	if err := b.db.PutSpamVote(b.runCtx, storage.SpamVote{
		ChatID:      chatID,
		BotMsgID:    sent.MessageID,
		TargetMsgID: message.MessageID,
		AuthorID:    user.ID,
		Prob:        prob,
		CreatedAt:   time.Now(),
	}); err != nil {
		// Без строки в БД кнопки мертвы — снимаем плашку, не мусорим.
		b.log.Error("persist spam vote", "err", err, "chat", chatID)
		_ = b.deleteMessage(b.runCtx, chatID, sent.MessageID)
	}
}

// buildSpamFacts собирает то, что уходит в Groq: контекст автора, факт
// вложения (сам файл — никогда), пометка о форварде и текст/подпись.
func buildSpamFacts(m telego.Message, memberFor string, msgTotal int) string {
	var sb strings.Builder
	name := strings.TrimSpace(m.From.FirstName + " " + m.From.LastName)
	if name == "" {
		name = "(без имени)"
	}
	fmt.Fprintf(&sb, "Автор: %s", name)
	if m.From.Username != "" {
		fmt.Fprintf(&sb, " (@%s)", m.From.Username)
	}
	if memberFor != "" {
		fmt.Fprintf(&sb, ", в чате %s", memberFor)
	}
	fmt.Fprintf(&sb, ", всего сообщений: %d.\n", msgTotal)

	if m.ForwardOrigin != nil {
		switch fo := m.ForwardOrigin.(type) {
		case *telego.MessageOriginChannel:
			fmt.Fprintf(&sb, "Переслано из канала «%s».\n", fo.Chat.Title)
		case *telego.MessageOriginChat:
			fmt.Fprintf(&sb, "Переслано из чата «%s».\n", fo.SenderChat.Title)
		default:
			sb.WriteString("Переслано из другого источника.\n")
		}
	}

	text := m.Text
	if text == "" {
		text = m.Caption
	}
	if kind := attachmentKindRU(m); kind != "" {
		if text == "" {
			fmt.Fprintf(&sb, "Вложение: %s, без текста.\n", kind)
		} else {
			fmt.Fprintf(&sb, "Вложение: %s с подписью.\n", kind)
		}
	}
	if text != "" {
		fmt.Fprintf(&sb, "Текст сообщения:\n%s", truncateLabel(text, spamFactsTextLimit))
	}
	return sb.String()
}

// attachmentKindRU — человекочитаемый тип вложения для промпта; "" = нет.
func attachmentKindRU(m telego.Message) string {
	switch {
	case m.Animation != nil: // проверять до Document: гифка выставляет оба
		return "гифка"
	case len(m.Photo) > 0:
		return "фото"
	case m.Video != nil:
		return "видео"
	case m.VideoNote != nil:
		return "видеосообщение (кружок)"
	case m.Voice != nil:
		return "голосовое сообщение"
	case m.Sticker != nil:
		return "стикер"
	case m.Audio != nil:
		return "аудио"
	case m.Document != nil:
		return "файл"
	case m.Poll != nil:
		return "опрос"
	}
	return ""
}

func humanDurationRU(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "меньше минуты"
	case d < time.Hour:
		n := int(d.Minutes())
		return fmt.Sprintf("%d %s", n, pluralRU(n, "минуту", "минуты", "минут"))
	case d < 48*time.Hour:
		n := int(d.Hours())
		return fmt.Sprintf("%d %s", n, pluralRU(n, "час", "часа", "часов"))
	default:
		n := int(d.Hours() / 24)
		return fmt.Sprintf("%d %s", n, pluralRU(n, "день", "дня", "дней"))
	}
}

func spamVoteText(prob, yes, no int) string {
	return fmt.Sprintf(
		"🤖 Мне кажется, это спам (уверенность %d%%).\n\n"+
			"Голосуйте кнопками — перевес в %d голоса решает. Голос админа решает сразу.\n\n"+
			"🚫 Спам: <b>%d</b> · ✅ Не спам: <b>%d</b>",
		prob, spamVoteMargin, yes, no)
}

func spamVoteKeyboard() *telego.InlineKeyboardMarkup {
	return tu.InlineKeyboard(tu.InlineKeyboardRow(
		tu.InlineKeyboardButton("🚫 Да, спам").WithCallbackData("sv:1"),
		tu.InlineKeyboardButton("✅ Нет, не спам").WithCallbackData("sv:0"),
	))
}

// handleSpamVoteCallback — нажатия «Да, спам» / «Нет, не спам».
func (b *Bot) handleSpamVoteCallback(ctx *th.Context, query telego.CallbackQuery) error {
	if query.Message == nil {
		return nil
	}
	isSpamVote := query.Data == "sv:1"
	chatID := query.Message.GetChat().ID
	botMsgID := query.Message.GetMessageID()
	voter := query.From.ID

	v, found, err := b.db.GetSpamVote(b.runCtx, chatID, botMsgID)
	if err != nil {
		b.log.Warn("get spam vote", "err", err, "chat", chatID)
		return nil
	}
	if !found {
		_ = b.api.AnswerCallbackQuery(ctx,
			tu.CallbackQuery(query.ID).WithText("Голосование уже закрыто."))
		return nil
	}
	if voter == v.AuthorID {
		_ = b.api.AnswerCallbackQuery(ctx,
			tu.CallbackQuery(query.ID).WithText("Нельзя голосовать за своё сообщение.").WithShowAlert())
		return nil
	}

	// Золотой голос: админ или владелец бота решает единолично.
	if b.isOwner(voter) || b.isChatAdminCached(b.runCtx, chatID, voter) {
		_ = b.api.AnswerCallbackQuery(ctx,
			tu.CallbackQuery(query.ID).WithText("Решено голосом админа."))
		b.resolveSpamVote(v, isSpamVote, fmt.Sprintf("админ %d", voter))
		return nil
	}

	if err := b.db.UpsertBallot(b.runCtx, chatID, botMsgID, voter, isSpamVote); err != nil {
		b.log.Warn("upsert ballot", "err", err, "chat", chatID)
		return nil
	}
	yes, no, err := b.db.CountBallots(b.runCtx, chatID, botMsgID)
	if err != nil {
		b.log.Warn("count ballots", "err", err, "chat", chatID)
		return nil
	}
	_ = b.api.AnswerCallbackQuery(ctx, tu.CallbackQuery(query.ID).WithText("Голос учтён."))

	switch verdict, decided := voteVerdict(yes, no); {
	case decided:
		b.resolveSpamVote(v, verdict, fmt.Sprintf("голоса %d:%d", yes, no))
	default:
		// Обновляем счёт на плашке; клавиатура остаётся той же.
		if _, err := b.api.EditMessageText(b.runCtx, &telego.EditMessageTextParams{
			ChatID:      tu.ID(chatID),
			MessageID:   botMsgID,
			Text:        spamVoteText(v.Prob, yes, no),
			ParseMode:   telego.ModeHTML,
			ReplyMarkup: spamVoteKeyboard(),
		}); err != nil {
			b.log.Warn("edit spam vote tally", "err", err, "chat", chatID)
		}
	}
	return nil
}

// voteVerdict — чистая логика кворума: перевес spamVoteMargin решает.
func voteVerdict(yes, no int) (isSpam, decided bool) {
	switch {
	case yes-no >= spamVoteMargin:
		return true, true
	case no-yes >= spamVoteMargin:
		return false, true
	}
	return false, false
}

// resolveSpamVote исполняет вердикт. TakeSpamVote атомарен: первый вызвавший
// исполняет, проигравшие гонку выходят молча — двойной бан/удаление исключены.
func (b *Bot) resolveSpamVote(v storage.SpamVote, spam bool, why string) {
	taken, err := b.db.TakeSpamVote(b.runCtx, v.ChatID, v.BotMsgID)
	if err != nil {
		b.log.Warn("take spam vote", "err", err, "chat", v.ChatID)
		return
	}
	if !taken {
		return
	}
	if spam {
		if err := b.banRevoke(b.runCtx, v.ChatID, v.AuthorID); err != nil {
			b.log.Warn("spam ban", "err", err, "chat", v.ChatID, "user", v.AuthorID)
		}
		_ = b.db.RecordEvent(b.runCtx, v.ChatID, v.AuthorID, storage.EventSpamBan, time.Now())
		// revoke обычно уже стёр сообщение; ручное удаление — страховка на
		// случай неудавшегося бана (нет прав), поэтому ошибку глушим тихо.
		if err := b.deleteMessage(b.runCtx, v.ChatID, v.TargetMsgID); err != nil {
			b.log.Debug("delete spam target (already gone?)", "err", err, "chat", v.ChatID)
		}
		b.log.Info("spam verdict: ban", "chat", v.ChatID, "user", v.AuthorID,
			"prob", v.Prob, "why", why)
	} else {
		b.log.Info("spam verdict: not spam", "chat", v.ChatID, "user", v.AuthorID,
			"prob", v.Prob, "why", why)
	}
	if err := b.deleteMessage(b.runCtx, v.ChatID, v.BotMsgID); err != nil {
		b.log.Warn("delete spam vote message", "err", err, "chat", v.ChatID)
	}
}

// spamVoteSweepLoop закрывает голосования без кворума: раз в час (и сразу на
// старте — рестарт мог проспать дедлайны) снимает плашки старше spamVoteTTL.
func (b *Bot) spamVoteSweepLoop(ctx context.Context) {
	sweep := func() {
		expired, err := b.db.ExpiredSpamVotes(ctx, time.Now().Add(-spamVoteTTL))
		if err != nil {
			b.log.Warn("expired spam votes", "err", err)
			return
		}
		for _, v := range expired {
			taken, err := b.db.TakeSpamVote(ctx, v.ChatID, v.BotMsgID)
			if err != nil || !taken {
				continue
			}
			if err := b.deleteMessage(ctx, v.ChatID, v.BotMsgID); err != nil {
				b.log.Warn("delete expired spam vote message", "err", err, "chat", v.ChatID)
			}
			b.log.Info("spam vote expired without quorum",
				"chat", v.ChatID, "user", v.AuthorID, "prob", v.Prob)
		}
	}
	sweep()
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}
