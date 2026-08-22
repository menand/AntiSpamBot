package bot

import (
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
	tu "github.com/mymmrac/telego/telegoutil"

	"github.com/menand/AntiSpamBot/internal/storage"
)

// handleGroupInfoCommand — /info в группе/супергруппе: карточка участника по
// цели resolveModTarget (text_mention → @username → реплай → реплай на
// приветствие). Админская команда: modPrologue даёт все ворота и наказывает
// самозванцев punishNonAdmin. Read-only: guardModTarget НЕ применяем — инфо
// о себе и об админах разрешена, запрещён только сам бот.
func (b *Bot) handleGroupInfoCommand(ctx *th.Context, message telego.Message) error {
	chatID, ok := b.modPrologue(ctx, message)
	if !ok {
		return nil
	}
	targetID, _, ok := b.resolveModTarget(message)
	if !ok {
		b.refuseAndDelete(ctx, message,
			"Не понял, о ком показать информацию. Ответь командой на сообщение юзера "+
				"или на моё приветствие о нём, либо укажи @username (я должен был его видеть).")
		return nil
	}
	if b.me != nil && targetID == b.me.ID {
		b.refuseAndDelete(ctx, message, "Себя я знаю наизусть 🙂")
		return nil
	}

	// Само сообщение-команду убираем для чистоты чата (best effort) —
	// карточка и так адресована вызвавшему.
	if err := b.deleteMessage(b.runCtx, chatID, message.MessageID); err != nil {
		b.log.Debug("delete /info command", "err", err, "chat", chatID)
	}

	card := b.buildUserInfoCard(chatID, targetID)
	b.sendHTML(chatID, threadOf(message), b.modReceiver(chatID, message), renderUserInfoCard(card))
	b.log.Info("user info card", "chat", chatID, "target", targetID, "by", message.From.ID)
	return nil
}

// userInfoCard — собранные данные карточки; рендер отделён для тестов.
type userInfoCard struct {
	header     string
	joined     string
	messages   string // "" — не удалось посчитать
	violations []string
	flags      []string
	live       string
}

// buildUserInfoCard собирает все данные одним проходом; каждая секция
// отказоустойчива независимо — ошибка одной не убивает карточку.
func (b *Bot) buildUserInfoCard(chatID, targetID int64) userInfoCard {
	c := userInfoCard{}

	infos, err := b.db.GetUserInfos(b.runCtx, []int64{targetID})
	if err != nil {
		b.log.Warn("info card: user infos", "err", err, "target", targetID)
	}
	c.header = fmt.Sprintf("👤 %s", mentionOrID(infos, targetID))
	if info, ok := infos[targetID]; ok && info.Username != "" {
		c.header += " · @" + html.EscapeString(info.Username)
	}

	c.joined = b.joinedLine(chatID, targetID)
	c.messages = b.messagesLine(chatID, targetID)
	c.violations = b.violationParts(chatID, targetID)

	// Флаги доверия.
	if trusted, err := b.db.IsTrusted(b.runCtx, chatID, targetID); err == nil && trusted {
		c.flags = append(c.flags, "✅ доверенный (/whitelist)")
	} else if err != nil {
		b.log.Warn("info card: is trusted", "err", err, "target", targetID)
	}
	if banned, err := b.db.IsSpamBanned(b.runCtx, targetID); err == nil && banned {
		c.flags = append(c.flags, "🚫 в глобальной базе спамеров")
	} else if err != nil {
		b.log.Warn("info card: is spam banned", "err", err, "target", targetID)
	}

	// Живой статус членства тем же запросом даёт и текущий мьют.
	m, err := b.api.GetChatMember(b.runCtx, &telego.GetChatMemberParams{
		ChatID: tu.ID(chatID),
		UserID: targetID,
	})
	if err != nil {
		b.log.Debug("info card: get chat member", "err", err, "chat", chatID)
		return c
	}
	switch cm := m.(type) {
	case *telego.ChatMemberRestricted:
		// Ограничение может быть и невидимым капча-мьютом на время проверки —
		// не надо называть его наказанием.
		if b.store.IsCaptchaActive(chatID, targetID) {
			c.live = "🔒 Сейчас проходит проверку входа (капча)"
		} else if cm.UntilDate == 0 {
			c.live = "🔇 Сейчас в мьюте — навсегда"
		} else if until := time.Unix(cm.UntilDate, 0); until.After(time.Now()) {
			c.live = "🔇 Сейчас в мьюте — до " + dateTimeMSK(until) + " МСК"
		}
	case *telego.ChatMemberLeft, *telego.ChatMemberBanned:
		c.live = "👤 Сейчас не в чате"
	}
	return c
}

// joinedLine — строка про вход. members.joined_at пишется при каждом
// прохождении капчи, так что это дата ПОСЛЕДНЕГО входа. Нет записи — юзер
// состоял в чате до бота: показываем нижнюю границу из chats.bot_added_at,
// а для старых чатов без неё — самое раннее событие чата (приблизительно).
func (b *Bot) joinedLine(chatID, targetID int64) string {
	if joinedAt, ok, err := b.db.MemberJoinedAt(b.runCtx, chatID, targetID); err != nil {
		b.log.Warn("info card: member joined at", "err", err, "target", targetID)
	} else if ok {
		return "🗓 Последний вход: <b>" + dateMSK(joinedAt) + "</b>"
	}

	botAt, hasBotAt, err := b.db.GetChatBotAddedAt(b.runCtx, chatID)
	if err != nil {
		b.log.Warn("info card: bot added at", "err", err, "chat", chatID)
	}
	if !hasBotAt {
		earliest, ok, err := b.db.ChatEarliestEventAt(b.runCtx, chatID)
		if err != nil {
			b.log.Warn("info card: earliest event", "err", err, "chat", chatID)
		}
		if ok {
			return "🗓 Вход: неизвестно — видимо, ранее <b>~" + dateMSK(earliest) +
				"</b> (самое раннее событие, которое я видел)"
		}
		return "🗓 Вход: неизвестно"
	}
	return "🗓 Вход: неизвестно — видимо, ранее <b>" + dateMSK(botAt) +
		"</b> (дата моего добавления в чат)"
}

// messagesLine — счётчики сообщений за окна статистики (те же границы, что у
// кнопок периодов DM-меню: календарные сутки МСК).
func (b *Bot) messagesLine(chatID, userID int64) string {
	now := time.Now()
	todayFrom, _ := statsRange(periodDay, now)
	yestFrom, _ := statsRange(periodYesterday, now)
	weekFrom, _ := statsRange(periodWeek, now)
	monthFrom, _ := statsRange(periodMonth, now)
	w, err := b.db.UserMessageWindowCounts(b.runCtx, chatID, userID,
		storage.DayOf(todayFrom), storage.DayOf(yestFrom),
		storage.DayOf(weekFrom), storage.DayOf(monthFrom))
	if err != nil {
		b.log.Warn("info card: message windows", "err", err, "chat", chatID, "user", userID)
		return ""
	}
	return fmt.Sprintf("💬 Сообщения: сегодня <b>%d</b> · вчера %d · неделю %d · месяц %d · всего %d",
		w.Today, w.Yesterday, w.Week, w.Month, w.Total)
}

// violationParts — ненулевые бакеты событий; пустой список = «чисто».
func (b *Bot) violationParts(chatID, userID int64) []string {
	c, err := b.db.UserEventCounts(b.runCtx, chatID, userID)
	if err != nil {
		b.log.Warn("info card: event counts", "err", err, "chat", chatID, "user", userID)
		return nil
	}
	var parts []string
	if c.CaptchaFails > 0 {
		parts = append(parts, fmt.Sprintf("капча не пройдена: %d", c.CaptchaFails))
	}
	if c.NoReply > 0 {
		parts = append(parts, fmt.Sprintf("молчание после приветствия: %d", c.NoReply))
	}
	if c.GlobalBans > 0 {
		parts = append(parts, fmt.Sprintf("мгновенный бан по базе спамеров: %d", c.GlobalBans))
	}
	if c.Suspects > 0 {
		parts = append(parts, fmt.Sprintf("подозрения на спам: %d", c.Suspects))
	}
	if c.SpamBanned > 0 {
		parts = append(parts, fmt.Sprintf("бан по спам-вердикту: %d", c.SpamBanned))
	}
	if c.ModKicked > 0 {
		parts = append(parts, fmt.Sprintf("кикнут админом: %d", c.ModKicked))
	}
	if c.ModBanned > 0 {
		parts = append(parts, fmt.Sprintf("забанен админом: %d", c.ModBanned))
	}
	if c.Mutes > 0 {
		parts = append(parts, fmt.Sprintf("мьюты: %d", c.Mutes))
	}
	return parts
}

// renderUserInfoCard — чистый рендер карточки в HTML.
func renderUserInfoCard(c userInfoCard) string {
	var sb strings.Builder
	sb.WriteString(c.header)
	sb.WriteString("\n")
	sb.WriteString(c.joined)
	if c.messages != "" {
		sb.WriteString("\n")
		sb.WriteString(c.messages)
	}
	if len(c.violations) > 0 {
		fmt.Fprintf(&sb, "\n⚠️ За ~180 дней: %s", strings.Join(c.violations, " · "))
	} else {
		sb.WriteString("\n🧼 Нарушений и проверок не было")
	}
	if len(c.flags) > 0 {
		fmt.Fprintf(&sb, "\n🏷 %s", strings.Join(c.flags, " · "))
	}
	if c.live != "" {
		sb.WriteString("\n")
		sb.WriteString(c.live)
	}
	return sb.String()
}

// dateMSK/dateTimeMSK — даты для пользовательских строк, пояс статистики.
func dateMSK(t time.Time) string {
	return t.In(storage.StatsLocation).Format("02.01.2006")
}

func dateTimeMSK(t time.Time) string {
	return t.In(storage.StatsLocation).Format("02.01.2006 15:04")
}
