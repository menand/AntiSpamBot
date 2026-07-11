package bot

import (
	"context"
	"errors"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
	tu "github.com/mymmrac/telego/telegoutil"

	"github.com/menand/AntiSpamBot/internal/storage"
)

const (
	// defaultSpamVoteMargin — перевес голосов, решающий вердикт (настройка чата).
	defaultSpamVoteMargin = 3
	// spamVoteTTL — сколько живёт плашка без кворума. Держим сильно меньше
	// 48 ч: дольше Telegram вообще не даст боту удалить своё сообщение.
	spamVoteTTL = 24 * time.Hour

	defaultSpamThreshold = 90
	defaultSpamWhitelist = 5

	// adminCacheTTL — кэш «юзер — админ». Позитивные ответы живут долго:
	// разжалование бот увидит по chat_member и инвалидирует. Негативные —
	// коротко: в чате, где бот сам не админ, Telegram не шлёт chat_member
	// вообще, и свежеповышенный админ иначе ждал бы до 6 часов.
	adminCacheTTL    = 6 * time.Hour
	adminCacheNegTTL = 10 * time.Minute

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

func effectiveSpamVoteMargin(s storage.ChatSettings) int {
	if s.SpamVoteMargin.Valid {
		if v := int(s.SpamVoteMargin.Int64); v >= 1 && v <= 10 {
			return v
		}
	}
	return defaultSpamVoteMargin
}

// userLabel — человекочитаемая подпись юзера для логов: «Имя Фамилия (@ник, id123)».
func userLabel(u telego.User) string {
	name := strings.TrimSpace(u.FirstName + " " + u.LastName)
	if name == "" {
		name = "(без имени)"
	}
	if u.Username != "" {
		return fmt.Sprintf("%s (@%s, id%d)", name, u.Username, u.ID)
	}
	return fmt.Sprintf("%s (id%d)", name, u.ID)
}

type adminCacheEntry struct {
	isAdmin bool
	until   time.Time
}

// isChatAdminCached — isChatAdmin с кэшем (TTL 6 ч + инвалидация из
// handleChatMember при смене статуса). Используется на каждом сообщении
// не-вайтлистнутых юзеров, на каждом голосе и во всём DM-меню — без кэша
// каждая такая проверка была бы API-вызовом.
func (b *Bot) isChatAdminCached(ctx context.Context, chatID, userID int64) bool {
	k := chatUser{chatID, userID}
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
	ttl := adminCacheTTL
	if !isAdmin {
		ttl = adminCacheNegTTL
	}
	b.adminMu.Lock()
	b.adminCache[k] = adminCacheEntry{isAdmin: isAdmin, until: time.Now().Add(ttl)}
	b.adminMu.Unlock()
	return isAdmin
}

func (b *Bot) invalidateAdminCache(chatID, userID int64) {
	b.adminMu.Lock()
	delete(b.adminCache, chatUser{chatID, userID})
	b.adminMu.Unlock()
}

// spamAIEnabled — доступен ли хоть один LLM-провайдер для спам-анализа.
func (b *Bot) spamAIEnabled() bool {
	return b.groqc.Enabled() || b.gigac.Enabled()
}

// maybeSpamCheck — хук в конце handleGroupMessage. Решает, надо ли гнать
// сообщение в LLM, и если да — делает это асинхронно (запрос в хендлере
// заблокировал бы обработку апдейтов).
func (b *Bot) maybeSpamCheck(message telego.Message) {
	if !b.spamAIEnabled() || message.From == nil {
		return
	}
	chatID := message.Chat.ID
	user := *message.From

	s := b.chatSettings(b.runCtx, chatID)
	if !s.SpamCheckEnabled {
		return
	}
	// Белый список, от дешёвого к дорогому. total уже включает текущее
	// сообщение (счётчики пишутся до хука): анализируются первые N сообщений.
	total, err := b.db.UserMessageTotal(b.runCtx, chatID, user.ID)
	if err != nil {
		b.log.Warn("spam check: message total", "err", err, "chat", chatID, "user", user.ID)
		return
	}
	wl := effectiveSpamWhitelist(s)
	if total > wl {
		return
	}
	// Доверие кросс-чатовое: наговорил на белый список в ЛЮБОМ одном
	// разрешённом чате бота — доверяем и здесь (в факты LLM идёт total
	// текущего чата). Запрос только для новичков — старожилы уже отсеялись
	// пер-чатовым гейтом выше. Фильтр chatAllowed не даёт нафармить доверие
	// в постороннем чате, куда бота затащили (работает при ALLOWED_CHATS).
	totals, err := b.db.UserMessageTotalsByChat(b.runCtx, user.ID)
	if err != nil {
		b.log.Warn("spam check: totals by chat", "err", err, "user", user.ID)
		return
	}
	for cid, n := range totals {
		if n > wl && b.chatAllowed(cid) {
			return
		}
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
	k := chatUser{chatID, user.ID}
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

	// Давность упоминаем только в первые сутки после входа: счётчики ведутся
	// с момента установки бота, и «неизвестно» неотличимо от «старожил» —
	// поэтому всё, что старше суток или без записи, для LLM просто не фактор.
	memberFor := ""
	if joinedAt, ok, err := b.db.MemberJoinedAt(b.runCtx, chatID, user.ID); err == nil && ok {
		if since := time.Since(joinedAt); since < 24*time.Hour {
			memberFor = humanDurationRU(since)
		}
	}
	facts := buildSpamFacts(message, memberFor, msgTotal)

	// Бюджет на всю цепочку провайдеров; Groq внутри ограничен отдельно,
	// чтобы его зависший вызов не съел время фолбека.
	ctx, cancel := context.WithTimeout(b.runCtx, 30*time.Second)
	defer cancel()
	prob, provider, err := b.classifySpam(ctx, chatID, user.ID, facts)
	if err != nil {
		// Fail-open: сбой всех провайдеров не трогает сообщение.
		b.log.Warn("spam check failed (fail-open)", "err", err,
			"provider", provider, "chat", chatID, "user", user.ID)
		return
	}
	threshold := effectiveSpamThreshold(s)
	// facts — ровно то, что ушло в LLM (автор, вложения, текст): владельцы
	// бота видят по логу, за что сообщению выставили оценку.
	b.log.Info("spam check verdict", "chat", chatID, "user", user.ID,
		"prob", prob, "threshold", threshold, "provider", provider,
		"msg_total", msgTotal, "facts", facts)
	if prob < threshold {
		return
	}

	sent, err := b.api.SendMessage(b.runCtx,
		tu.Message(tu.ID(chatID), spamVoteText(prob, 0, 0, effectiveSpamVoteMargin(s))).
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
		return
	}
	// Уже в своей горутине (maybeSpamCheck) — уведомляем синхронно.
	b.notifySpamSuspicion(message, prob, threshold)
}

// spamNotifyTargets — владельцы, включившие ЛС-уведомления о спаме. Один
// запрос на всех; пересечение с OWNER_IDS отсеивает строки бывших владельцев.
func (b *Bot) spamNotifyTargets(ctx context.Context) []int64 {
	ids, err := b.db.SpamNotifyOwners(ctx)
	if err != nil {
		b.log.Warn("spam notify owners", "err", err)
		return nil
	}
	var out []int64
	for _, id := range ids {
		if b.isOwner(id) {
			out = append(out, id)
		}
	}
	return out
}

// notifySpamSuspicion шлёт подписанным владельцам форвард подозрительного
// сообщения + карточку с оценкой. Best effort: ошибки только в Warn.
func (b *Bot) notifySpamSuspicion(message telego.Message, prob, threshold int) {
	targets := b.spamNotifyTargets(b.runCtx)
	if len(targets) == 0 {
		return
	}
	chatID := message.Chat.ID
	title := message.Chat.Title
	if title == "" {
		title = fmt.Sprintf("Chat %d", chatID)
	}
	info := fmt.Sprintf("🚨 Подозрение на спам в «%s»\nАвтор: %s\nОценка: %d%% (порог %d)",
		html.EscapeString(title),
		html.EscapeString(userLabel(*message.From)), prob, threshold)
	for _, ownerID := range targets {
		if _, err := b.api.ForwardMessage(b.runCtx, &telego.ForwardMessageParams{
			ChatID:     tu.ID(ownerID),
			FromChatID: tu.ID(chatID),
			MessageID:  message.MessageID,
		}); err != nil {
			// Форвард может быть запрещён (protected content) — карточка ниже
			// всё равно уйдёт, но без текста сообщения было бы слепо: дошлём.
			b.log.Warn("forward spam suspicion", "err", err, "owner", ownerID)
		}
		if _, err := b.api.SendMessage(b.runCtx, tu.Message(tu.ID(ownerID), info).
			WithParseMode(telego.ModeHTML)); err != nil {
			b.log.Warn("notify spam suspicion", "err", err, "owner", ownerID)
		}
	}
}

// notifySpamVerdict шлёт подписанным владельцам итог голосования: кто решил,
// раскладку голосов по именам и список чатов, где спамер добанен. targets
// вычислил resolveSpamVote — там же, где решалось, читать ли бюллетени.
func (b *Bot) notifySpamVerdict(targets []int64, v storage.SpamVote, spam bool, why string,
	ballots []storage.Ballot, alsoBanned []string) {
	if len(targets) == 0 {
		return
	}
	ids := []int64{v.AuthorID}
	for _, bl := range ballots {
		ids = append(ids, bl.VoterID)
	}
	infos, err := b.db.GetUserInfos(b.runCtx, ids)
	if err != nil {
		b.log.Warn("verdict notify: user infos", "err", err)
		infos = map[int64]storage.UserInfo{}
	}

	var sb strings.Builder
	if spam {
		sb.WriteString("⚖️ Вердикт: <b>спам</b>, автор забанен")
	} else {
		sb.WriteString("⚖️ Вердикт: <b>не спам</b>")
	}
	fmt.Fprintf(&sb, "\nЧат: %s\nАвтор: %s\nОценка была %d%%\nРешение: %s",
		html.EscapeString(b.chatTitle(b.runCtx, v.ChatID)),
		mentionWithUsername(infos, v.AuthorID), v.Prob, html.EscapeString(why))
	var yes, no []string
	for _, bl := range ballots {
		if bl.IsSpam {
			yes = append(yes, mentionWithUsername(infos, bl.VoterID))
		} else {
			no = append(no, mentionWithUsername(infos, bl.VoterID))
		}
	}
	if len(yes) > 0 {
		sb.WriteString("\nЗа спам: " + strings.Join(yes, ", "))
	}
	if len(no) > 0 {
		sb.WriteString("\nПротив: " + strings.Join(no, ", "))
	}
	if len(alsoBanned) > 0 {
		escaped := make([]string, len(alsoBanned))
		for i, t := range alsoBanned {
			escaped[i] = html.EscapeString(t)
		}
		sb.WriteString("\nТакже забанен в: " + strings.Join(escaped, ", "))
	}
	text := sb.String()
	for _, ownerID := range targets {
		if _, err := b.api.SendMessage(b.runCtx, tu.Message(tu.ID(ownerID), text).
			WithParseMode(telego.ModeHTML)); err != nil {
			b.log.Warn("notify spam verdict", "err", err, "owner", ownerID)
		}
	}
}

// classifySpam гоняет факты по цепочке провайдеров: Groq первичен (быстрее и
// с бо́льшим лимитом), GigaChat подхватывает при ЛЮБОЙ его ошибке — чаще всего
// это минутный rate-limit Groq (суточный запас ещё есть, но ждать минуту
// нельзя). Ошибка возвращается только когда упали все доступные провайдеры.
// chatID/userID — только для логов.
func (b *Bot) classifySpam(ctx context.Context, chatID, userID int64, facts string) (prob int, provider string, err error) {
	if b.groqc.Enabled() {
		gctx := ctx
		if b.gigac.Enabled() {
			// Суб-бюджет нужен только чтобы зависший Groq оставил время
			// фолбеку; без фолбека Groq получает весь бюджет проверки.
			var cancel context.CancelFunc
			gctx, cancel = context.WithTimeout(ctx, 12*time.Second)
			defer cancel()
		}
		prob, err = b.groqc.SpamProbability(gctx, facts)
		if err == nil {
			return prob, "groq", nil
		}
		if !b.gigac.Enabled() {
			return 0, "groq", err
		}
		b.log.Warn("groq spam check failed, falling back to gigachat",
			"err", err, "chat", chatID, "user", userID)
	}
	if !b.gigac.Enabled() {
		// Недостижимо при гейте spamAIEnabled в maybeSpamCheck; страховка от
		// будущих прямых вызовов.
		return 0, "none", errors.New("no spam providers enabled")
	}
	prob, err = b.gigac.SpamProbability(ctx, facts)
	return prob, "gigachat", err
}

// buildSpamFacts собирает то, что уходит в LLM: контекст автора, факт
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

func spamVoteText(prob, yes, no, margin int) string {
	return fmt.Sprintf(
		"🤖 Мне кажется, это спам (уверенность %d%%).\n\n"+
			"Голосуйте кнопками — перевес в %d %s решает. Голос админа решает сразу.\n\n"+
			"🚫 Спам: <b>%d</b> · ✅ Не спам: <b>%d</b>",
		prob, margin, pluralRU(margin, "голос", "голоса", "голосов"), yes, no)
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
	// В кэш имён: уведомление владельцу о вердикте рендерит голосовавших по id.
	b.rememberUser(b.runCtx, storage.UserInfo{
		UserID:    query.From.ID,
		FirstName: query.From.FirstName,
		LastName:  query.From.LastName,
		Username:  query.From.Username,
	})

	voteWord := "не спам"
	if isSpamVote {
		voteWord = "спам"
	}

	// Золотой голос: админ или владелец бота решает единолично.
	if b.isOwner(voter) || b.isChatAdminCached(b.runCtx, chatID, voter) {
		_ = b.api.AnswerCallbackQuery(ctx,
			tu.CallbackQuery(query.ID).WithText("Решено голосом админа."))
		b.log.Info("spam vote ballot", "chat", chatID, "bot_msg", botMsgID,
			"voter", userLabel(query.From), "vote", voteWord, "golden", true)
		b.resolveSpamVote(v, isSpamVote, "админ "+userLabel(query.From))
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
	b.log.Info("spam vote ballot", "chat", chatID, "bot_msg", botMsgID,
		"voter", userLabel(query.From), "vote", voteWord,
		"tally", fmt.Sprintf("%d:%d", yes, no))

	margin := effectiveSpamVoteMargin(b.chatSettings(b.runCtx, chatID))
	switch verdict, decided := voteVerdict(yes, no, margin); {
	case decided:
		b.resolveSpamVote(v, verdict, fmt.Sprintf("голоса %d:%d", yes, no))
	default:
		// Обновляем счёт на плашке; клавиатура остаётся той же.
		// Повторный тап того же голоса даёт байт-в-байт тот же текст —
		// «message is not modified» здесь ожидаем, не предупреждение.
		if _, err := b.api.EditMessageText(b.runCtx, &telego.EditMessageTextParams{
			ChatID:      tu.ID(chatID),
			MessageID:   botMsgID,
			Text:        spamVoteText(v.Prob, yes, no, margin),
			ParseMode:   telego.ModeHTML,
			ReplyMarkup: spamVoteKeyboard(),
		}); err != nil && !isNotModified(err) {
			b.log.Warn("edit spam vote tally", "err", err, "chat", chatID)
		}
	}
	return nil
}

// voteVerdict — чистая логика кворума: |за − против| ≥ margin решает.
func voteVerdict(yes, no, margin int) (isSpam, decided bool) {
	switch {
	case yes-no >= margin:
		return true, true
	case no-yes >= margin:
		return false, true
	}
	return false, false
}

// resolveSpamVote исполняет вердикт. TakeSpamVote атомарен: первый вызвавший
// исполняет, проигравшие гонку выходят молча — двойной бан/удаление исключены.
func (b *Bot) resolveSpamVote(v storage.SpamVote, spam bool, why string) {
	// Подписчики уведомлений и бюллетени — до Take: он удаляет бюллетени в
	// своей транзакции. Бюллетени читаем только когда они кому-то нужны;
	// проигравший гонку прочитает зря — не страшно, он выйдет по !taken.
	targets := b.spamNotifyTargets(b.runCtx)
	var ballots []storage.Ballot
	if len(targets) > 0 {
		var err error
		ballots, err = b.db.ListBallots(b.runCtx, v.ChatID, v.BotMsgID)
		if err != nil {
			b.log.Warn("list ballots", "err", err, "chat", v.ChatID)
		}
	}
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
			// Бан не прошёл (обычно нет прав): юзер остаётся в чате, поэтому
			// событие spamban не пишем и его приветствие не трогаем.
			b.log.Warn("spam ban", "err", err, "chat", v.ChatID, "user", v.AuthorID)
		} else {
			_ = b.db.RecordEvent(b.runCtx, v.ChatID, v.AuthorID, storage.EventSpamBan, time.Now())
			// Приветствие бота для этого юзера revoke не трогает — сносим сами.
			if msgID, ok, err := b.db.TakeGreetingMsg(b.runCtx, v.ChatID, v.AuthorID); err == nil && ok {
				if err := b.deleteMessage(b.runCtx, v.ChatID, msgID); err != nil {
					b.log.Debug("delete greeting of banned user", "err", err, "chat", v.ChatID)
				}
			}
		}
		// revoke обычно уже стёр сообщение; ручное удаление — страховка на
		// случай неудавшегося бана (нет прав), поэтому ошибку глушим тихо.
		if err := b.deleteMessage(b.runCtx, v.ChatID, v.TargetMsgID); err != nil {
			b.log.Debug("delete spam target (already gone?)", "err", err, "chat", v.ChatID)
		}
		// Общая база — по вердикту, независимо от того, хватило ли прав в
		// исходном чате: вердикт вынесен людьми.
		if err := b.db.AddSpamBanned(b.runCtx, v.AuthorID, v.ChatID, time.Now()); err != nil {
			b.log.Warn("add spam banned", "err", err, "user", v.AuthorID)
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
	// Кросс-бан и уведомления — в горутине: обход всех чатов не должен
	// держать колбэк-хендлер (плашка уже снята, вердикт исполнен).
	if spam || len(targets) > 0 {
		go func() {
			var alsoBanned []string
			if spam {
				alsoBanned = b.banEverywhere(v.ChatID, v.AuthorID)
			}
			b.notifySpamVerdict(targets, v, spam, why, ballots, alsoBanned)
		}()
	}
}

// banEverywhere банит юзера во всех группах бота, кроме исходной. Возвращает
// названия чатов, где бан прошёл. Событие spamban пишется только в чате
// вердикта (вызывающим) — иначе статистика «Забанены» размножится.
// Best effort в один заход, без retryTG: типовая ошибка тут — «нет прав»,
// перманентная, и лестница ретраев лишь растянула бы обход на минуты.
func (b *Bot) banEverywhere(originChatID, userID int64) []string {
	chats, err := b.db.ListChats(b.runCtx)
	if err != nil {
		b.log.Warn("ban everywhere: list chats", "err", err)
		return nil
	}
	var banned []string
	for _, c := range chats {
		if c.ChatID == originChatID || (c.Type != "group" && c.Type != "supergroup") ||
			!b.chatAllowed(c.ChatID) {
			continue
		}
		if err := b.api.BanChatMember(b.runCtx, &telego.BanChatMemberParams{
			ChatID:         tu.ID(c.ChatID),
			UserID:         userID,
			RevokeMessages: true,
		}); err != nil {
			// Обычно нет прав на бан в этом чате — ок, идём дальше.
			b.log.Warn("cross-chat spam ban", "err", err, "chat", c.ChatID, "user", userID)
			continue
		}
		b.log.Info("cross-chat spam ban", "chat", c.ChatID, "user", userID)
		banned = append(banned, titleOrID(c))
	}
	return banned
}

// spamVoteSweepLoop закрывает голосования без кворума: раз в час (и сразу на
// старте — рестарт мог проспать дедлайны) снимает плашки старше spamVoteTTL.
func (b *Bot) spamVoteSweepLoop(ctx context.Context) {
	sweep := func() {
		// Заодно чистим устаревшие записи приветствий: сообщения старше 48 ч
		// Telegram боту удалять не даёт, их id бесполезны.
		if err := b.db.PruneGreetings(ctx, time.Now().Add(-48*time.Hour)); err != nil {
			b.log.Warn("prune greetings", "err", err)
		}
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
