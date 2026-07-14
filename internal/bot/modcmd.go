package bot

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
	tu "github.com/mymmrac/telego/telegoutil"

	"github.com/menand/AntiSpamBot/internal/storage"
)

// handleKickCommand / handleBanCommand — админские команды модерации в чате.
// /ban банит навсегда, /kick кикает (с возможностью перезайти); обе стирают
// все сообщения цели. Доступ — админ чата или владелец бота; остальным ответ
// «не для тебя». В SetMyCommands НЕ регистрируются — «/»-меню в группах
// остаётся пустым, новые кнопки в поле ввода не появляются.
func (b *Bot) handleKickCommand(ctx *th.Context, message telego.Message) error {
	return b.handleModCommand(ctx, message, false)
}

func (b *Bot) handleBanCommand(ctx *th.Context, message telego.Message) error {
	return b.handleModCommand(ctx, message, true)
}

func (b *Bot) handleModCommand(ctx *th.Context, message telego.Message, permanent bool) error {
	if message.Chat.Type != "group" && message.Chat.Type != "supergroup" {
		return nil
	}
	if !b.chatAllowed(message.Chat.ID) || message.From == nil {
		return nil
	}
	chatID := message.Chat.ID
	action := "кик"
	if permanent {
		action = "бан"
	}

	if !b.canManageChat(ctx, message.From.ID, chatID) {
		b.punishNonAdmin(ctx, message)
		return nil
	}

	targetID, targetMsgID, ok := b.resolveModTarget(message)
	if !ok {
		b.replyTo(ctx, message,
			"Не понял, кого "+action+"ать. Ответь командой на сообщение юзера "+
				"или на моё приветствие о нём, либо укажи @username (я должен был его видеть).")
		return nil
	}
	if !b.guardModTarget(ctx, message, targetID) {
		return nil
	}

	// Само сообщение-команду убираем для чистоты чата (best effort).
	if err := b.deleteMessage(b.runCtx, chatID, message.MessageID); err != nil {
		b.log.Debug("delete mod command", "err", err, "chat", chatID)
	}

	reason := storage.ReasonModPrefix + fmt.Sprintf("%d", message.From.ID)
	var actErr error
	if permanent {
		actErr = b.banRevoke(b.runCtx, chatID, targetID)
	} else {
		actErr = b.kickRevoke(b.runCtx, chatID, targetID)
	}
	if actErr != nil {
		b.log.Warn("mod command action failed", "err", actErr, "chat", chatID, "target", targetID)
		b.sendPlain(chatID, "Не получилось — проверь мои права на блокировку.")
		return nil
	}

	kind := storage.EventKick
	if permanent {
		kind = storage.EventBan
	}
	_ = b.db.RecordEvent(b.runCtx, chatID, targetID, kind, time.Now(), reason)
	b.cleanupTargetTraces(chatID, targetID)
	// revoke обычно уже стирает исходное сообщение цели; ручное удаление —
	// страховка на случай, если конкретно оно осталось (тот же приём, что и
	// удаление TargetMsgID в resolveSpamVote), поэтому ошибку глушим тихо.
	if targetMsgID != 0 {
		if err := b.deleteMessage(b.runCtx, chatID, targetMsgID); err != nil {
			b.log.Debug("delete mod target message (already gone?)", "err", err, "chat", chatID)
		}
	}

	infos, _ := b.db.GetUserInfos(b.runCtx, []int64{targetID})
	mention := mentionOrID(infos, targetID)
	if permanent {
		b.sendHTML(chatID, "🚫 "+mention+" забанен.")
	} else {
		b.sendHTML(chatID, "👢 "+mention+" кикнут.")
	}
	b.log.Info("mod command", "action", action, "chat", chatID,
		"target", targetID, "by", message.From.ID)
	b.notifyModAction(chatID, targetID, kind, reason)
	return nil
}

// handleDeleteCommand — /del и /delete: тихо удалить сообщение, на которое
// реплайнули, и саму команду. Только админ чата / владелец бота; никаких
// подтверждений и событий — ноль флуда по требованию. Без реплая удаляется
// только сама команда.
func (b *Bot) handleDeleteCommand(ctx *th.Context, message telego.Message) error {
	if message.Chat.Type != "group" && message.Chat.Type != "supergroup" {
		return nil
	}
	if !b.chatAllowed(message.Chat.ID) || message.From == nil {
		return nil
	}
	chatID := message.Chat.ID
	if !b.canManageChat(ctx, message.From.ID, chatID) {
		b.punishNonAdmin(ctx, message)
		return nil
	}
	if r := message.ReplyToMessage; r != nil {
		if err := b.deleteMessage(b.runCtx, chatID, r.MessageID); err != nil {
			b.log.Debug("delete target via /del", "err", err, "chat", chatID)
		}
	}
	if err := b.deleteMessage(b.runCtx, chatID, message.MessageID); err != nil {
		b.log.Debug("delete /del command", "err", err, "chat", chatID)
	}
	b.log.Info("del command", "chat", chatID, "by", message.From.ID)
	return nil
}

// handleMuteCommand — /mute <N[m|h|d]>: рид-онли на срок, цель — реплаем или
// @username (тот же резолв, что у /kick|/ban). Размьючивает сам Telegram по
// until_date — рестарты бота на это не влияют.
func (b *Bot) handleMuteCommand(ctx *th.Context, message telego.Message) error {
	if message.Chat.Type != "group" && message.Chat.Type != "supergroup" {
		return nil
	}
	if !b.chatAllowed(message.Chat.ID) || message.From == nil {
		return nil
	}
	chatID := message.Chat.ID
	if !b.canManageChat(ctx, message.From.ID, chatID) {
		b.punishNonAdmin(ctx, message)
		return nil
	}
	d, ok := parseMuteDuration(message.Text)
	if !ok {
		b.replyTo(ctx, message,
			"Не понял срок. Примеры: /mute 45, /mute 45m, /mute 3h, /mute 5d — "+
				"реплаем на сообщение юзера или с @username.")
		return nil
	}
	targetID, _, ok := b.resolveModTarget(message)
	if !ok {
		b.replyTo(ctx, message,
			"Не понял, кого мьютить. Ответь командой на сообщение юзера "+
				"или укажи @username (я должен был его видеть).")
		return nil
	}
	if !b.guardModTarget(ctx, message, targetID) {
		return nil
	}

	if err := b.deleteMessage(b.runCtx, chatID, message.MessageID); err != nil {
		b.log.Debug("delete mute command", "err", err, "chat", chatID)
	}
	if err := b.mute(b.runCtx, chatID, targetID, time.Now().Add(d)); err != nil {
		b.log.Warn("mute failed", "err", err, "chat", chatID, "target", targetID)
		b.sendPlain(chatID, "Не получилось — проверь мои права на ограничение участников.")
		return nil
	}
	infos, _ := b.db.GetUserInfos(b.runCtx, []int64{targetID})
	b.sendHTML(chatID, "🔇 "+mentionOrID(infos, targetID)+" в рид-онли на "+muteLabel(d)+".")
	b.log.Info("mute command", "chat", chatID, "target", targetID,
		"minutes", int(d.Minutes()), "by", message.From.ID)
	return nil
}

// punishNonAdmin — не-админ дёрнул админскую команду: минутный мьют + ответ.
// Мьют best-effort: в обычной group рестрикт недоступен, без прав не пройдёт
// — тогда остаётся только ответ.
func (b *Bot) punishNonAdmin(ctx *th.Context, message telego.Message) {
	if err := b.mute(b.runCtx, message.Chat.ID, message.From.ID, time.Now().Add(time.Minute)); err != nil {
		b.log.Debug("punish mute failed", "err", err, "chat", message.Chat.ID)
	}
	b.replyTo(ctx, message, "🙅 Это админская команда, не балуйся. Вот тебе мьют на 1 минуту, раз хотел.")
	b.log.Info("non-admin punished for mod command", "chat", message.Chat.ID, "user", message.From.ID)
}

// guardModTarget — общие защиты модкоманд: не бот, не сам вызывающий, не
// другой админ/владелец. false = цель трогать нельзя, отказ уже отправлен.
func (b *Bot) guardModTarget(ctx *th.Context, message telego.Message, targetID int64) bool {
	if b.me != nil && targetID == b.me.ID {
		b.replyTo(ctx, message, "Себя трогать не дам 🙂")
		return false
	}
	if targetID == message.From.ID {
		b.replyTo(ctx, message, "Себя-то за что? 🙂")
		return false
	}
	if b.canManageChat(ctx, targetID, message.Chat.ID) {
		b.replyTo(ctx, message, "Это админ — не трону.")
		return false
	}
	return true
}

// parseMuteDuration ищет в тексте команды первый токен вида N, Nm, Nh или Nd
// (голое число — минуты). Кап 365 дней: у Telegram until_date дальше 366 дней
// означает «навсегда», а минимум у нас минута — больше его нижней границы
// в 30 секунд.
func parseMuteDuration(text string) (time.Duration, bool) {
	fields := strings.Fields(text)
	if len(fields) < 2 {
		return 0, false
	}
	for _, f := range fields[1:] { // fields[0] — сама команда
		num, unit := f, "m"
		if last := f[len(f)-1]; last == 'm' || last == 'h' || last == 'd' {
			num, unit = f[:len(f)-1], string(last)
		}
		v, err := strconv.Atoi(num)
		if err != nil || v <= 0 {
			continue
		}
		const capMinutes = 365 * 24 * 60
		if v > capMinutes { // до умножения — защита от overflow
			v = capMinutes
		}
		mins := v
		switch unit {
		case "h":
			mins = v * 60
		case "d":
			mins = v * 60 * 24
		}
		if mins > capMinutes {
			mins = capMinutes
		}
		return time.Duration(mins) * time.Minute, true
	}
	return 0, false
}

// muteLabel — срок для подтверждения: «45 мин» / «3 ч» / «5 дн», без склонений.
func muteLabel(d time.Duration) string {
	day := 24 * time.Hour
	switch {
	case d >= day && d%day == 0:
		return fmt.Sprintf("%d дн", d/day)
	case d >= time.Hour && d%time.Hour == 0:
		return fmt.Sprintf("%d ч", d/time.Hour)
	default:
		return fmt.Sprintf("%d мин", d/time.Minute)
	}
}

// resolveModTarget вычисляет цель команды по приоритету: text_mention (админ
// выбрал юзера автокомплитом) → @username в аргументе (по нашему кэшу) →
// reply на сообщение цели → reply на приветствие бота о цели.
// targetMsgID — id реплай-сообщения САМОЙ цели (кейс «reply на сообщение
// цели»), нужен как страховка на удаление, если revoke его не стёр. Для
// остальных путей резолва конкретного сообщения нет — 0 (не путать с
// приветствием бота, его чистит cleanupTargetTraces по таблице greetings).
func (b *Bot) resolveModTarget(message telego.Message) (targetID int64, targetMsgID int, ok bool) {
	// 1. text_mention — id прямо в entity.
	for _, e := range message.Entities {
		if e.Type == telego.EntityTypeTextMention && e.User != nil {
			return e.User.ID, 0, true
		}
	}
	// 2. @username из аргумента команды.
	if uname := firstUsernameArg(message.Text); uname != "" {
		if id, ok, err := b.db.UserIDByUsername(b.runCtx, uname); err == nil && ok {
			return id, 0, true
		}
	}
	// 3/4. reply: на сообщение цели или на приветствие бота о цели.
	if r := message.ReplyToMessage; r != nil {
		if b.me != nil && r.From != nil && r.From.ID == b.me.ID {
			// Реплай на наше приветствие — цель по таблице greetings.
			if id, ok, err := b.db.GreetingUserByMsg(b.runCtx, message.Chat.ID, r.MessageID); err == nil && ok {
				return id, 0, true
			}
		}
		if r.From != nil && !r.From.IsBot {
			return r.From.ID, r.MessageID, true
		}
	}
	return 0, 0, false
}

// firstUsernameArg вытаскивает первый @username из текста команды (без «@»).
func firstUsernameArg(text string) string {
	for _, f := range strings.Fields(text) {
		if strings.HasPrefix(f, "@") && len(f) > 1 {
			return strings.TrimPrefix(f, "@")
		}
	}
	return ""
}

// cleanupTargetTraces сносит наши сообщения о цели: приветствие и активную
// плашку голосования. Чужие сообщения с упоминанием цели удалить нельзя —
// Bot API не ищет по истории, а тексты мы не храним.
func (b *Bot) cleanupTargetTraces(chatID, targetID int64) {
	if msgID, ok, err := b.db.TakeGreetingMsg(b.runCtx, chatID, targetID); err == nil && ok {
		if err := b.deleteMessage(b.runCtx, chatID, msgID); err != nil {
			b.log.Debug("delete greeting of moderated user", "err", err, "chat", chatID)
		}
	}
	if botMsgID, ok, err := b.db.TakeSpamVoteByAuthor(b.runCtx, chatID, targetID); err == nil && ok {
		if err := b.deleteMessage(b.runCtx, chatID, botMsgID); err != nil {
			b.log.Debug("delete vote plashka of moderated user", "err", err, "chat", chatID)
		}
	}
}

// replyTo отвечает реплаем на сообщение-команду (для отказов).
func (b *Bot) replyTo(ctx *th.Context, message telego.Message, text string) {
	params := tu.Message(tu.ID(message.Chat.ID), text).
		WithReplyParameters(&telego.ReplyParameters{MessageID: message.MessageID})
	if message.IsTopicMessage {
		params = params.WithMessageThreadID(message.MessageThreadID)
	}
	_, _ = b.api.SendMessage(ctx, params)
}

func (b *Bot) sendPlain(chatID int64, text string) {
	_, _ = b.api.SendMessage(b.runCtx, tu.Message(tu.ID(chatID), text))
}

func (b *Bot) sendHTML(chatID int64, text string) {
	_, _ = b.api.SendMessage(b.runCtx, tu.Message(tu.ID(chatID), text).
		WithParseMode(telego.ModeHTML))
}

// notifyModAction определён в notify.go (часть mod-уведомлений).
