package bot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html"
	"strconv"
	"strings"
	"time"

	"github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegoapi"
	th "github.com/mymmrac/telego/telegohandler"
	tu "github.com/mymmrac/telego/telegoutil"

	"github.com/menand/AntiSpamBot/internal/captcha"
	"github.com/menand/AntiSpamBot/internal/storage"
)

// telegramServiceUserID — псевдо-юзер «Telegram» (не помечен как бот),
// авторствующий автофорварды привязанного канала и прочие сервисные посты.
const telegramServiceUserID = 777000

func (b *Bot) handleChatMember(ctx *th.Context, update telego.Update) error {
	upd := update.ChatMember
	if upd == nil {
		return nil
	}

	oldStatus := upd.OldChatMember.MemberStatus()
	newStatus := upd.NewChatMember.MemberStatus()
	user := upd.NewChatMember.MemberUser()

	b.log.Info("chat_member event",
		"chat", upd.Chat.ID,
		"chat_type", upd.Chat.Type,
		"user", user.ID,
		"old", oldStatus,
		"new", newStatus)

	// Любая смена статуса могла дать или отнять админку — кэш для белого
	// списка антиспама и золотого голоса не должен пережить это событие.
	b.invalidateAdminCache(upd.Chat.ID, user.ID)

	if upd.Chat.Type != "group" && upd.Chat.Type != "supergroup" {
		return nil
	}
	if !b.chatAllowed(upd.Chat.ID) {
		return nil
	}
	if user.IsBot {
		return nil
	}
	if b.me != nil && user.ID == b.me.ID {
		return nil
	}

	// Ручной разбан админом (kicked → любой другой статус чужой рукой, но не
	// нашей собственной — kick() бота делает ban+unban) снимает и глобальный
	// флаг спамера, иначе ошибочный вердикт был бы неисправим: join-хук
	// банил бы заново при каждом входе. ДО joined-ветки: kicked→member — это
	// тоже join, и без прощения он тут же упёрся бы в IsSpamBanned.
	if oldStatus == "kicked" && newStatus != "kicked" &&
		upd.From.ID != user.ID && (b.me == nil || upd.From.ID != b.me.ID) {
		if removed, err := b.db.DeleteSpamBanned(b.runCtx, user.ID); err != nil {
			b.log.Warn("delete spam banned", "err", err, "user", user.ID)
		} else if removed {
			b.log.Info("spam ban forgiven — admin unbanned manually",
				"chat", upd.Chat.ID, "user", user.ID, "by", upd.From.ID)
		}
	}

	joined := (oldStatus == "left" || oldStatus == "kicked") &&
		(newStatus == "member" || newStatus == "restricted")
	if joined {
		// chat_member-апдейты не несут информации о топике; капча уйдёт в General (0).
		b.onUserJoined(upd.Chat, user, 0)
		return nil
	}

	// Юзер вышел (или его убрал админ), пока капча была активна: тихо
	// отменяем её. Проверку он не провалил — событие kick здесь исказило бы
	// статистику провалов, а наш собственный кик после провала не задет
	// (вызывающие onFail всегда забирают pending через Take ДО кика, так что
	// для них этот lookup промахнётся).
	if newStatus == "left" || newStatus == "kicked" {
		if p, ok := b.store.Take(upd.Chat.ID, user.ID); ok {
			p.Cancel()
			_ = b.db.DeletePending(b.runCtx, upd.Chat.ID, user.ID)
			if err := b.deleteBotMessage(b.runCtx, upd.Chat.ID, p.MessageID, p.EphemeralID, p.UserID); err != nil {
				b.log.Warn("delete captcha after user left",
					"err", err, "chat", upd.Chat.ID, "msg", p.MessageID)
			}
			b.log.Info("captcha cancelled — user left mid-captcha",
				"chat", upd.Chat.ID, "user", user.ID)
		}
		// Ожидание «ответь на приветствие» тоже снимается тихо: ушедшему
		// (или забаненному вердиктом/командой) кик за молчание не грозит.
		b.cancelReplyWait(upd.Chat.ID, user.ID)
	}
	return nil
}

// handleMyChatMember отслеживает собственное членство бота в чатах. При
// уходе (добровольном или кике) чат выбрасывается из реестра, а его активные
// капчи отменяются — иначе их таймауты стреляли бы kick/ban-вызовами в чате,
// где бота уже нет. Историческая статистика остаётся как архив. При
// добавлении/повышении — регистрирует чат и говорит админам, каких прав не
// хватает, вместо молчаливых отказов потом.
func (b *Bot) handleMyChatMember(ctx *th.Context, update telego.Update) error {
	upd := update.MyChatMember
	if upd == nil {
		return nil
	}
	oldStatus := upd.OldChatMember.MemberStatus()
	newStatus := upd.NewChatMember.MemberStatus()
	b.log.Info("my_chat_member event",
		"chat", upd.Chat.ID,
		"chat_type", upd.Chat.Type,
		"old", oldStatus, "new", newStatus)

	if newStatus == "left" || newStatus == "kicked" {
		b.dropChat(b.runCtx, upd.Chat.ID, "bot left/kicked")
		return nil
	}

	if upd.Chat.Type != "group" && upd.Chat.Type != "supergroup" {
		return nil
	}
	if !b.chatAllowed(upd.Chat.ID) {
		b.log.Info("chat not in ALLOWED_CHATS, ignoring",
			"chat", upd.Chat.ID, "title", upd.Chat.Title)
		return nil
	}
	b.rememberChat(b.runCtx, storage.ChatInfo{
		ChatID:   upd.Chat.ID,
		Title:    upd.Chat.Title,
		Type:     upd.Chat.Type,
		Username: upd.Chat.Username,
	})
	b.checkAdminRights(upd)
	return nil
}

// dropChat убирает чат из реестра DM-меню и отменяет его активные капчи.
// Историческая статистика остаётся как архив. Выселить чат из
// write-through-кэша обязательно: иначе позднейший rememberChat с
// неизменившимся названием пропустит запись в БД и чат никогда не вернётся
// в реестр.
func (b *Bot) dropChat(ctx context.Context, chatID int64, why string) {
	b.log.Info("dropping chat from registry", "chat", chatID, "reason", why)
	for _, p := range b.store.TakeChat(chatID) {
		p.Cancel()
	}
	if err := b.db.DeletePendingChat(ctx, chatID); err != nil {
		b.log.Warn("delete pending captchas", "err", err, "chat", chatID)
	}
	for _, p := range b.replies.TakeChat(chatID) {
		p.Cancel()
	}
	if err := b.db.DeletePendingRepliesChat(ctx, chatID); err != nil {
		b.log.Warn("delete pending replies", "err", err, "chat", chatID)
	}
	if err := b.db.DeleteChat(ctx, chatID); err != nil {
		b.log.Warn("delete chat", "err", err, "chat", chatID)
	}
	b.cacheMu.Lock()
	delete(b.chatCache, chatID)
	b.cacheMu.Unlock()
}

// reconcileChats один раз на старте прочёсывает реестр чатов и выбрасывает
// строки чатов, где бота на самом деле нет. Строки переживают членство,
// когда BOT_TOKEN переключили на другого бота (чаты старого остаются в общей
// БД) или когда бота кикнули, пока он лежал — my_chat_member в обоих случаях
// не приходит, и DM-меню вечно показывало бы мёртвые чаты.
func (b *Bot) reconcileChats(ctx context.Context) {
	chats, err := b.db.ListChats(ctx)
	if err != nil {
		b.log.Warn("reconcile chats: list", "err", err)
		return
	}
	for _, c := range chats {
		if !b.chatAllowed(c.ChatID) {
			b.dropChat(ctx, c.ChatID, "not in ALLOWED_CHATS")
			continue
		}
		m, err := b.api.GetChatMember(ctx, &telego.GetChatMemberParams{
			ChatID: tu.ID(c.ChatID),
			UserID: b.me.ID,
		})
		if reason, stale := staleChatReason(m, err); stale {
			b.dropChat(ctx, c.ChatID, reason)
		} else if err != nil {
			// Транзиентное (сеть, 429, 5xx): строку оставляем, следующий
			// рестарт попробует снова.
			b.log.Warn("reconcile chats: check membership", "err", err, "chat", c.ChatID)
		}
	}
}

// staleChatReason решает, доказывает ли результат getChatMember(self), что
// бота в чате нет. Telegram отвечает 400 «chat not found» на чаты, которых
// этот бот никогда не видел, и 403 «bot was kicked»/«not a member» на
// потерянное членство — оба ответа окончательны. Всё остальное строку
// выбрасывать НЕ должно.
func staleChatReason(m telego.ChatMember, err error) (string, bool) {
	if err != nil {
		var apiErr *telegoapi.Error
		if errors.As(err, &apiErr) && (apiErr.ErrorCode == 400 || apiErr.ErrorCode == 403) {
			return apiErr.Description, true
		}
		return "", false
	}
	if s := m.MemberStatus(); s == "left" || s == "kicked" {
		return "status " + s, true
	}
	return "", false
}

// checkAdminRights постит в чат подсказку по настройке, когда бота добавили
// без нужных прав (restrict + delete), и подтверждение, когда недостающие
// права выдали. Молчит, если с самого начала всё в порядке.
func (b *Bot) checkAdminRights(upd *telego.ChatMemberUpdated) {
	missing := missingRights(upd.NewChatMember)
	if len(missing) > 0 {
		text := "⚠️ Мне не хватает прав, капча работать не будет.\nВыдай мне: " +
			strings.Join(missing, ", ") + "."
		if _, err := b.api.SendMessage(b.runCtx,
			tu.Message(tu.ID(upd.Chat.ID), text)); err != nil {
			b.log.Warn("send missing-rights hint", "err", err, "chat", upd.Chat.ID)
		}
		return
	}
	// Подтверждаем только переход ИЗ сломанного состояния — не на каждое
	// постороннее повышение/изменение прав.
	if len(missingRights(upd.OldChatMember)) > 0 {
		if _, err := b.api.SendMessage(b.runCtx,
			tu.Message(tu.ID(upd.Chat.ID), "✅ Все нужные права на месте — я работаю.")); err != nil {
			b.log.Warn("send rights-ok confirmation", "err", err, "chat", upd.Chat.ID)
		}
	}
}

// missingRights перечисляет человекочитаемые админ-права, которых боту не
// хватает для капчи. Обычному участнику не хватает всего; администратору
// могут не выдать отдельные тумблеры.
func missingRights(m telego.ChatMember) []string {
	switch v := m.(type) {
	case *telego.ChatMemberAdministrator:
		var missing []string
		if !v.CanRestrictMembers {
			missing = append(missing, "«Блокировка пользователей»")
		}
		if !v.CanDeleteMessages {
			missing = append(missing, "«Удаление сообщений»")
		}
		return missing
	case *telego.ChatMemberOwner:
		return nil
	case *telego.ChatMemberMember:
		return []string{"права администратора («Блокировка пользователей», «Удаление сообщений»)"}
	default:
		// Состояния restricted/left/banned обрабатываются в других местах.
		return nil
	}
}

// onUserJoined — общий kickoff для chat_member-событий и сервис-сообщений
// message.new_chat_members. Безопасен при повторных вызовах для одного
// юзера: startCaptcha дедупит через in-memory store, а событие join пишет
// только тот вызов, который реально запустил капчу, — вход, доставленный
// обоими типами апдейтов, попадает в статистику один раз. threadID — топик
// форума, где замечен вход (0 = нет/General).
func (b *Bot) onUserJoined(chat telego.Chat, user telego.User, threadID int) {
	chatID := chat.ID
	b.rememberChat(b.runCtx, storage.ChatInfo{
		ChatID:   chatID,
		Title:    chat.Title,
		Type:     chat.Type,
		Username: chat.Username,
	})
	// Доверенный (/whitelist): вход без капчи, reply-ожидания и профиль-чека.
	// Проверка ДО глобальной базы спамеров — пер-чатовое доверие админа
	// перекрывает её в этом чате. Ошибка чтения — обычная капча (fail-safe).
	if trusted, err := b.db.IsTrusted(b.runCtx, chatID, user.ID); err == nil && trusted {
		// Дедуп дубль-доставки (chat_member + new_chat_members) — по свежему
		// joined_at от первой доставки. Минутный kickoff-замок, как у
		// banKnownSpammer, здесь нельзя: IsCaptchaActive всё это время удалял
		// бы сообщения только что впущенного. Побочный эффект: повторный вход
		// в течение минуты обходится без второго приветствия и событий — ок.
		if joinedAt, ok, jerr := b.db.MemberJoinedAt(b.runCtx, chatID, user.ID); jerr == nil && ok &&
			time.Since(joinedAt) < time.Minute {
			return
		}
		if !b.store.BeginKickoff(chatID, user.ID) {
			return
		}
		b.goSafe("admitTrusted", func() {
			// Замок снимается сразу после записи joined_at (дедуп-маркер выше
			// уже перехватывает дубли) и ДО отправки приветствия — окно, где
			// IsCaptchaActive удаляет сообщения юзера, сжимается до пары
			// DB-записей. Defer — страховка от паники до штатного снятия.
			released := false
			defer func() {
				if !released {
					b.store.FinishKickoff(chatID, user.ID)
				}
			}()
			// Имя в кэш до приветствия — путь сообщений его ещё не заполнял.
			b.rememberUser(b.runCtx, storage.UserInfo{
				UserID:    user.ID,
				FirstName: user.FirstName,
				LastName:  user.LastName,
				Username:  user.Username,
			})
			_ = b.db.RecordEvent(b.runCtx, chatID, user.ID, storage.EventJoin, time.Now(), "")
			_ = b.db.RecordEvent(b.runCtx, chatID, user.ID, storage.EventPass, time.Now(), "")
			if err := b.db.UpsertMember(b.runCtx, chatID, user.ID, time.Now()); err != nil {
				b.log.Warn("upsert trusted member", "err", err)
			}
			b.store.FinishKickoff(chatID, user.ID)
			released = true
			b.log.Info("trusted user admitted without captcha", "chat", chatID, "user", user.ID)
			b.maybeSendGreeting(b.runCtx, b.chatSettings(b.runCtx, chatID), chatID, user.ID, threadID)
		})
		return
	}
	// Известный спамер (вердикт «спам» в любом чате бота): мгновенный бан
	// вместо капчи. BeginKickoff гасит дубль-доставку chat_member +
	// new_chat_members; в горутине — banRevoke с ретраями не должен
	// блокировать обработку апдейтов.
	if banned, err := b.db.IsSpamBanned(b.runCtx, user.ID); err == nil && banned {
		if !b.store.BeginKickoff(chatID, user.ID) {
			return
		}
		b.goSafe("banKnownSpammer", func() {
			// Замок снимается двумя штатными путями ниже (fail — сразу,
			// success — через минуту в AfterFunc). Паника между ними раньше
			// роняла процесс и рестарт чистил in-memory карту; с recover в
			// goSafe не снятый замок жил бы вечно — BeginKickoff навсегда
			// false, и этот спамер в этом чате не получал бы ни бана, ни
			// капчи. Defer-страховка снимает его, если владение не передано.
			handedOff := false
			defer func() {
				if !handedOff {
					b.store.FinishKickoff(chatID, user.ID)
				}
			}()
			if err := b.banRevoke(b.runCtx, chatID, user.ID); err != nil {
				// Бан не прошёл (обычно нет прав) — не оставляем спамера
				// без присмотра: обычная капча, как до этой фичи.
				b.log.Warn("ban known spammer on join", "err", err, "chat", chatID, "user", user.ID)
				b.store.FinishKickoff(chatID, user.ID)
				handedOff = true // снят вручную ДО startCaptcha — он берёт замок заново
				if b.startCaptcha(chatID, user, threadID) {
					_ = b.db.RecordEvent(b.runCtx, chatID, user.ID, storage.EventJoin, time.Now(), "")
				}
				return
			}
			b.log.Info("banned known spammer on join", "chat", chatID, "user", user.ID)
			_ = b.db.RecordEvent(b.runCtx, chatID, user.ID, storage.EventSpamBan, time.Now(), storage.ReasonGlobal)
			b.notifyModAction(chatID, user.ID, storage.EventSpamBan, storage.ReasonGlobal)
			// Замок держим ещё минуту: у капчи роль «маркера после kickoff»
			// играет Pending в store, у бана ничего не остаётся — а дубль
			// new_chat_members может прийти следующим poll'ом и задвоить
			// событие spamban в статистике.
			handedOff = true
			time.AfterFunc(time.Minute, func() { b.store.FinishKickoff(chatID, user.ID) })
		})
		return
	}
	if !b.startCaptcha(chatID, user, threadID) {
		// Дубль-доставка (chat_member + new_chat_members) — уже посчитано.
		return
	}
	if err := b.db.RecordEvent(b.runCtx, chatID, user.ID, storage.EventJoin, time.Now(), ""); err != nil {
		b.log.Warn("record join event", "err", err)
	}
}

func (b *Bot) handleCallback(ctx *th.Context, query telego.CallbackQuery) error {
	targetUserID, optIdx, ok := parseCallback(query.Data)
	if !ok {
		_ = b.api.AnswerCallbackQuery(ctx, tu.CallbackQuery(query.ID))
		return nil
	}
	if query.From.ID != targetUserID {
		_ = b.api.AnswerCallbackQuery(ctx,
			tu.CallbackQuery(query.ID).
				WithText("Эта капча не для тебя.").
				WithShowAlert())
		return nil
	}
	if query.Message == nil {
		return nil
	}

	chatID := query.Message.GetChat().ID
	p, ok := b.store.Take(chatID, query.From.ID)
	if !ok {
		_ = b.api.AnswerCallbackQuery(ctx,
			tu.CallbackQuery(query.ID).WithText("Время вышло."))
		return nil
	}
	p.Cancel()
	_ = b.db.DeletePending(b.runCtx, chatID, query.From.ID)

	capMsg := query.Message.Message()
	if capMsg != nil {
		// Callback приехал не на живом сообщении капчи (stale-сообщение
		// после неудавшегося delete или подделанный data) — его клавиатура
		// не наша, эмодзи из неё врали бы; остаёмся на номерах кнопок.
		// У эфемерной капчи свой id — обычные message_id там оба нулевые,
		// и сравнение по ним пропускало бы stale.
		stale := capMsg.MessageID != p.MessageID
		if p.EphemeralID != 0 {
			stale = capMsg.EphemeralMessageID != p.EphemeralID
		}
		if stale {
			capMsg = nil
		}
	}
	if optIdx == p.CorrectIdx {
		_ = b.api.AnswerCallbackQuery(ctx,
			tu.CallbackQuery(query.ID).WithText("Правильно, добро пожаловать!"))
		if err := b.onSuccess(b.runCtx, p, "выбрал "+buttonLabel(capMsg, optIdx)); err != nil {
			b.log.Error("on success", "err", err, "chat", chatID, "user", query.From.ID)
		}
	} else {
		_ = b.api.AnswerCallbackQuery(ctx,
			tu.CallbackQuery(query.ID).WithText("Неверно.").WithShowAlert())
		reason := "неверный ответ" + pickedVsCorrect(capMsg, optIdx, p.CorrectIdx)
		if err := b.onFail(b.runCtx, p, reason); err != nil {
			b.log.Error("on fail", "err", err, "chat", chatID, "user", query.From.ID)
		}
	}
	return nil
}

// handleApproveCallback обрабатывает кнопку «✅ Впустить» на клавиатуре капчи
// (callback data "capok:<userID>"). Админ чата или владелец бота может
// впустить застрявшего человека вручную — эффект тот же, что у правильного
// ответа.
func (b *Bot) handleApproveCallback(ctx *th.Context, query telego.CallbackQuery) error {
	if query.Message == nil {
		return nil
	}
	targetUserID, err := strconv.ParseInt(strings.TrimPrefix(query.Data, "capok:"), 10, 64)
	if err != nil {
		_ = b.api.AnswerCallbackQuery(ctx, tu.CallbackQuery(query.ID))
		return nil
	}
	chatID := query.Message.GetChat().ID

	if !b.canManageChat(ctx, query.From.ID, chatID) {
		_ = b.api.AnswerCallbackQuery(ctx,
			tu.CallbackQuery(query.ID).
				WithText("Эта кнопка только для админов чата.").
				WithShowAlert())
		return nil
	}
	p, ok := b.store.Take(chatID, targetUserID)
	if !ok {
		_ = b.api.AnswerCallbackQuery(ctx,
			tu.CallbackQuery(query.ID).WithText("Капча уже не активна."))
		return nil
	}
	p.Cancel()
	_ = b.db.DeletePending(b.runCtx, chatID, targetUserID)
	_ = b.api.AnswerCallbackQuery(ctx,
		tu.CallbackQuery(query.ID).WithText("Пользователь впущен."))
	b.log.Info("captcha approved by admin",
		"chat", chatID, "user", targetUserID, "admin", query.From.ID)
	if err := b.onSuccess(b.runCtx, p, ""); err != nil {
		b.log.Error("on success (admin approve)", "err", err, "chat", chatID, "user", targetUserID)
	}
	return nil
}

// handleHelpCommand — /help: полный список ВСЕХ команд с пояснениями. В ЛС —
// helpText с кнопкой «Назад» (она отредактирует сообщение в главное меню —
// обычная editWithMenu-механика); в группе — прежняя всегда-эфемерная
// справка handleGroupHelpCommand.
func (b *Bot) handleHelpCommand(ctx *th.Context, message telego.Message) error {
	if message.Chat.Type != "private" {
		return b.handleGroupHelpCommand(ctx, message)
	}
	_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(message.Chat.ID), helpText).
		WithParseMode(telego.ModeHTML).
		WithReplyMarkup(backKeyboard()))
	return nil
}

func (b *Bot) handlePrivateStart(ctx *th.Context, message telego.Message) error {
	if message.Chat.Type != "private" {
		// /start в группе молчит; /help живёт в своём handleHelpCommand.
		return nil
	}

	userID := int64(0)
	if message.From != nil {
		userID = message.From.ID
	}

	text := b.mainMenuText(userID)
	if message.From != nil {
		text += fmt.Sprintf("\n\n<i>Твой Telegram ID: <code>%d</code></i>", message.From.ID)
	}

	_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(message.Chat.ID), text).
		WithParseMode(telego.ModeHTML).
		WithReplyMarkup(b.mainMenuKeyboard(userID)))
	return nil
}

func (b *Bot) handleGroupMessage(ctx *th.Context, message telego.Message) error {
	if message.Chat.Type != "group" && message.Chat.Type != "supergroup" {
		return nil
	}

	// Сервис-сообщение: basic-группа апгрейднулась до супергруппы. Telegram
	// шлёт MigrateToChatID в старую группу и MigrateFromChatID в новую;
	// обрабатываем оба для подстраховки. MigrateChat идемпотентен — двойное
	// срабатывание безвредно.
	if message.MigrateToChatID != 0 {
		oldID := message.Chat.ID
		newID := message.MigrateToChatID
		b.log.Info("chat migrating to supergroup", "old", oldID, "new", newID)
		if err := b.db.MigrateChat(b.runCtx, oldID, newID); err != nil {
			b.log.Error("migrate chat data", "err", err, "old", oldID, "new", newID)
		}
		return nil
	}
	if message.MigrateFromChatID != 0 {
		oldID := message.MigrateFromChatID
		newID := message.Chat.ID
		b.log.Info("chat migrated from basic group", "old", oldID, "new", newID)
		if err := b.db.MigrateChat(b.runCtx, oldID, newID); err != nil {
			b.log.Error("migrate chat data", "err", err, "old", oldID, "new", newID)
		}
		return nil
	}

	// Всё дальше — обслуживание чата: реестр, капчи по сервис-сообщениям,
	// статистика, анонсы, спам-чек. Посторонние чаты (вне ALLOWED_CHATS) не
	// обслуживаем — иначе первое же сообщение заносит чат в реестр и открывает
	// его админам DM-меню (вплоть до включения ИИ-антиспама за счёт владельца).
	// Ветки миграции выше гейта: MigrateFromChatID приходит уже с НОВЫМ
	// chat_id, которого в ALLOWED_CHATS ещё нет.
	if !b.chatAllowed(message.Chat.ID) {
		return nil
	}

	// Сервис-сообщение: вошли новые участники. Это фолбэк для случаев, когда
	// Telegram не шлёт chat_member-апдейт (некоторые типы групп, некоторые
	// сценарии повторного входа). startCaptcha дедупит через in-memory store,
	// так что даже если chat_member тоже придёт для того же юзера, капча
	// будет одна.
	if len(message.NewChatMembers) > 0 {
		// В форум-супергруппах сервис-сообщение о входе падает в топик; шлём
		// капчу туда же, чтобы юзер её реально увидел.
		threadID := 0
		if message.IsTopicMessage {
			threadID = message.MessageThreadID
		}
		hadHuman := false
		for _, nm := range message.NewChatMembers {
			if nm.IsBot {
				continue
			}
			if b.me != nil && nm.ID == b.me.ID {
				continue
			}
			hadHuman = true
			b.log.Info("new_chat_members service message",
				"chat", message.Chat.ID, "user", nm.ID)
			b.onUserJoined(message.Chat, nm, threadID)
		}
		// Сносим телеграмное «X вошёл в чат» — засоряет чат, а капчу мы уже
		// показываем.
		if hadHuman {
			if err := b.deleteMessage(b.runCtx, message.Chat.ID, message.MessageID); err != nil {
				b.log.Warn("delete join service message",
					"err", err, "chat", message.Chat.ID, "msg", message.MessageID)
			}
		}
		return nil
	}

	// Сервис-сообщение: участник вышел или был кикнут. Удаляем (по той же
	// причине, что и new_chat_members — спам «бот исключил X» / «X вышел»).
	if message.LeftChatMember != nil {
		if err := b.deleteMessage(b.runCtx, message.Chat.ID, message.MessageID); err != nil {
			b.log.Warn("delete leave service message",
				"err", err, "chat", message.Chat.ID, "msg", message.MessageID)
		}
		return nil
	}

	if message.From == nil || message.From.IsBot {
		return nil
	}
	// Автофорварды из привязанного канала приходят от сервис-юзера 777000
	// («Telegram», is_bot=false) — без этого фильтра редко постящий канал
	// зарабатывал бы анонсы «молчаливого возвращенца» и засорял топ
	// писателей.
	if message.From.ID == telegramServiceUserID || message.IsAutomaticForward {
		return nil
	}
	// Пропускаем прочие сервис-сообщения (смена названия, пины и т.п.).
	if message.NewChatTitle != "" || message.NewChatPhoto != nil ||
		message.PinnedMessage != nil {
		return nil
	}

	// Юзер в окне задержки перед капчей (или у него активная капча, а
	// сообщение как-то проскочило мимо рестрикта): удаляем всё, что он
	// написал. К статистике/детектору тишины эти сообщения не идут.
	if b.store.IsCaptchaActive(message.Chat.ID, message.From.ID) {
		if err := b.deleteMessage(b.runCtx, message.Chat.ID, message.MessageID); err != nil {
			b.log.Warn("delete pre-captcha message",
				"err", err, "chat", message.Chat.ID, "user", message.From.ID)
		}
		return nil
	}

	chatID := message.Chat.ID
	user := *message.From
	when := time.Unix(int64(message.Date), 0)

	b.rememberChat(b.runCtx, storage.ChatInfo{
		ChatID:   chatID,
		Title:    message.Chat.Title,
		Type:     message.Chat.Type,
		Username: message.Chat.Username,
	})
	b.rememberUser(b.runCtx, storage.UserInfo{
		UserID:    user.ID,
		FirstName: user.FirstName,
		LastName:  user.LastName,
		Username:  user.Username,
	})

	newcomer := b.isNewcomer(b.runCtx, chatID, user.ID, when)
	if err := b.db.IncMessage(b.runCtx, chatID, when, newcomer); err != nil {
		b.log.Warn("inc message", "err", err)
	}

	rec, err := b.db.RecordMessage(b.runCtx, chatID, user.ID, when)
	if err != nil {
		b.log.Warn("record message", "err", err)
		return nil
	}
	b.maybeAnnounceReturn(ctx, message, user, rec)
	b.maybeSpamCheck(message)
	return nil
}

// handleEditedGroupMessage — правки сообщений. Единственная задача — прогнать
// отредактированный текст через ИИ-антиспам: «невинное сообщение → правка в
// спам» иначе полностью обходит проверку (она видит только первые N сообщений
// автора). Счётчики и анонсы не трогаем — оригинал уже посчитан при отправке,
// иначе правки накручивали бы статистику и ускоряли выход в белый список.
func (b *Bot) handleEditedGroupMessage(ctx *th.Context, message telego.Message) error {
	if message.Chat.Type != "group" && message.Chat.Type != "supergroup" {
		return nil
	}
	if !b.chatAllowed(message.Chat.ID) {
		return nil
	}
	if message.From == nil || message.From.IsBot {
		return nil
	}
	if message.From.ID == telegramServiceUserID || message.IsAutomaticForward {
		return nil
	}
	// Live-локация шлёт edited_message каждые несколько секунд всю трансляцию —
	// это не «правка текста», жечь на неё LLM-запросы нельзя.
	if message.Location != nil {
		return nil
	}
	// Правка в спам мид-капча (сообщение проскочило в секунды до рестрикта):
	// удаляем, как handleGroupMessage удаляет оригиналы.
	if b.store.IsCaptchaActive(message.Chat.ID, message.From.ID) {
		if err := b.deleteMessage(b.runCtx, message.Chat.ID, message.MessageID); err != nil {
			b.log.Warn("delete edited pre-captcha message",
				"err", err, "chat", message.Chat.ID, "user", message.From.ID)
		}
		return nil
	}
	// Бюджет-предохранитель: правки не растят счётчик сообщений, поэтому
	// новичок (total ≤ whitelist) мог бы бесконечными правками одного
	// сообщения гонять LLM-запросы без всякого самоограничения — обычный
	// путь ограничивает себя сам ростом счётчика до белого списка. Не чаще
	// одной проверки правок на (chat, user) в editCheckCooldown; правка в
	// спам сразу после проверенной benign-правки проскочит окно — осознанный
	// трейд-офф против выжигания квоты.
	k := chatUser{message.Chat.ID, message.From.ID}
	b.spamMu.Lock()
	if _, busy := b.spamInflight[k]; busy {
		// Проверка этого юзера уже в полёте (секунды LLM-вызова): правку
		// пропускаем, но кулдаун НЕ жжём — иначе правка, попавшая в это
		// окно, блокировала бы перепроверку на весь editCheckCooldown.
		b.spamMu.Unlock()
		return nil
	}
	last, seen := b.editChecked[k]
	if seen && time.Since(last) < editCheckCooldown {
		b.spamMu.Unlock()
		return nil
	}
	b.editChecked[k] = time.Now()
	b.spamMu.Unlock()
	b.maybeSpamCheck(message)
	return nil
}

func (b *Bot) maybeAnnounceReturn(ctx *th.Context, message telego.Message, user telego.User, rec storage.MessageRecord) {
	if b.cfg.SilentAnnounceDays == 0 || !rec.HasBaseline {
		return
	}
	threshold := time.Duration(b.cfg.SilentAnnounceDays) * 24 * time.Hour
	if rec.Silence < threshold {
		return
	}
	// Пер-чатовый тумблер; проверяется ПОСЛЕ порога, чтобы запрос настроек
	// шёл только на редких анонсо-достойных сообщениях, а не на каждом.
	if !b.chatSettings(b.runCtx, message.Chat.ID).SilentAnnounceEnabled {
		return
	}
	days := int(rec.Silence / (24 * time.Hour))
	mention := mentionHTML(user)
	var text string
	switch {
	case rec.WasFirstMessage:
		text = fmt.Sprintf("🎉 Смотрите-ка! %s был(а) в чате <b>%s</b> и наконец-то впервые что-то пишет.",
			mention, humanDaysRU(days))
	case days >= 365:
		text = fmt.Sprintf("🎊 Сенсация! %s молчал(а) <b>%s</b> и вот наконец-то написал(а)!",
			mention, humanDaysRU(days))
	case days >= 90:
		text = fmt.Sprintf("👀 Ого! %s вернулся(ась) после <b>%s</b> тишины.",
			mention, humanDaysGenRU(days))
	default:
		text = fmt.Sprintf("✨ %s снова с нами после <b>%s</b> молчания.",
			mention, humanDaysGenRU(days))
	}
	params := tu.Message(tu.ID(message.Chat.ID), text).
		WithParseMode(telego.ModeHTML).
		WithReplyParameters(&telego.ReplyParameters{MessageID: message.MessageID})
	if message.IsTopicMessage {
		params = params.WithMessageThreadID(message.MessageThreadID)
	}
	_, err := b.api.SendMessage(ctx, params)
	if err != nil {
		b.log.Warn("announce return", "err", err, "chat", message.Chat.ID, "user", user.ID)
	}
}

func (b *Bot) isNewcomer(ctx context.Context, chatID, userID int64, when time.Time) bool {
	joinedAt, ok, err := b.db.MemberJoinedAt(ctx, chatID, userID)
	if err != nil {
		b.log.Warn("member joined_at", "err", err)
		return false
	}
	if !ok {
		// Участник, состоявший в чате ещё до добавления бота.
		return false
	}
	window := time.Duration(b.cfg.NewcomerDays) * 24 * time.Hour
	return when.Sub(joinedAt) < window
}

// startCaptcha отвечает, выиграл ли этот вызов kickoff и реально ли запустил
// капчу; false — капча для юзера уже активна или уже запускается (дубль
// доставки входа), и вызов был no-op.
func (b *Bot) startCaptcha(chatID int64, user telego.User, threadID int) bool {
	// Страховка от гонки: chat_member и message.new_chat_members могут
	// прийти на один и тот же вход. Без kickoff-замка они наперегонки
	// пробегают фазу до Put (restrict + отправка) и дают два сообщения капчи.
	if !b.store.BeginKickoff(chatID, user.ID) {
		b.log.Debug("captcha already in progress, skipping duplicate kickoff",
			"chat", chatID, "user", user.ID)
		return false
	}
	// Капча-флоу асинхронный: рестрикт сразу, потом сон CaptchaDelay перед
	// отправкой сообщения. Всё это окно handleGroupMessage удаляет любые
	// сообщения юзера (store.IsCaptchaActive true, пока держится inflight).
	b.goSafe("runCaptcha", func() { b.runCaptcha(chatID, user, threadID) })
	return true
}

func (b *Bot) runCaptcha(chatID int64, user telego.User, threadID int) {
	defer b.store.FinishKickoff(chatID, user.ID)

	ctx := b.runCtx

	// Кэшируем display-имя сейчас — оно понадобится приветствию после
	// успешного прохождения (к тому моменту юзер ещё ничего не писал, и
	// путь обработки сообщений user_info не заполнил бы).
	b.rememberUser(ctx, storage.UserInfo{
		UserID:    user.ID,
		FirstName: user.FirstName,
		LastName:  user.LastName,
		Username:  user.Username,
	})

	// Restrict ПЕРВЫМ — каждая секунда до этого вызова открытое окно для
	// спам-ботов «вошёл-и-запостил»: сообщение-то удалится, но
	// push-уведомления уже разлетелись. Сам рестрикт юзеру не виден, поэтому
	// задержка на отрисовку (ниже) ему не нужна.
	if err := b.restrict(ctx, chatID, user.ID); err != nil {
		b.log.Error("restrict", "err", err, "chat", chatID, "user", user.ID)
		return
	}

	// Теперь даём клиенту юзера время полностью открыть чат. Без этого
	// сообщение капчи иногда не подклеивается в уже отрисованную ленту, и
	// юзер видит его только после повторного открытия чата.
	if b.cfg.CaptchaDelay > 0 {
		select {
		case <-ctx.Done():
			// Shutdown в окне задержки: рестрикт уже применён, а pending ещё
			// не записан — рестарт юзера не восстановит. Снимаем мут.
			b.releaseOnAbort(ctx, chatID, user.ID)
			return
		case <-time.After(b.cfg.CaptchaDelay):
		}
	}

	settings := b.chatSettings(ctx, chatID)
	mode := effectiveCaptchaMode(settings)
	ch := captcha.New(mode)
	captchaTimeout := b.effectiveCaptchaTimeout(settings)
	correct := ch.Correct()

	// Режим картинки: фото рендерим заранее. Любая ошибка рендера — фолбэк
	// на текстовый промпт: капча обязана уйти всегда.
	var photo []byte
	if mode == captcha.ModeImage {
		var rerr error
		photo, rerr = captcha.RenderImage(correct)
		if rerr != nil {
			b.log.Warn("render image captcha, falling back to text",
				"err", rerr, "emoji", correct.Emoji)
		}
	}

	// Эфемерный режим: капча видна только вступившему (и боту). Ряд
	// админ-аппрува в нём бессмысленен — админы сообщения не видят.
	ephemeral := settings.EphemeralEnabled
	if ephemeral {
		// Со второй попытки — публично: эфемерка могла не доставиться
		// оффлайн-юзеру (известный трейд-офф режима), а публичная капча несёт
		// и ряд «Впустить». Ошибка чтения счётчика — тоже публично:
		// гарантированная доставка важнее тишины.
		if n, aerr := b.db.AttemptCount(ctx, chatID, user.ID, attemptsTTL); aerr != nil || n >= 1 {
			ephemeral = false
		}
	}

	// Контракт раскладки: ряд 0 — варианты в порядке индексов, ряд 1 — кнопка
	// админа. buttonLabel читает row 0 по этому контракту — при перестановке
	// рядов обнови и его.
	buttons := make([]telego.InlineKeyboardButton, 0, len(ch.Options))
	for i, c := range ch.Options {
		buttons = append(buttons,
			tu.InlineKeyboardButton(c.Emoji).
				WithCallbackData(fmt.Sprintf("cap:%d:%d", user.ID, i)))
	}
	rows := [][]telego.InlineKeyboardButton{tu.InlineKeyboardRow(buttons...)}
	if !ephemeral {
		rows = append(rows, tu.InlineKeyboardRow(
			tu.InlineKeyboardButton("✅ Впустить (для админов)").
				WithCallbackData(fmt.Sprintf("capok:%d", user.ID))))
	}
	kb := tu.InlineKeyboard(rows...)

	// Отправка ретраится: 429 прилетает ровно во время масс-джойна, а
	// single-shot фейл здесь release'ил юзера БЕЗ капчи — щит отключался
	// как раз под флудом.
	var msg *telego.Message
	var err error
	if photo != nil {
		caption := fmt.Sprintf(
			"Привет, %s!\nДля защиты от спама выбери эмодзи, наиболее похожую на картинку, за %d секунд.",
			mentionHTML(user), int(captchaTimeout.Seconds()),
		)
		err = retryTG(ctx, func() error {
			// Params пересоздаются на каждую попытку: bytes.Reader одноразовый,
			// повторная отправка того же объекта ушла бы с пустым телом.
			p := tu.Photo(tu.ID(chatID),
				tu.File(tu.NameReader(bytes.NewReader(photo), "captcha.png"))).
				WithCaption(caption).
				WithParseMode(telego.ModeHTML).
				WithReplyMarkup(kb)
			if threadID != 0 {
				p = p.WithMessageThreadID(threadID)
			}
			if ephemeral {
				p = p.WithReceiverUserID(user.ID)
			}
			var e error
			msg, e = b.api.SendPhoto(ctx, p)
			return e
		})
	} else {
		text := fmt.Sprintf(
			"Привет, %s!\nДля защиты от спама выбери <b>%s</b> за %d секунд.",
			mentionHTML(user), correct.Prompt, int(captchaTimeout.Seconds()),
		)
		params := tu.Message(tu.ID(chatID), text).
			WithParseMode(telego.ModeHTML).
			WithReplyMarkup(kb)
		if threadID != 0 {
			params = params.WithMessageThreadID(threadID)
		}
		if ephemeral {
			params = params.WithReceiverUserID(user.ID)
		}
		err = retryTG(ctx, func() error {
			var e error
			msg, e = b.api.SendMessage(ctx, params)
			return e
		})
	}
	if err != nil {
		b.log.Error("send captcha", "err", err, "chat", chatID, "user", user.ID)
		// releaseOnAbort: при живом ctx (сетевой фейл) — полный бюджет ретраев,
		// при отменённом (shutdown) — detached, иначе мут не снялся бы.
		b.releaseOnAbort(ctx, chatID, user.ID)
		return
	}

	expires := time.Now().Add(captchaTimeout)
	p := b.store.Put(chatID, user.ID, msg.MessageID, ch.CorrectIdx, expires, threadID, msg.EphemeralMessageID)

	if err := b.db.PutPending(ctx, storage.PendingRow{
		ChatID:      chatID,
		UserID:      user.ID,
		MessageID:   msg.MessageID,
		CorrectIdx:  ch.CorrectIdx,
		ExpiresAt:   expires,
		ThreadID:    threadID,
		EphemeralID: msg.EphemeralMessageID,
	}); err != nil {
		// Третий pre-persist обрыв (после shutdown-в-задержке и фейла
		// отправки): капча только в памяти не переживёт рестарт — юзер
		// остался бы замьючен навсегда, причём сбой БД как раз коррелирует
		// со скорым рестартом. Fail-open, как и при неудачной отправке:
		// снимаем капчу и впускаем. Take с проверкой — юзер мог успеть
		// ответить за эти миллисекунды, тогда исход уже решён без нас.
		b.log.Warn("persist pending — dropping captcha, letting user in (fail-open)",
			"err", err, "chat", chatID, "user", user.ID)
		if taken, ok := b.store.Take(chatID, user.ID); ok && taken == p {
			taken.Cancel()
			if derr := b.deleteBotMessage(ctx, chatID, msg.MessageID, msg.EphemeralMessageID, user.ID); derr != nil {
				b.log.Warn("delete captcha after persist failure",
					"err", derr, "chat", chatID, "msg", msg.MessageID)
			}
			b.releaseOnAbort(ctx, chatID, user.ID)
		}
		return
	}

	b.goSafe("waitTimeout", func() { b.waitTimeout(p) })
}

func (b *Bot) waitTimeout(p *captcha.Pending) {
	timer := time.NewTimer(time.Until(p.ExpiresAt))
	defer timer.Stop()

	select {
	case <-p.Done():
		return
	case <-b.runCtx.Done():
		return
	case <-timer.C:
	}

	existing, ok := b.store.Take(p.ChatID, p.UserID)
	if !ok || existing != p {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = b.db.DeletePending(cleanupCtx, p.ChatID, p.UserID)
	if err := b.onFail(cleanupCtx, p, "таймаут"); err != nil {
		b.log.Error("on fail timeout", "err", err, "chat", p.ChatID, "user", p.UserID)
	}
}

// onSuccess завершает капчу победой. answer — «выбрал N-й (эмодзи)» с кнопки,
// которую нажал юзер; пустая строка на admin-approve пути (выбора не было —
// уведомление владельцам пропускается, лог-поле answer остаётся пустым).
func (b *Bot) onSuccess(ctx context.Context, p *captcha.Pending, answer string) error {
	_ = b.db.ResetAttempts(ctx, p.ChatID, p.UserID)
	if err := b.db.UpsertMember(ctx, p.ChatID, p.UserID, time.Now()); err != nil {
		b.log.Warn("upsert member", "err", err)
	}
	if err := b.db.RecordEvent(ctx, p.ChatID, p.UserID, storage.EventPass, time.Now(), ""); err != nil {
		b.log.Warn("record pass event", "err", err)
	}
	b.log.Info("captcha passed", "chat", p.ChatID, "user", p.UserID, "answer", answer)
	if answer != "" {
		b.notifyModAction(p.ChatID, p.UserID, storage.EventPass, "", answer)
	}
	if err := b.deleteBotMessage(ctx, p.ChatID, p.MessageID, p.EphemeralID, p.UserID); err != nil {
		b.log.Warn("delete captcha on pass",
			"err", err, "chat", p.ChatID, "msg", p.MessageID)
	}
	if err := b.release(ctx, p.ChatID, p.UserID); err != nil {
		return err
	}
	s := b.chatSettings(ctx, p.ChatID)
	// Ожидание ответа взводим СРАЗУ после размьюта, до сетевой отправки
	// приветствия: юзер уже может писать, и его первое сообщение должно
	// застать ожидание активным (иначе гонка → кик написавшего).
	b.maybeArmReplyWait(s, p.ChatID, p.UserID)
	b.maybeSendGreeting(ctx, s, p.ChatID, p.UserID, p.ThreadID)
	// Затем — ИИ-оценка профиля новичка (асинхронная внутри).
	b.maybeProfileCheck(p.ChatID, p.UserID, p.ThreadID)
	return nil
}

func (b *Bot) onFail(ctx context.Context, p *captcha.Pending, reason string) error {
	count, err := b.db.IncrementAttempt(ctx, p.ChatID, p.UserID, attemptsTTL)
	if err != nil {
		b.log.Warn("increment attempt", "err", err)
		count = 1 // считаем первой попыткой и едем дальше
	}
	if err := b.deleteBotMessage(ctx, p.ChatID, p.MessageID, p.EphemeralID, p.UserID); err != nil {
		b.log.Warn("delete captcha on fail/timeout",
			"err", err, "chat", p.ChatID, "msg", p.MessageID, "reason", reason)
	}

	if count >= b.effectiveMaxAttempts(b.chatSettings(ctx, p.ChatID)) {
		b.log.Info("banning user", "chat", p.ChatID, "user", p.UserID, "reason", reason, "attempts", count)
		_ = b.db.RecordEvent(ctx, p.ChatID, p.UserID, storage.EventBan, time.Now(), storage.ReasonCaptcha)
		b.notifyCaptchaFail(p.ChatID, p.UserID, storage.EventBan, reason, count)
		return b.ban(ctx, p.ChatID, p.UserID)
	}
	b.log.Info("kicking user", "chat", p.ChatID, "user", p.UserID, "reason", reason, "attempts", count)
	_ = b.db.RecordEvent(ctx, p.ChatID, p.UserID, storage.EventKick, time.Now(), storage.ReasonCaptcha)
	b.notifyCaptchaFail(p.ChatID, p.UserID, storage.EventKick, reason, count)
	return b.kick(ctx, p.ChatID, p.UserID)
}

func (b *Bot) chatAllowed(chatID int64) bool {
	if b.cfg.AllowedChats == nil {
		return true
	}
	_, ok := b.cfg.AllowedChats[chatID]
	return ok
}

// buttonLabel — «N-й (эмодзи)» для кнопки idx клавиатуры капчи (row 0 —
// эмодзи-варианты). Если сообщение недоступно (inaccessible callback) или
// индекс не влезает в ряд — деградирует до номера кнопки (1-based).
func buttonLabel(msg *telego.Message, idx int) string {
	label := fmt.Sprintf("%d-й", idx+1)
	if msg == nil || msg.ReplyMarkup == nil || len(msg.ReplyMarkup.InlineKeyboard) == 0 {
		return label
	}
	row := msg.ReplyMarkup.InlineKeyboard[0]
	if idx < 0 || idx >= len(row) {
		return label
	}
	return label + " (" + row[idx].Text + ")"
}

// pickedVsCorrect строит суффикс причины «: выбрал N-й (X), верный M-й (Y)».
func pickedVsCorrect(msg *telego.Message, picked, correct int) string {
	return ": выбрал " + buttonLabel(msg, picked) + ", верный " + buttonLabel(msg, correct)
}

func parseCallback(data string) (userID int64, optIdx int, ok bool) {
	parts := strings.Split(data, ":")
	if len(parts) != 3 || parts[0] != "cap" {
		return 0, 0, false
	}
	uid, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	idx, err := strconv.Atoi(parts[2])
	if err != nil {
		return 0, 0, false
	}
	return uid, idx, true
}

func mentionHTML(u telego.User) string {
	name := strings.TrimSpace(u.FirstName + " " + u.LastName)
	if name == "" {
		name = fmt.Sprintf("id%d", u.ID)
	}
	return fmt.Sprintf(`<a href="tg://user?id=%d">%s</a>`, u.ID, html.EscapeString(name))
}
