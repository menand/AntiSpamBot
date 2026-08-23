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
// все сообщения цели. Доступ — админ чата (в т.ч. анонимный) или владелец
// бота; самозванцам — punishNonAdmin. В SetMyCommands НЕ регистрируются —
// «/»-меню в группах остаётся пустым, новые кнопки в поле ввода не появляются.
func (b *Bot) handleKickCommand(ctx *th.Context, message telego.Message) error {
	return b.handleModCommand(ctx, message, false)
}

func (b *Bot) handleBanCommand(ctx *th.Context, message telego.Message) error {
	return b.handleModCommand(ctx, message, true)
}

func (b *Bot) handleModCommand(ctx *th.Context, message telego.Message, permanent bool) error {
	chatID, ok := b.modPrologue(ctx, message)
	if !ok {
		return nil
	}
	action := "кик"
	if permanent {
		action = "бан"
	}

	targetID, targetMsgID, ok := b.resolveModTarget(message)
	if !ok {
		b.refuseAndDelete(ctx, message,
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
		b.sendPlain(chatID, threadOf(message), b.modReceiver(chatID, message), "Не получилось — проверь мои права на блокировку.")
		return nil
	}

	kind := storage.EventKick
	if permanent {
		kind = storage.EventBan
	}
	if err := b.db.RecordEvent(b.runCtx, chatID, targetID, kind, time.Now(), reason); err != nil {
		b.log.Warn("record mod event", "err", err, "chat", chatID, "target", targetID)
	}
	b.cleanupTargetTraces(chatID, targetID)
	// revoke обычно уже стирает исходное сообщение цели; ручное удаление —
	// страховка на случай, если конкретно оно осталось (тот же приём, что и
	// удаление TargetMsgID в resolveSpamVote), поэтому ошибку глушим тихо.
	if targetMsgID != 0 {
		if err := b.deleteMessage(b.runCtx, chatID, targetMsgID); err != nil {
			b.log.Debug("delete mod target message (already gone?)", "err", err, "chat", chatID)
		}
	}

	mention := b.mentionFor(targetID)
	recv := b.modReceiver(chatID, message)
	if permanent {
		b.sendHTML(chatID, threadOf(message), recv, "🚫 "+mention+" забанен.")
	} else {
		b.sendHTML(chatID, threadOf(message), recv, "👢 "+mention+" кикнут.")
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
	chatID, ok := b.modPrologue(ctx, message)
	if !ok {
		return nil
	}
	// ForumTopicCreated: в форумах сообщение без явного реплая несёт неявный
	// reply_to_message на корень топика — его удалять нельзя.
	if r := message.ReplyToMessage; r != nil && r.ForumTopicCreated == nil {
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
	chatID, ok := b.modPrologue(ctx, message)
	if !ok {
		return nil
	}
	// RestrictChatMember работает только в супергруппах — честный отказ
	// вместо «проверь права» после бесполезной лестницы ретраев.
	if message.Chat.Type != "supergroup" {
		b.refuseAndDelete(ctx, message, "Мьют работает только в супергруппах.")
		return nil
	}
	d, ok := parseMuteDuration(message.Text)
	if !ok {
		b.refuseAndDelete(ctx, message,
			"Не понял срок. Примеры: /mute 45, /mute 45m, /mute 3h, /mute 5d — "+
				"реплаем на сообщение юзера или с @username.")
		return nil
	}
	targetID, _, ok := b.resolveModTarget(message)
	if !ok {
		b.refuseAndDelete(ctx, message,
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
	if err := b.mute(b.runCtx, chatID, targetID, d); err != nil {
		b.log.Warn("mute failed", "err", err, "chat", chatID, "target", targetID)
		b.sendPlain(chatID, threadOf(message), b.modReceiver(chatID, message), "Не получилось — проверь мои права на ограничение участников.")
		return nil
	}
	// Замьюченный физически не может выполнить «напиши что-нибудь» — снимаем
	// ожидание реплая тихо, иначе таймер кикнул бы его за молчание. Если
	// ожидание было активным (reply-check), «прошёл» ещё не записан —
	// фиксируем пасс здесь: капча уже позади, а невозможность ответить — не
	// вина юзера (раньше этот пасс писался сразу на капче, поведение то же).
	if b.cancelReplyWait(chatID, targetID) {
		if err := b.db.RecordEvent(b.runCtx, chatID, targetID, storage.EventPass, time.Now(), ""); err != nil {
			b.log.Warn("record pass event (muted)", "err", err)
		}
	}
	// Событие mute питает список «10 последних» команды /unmute (в воронку
	// статистики не идёт). Наказательный минутный мьют punishNonAdmin сюда
	// не пишется — это не админское решение о юзере.
	if err := b.db.RecordEvent(b.runCtx, chatID, targetID, storage.EventMute, time.Now(),
		storage.ReasonModPrefix+fmt.Sprintf("%d", message.From.ID)); err != nil {
		b.log.Warn("record mute event", "err", err)
	}
	b.sendHTML(chatID, threadOf(message), b.modReceiver(chatID, message), "🔇 "+b.mentionFor(targetID)+" в рид-онли на "+muteLabel(d)+".")
	b.log.Info("mute command", "chat", chatID, "target", targetID,
		"minutes", int(d.Minutes()), "by", message.From.ID)
	return nil
}

// modPrologue — общие ворота всех админских команд в группе: тип чата,
// chatServiceable, отсев чужих адресатов (/cmd@ДругойБот), доступ. Возвращает
// ok=false, если команду надо проигнорировать или отказ уже отправлен.
func (b *Bot) modPrologue(ctx *th.Context, message telego.Message) (int64, bool) {
	if message.Chat.Type != "group" && message.Chat.Type != "supergroup" {
		return 0, false
	}
	chatID := message.Chat.ID
	if !b.chatServiceable(chatID) || message.From == nil {
		return 0, false
	}
	if !b.commandForUs(message.Text) {
		return 0, false
	}
	// Анонимный админ пишет от имени самого чата (sender_chat == чат) —
	// getChatMember его не различает, но право модерации у него есть. Ветвь
	// ДО бот-фильтров: его From — как раз бот (GroupAnonymousBot).
	if sc := message.SenderChat; sc != nil && sc.ID == chatID {
		return chatID, true
	}
	// Боты, сервисный «Telegram» и автофорварды привязанного канала команд
	// не исполняют и наказанием не punished: их From серверно подлинный, но
	// это не рука участника (иначе текст поста канала «/mute …» замьютил бы
	// самого канал-бота с публичным «не балуйся»).
	if message.From.IsBot || message.From.ID == telegramServiceUserID ||
		message.IsAutomaticForward {
		return 0, false
	}
	if b.isOwner(message.From.ID) {
		return chatID, true
	}
	// Админство проверяем ЖИВО, мимо кэш-негатива: в чатах, где бот сам не
	// админ, Telegram chat_member-события не доставляет, и свежепроизведённый
	// админ сидел бы в негативном кэше все 10 минут TTL — а наказывать по
	// устаревшему «не админ» нельзя. Ошибка живой проверки: команду разрешаем
	// по кэшу (сетевой шторм не должен обезоруживать настоящего админа), но
	// наказание допустимо только по свежему подтверждённому «не админ».
	isAdmin, sure := b.isChatAdminFresh(ctx, chatID, message.From.ID)
	if !sure {
		if b.isChatAdminCached(ctx, chatID, message.From.ID) {
			return chatID, true
		}
		// На ошибке getChatMember молча игнорируем — настоящий админ повторит
		// команду.
		return 0, false
	}
	if !isAdmin {
		b.punishNonAdmin(ctx, message)
		return 0, false
	}
	return chatID, true
}

// commandForUs — false, если команда явно адресована другому боту
// (/mute@ДругойБот): telego CommandEqual сравнивает только имя команды,
// а наказывать юзера за чужую команду нельзя.
func (b *Bot) commandForUs(text string) bool {
	f := strings.Fields(text)
	if len(f) == 0 || b.me == nil {
		return true
	}
	if i := strings.IndexByte(f[0], '@'); i >= 0 {
		return strings.EqualFold(f[0][i+1:], b.me.Username)
	}
	return true
}

// refuseAndDelete — отказ по неразрешённой мод-команде: ответ якорится на
// команду, ПОТОМ команда удаляется (порядок punishNonAdmin) — служебной
// команде нечего висеть в чате, как и на успешных ветках.
func (b *Bot) refuseAndDelete(ctx *th.Context, message telego.Message, text string) {
	b.replyTo(ctx, message, text)
	if err := b.deleteMessage(b.runCtx, message.Chat.ID, message.MessageID); err != nil {
		b.log.Debug("delete refused mod command", "err", err, "chat", message.Chat.ID)
	}
}

// punishNonAdmin — не-админ дёрнул админскую команду: минутный мьют + ответ +
// удаление самой команды (как на успешных админских ветках — служебной команде
// нечего висеть в чате). Мьют и удаление best-effort: в обычной group рестрикт
// недоступен, без прав не пройдёт — тогда честно отвечаем без обещания мьюта,
// которого не было.
func (b *Bot) punishNonAdmin(ctx *th.Context, message telego.Message) {
	muted := b.mute(b.runCtx, message.Chat.ID, message.From.ID, time.Minute) == nil
	if !muted {
		b.log.Debug("punish mute failed", "chat", message.Chat.ID)
	}
	// Сначала ответ (пока якорь-сообщение живо), потом удаление команды.
	text := "🙅 Это админская команда, не балуйся."
	if muted {
		text += " Мьют на 1 минуту."
	}
	b.replyTo(ctx, message, text)
	if err := b.deleteMessage(b.runCtx, message.Chat.ID, message.MessageID); err != nil {
		b.log.Debug("delete punished command", "err", err, "chat", message.Chat.ID)
	}
	b.log.Info("non-admin punished for mod command", "chat", message.Chat.ID,
		"user", message.From.ID, "muted", muted)
}

// handleGroupHelpCommand — /help в группе: справка о командах, ВСЕГДА
// эфемерно (независимо от ephemeral_enabled — ответ адресован одному юзеру,
// чат не засоряется), сама команда удаляется. Анонимному отправителю
// (From = GroupAnonymousBot) эфемерку не доставить — ему публично, как в
// modReceiver. /help@ДругойБот отсеивает commandForUs. Вызывается из
// handlePrivateStart (общая регистрация /start|/help без chat-предиката).
func (b *Bot) handleGroupHelpCommand(_ *th.Context, message telego.Message) error {
	if message.Chat.Type != "group" && message.Chat.Type != "supergroup" {
		return nil
	}
	chatID := message.Chat.ID
	if !b.chatServiceable(chatID) || message.From == nil || !b.commandForUs(message.Text) {
		return nil
	}
	var recv int64
	if !message.From.IsBot {
		recv = message.From.ID
	}
	b.sendHTML(chatID, threadOf(message), recv, b.groupHelpText())
	if err := b.deleteMessage(b.runCtx, chatID, message.MessageID); err != nil {
		b.log.Debug("delete /help command", "err", err, "chat", chatID)
	}
	b.log.Info("group help", "chat", chatID, "user", message.From.ID)
	return nil
}

// groupCommandsList — перечень групповых команд; ЕДИНЫЙ источник для
// групповой справки (/help в чате) и полной ЛС-справки (helpText в menu.go) —
// при добавлении команды списки не разъезжаются. /spam — единственная команда
// не-админов (порог доверия, без наказаний); /info формально админская:
// не-админ карается punishNonAdmin, как и за модкоманды.
const groupCommandsList = "/spam — репорт на спамера (реплаем; могут все участники с историей сообщений)\n" +
	"/whatsnew — что нового в боте\n" +
	"\nКоманды админов:\n" +
	"/info @user — карточка юзера: вход, активность, нарушения\n" +
	"/kick — кикнуть (реплаем на сообщение или @username)\n" +
	"/ban — забанить навсегда и стереть сообщения\n" +
	"/mute 30 | 2h | 3d — рид-онли на срок\n" +
	"/del — тихо удалить сообщение (реплаем)\n" +
	"/unban — разбанить (список последних банов или @username)\n" +
	"/unmute — снять мьют (список или @username)\n" +
	"/whitelist — впускать без капчи (список или @username)"

func (b *Bot) groupHelpText() string {
	t := "🛡 <b>Команды бота</b> (/spam доступна всем участникам с историей сообщений):\n" + groupCommandsList +
		"\n/help — эта справка (видна только тебе)"
	if b.me != nil && b.me.Username != "" {
		t += "\n\nСтатистика и настройки — в ЛС: @" + b.me.Username
	}
	return t
}

// mentionFor — HTML-упоминание юзера по кэшу user_info (или голый ID).
func (b *Bot) mentionFor(targetID int64) string {
	infos, _ := b.db.GetUserInfos(b.runCtx, []int64{targetID})
	return mentionOrID(infos, targetID)
}

// threadOf — топик команды для ответных сообщений (0 — не форум): без него
// подтверждение улетело бы в General вместо топика команды.
func threadOf(message telego.Message) int {
	if message.IsTopicMessage {
		return message.MessageThreadID
	}
	return 0
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

// muteLabel — срок для подтверждения «в рид-онли на …», винительный падеж
// через pluralRU (как остальные пользовательские строки бота).
func muteLabel(d time.Duration) string {
	day := 24 * time.Hour
	switch {
	case d%day == 0:
		n := int(d / day)
		return fmt.Sprintf("%d %s", n, pluralRU(n, "день", "дня", "дней"))
	case d%time.Hour == 0:
		n := int(d / time.Hour)
		return fmt.Sprintf("%d %s", n, pluralRU(n, "час", "часа", "часов"))
	default:
		n := int(d / time.Minute)
		return fmt.Sprintf("%d %s", n, pluralRU(n, "минуту", "минуты", "минут"))
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
		id, ok, err := b.db.UserIDByUsername(b.runCtx, uname)
		if err != nil {
			b.log.Warn("resolve target by username", "err", err, "username", uname)
		}
		if ok {
			return id, 0, true
		}
	}
	// 3/4. reply: на сообщение цели или на приветствие бота о цели.
	// ForumTopicCreated: в форумах сообщение без явного реплая несёт неявный
	// reply_to_message на корень топика — иначе голая команда в топике молча
	// нацелилась бы на создателя топика.
	if r := message.ReplyToMessage; r != nil && r.ForumTopicCreated == nil {
		if b.me != nil && r.From != nil && r.From.ID == b.me.ID {
			// Реплай на наше приветствие — цель по таблице greetings.
			id, ok, err := b.db.GreetingUserByMsg(b.runCtx, message.Chat.ID, r.MessageID)
			if err != nil {
				b.log.Warn("resolve target by greeting", "err", err,
					"chat", message.Chat.ID, "msg", r.MessageID)
			}
			if ok {
				return id, 0, true
			}
		}
		// Та же гигиена цели, что у /spam (resolveReportTarget): автофорварды
		// привязанного канала (From там сервисный «Telegram», IsBot=false!),
		// боты и каналы целью наказания быть не могут — иначе ban/kick упал
		// бы об API, а админ получил ложное «проверь мои права» вместо
		// честного «не понял цель».
		if !r.IsAutomaticForward && r.From != nil && !r.From.IsBot &&
			r.From.ID > 0 && r.From.ID != telegramServiceUserID {
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

// cleanupTargetTraces сносит наши сообщения о цели и гасит её активные
// проверки: приветствие, плашку голосования, капчу и reply-wait. Капча нужна:
// /kick по цели с активной капчей иначе дал бы таймауту право записать СВОЙ
// kick/ban поверх только что записанного события команды (двойной счёт в
// воронке). Reply-wait — по той же причине: waitTimeout кикнул бы за
// «молчание» того, кого уже кикнули. Без событий — их пишет команда.
// Чужие сообщения с упоминанием цели удалить нельзя — Bot API не ищет по
// истории, а тексты мы не храним.
func (b *Bot) cleanupTargetTraces(chatID, targetID int64) {
	b.cancelCaptchaSilent(chatID, targetID)
	b.cancelReplyWait(chatID, targetID)
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

// modReceiver — получатель эфемерного ответа на команду: id вызвавшего, когда
// в чате включён эфемерный режим; 0 = отвечать публично. Анонимный админ пишет
// от имени GroupAnonymousBot — боту эфемерное не доставить, ему публично.
func (b *Bot) modReceiver(chatID int64, message telego.Message) int64 {
	if message.From == nil || message.From.IsBot {
		return 0
	}
	if !b.chatSettings(b.runCtx, chatID).EphemeralEnabled {
		return 0
	}
	return message.From.ID
}

// replyTo отвечает реплаем на сообщение-команду (для отказов). При включённом
// эфемерном режиме ответ видит только вызвавший.
func (b *Bot) replyTo(ctx *th.Context, message telego.Message, text string) {
	params := tu.Message(tu.ID(message.Chat.ID), text).
		WithReplyParameters(&telego.ReplyParameters{MessageID: message.MessageID})
	if message.IsTopicMessage {
		params = params.WithMessageThreadID(message.MessageThreadID)
	}
	if recv := b.modReceiver(message.Chat.ID, message); recv != 0 {
		params = params.WithReceiverUserID(recv)
	}
	if _, err := b.api.SendMessage(ctx, params); err != nil {
		b.log.Debug("reply send", "err", err, "chat", message.Chat.ID)
	}
}

// sendPlain/sendHTML: receiverID ≠ 0 — эфемерно этому юзеру, 0 — публично.
func (b *Bot) sendPlain(chatID int64, threadID int, receiverID int64, text string) {
	params := tu.Message(tu.ID(chatID), text)
	if threadID != 0 {
		params = params.WithMessageThreadID(threadID)
	}
	if receiverID != 0 {
		params = params.WithReceiverUserID(receiverID)
	}
	if _, err := b.api.SendMessage(b.runCtx, params); err != nil {
		b.log.Debug("plain send", "err", err, "chat", chatID)
	}
}

func (b *Bot) sendHTML(chatID int64, threadID int, receiverID int64, text string) {
	params := tu.Message(tu.ID(chatID), text).WithParseMode(telego.ModeHTML)
	if threadID != 0 {
		params = params.WithMessageThreadID(threadID)
	}
	if receiverID != 0 {
		params = params.WithReceiverUserID(receiverID)
	}
	if _, err := b.api.SendMessage(b.runCtx, params); err != nil {
		b.log.Debug("html send", "err", err, "chat", chatID)
	}
}

// notifyModAction определён в notify.go (часть mod-уведомлений).
