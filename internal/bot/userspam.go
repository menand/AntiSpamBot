package bot

import (
	"fmt"
	"time"

	"github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
	tu "github.com/mymmrac/telego/telegoutil"

	"github.com/menand/AntiSpamBot/internal/storage"
)

// handleSpamCommand — /spam реплаем на сообщение: народный репорт. Плашку с
// кнопками «Да, спам»/«Нет, не спам» может поднять ЛЮБОЙ участник с историей
// сообщений в этом чате (тот же порог доверия, что у голосующих), дальше
// работает штатное голосование: гейт доверия бюллетеней, золотой голос
// админа, перевес effectiveSpamVoteMargin, вердикт = глобальный кросс-бан.
// Инициатор и подозреваемый в собственном репорте не голосуют.
func (b *Bot) handleSpamCommand(ctx *th.Context, message telego.Message) error {
	if message.Chat.Type != "group" && message.Chat.Type != "supergroup" {
		return nil
	}
	chatID := message.Chat.ID
	if !b.chatServiceable(chatID) || message.From == nil || !b.commandForUs(message.Text) {
		return nil
	}
	// Анонимный админ пишет от имени самого чата (From = GroupAnonymousBot):
	// репортов «от имени чата» не поддерживаем — но молчать, как остальные
	// служебные фильтры, не будем: живой админ заслуживает объяснения.
	if sc := message.SenderChat; sc != nil && sc.ID == chatID {
		b.refuseAndDelete(ctx, message,
			"Репорты от имени чата не принимаются — напишите от своего имени; "+
				"а так у вас и есть золотой голос на любой плашке.")
		return nil
	}
	// Боты, «Telegram» и автофорварды команд не пишут (их From серверно
	// подлинный, но это не рука участника).
	if message.From.IsBot || message.From.ID == telegramServiceUserID ||
		message.IsAutomaticForward {
		return nil
	}

	r := message.ReplyToMessage
	if r == nil || r.ForumTopicCreated != nil {
		b.refuseAndDelete(ctx, message,
			"Репорт работает реплаем: ответь /spam на сообщение спамера "+
				"или на моё приветствие о нём.")
		return nil
	}

	// Гейт доверия инициатора — тот же порог, что у бюллетеней. Fail-closed:
	// ошибка чтения счётчика репорту не доверяет (как и голосу).
	s := b.chatSettings(b.runCtx, chatID)
	total, err := b.db.UserMessageTotal(b.runCtx, chatID, message.From.ID)
	if err != nil || total <= effectiveSpamWhitelist(s) {
		if err != nil {
			b.log.Warn("spam report trust gate", "err", err, "chat", chatID, "user", message.From.ID)
		}
		b.refuseAndDelete(ctx, message,
			"Репорты могут отправлять участники с историей сообщений в этом чате.")
		return nil
	}

	targetID, ok := b.resolveReportTarget(chatID, r)
	if !ok {
		b.refuseAndDelete(ctx, message,
			"Не понял, кого репортишь. Ответь командой на сообщение юзера "+
				"или на моё приветствие о нём.")
		return nil
	}
	// Те же защиты цели, что у модкоманд: не сам репортёр, не бот, не админ.
	if !b.guardModTarget(ctx, message, targetID) {
		return nil
	}

	if pending, perr := b.db.HasPendingVoteForAuthor(b.runCtx, chatID, targetID); perr != nil {
		// Fail-closed: не смогли проверить — репорт не принимаем. Пропущенный
		// репорт ничего не стоит, двойная плашка — стоит (дубли событий и ЛС).
		b.log.Warn("pending vote check", "err", perr, "chat", chatID)
		b.refuseAndDelete(ctx, message, "Не получилось проверить активные голосования, попробуй позже.")
		return nil
	} else if pending {
		b.refuseAndDelete(ctx, message,
			"Плашка голосования на этого юзера уже висит — дождись её исхода.")
		return nil
	}

	// Дальше ветки успеха/полууспеха: команду убираем сразу для чистоты чата
	// (best effort), как на успешных модкомандах.
	if err := b.deleteMessage(b.runCtx, chatID, message.MessageID); err != nil {
		b.log.Debug("delete /spam command", "err", err, "chat", chatID)
	}

	// Имена обоих в кэш: текст плашки и уведомления владельцам рендерятся
	// по id из user_info.
	b.rememberUser(b.runCtx, storage.UserInfo{
		UserID:    message.From.ID,
		FirstName: message.From.FirstName,
		LastName:  message.From.LastName,
		Username:  message.From.Username,
	})
	b.rememberUser(b.runCtx, storage.UserInfo{
		UserID:    r.From.ID,
		FirstName: r.From.FirstName,
		LastName:  r.From.LastName,
		Username:  r.From.Username,
	})
	infos, err := b.db.GetUserInfos(b.runCtx, []int64{message.From.ID, targetID})
	if err != nil {
		b.log.Warn("spam report: user infos", "err", err, "chat", chatID)
		infos = map[int64]storage.UserInfo{}
	}

	sent, err := b.api.SendMessage(b.runCtx,
		tu.Message(tu.ID(chatID), manualVoteText(
			mentionOrID(infos, message.From.ID),
			mentionOrID(infos, targetID),
			0, 0, effectiveSpamVoteMargin(s))).
			WithParseMode(telego.ModeHTML).
			WithReplyParameters(&telego.ReplyParameters{MessageID: r.MessageID}).
			WithReplyMarkup(spamVoteKeyboard()))
	if err != nil {
		b.log.Warn("send spam report message", "err", err, "chat", chatID)
		b.sendPlain(chatID, threadOf(message), b.modReceiver(chatID, message),
			"Не получилось повесить плашку — попробуй ещё раз.")
		return nil
	}
	// Атомарный захват «одна плашка на автора»: проиграв гонку, сносим свою
	// свежую плашку — иначе две живых голосовалки дали бы два независимых
	// вердикта (двойной spamban/banEverywhere и дубли уведомлений).
	inserted, err := b.db.PutSpamVoteOnce(b.runCtx, storage.SpamVote{
		ChatID:      chatID,
		BotMsgID:    sent.MessageID,
		TargetMsgID: r.MessageID,
		AuthorID:    targetID,
		InitiatorID: message.From.ID,
		Prob:        100, // легаси-колонка NOT NULL, никогда не отображается
		CreatedAt:   time.Now(),
	})
	if err != nil {
		b.log.Error("persist spam report vote", "err", err, "chat", chatID)
		_ = b.deleteMessage(b.runCtx, chatID, sent.MessageID)
		b.sendPlain(chatID, threadOf(message), b.modReceiver(chatID, message),
			"Не получилось сохранить голосование — попробуй позже.")
		return nil
	}
	if !inserted {
		_ = b.deleteMessage(b.runCtx, chatID, sent.MessageID)
		b.log.Info("spam report lost race — plashka already up for author",
			"chat", chatID, "target", targetID)
		b.sendHTML(chatID, threadOf(message), b.modReceiver(chatID, message),
			"Плашка голосования на этого юзера уже висит — голосуй там.")
		return nil
	}

	// История подозрений для /info: только при живой плашке.
	if err := b.db.RecordEvent(b.runCtx, chatID, targetID, storage.EventSuspect, time.Now(), ""); err != nil {
		b.log.Warn("record suspect event (report)", "err", err, "chat", chatID, "target", targetID)
	}
	// Уведомление подписанным владельцам — синхронно, мы уже в хендлере.
	b.notifySpamSuspicion(*r, "🚩 Репорт от "+userLabel(*message.From))

	b.log.Info("spam report", "chat", chatID, "target", targetID,
		"by", message.From.ID, "bot_msg", sent.MessageID)
	return nil
}

// resolveReportTarget — цель репорта ТОЛЬКО реплаем: на сообщение цели или
// на приветствие бота о ней (удобно репортить только что вошедшего). Каналы
// и сервисные отправители целью быть не могут: их нельзя забанить как
// участника, а плашка повисла бы мёртвым грузом.
func (b *Bot) resolveReportTarget(chatID int64, r *telego.Message) (int64, bool) {
	if r.From == nil {
		return 0, false
	}
	// Сначала приветствие: его автор — наш собственный бот, общий фильтр
	// ботов ниже иначе закрыл бы этот путь навсегда.
	if b.me != nil && r.From.ID == b.me.ID {
		id, ok, err := b.db.GreetingUserByMsg(b.runCtx, chatID, r.MessageID)
		if err != nil {
			b.log.Warn("resolve report by greeting", "err", err, "chat", chatID, "msg", r.MessageID)
		}
		if ok {
			return id, true
		}
		return 0, false
	}
	// Автофорвард поста привязанного канала: From там канал-бот, а SenderChat
	// — сам канал; banRevoke по отрицательному id выгнал бы канал из чата.
	if r.IsAutomaticForward {
		return 0, false
	}
	if r.From.IsBot || r.From.ID <= 0 || r.From.ID == telegramServiceUserID {
		return 0, false
	}
	return r.From.ID, true
}

// voteRuleLine/voteTallyLine — общие фразы всех спам-плашек (ИИ и ручных):
// единый источник, чтобы формулировки и падежи не разъезжались.
func voteRuleLine(margin int) string {
	return fmt.Sprintf("Голосуйте кнопками — перевес в %d %s решает. Голос админа решает сразу.",
		margin, pluralRU(margin, "голос", "голоса", "голосов"))
}

func voteTallyLine(yes, no int) string {
	return fmt.Sprintf("🚫 Спам: <b>%d</b> · ✅ Не спам: <b>%d</b>", yes, no)
}

// manualVoteText — текст плашки народного репорта; заголовок с инициатором
// переживает перерисовки счёта и рестарты (voteView читает initiator_id).
func manualVoteText(reporter, suspect string, yes, no, margin int) string {
	return fmt.Sprintf("<b>🚩 Репорт:</b> %s считает, что %s — спамер. Забанить?\n\n%s\n"+
		"<i>Инициатор репорта и подозреваемый не голосуют.</i>\n\n%s",
		reporter, suspect, voteRuleLine(margin), voteTallyLine(yes, no))
}
