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
//
// Репорт админа или владельца бота (isGoldenVoice) плашки не создаёт — он
// ЗОЛОТОЙ ГОЛОС и исполняется сразу: banRevoke в этом чате, вычеркание цели
// не требуется — бан с чисткой, глобальная база спамеров, кросс-бан по всем
// чатам. Гейт доверия на него не действует (золотой голос нигде не проходит
// через порог истории сообщений).
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
			"Репорты от имени чата не принимаются — напишите от своего имени: "+
				"репорт админа банит спамера сразу.")
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

	// Золотой голос проверяем ДО резолва цели: он зависит только от репортёра,
	// и ниже-пороговый не-золотой юзер тогда сжигает ровно ОДИН живой вызов
	// getChatMember до дешёвого отказа доверия (второй — guardModTarget —
	// ему уже не достаётся). Негатив кэшируется на 10 минут.
	if b.isGoldenVoice(chatID, message.From.ID) {
		targetID, ok := b.resolveReportTarget(chatID, r)
		if !ok {
			b.refuseAndDelete(ctx, message,
				"Не понял, кого репортишь. Ответь командой на сообщение юзера "+
					"или на моё приветствие о нём.")
			return nil
		}
		// Те же защиты цели, что у модкоманд: админом мгновенный бан не
		// целится, даже золотым голосом.
		if !b.guardModTarget(ctx, message, targetID) {
			return nil
		}
		b.execGoldenSpamReport(message, r, targetID)
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

	msgID, out := b.createSpamPlashka("spam report",
		tu.Message(tu.ID(chatID), manualVoteText(
			mentionOrID(infos, message.From.ID),
			mentionOrID(infos, targetID),
			0, 0, effectiveSpamVoteMargin(s))).
			WithParseMode(telego.ModeHTML).
			WithReplyParameters(&telego.ReplyParameters{MessageID: r.MessageID}).
			WithReplyMarkup(spamVoteKeyboard()),
		storage.SpamVote{
			ChatID:      chatID,
			TargetMsgID: r.MessageID,
			AuthorID:    targetID,
			InitiatorID: message.From.ID,
			Prob:        100, // легаси-колонка NOT NULL, никогда не отображается
			CreatedAt:   time.Now(),
		})
	if out != plashkaSent {
		switch out {
		case plashkaLostRace:
			b.sendHTML(chatID, threadOf(message), b.modReceiver(chatID, message),
				"Плашка голосования на этого юзера уже висит — голосуй там.")
		case plashkaPersistFailed:
			b.sendPlain(chatID, threadOf(message), b.modReceiver(chatID, message),
				"Не получилось сохранить голосование — попробуй позже.")
		case plashkaSendFailed:
			b.sendPlain(chatID, threadOf(message), b.modReceiver(chatID, message),
				"Не получилось отправить сообщение — попробуй позже.")
		case plashkaSent:
			// unreachable: guarded by `out != plashkaSent` above
		default:
			b.sendPlain(chatID, threadOf(message), b.modReceiver(chatID, message),
				"Не получилось повесить плашку — попробуй ещё раз.")
		}
		return nil
	}

	// История подозрений для /info: только при живой плашке.
	if err := b.db.RecordEvent(b.runCtx, chatID, targetID, storage.EventSuspect, time.Now(), ""); err != nil {
		b.log.Warn("record suspect event (report)", "err", err, "chat", chatID, "target", targetID)
	}
	// Уведомление подписанным владельцам — синхронно, мы уже в хендлере.
	b.notifySpamSuspicion(*r, "🚩 Репорт от "+userLabel(*message.From))

	b.log.Info("spam report", "chat", chatID, "target", targetID,
		"by", message.From.ID, "bot_msg", msgID)
	return nil
}

// execGoldenSpamReport — мгновенное исполнение репорта админом/владельцем:
// тот же путь, что вердикт «спам» на плашке, но без голосования. Висящая
// плашка этого автора снимается (эквивалент золотого «Да, спам»), улика
// уходит владельцам ДО бана — banRevoke стирает сообщения автора вместе с ней.
// Событие suspect не пишется: подозрения не было, вердикт виден как spamban.
//
// Single-winner: синтетическая строка в spam_votes через PutSpamVoteOnce —
// атомарный «один исполнитель на автора», как у голосований. Проигравший
// гонку выходит с тихим ответом; строка снимается сразу после исполнения
// (крах между вставкой и Take оставит её до суточного свипа — редкий
// задвоенный отказ репортам на этого автора, безобидный).
func (b *Bot) execGoldenSpamReport(message telego.Message, r *telego.Message, targetID int64) {
	chatID := message.Chat.ID

	// Команду убираем сразу для чистоты чата (best effort).
	if err := b.deleteMessage(b.runCtx, chatID, message.MessageID); err != nil {
		b.log.Debug("delete /spam command", "err", err, "chat", chatID)
	}

	// Имена обоих в кэш: подтверждение в чате и уведомления владельцам
	// рендерятся по id из user_info.
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

	// Висящую на авторе плашку снимаем: репорт золотого голоса закрыл вопрос
	// раньше сообщества. Best effort — ошибка БД не отменяет вердикт.
	// botMsgID == 0 — синтетическая lock-строка прошлого golden-репорта:
	// deleteMessage(0) не вызываем (инвариант проекта).
	if botMsgID, up, terr := b.db.TakeSpamVoteByAuthor(b.runCtx, chatID, targetID); terr != nil {
		b.log.Warn("golden report: take pending vote", "err", terr, "chat", chatID, "target", targetID)
	} else if up && botMsgID != 0 {
		if err := b.deleteMessage(b.runCtx, chatID, botMsgID); err != nil {
			b.log.Debug("delete taken vote plashka", "err", err, "chat", chatID)
		}
	}

	v := storage.SpamVote{
		ChatID:      chatID,
		BotMsgID:    0, // плашки нет — синтетическая строка-замок
		TargetMsgID: r.MessageID,
		AuthorID:    targetID,
		InitiatorID: message.From.ID,
		Prob:        100, // легаси-колонка NOT NULL, никогда не отображается
		CreatedAt:   time.Now(),
	}
	inserted, err := b.db.PutSpamVoteOnce(b.runCtx, v)
	if err != nil || !inserted {
		// Fail-closed на ошибке БД (прецедент pending-проверки) и тихий
		// выход проигравшего гонку: второй параллельный голден-репорт не
		// должен дать дубль spamban/уведомлений/подтверждений.
		if err != nil {
			b.log.Warn("golden report: claim", "err", err, "chat", chatID, "target", targetID)
			b.sendPlain(chatID, threadOf(message), b.modReceiver(chatID, message),
				"Не получилось сохранить решение — попробуй ещё раз.")
			return
		}
		b.log.Info("golden report lost race — already executing/executed",
			"chat", chatID, "target", targetID)
		b.sendPlain(chatID, threadOf(message), b.modReceiver(chatID, message),
			"Этого спамера уже банят — дубль не нужен.")
		return
	}

	// Улика владельцам до бана: revoke сотрёт сообщение вместе с форвардом.
	// После победы в гонке — двойного форварда от второго админа не будет.
	b.notifySpamSuspicion(*r, "🚩 Репорт от "+userLabel(*message.From))

	role := "админ"
	if b.isOwner(message.From.ID) {
		role = "владелец"
	}
	why := role + " " + userLabel(*message.From)
	banned := b.executeSpamBan(v, storage.ReasonVotePrefix, why)

	// Замок снят: исполнение завершено (успешное или нет — повторять его
	// осознанно не нужно, у бана нет частичного успеха, требующего ретрая
	// руками человека).
	if _, terr := b.db.TakeSpamVote(b.runCtx, chatID, v.BotMsgID); terr != nil {
		b.log.Warn("golden report: release claim", "err", terr, "chat", chatID)
	}

	b.log.Info("golden spam report", "chat", chatID, "target", targetID,
		"by", message.From.ID, "banned", banned)

	// Кросс-бан и уведомления — в горутине: обход всех чатов не должен
	// держать хендлер (локальный бан уже исполнен). Кросс-бан гейтится тем
	// же флагом banned, что у голосований: неисполнимый локально вердикт
	// не банит юзера по всему флоту без единого бюллетеня там.
	targets := b.spamNotifyTargets(b.runCtx)
	b.goSafe("goldenReportFanout", func() {
		var alsoBanned []string
		if banned {
			alsoBanned = b.banEverywhere(chatID, targetID)
		}
		b.notifySpamVerdict(targets, v, true, banned, why, nil, alsoBanned)
	})

	// Подтверждение адресовано исполнителю (modReceiver: в эфемерном чате
	// увидит только он, как у /kick|/ban) — реплаем на цель якориться нельзя,
	// revoke уже стёр её. Исход честный по флагу бана.
	text := "🚩 " + b.mentionFor(message.From.ID) + " распознал(а) спамера — " +
		b.mentionFor(targetID) + " забанен с чисткой сообщений."
	if !banned {
		text = "🚩 Репорт принят, но забанить не удалось — проверьте права бота."
	}
	b.sendHTML(chatID, threadOf(message), b.modReceiver(chatID, message), text)
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
// Формулировка «считает сообщение спамом» согласована с кнопками
// «Да, спам / Нет, не спам» (spamVoteKeyboard): вопрос «Забанить?» при таких
// кнопках читался как подмена темы голосования.
func manualVoteText(reporter, suspect string, yes, no, margin int) string {
	return fmt.Sprintf("<b>🚩 Репорт:</b> %s считает сообщение от %s спамом.\n\n%s\n"+
		"<i>Инициатор репорта и подозреваемый не голосуют.</i>\n\n%s",
		reporter, suspect, voteRuleLine(margin), voteTallyLine(yes, no))
}
