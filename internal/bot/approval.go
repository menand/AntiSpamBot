package bot

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
	tu "github.com/mymmrac/telego/telegoutil"

	"github.com/menand/AntiSpamBot/internal/storage"
)

// Подтверждение чатов владельцем бота.
//
// Когда бота добавили в НОВЫЙ чат не владельцы (OWNER_IDS), чат помечается
// 'pending' и владельцам уходит ЛС с кнопками «Да, работать» / «Нет, выйти».
// До решения чат полностью инертен — никаких капч, счётчиков, приветствий и
// LLM-запросов (гейт chatServiceable на всех путях обслуживания). Чат,
// добавленный самим владельцем, апрувится его действием без переспроса.
// Существующие чаты (строки реестра, созданные до фичи) автоматически
// считаются approved миграцией DEFAULT 'approved'.
//
// Форматы callback data:
//
//	appr:y:<chatID>  — владелец включает чат
//	appr:n:<chatID>  — владелец отклоняет (бот выходит из чата)

// chatServiceable — гейт «обслуживать ли чат»: он в ALLOWED_CHATS И
// подтверждён владельцем. Единственное место, где этот фильтр НЕ применяется, —
// handleMyChatMember (входная точка самого подтверждения) и reconcileChats
// (иначе pending-строки не пережили бы рестарт).
func (b *Bot) chatServiceable(chatID int64) bool {
	return b.chatAllowed(chatID) && b.chatApproved(chatID)
}

// chatApproved отвечает, подтвердил ли владелец работу бота в чате. Кэш —
// паттерн adminCache: редкие записи, частые чтения (на каждом групповом
// сообщении). Кэшируются только существующие строки: незарегистрированный чат
// (новый, ещё в обработке handleMyChatMember) каждый раз перечитывается, и
// апрув, пришедший в любой момент, сразу подхватывается. Ошибка БД — fail
// closed: чат, чей статус не смогли прочитать, не обслуживаем (дешевле
// проигнорировать сообщение, чем потратить LLM-квоту на неодобренный чат).
func (b *Bot) chatApproved(chatID int64) bool {
	b.approvalMu.Lock()
	a, ok := b.approvalCache[chatID]
	b.approvalMu.Unlock()
	if ok {
		return a
	}
	status, exists, err := b.db.GetChatApproval(b.runCtx, chatID)
	if err != nil {
		b.log.Warn("get chat approval", "err", err, "chat", chatID)
		return false
	}
	if !exists {
		return false
	}
	approved := status == storage.ChatApproved
	b.setApprovalCache(chatID, approved)
	return approved
}

func (b *Bot) setApprovalCache(chatID int64, approved bool) {
	b.approvalMu.Lock()
	b.approvalCache[chatID] = approved
	b.approvalMu.Unlock()
}

func (b *Bot) delApprovalCache(chatID int64) {
	b.approvalMu.Lock()
	delete(b.approvalCache, chatID)
	b.approvalMu.Unlock()
}

// handleApprovalCallback — кнопки «Да/Нет» на ЛС-вопросе о новом чате. Решает
// только владелец бота. Решение атомарное (ClaimChatApproval): первое нажатие
// выигрывает, проигравший в гонке владелец получает правку своего ЛС под
// фактический статус. telego обрабатывает callback'и параллельно, поэтому
// гонка «Да»/«Нет» двух владельцев реальна и её нужно разрешать на уровне БД.
func (b *Bot) handleApprovalCallback(ctx *th.Context, query telego.CallbackQuery) error {
	_ = b.api.AnswerCallbackQuery(ctx, tu.CallbackQuery(query.ID))
	if !b.isOwner(query.From.ID) {
		return nil
	}
	approve, chatID, ok := parseApprovalCallback(query.Data)
	if !ok {
		return nil
	}
	status, exists, err := b.db.GetChatApproval(b.runCtx, chatID)
	if err != nil {
		b.log.Warn("approval callback: get status", "err", err, "chat", chatID)
		return nil
	}
	if !exists {
		// Бот вышел/был кикнут или чат перенесён в супергруппу: живого
		// вопроса по этому chat_id уже нет (см. carryApprovalOnMigrate).
		// Записывать статус заново нельзя — он оживил бы мёртвый чат.
		b.editApprovalMessage(ctx, query,
			"ℹ️ Чат больше не в реестре бота (бот вышел или перенесён в супергруппу).", nil)
		return nil
	}

	if approve {
		switch status {
		case storage.ChatApproved:
			b.editApprovalMessage(ctx, query, "✅ Бот уже включён в этом чате", nil)
			return nil
		case storage.ChatRejected:
			// Страховочный повторный апрув: LeaveChat при отклонении мог не
			// пройти (бот всё ещё участник), и владельцу нужен путь назад.
			// Условный UPDATE (ReapproveChat), а не SetChatApproval: если выход
			// всё же прошёл и строка удалена (dropChat), слепой upsert
			// воскресил бы чат, из которого бот уже вышел.
			reapproved, err := b.db.ReapproveChat(b.runCtx, chatID)
			if err != nil {
				b.log.Warn("re-approve rejected chat", "err", err, "chat", chatID)
				return nil
			}
			if !reapproved {
				// Гонка с dropChat: после чтения статуса строка исчезла
				// (бот вышел). Мёртвый чат не воскрешаем.
				b.editApprovalMessage(ctx, query,
					"ℹ️ Бот уже вышел из этого чата — включить его заново нельзя.", nil)
				return nil
			}
		default: // pending
			claimed, err := b.db.ClaimChatApproval(b.runCtx, chatID, storage.ChatApproved)
			if err != nil {
				b.log.Warn("claim approve", "err", err, "chat", chatID)
				return nil
			}
			if !claimed {
				b.loserOfApprovalRace(ctx, query, chatID)
				return nil
			}
		}
		b.setApprovalCache(chatID, true)
		b.postApprovedConfirmation(chatID)
		b.editApprovalMessage(ctx, query, "✅ Бот включён в этом чате", nil)
		return nil
	}

	switch status {
	case storage.ChatApproved:
		// Чат уже включён — поздним «Нет» активный чат не выключаем.
		b.editApprovalMessage(ctx, query, "✅ Чат уже включён другим владельцем", nil)
		return nil
	case storage.ChatRejected:
		b.editApprovalMessage(ctx, query, "🚫 Чат уже отклонён — бот вышел", nil)
		return nil
	default: // pending
		claimed, err := b.db.ClaimChatApproval(b.runCtx, chatID, storage.ChatRejected)
		if err != nil {
			b.log.Warn("claim reject", "err", err, "chat", chatID)
			return nil
		}
		if !claimed {
			b.loserOfApprovalRace(ctx, query, chatID)
			return nil
		}
	}
	b.setApprovalCache(chatID, false)
	b.leaveRejectedChat(chatID)
	b.editApprovalMessage(ctx, query, "🚫 Чат отклонён — бот выходит из чата", nil)
	return nil
}

// loserOfApprovalRace — владелец проиграл гонку решения (первый нажал кнопку
// раньше него). Перечитываем фактический статус, чтобы текст ЛС соответствовал
// реальности: в гонке «Да»/«Да» чат включён, в гонке «Нет»/«Нет» — отклонён.
func (b *Bot) loserOfApprovalRace(ctx *th.Context, query telego.CallbackQuery, chatID int64) {
	cur, exists, err := b.db.GetChatApproval(b.runCtx, chatID)
	if err != nil || !exists {
		b.editApprovalMessage(ctx, query, "ℹ️ Решение по чату уже принято", nil)
		return
	}
	switch cur {
	case storage.ChatApproved:
		b.editApprovalMessage(ctx, query, "✅ Чат уже включён другим владельцем", nil)
	case storage.ChatRejected:
		b.editApprovalMessage(ctx, query, "🚫 Чат уже отклонён другим владельцем", nil)
	default:
		b.editApprovalMessage(ctx, query, "ℹ️ Решение по чату ещё не принято", nil)
	}
}

// parseApprovalCallback разбирает appr:y:<chatID> / appr:n:<chatID>.
func parseApprovalCallback(data string) (approve bool, chatID int64, ok bool) {
	parts := strings.Split(data, ":")
	if len(parts) != 3 || parts[0] != "appr" {
		return false, 0, false
	}
	chatID, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return false, 0, false
	}
	switch parts[1] {
	case "y":
		return true, chatID, true
	case "n":
		return false, chatID, true
	}
	return false, 0, false
}

// editApprovalMessage правит текст ЛС-вопроса после решения и снимает кнопки.
// kb == nil означает «убрать клавиатуру» (пустой InlineKeyboard).
func (b *Bot) editApprovalMessage(ctx *th.Context, query telego.CallbackQuery, text string, kb *telego.InlineKeyboardMarkup) {
	if query.Message == nil {
		return
	}
	if kb == nil {
		kb = emptyKeyboard()
	}
	chat := query.Message.GetChat()
	_, err := b.api.EditMessageText(ctx, &telego.EditMessageTextParams{
		ChatID:      tu.ID(chat.ID),
		MessageID:   query.Message.GetMessageID(),
		Text:        text,
		ParseMode:   telego.ModeHTML,
		ReplyMarkup: kb,
	})
	if err != nil && !isNotModified(err) {
		b.log.Warn("edit approval message", "err", err, "chat", chat.ID)
	}
}

func emptyKeyboard() *telego.InlineKeyboardMarkup {
	return &telego.InlineKeyboardMarkup{InlineKeyboard: [][]telego.InlineKeyboardButton{}}
}

// postApprovedConfirmation постит в чат подтверждение, что бот заработал
// (включая подсказку о недостающих правах, если они не выданы).
func (b *Bot) postApprovedConfirmation(chatID int64) {
	b.log.Info("chat approved", "chat", chatID)
	text := "✅ Бот включён владельцем — начинаю работу."
	if hint := b.missingRightsHint(b.runCtx, chatID); hint != "" {
		text = "✅ Бот включён владельцем.\n⚠️ " + hint
	}
	if _, err := b.api.SendMessage(b.runCtx, tu.Message(tu.ID(chatID), text)); err != nil {
		b.log.Warn("send approved confirmation", "err", err, "chat", chatID)
	}
}

// leaveRejectedChat выводит бота из отклонённого чата. Пока выход не прошёл
// (или не удался), строка 'rejected' держит чат инертным; reconcileChats его
// не выкинет (бот — участник), так что случайного «простоял и ожил» нет.
func (b *Bot) leaveRejectedChat(chatID int64) {
	b.leaveChatAndCleanup(chatID, "owner rejected chat", nil)
}

// leaveChatAndCleanup — общий вывод бота из чата по воле владельца: после
// отклонения (leaveRejectedChat) или прямого запроса из меню (menu:leavec).
// Сначала отключаем ежедневную сводку — защита от призрачных рассылок после
// ухода (сводка смотрит на chat_settings напрямую и не заметила бы, что бота
// в чате больше нет). Затем в goroutine — LeaveChat через retryTG и, только
// после успеха, dropChat (реестр, кэши, активные капчи). Пока выход не прошёл,
// реестр не трогаем: строка держит чат известным и оставляет возможность
// повтора. onDone (может быть nil) вызывается с результатом — nil при успехе
// (dropChat уже сделан) или ошибкой LeaveChat.
func (b *Bot) leaveChatAndCleanup(chatID int64, why string, onDone func(error)) {
	if err := b.db.SetDailyStatsEnabled(b.runCtx, chatID, false); err != nil {
		b.log.Warn("disable daily stats on leave", "err", err, "chat", chatID)
	}
	b.goSafe("leaveChatAndCleanup", func() {
		// onDone обязан прозвучать при любом исходе — включая панику посреди
		// retryTG/dropChat: иначе защёлкнувшийся leaveInflight навсегда
		// обезоружил бы кнопку выхода (см. leaveChatByOwner).
		completed := false
		defer func() {
			if !completed && onDone != nil {
				onDone(fmt.Errorf("leaveChatAndCleanup panicked"))
			}
		}()
		if err := retryTG(b.runCtx, func() error {
			return b.api.LeaveChat(b.runCtx, &telego.LeaveChatParams{ChatID: tu.ID(chatID)})
		}); err != nil {
			b.log.Warn("leave chat", "err", err, "chat", chatID, "why", why)
			completed = true
			if onDone != nil {
				onDone(err)
			}
			return
		}
		// Событие my_chat_member(left) тоже вызовет dropChat; здесь чистим
		// сразу, чтобы строка реестра не пережила выход.
		b.dropChat(b.runCtx, chatID, why)
		completed = true
		if onDone != nil {
			onDone(nil)
		}
	})
}

// askOwnerApproval — новый чат, добавленный не владельцем: помечаем pending и
// спрашиваем владельцев ЛС.
func (b *Bot) askOwnerApproval(upd *telego.ChatMemberUpdated) {
	chatID := upd.Chat.ID
	if err := b.db.SetChatApproval(b.runCtx, chatID, storage.ChatPending); err != nil {
		b.log.Warn("mark chat pending", "err", err, "chat", chatID)
		// Выходим, не создавая строку реестра: rememberChat записал бы чат с
		// DEFAULT 'approved', и после рестарта (когда погаснет in-memory
		// approvalCache) чужой чат стал бы обслуживаемым навсегда, минуя
		// решение владельца. Без строки чат инертен, следующий my_chat_member
		// повторит запрос.
		return
	}
	b.setApprovalCache(chatID, false)
	info := storage.ChatInfo{
		ChatID:   chatID,
		Title:    upd.Chat.Title,
		Type:     upd.Chat.Type,
		Username: upd.Chat.Username,
	}
	b.rememberChat(b.runCtx, info)
	b.log.Info("chat awaiting owner approval",
		"chat", chatID, "title", upd.Chat.Title, "added_by", upd.From.ID)

	adderLabel := fmt.Sprintf("Кто добавил: %s (id%d)", mentionHTML(upd.From), upd.From.ID)
	configured, delivered := b.sendOwnerApprovalPrompt(b.runCtx, chatID, info, adderLabel)
	switch {
	case !configured:
		// OWNER_IDS пуст — владельцев нет, спрашивать не у кого; авто-апрув
		// (прежнее поведение), иначе чат завис бы инертным навсегда.
		b.log.Info("no owners configured, auto-approving chat", "chat", chatID)
		if err := b.db.SetChatApproval(b.runCtx, chatID, storage.ChatApproved); err != nil {
			b.log.Warn("auto-approve chat", "err", err, "chat", chatID)
			return
		}
		b.setApprovalCache(chatID, true)
		b.checkAdminRights(upd)
	case !delivered:
		// Владельцы настроены, но вопрос не доставлен никому (ни разу не
		// запускали бота в ЛС / закрыли ЛС): честно скажем об этом в чате —
		// иначе добавивший видит мёртвого бота без объяснений.
		b.log.Warn("owner approval prompt undelivered — notifying chat", "chat", chatID)
		if _, err := b.api.SendMessage(b.runCtx, tu.Message(tu.ID(chatID),
			"🤖 Жду подтверждения владельца: напиши мне в личку команду /start — "+
				"пришлю туда вопрос об этом чате. После подтверждения начну работать.").
			WithParseMode(telego.ModeHTML)); err != nil {
			b.log.Warn("send pending hint to chat", "err", err, "chat", chatID)
		}
	}
}

// sendOwnerApprovalPrompt рассылает владельцам ЛС-вопрос о чате.
// configured=false — владельцев не настроено (вопрос некому отправить);
// delivered — доставился ли вопрос хотя бы одному владельцу.
func (b *Bot) sendOwnerApprovalPrompt(ctx context.Context, chatID int64, info storage.ChatInfo, adderLine string) (configured, delivered bool) {
	if len(b.cfg.OwnerIDs) == 0 {
		return false, false
	}
	text := fmt.Sprintf(
		"🤖 <b>Бота добавили в новый чат</b>\n%s\n%s\n\nРаботать боту в этом чате?",
		chatLinkHTML(info), adderLine)
	kb := approvalKeyboard(chatID)
	for ownerID := range b.cfg.OwnerIDs {
		if _, err := b.api.SendMessage(ctx, tu.Message(tu.ID(ownerID), text).
			WithParseMode(telego.ModeHTML).
			WithLinkPreviewOptions(&telego.LinkPreviewOptions{IsDisabled: true}).
			WithReplyMarkup(kb)); err != nil {
			b.log.Warn("send approval request", "err", err, "owner", ownerID, "chat", chatID)
			continue
		}
		delivered = true
	}
	return true, delivered
}

func approvalKeyboard(chatID int64) *telego.InlineKeyboardMarkup {
	return &telego.InlineKeyboardMarkup{InlineKeyboard: [][]telego.InlineKeyboardButton{{
		tu.InlineKeyboardButton("✅ Да, работать").WithCallbackData(fmt.Sprintf("appr:y:%d", chatID)),
		tu.InlineKeyboardButton("🚫 Нет, выйти").WithCallbackData(fmt.Sprintf("appr:n:%d", chatID)),
	}}}
}

// missingRightsHint — подсказка о недостающих админ-правах для чата, где бота
// только что включили ("" — всё в порядке или статус не прочитать).
func (b *Bot) missingRightsHint(ctx context.Context, chatID int64) string {
	m, err := b.api.GetChatMember(ctx, &telego.GetChatMemberParams{
		ChatID: tu.ID(chatID),
		UserID: b.me.ID,
	})
	if err != nil {
		return ""
	}
	missing := missingRights(m)
	if len(missing) == 0 {
		return ""
	}
	return "Мне не хватает прав: " + strings.Join(missing, ", ") + "."
}

// carryApprovalOnMigrate переносит статус подтверждения на новый chat_id при
// апгрейде basic group → supergroup. MigrateChat удаляет старую строку
// реестра, поэтому статус читается ДО миграции. Без переноса мигрировавший
// approved-чат молча ушёл бы в ожидание подтверждения, а pending — потерял
// бы статус вовсе (старые кнопки указывали бы на мёртвый chat_id). Статус
// переносится КАК ЕСТЬ; pending при этом переспрашивается с новым chat_id.
func (b *Bot) carryApprovalOnMigrate(ctx context.Context, oldID, newID int64) {
	status, exists, err := b.db.GetChatApproval(ctx, oldID)
	if err != nil {
		b.log.Warn("carry chat approval: read", "err", err, "old", oldID)
		return
	}
	if !exists {
		return
	}
	info, _, err := b.db.GetChat(ctx, oldID)
	if err != nil {
		b.log.Warn("carry chat approval: read chat info", "err", err, "old", oldID)
	}
	if err := b.db.SetChatApproval(ctx, newID, status); err != nil {
		b.log.Warn("carry chat approval: write", "err", err, "new", newID)
		return
	}
	b.setApprovalCache(newID, status == storage.ChatApproved)
	b.delApprovalCache(oldID)
	if status == storage.ChatPending {
		// Кнопки старого вопроса указывают на мёртвый oldID — переспрашиваем
		// с новым. Без владельцев — авто-апрув, как при обычном добавлении;
		// с владельцами, но без доставки — та же подсказка в чате, что и при
		// добавлении (иначе мигрировавший pending-чат молча висит инертным).
		configured, delivered := b.sendOwnerApprovalPrompt(ctx, newID, info,
			"чат перенесён в новую супергруппу")
		switch {
		case !configured:
			if err := b.db.SetChatApproval(ctx, newID, storage.ChatApproved); err != nil {
				b.log.Warn("auto-approve migrated chat", "err", err, "chat", newID)
				return
			}
			b.setApprovalCache(newID, true)
		case !delivered:
			b.log.Warn("migrated approval prompt undelivered — notifying chat", "chat", newID)
			if _, err := b.api.SendMessage(ctx, tu.Message(tu.ID(newID),
				"🤖 Жду подтверждения владельца: напиши мне в личку команду /start — "+
					"пришлю туда вопрос об этом чате. После подтверждения начну работать.").
				WithParseMode(telego.ModeHTML)); err != nil {
				b.log.Warn("send pending hint to chat", "err", err, "chat", newID)
			}
		}
	}
}
