package bot

import (
	"github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
)

// handleTrustCommand — /trust: досрочно снять карантин новичка (реплаем на
// сообщение юзера / моё приветствие о нём / @username — тот же резолв, что у
// /kick). Гейты те же, что у прочих админских команд (modPrologue +
// guardModTarget): админ чата (в т.ч. анонимный) или владелец бота. Снимает
// и серверный «только текст»-рестрикт (обратный release), и строку с
// зеркалом; если карантина нет — честный отказ, а не ложное «готово». В
// SetMyCommands НЕ регистрируется — «/»-меню в группах остаётся пустым.
func (b *Bot) handleTrustCommand(ctx *th.Context, message telego.Message) error {
	chatID, ok := b.modPrologue(ctx, message)
	if !ok {
		return nil
	}
	targetID, _, ok := b.resolveModTarget(message)
	if !ok {
		b.refuseAndDelete(ctx, message,
			"Не понял, с кого снять карантин. Ответь командой на сообщение юзера "+
				"или на моё приветствие о нём, либо укажи @username (я должен был его видеть).")
		return nil
	}
	if !b.guardModTarget(ctx, message, targetID) {
		return nil
	}
	if err := b.deleteMessage(b.runCtx, chatID, message.MessageID); err != nil {
		b.log.Debug("delete trust command", "err", err, "chat", chatID)
	}
	recv := b.modReceiver(chatID, message)
	mention := b.mentionFor(targetID)
	if b.releaseQuarantineUser(chatID, targetID) {
		b.sendHTML(chatID, threadOf(message), recv,
			"🧫 Карантин снят с "+mention+" — снова можно ссылки и пересылки.")
		b.log.Info("trust command", "chat", chatID, "target", targetID, "by", message.From.ID)
	} else {
		b.sendHTML(chatID, threadOf(message), recv,
			"У "+mention+" нет активного карантина.")
		b.log.Info("trust command: nothing to release",
			"chat", chatID, "target", targetID, "by", message.From.ID)
	}
	return nil
}
