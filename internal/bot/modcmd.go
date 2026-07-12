package bot

import (
	"fmt"
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
		b.replyTo(ctx, message, "🙅 Не балуйся, эта команда только для админов.")
		return nil
	}

	targetID, ok := b.resolveModTarget(message)
	if !ok {
		b.replyTo(ctx, message,
			"Не понял, кого "+action+"ать. Ответь командой на сообщение юзера "+
				"или на моё приветствие о нём, либо укажи @username (я должен был его видеть).")
		return nil
	}
	// Защиты: не себя, не вызывающего, не другого админа/владельца.
	if b.me != nil && targetID == b.me.ID {
		b.replyTo(ctx, message, "Себя банить не дам 🙂")
		return nil
	}
	if targetID == message.From.ID {
		b.replyTo(ctx, message, "Себя-то за что? 🙂")
		return nil
	}
	if b.canManageChat(ctx, targetID, chatID) {
		b.replyTo(ctx, message, "Это админ — не трону.")
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

// resolveModTarget вычисляет цель команды по приоритету: text_mention (админ
// выбрал юзера автокомплитом) → @username в аргументе (по нашему кэшу) →
// reply на сообщение цели → reply на приветствие бота о цели.
func (b *Bot) resolveModTarget(message telego.Message) (int64, bool) {
	// 1. text_mention — id прямо в entity.
	for _, e := range message.Entities {
		if e.Type == telego.EntityTypeTextMention && e.User != nil {
			return e.User.ID, true
		}
	}
	// 2. @username из аргумента команды.
	if uname := firstUsernameArg(message.Text); uname != "" {
		if id, ok, err := b.db.UserIDByUsername(b.runCtx, uname); err == nil && ok {
			return id, true
		}
	}
	// 3/4. reply: на сообщение цели или на приветствие бота о цели.
	if r := message.ReplyToMessage; r != nil {
		if b.me != nil && r.From != nil && r.From.ID == b.me.ID {
			// Реплай на наше приветствие — цель по таблице greetings.
			if id, ok, err := b.db.GreetingUserByMsg(b.runCtx, message.Chat.ID, r.MessageID); err == nil && ok {
				return id, true
			}
		}
		if r.From != nil && !r.From.IsBot {
			return r.From.ID, true
		}
	}
	return 0, false
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
