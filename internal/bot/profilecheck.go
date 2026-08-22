package bot

import (
	"context"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"

	"github.com/menand/AntiSpamBot/internal/groq"
	"github.com/menand/AntiSpamBot/internal/storage"
)

// profileBioLimit — сколько рун bio уходит в LLM: профили короткие, спамные
// простыни длиннее не делают вердикт лучше.
const profileBioLimit = 500

// profileCheckCooldown — минимальный интервал между ИИ-чеками профиля одного
// юзера. Чек срабатывает на каждом pass/approve; без кулдауна цикл
// «вышел-зашёл» (или одобрения подельником-админом) жёг бы LLM-квоту на
// каждый проход, а inflight дедупит только параллельные проверки.
const profileCheckCooldown = time.Hour

// maybeProfileCheck — хук в конце onSuccess: после капчи и приветствия
// профиль новичка (имя/ник/bio/фото) оценивается LLM, и при подозрении в
// чат вешается плашка «забанить?» на общей инфраструктуре голосования.
// Настройки общие со спам-чеком сообщений: тумблер spam_check_enabled,
// перевес spam_vote_margin.
func (b *Bot) maybeProfileCheck(chatID, userID int64, threadID int) {
	if !b.spamAIEnabled() {
		return
	}
	s := b.chatSettings(b.runCtx, chatID)
	if !s.SpamCheckEnabled {
		return
	}
	// Те же гейты доверия, что у спам-чека сообщений (перезашедший старожил
	// не проверяется) — общий предикат, чтобы модели не разъехались.
	if _, skip := b.spamGatesPass(chatID, userID, s); skip {
		return
	}
	// Перезаходной кулдаун (пер-юзер глобально: вопрос задаётся про
	// человека, а не про пару чат+юзер). Штамп ставим вместе с захватом
	// inflight-слота: если проверка не стартовала (слот занят), кулдаун
	// не расходуем.
	b.spamMu.Lock()
	if last, seen := b.profileChecked[userID]; seen && time.Since(last) < profileCheckCooldown {
		b.spamMu.Unlock()
		b.log.Debug("profile check skipped: cooldown", "chat", chatID, "user", userID)
		return
	}
	b.spamMu.Unlock()

	// Общий inflight-ключ со спам-чеком сообщений: один вопрос «банить ли X»
	// за раз.
	k := chatUser{chatID, userID}
	b.spamMu.Lock()
	if _, busy := b.spamInflight[k]; busy {
		b.spamMu.Unlock()
		b.log.Debug("profile check skipped: inflight busy", "chat", chatID, "user", userID)
		return
	}
	b.spamInflight[k] = struct{}{}
	b.profileChecked[userID] = time.Now()
	b.spamMu.Unlock()

	b.goSafe("runProfileCheck", func() {
		defer func() {
			b.spamMu.Lock()
			delete(b.spamInflight, k)
			b.spamMu.Unlock()
		}()
		b.runProfileCheck(chatID, userID, threadID, s)
	})
}

func (b *Bot) runProfileCheck(chatID, userID int64, threadID int, s storage.ChatSettings) {
	facts := b.buildProfileFactsFromAPI(chatID, userID)

	ctx, cancel := context.WithTimeout(b.runCtx, 30*time.Second)
	defer cancel()
	spam, provider, err := b.classifyVerdict(ctx, groq.ProfileSystemPrompt, facts, chatID, userID)
	if err != nil {
		// Fail-open: сбой всех провайдеров — профиль не трогаем.
		b.log.Warn("profile check failed (fail-open)", "err", err,
			"provider", provider, "chat", chatID, "user", userID)
		return
	}
	// facts — ровно то, что ушло в LLM: владельцы видят по логу, за что
	// профилю выставили вердикт (тот же verbose-контур, что у спам-чека).
	b.log.Info("profile check verdict", "chat", chatID, "user", userID,
		"spam", spam, "provider", provider, "facts", facts)
	if !spam {
		return
	}

	infos, err := b.db.GetUserInfos(b.runCtx, []int64{userID})
	if err != nil {
		b.log.Warn("profile check: user infos", "err", err)
		infos = map[int64]storage.UserInfo{}
	}
	mention := mentionOrID(infos, userID)
	margin := effectiveSpamVoteMargin(s)

	params := tu.Message(tu.ID(chatID), profileVoteText(mention, 0, 0, margin)).
		WithParseMode(telego.ModeHTML).
		WithReplyMarkup(profileVoteKeyboard())
	if threadID != 0 {
		params = params.WithMessageThreadID(threadID)
	}
	sent, err := b.api.SendMessage(b.runCtx, params)
	if err != nil {
		b.log.Warn("send profile vote message", "err", err, "chat", chatID)
		return
	}
	inserted, err := b.db.PutSpamVoteOnce(b.runCtx, storage.SpamVote{
		ChatID:      chatID,
		BotMsgID:    sent.MessageID,
		TargetMsgID: 0, // маркер профильной плашки: целевого сообщения нет
		AuthorID:    userID,
		Prob:        100, // ponytail: легаси-колонка NOT NULL, вердикт теперь бинарный — нигде не отображается
		CreatedAt:   time.Now(),
	})
	if err != nil {
		// Без строки в БД кнопки мертвы, а свипер плашку не увидит (он читает
		// только БД) — сообщение висело бы вечно. Снимаем сразу.
		b.log.Error("persist profile vote", "err", err, "chat", chatID)
		_ = b.deleteMessage(b.runCtx, chatID, sent.MessageID)
		return
	}
	if !inserted {
		// Плашка на этого юзера уже висит (например, успел прийти /spam) —
		// сносим дубль, событий и уведомлений не пишем.
		b.log.Info("profile plashka lost race — vote already pending", "chat", chatID, "user", userID)
		_ = b.deleteMessage(b.runCtx, chatID, sent.MessageID)
		return
	}
	// История подозрений для /info: только при живой плашке.
	if err := b.db.RecordEvent(b.runCtx, chatID, userID, storage.EventSuspect, time.Now(), ""); err != nil {
		b.log.Warn("record suspect event (profile)", "err", err, "chat", chatID, "user", userID)
	}
	b.notifyProfileSuspicion(chatID, userID, facts)
}

// buildProfileFactsFromAPI собирает факты профиля: getChat(userID) даёт
// имя/фамилию/ник/bio одним вызовом (сам аватар никогда не скачивается —
// только количество фото). Приватность бланкует bio и фото БЕЗ ошибки —
// пустота формулируется нейтрально, а промпт явно трактует её как слабый
// признак. Ошибка getChat — фолбэк на кэш user_info (имя там точно есть:
// runCaptcha сохраняет его до капчи).
func (b *Bot) buildProfileFactsFromAPI(chatID, userID int64) string {
	var first, last, username, bio string
	bioKnown := false
	if chat, err := b.api.GetChat(b.runCtx, &telego.GetChatParams{ChatID: tu.ID(userID)}); err == nil && chat != nil {
		first, last, username, bio = chat.FirstName, chat.LastName, chat.Username, chat.Bio
		bioKnown = true
	} else {
		b.log.Warn("profile check: getChat", "err", err, "user", userID)
		if infos, ierr := b.db.GetUserInfos(b.runCtx, []int64{userID}); ierr == nil {
			if info, ok := infos[userID]; ok {
				first, last, username = info.FirstName, info.LastName, info.Username
			}
		}
	}

	photos := -1 // -1 = не удалось узнать, факт опускается
	if up, err := b.api.GetUserProfilePhotos(b.runCtx, &telego.GetUserProfilePhotosParams{
		UserID: userID, Limit: 1,
	}); err == nil && up != nil {
		photos = up.TotalCount
	}

	return buildProfileFacts(first, last, username, bio, bioKnown, photos)
}

// buildProfileFacts — чистая сборка строки фактов (отделена от API для тестов).
func buildProfileFacts(first, last, username, bio string, bioKnown bool, photos int) string {
	var sb strings.Builder
	name := strings.TrimSpace(first + " " + last)
	if name == "" {
		name = "(без имени)"
	}
	fmt.Fprintf(&sb, "Имя: %s.\n", name)
	if username != "" {
		fmt.Fprintf(&sb, "Ник: @%s.\n", username)
	} else {
		sb.WriteString("Ник: не задан.\n")
	}
	switch {
	case !bioKnown:
		sb.WriteString("О себе: недоступно.\n")
	case strings.TrimSpace(bio) == "":
		sb.WriteString("О себе: пусто.\n")
	default:
		fmt.Fprintf(&sb, "О себе: %s\n", truncateLabel(bio, profileBioLimit))
	}
	if photos >= 0 {
		fmt.Fprintf(&sb, "Фото профиля: %d.\n", photos)
	}
	return sb.String()
}

func profileVoteText(mention string, yes, no, margin int) string {
	return fmt.Sprintf(
		"👤 %s вошёл(а) в чат, но профиль выглядит подозрительно. Забанить?\n\n"+
			"Голосуйте кнопками — перевес в %d %s решает. Голос админа решает сразу.\n\n"+
			"🚫 Забанить: <b>%d</b> · ✅ Оставить: <b>%d</b>",
		mention, margin, pluralRU(margin, "голос", "голоса", "голосов"), yes, no)
}

// profileVoteKeyboard — те же callback data sv:1/sv:0, что у спам-плашки:
// весь механизм голосования (гейт доверия, золотой голос, margin) общий.
func profileVoteKeyboard() *telego.InlineKeyboardMarkup {
	return tu.InlineKeyboard(tu.InlineKeyboardRow(
		tu.InlineKeyboardButton("🚫 Забанить").WithCallbackData("sv:1"),
		tu.InlineKeyboardButton("✅ Оставить").WithCallbackData("sv:0"),
	))
}

// notifyProfileSuspicion шлёт подписанным владельцам карточку подозрительного
// профиля (форвардить нечего — сообщений у юзера ещё нет).
func (b *Bot) notifyProfileSuspicion(chatID, userID int64, facts string) {
	targets := b.spamNotifyTargets(b.runCtx)
	if len(targets) == 0 {
		return
	}
	info := fmt.Sprintf("👤 Подозрительный профиль в %s\n%s",
		b.chatLink(b.runCtx, chatID),
		html.EscapeString(strings.TrimSpace(facts)))
	for _, ownerID := range targets {
		if _, err := b.api.SendMessage(b.runCtx, tu.Message(tu.ID(ownerID), info).
			WithParseMode(telego.ModeHTML).
			WithLinkPreviewOptions(&telego.LinkPreviewOptions{IsDisabled: true})); err != nil {
			b.log.Warn("notify profile suspicion", "err", err, "owner", ownerID)
		}
	}
}
