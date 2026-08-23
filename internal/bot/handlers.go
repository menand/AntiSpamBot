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

// groupAnonymousBotID — Telegram-бот, от имени которого пишут анонимные
// админы; его From в chat_member означает руку человека, не автоматику.
const groupAnonymousBotID int64 = 1087968824

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
	// Только подтверждённые владельцем чаты: в pending/rejected никаких капч,
	// прощений и отмен — чат полностью инертен до решения.
	if !b.chatServiceable(upd.Chat.ID) {
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
	// Прочие БОТЫ прощения не дарят: их автоматические циклы ban→unban
	// молча стирали бы глобальный флаг. Исключение — GroupAnonymousBot:
	// его From означает руку человека-анонимного админа.
	if oldStatus == "kicked" && newStatus != "kicked" &&
		upd.From.ID != user.ID && (b.me == nil || upd.From.ID != b.me.ID) &&
		(!upd.From.IsBot || upd.From.ID == groupAnonymousBotID) {
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
		// Свои события (fail-кик капчи, вердикт антиспама, captchaStageLoop,
		// кросс-бан banEverywhere в ДРУГОМ чате) уже записали своё — лево-ветка
		// не пишет поверх них НИЧЕГО. Но чистку состояния (pending/reply/таймер)
		// она выполняет и для своих: инициатор мог погасить капчу только в
		// своём чате (cancelCaptchaSilent), а сиротский таймаут в чужом чате
		// поднял бы кросс-бан через unban-половину kick'а.
		botOrigin := b.me != nil && upd.From.ID == b.me.ID
		if p, ok := b.store.Take(upd.Chat.ID, user.ID); ok {
			p.Cancel()
			if err := b.db.DeletePending(b.runCtx, upd.Chat.ID, user.ID); err != nil {
				b.log.Warn("delete pending of departed user", "err", err,
					"chat", upd.Chat.ID, "user", user.ID)
			}
			if err := b.deleteBotMessage(b.runCtx, upd.Chat.ID, p.MessageID, p.EphemeralID, p.UserID); err != nil {
				b.log.Debug("delete captcha after user left (already gone?)",
					"err", err, "chat", upd.Chat.ID, "msg", p.MessageID)
			}
			// Не пасс и не провал — отдельный счётчик, чтобы воронка в
			// статистике сходилась (иначе такие юзеры навсегда в «В процессе»).
			if !botOrigin {
				if err := b.db.RecordEvent(b.runCtx, upd.Chat.ID, user.ID, storage.EventLeft, time.Now(), ""); err != nil {
					b.log.Warn("record left event", "err", err)
				}
			}
			b.log.Info("captcha cancelled — user left mid-captcha",
				"chat", upd.Chat.ID, "user", user.ID, "bot_origin", botOrigin)
		}
		// Ожидание «ответь на приветствие» тоже снимается тихо: ушедшему
		// (или забаненному вердиктом/командой) кик за молчание не грозит.
		// Любой ЧУЖОЙ уход (left или kicked) закрывает воронку событием:
		// при reply-check «прошёл» ещё не записан, и без left юзер навсегда
		// висел бы в «В процессе» — ручной кик админа из клиента не пишет
		// ничего сам. Свои события не дублируем: бот-инициаторы (бот-кик,
		// replyWaitLoop, /kick, /ban, вердикт антиспама) уже записали
		// своё (botOrigin), а их Take снимает ожидание до нашей ветки.
		if b.cancelReplyWait(upd.Chat.ID, user.ID) && !botOrigin {
			if err := b.db.RecordEvent(b.runCtx, upd.Chat.ID, user.ID, storage.EventLeft, time.Now(), ""); err != nil {
				b.log.Warn("record left event (reply wait)", "err", err)
			}
		}
	}
	return nil
}

// handleMyChatMember отслеживает собственное членство бота в чатах. При
// уходе (добровольном или кике) чат выбрасывается из реестра, а его активные
// капчи отменяются — иначе их таймауты стреляли бы kick/ban-вызовами в чате,
// где бота уже нет. Историческая статистика остаётся как архив. При
// добавлении/повышении — регистрирует чат и говорит админам, каких прав не
// хватает, вместо молчаливых отказов потом. НОВЫЙ чат (нет строки в реестре),
// добавленный НЕ владельцем, помечается pending и ждёт решения владельца:
// до него бот в чате инертен (см. approval.go).
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
	info := storage.ChatInfo{
		ChatID:   upd.Chat.ID,
		Title:    upd.Chat.Title,
		Type:     upd.Chat.Type,
		Username: upd.Chat.Username,
	}
	status, exists, err := b.db.GetChatApproval(b.runCtx, upd.Chat.ID)
	if err != nil {
		// Fail-closed: чат, чей статус не смогли прочитать, не обслуживаем и
		// не спрашиваем повторно — строка останется прежней, следующий
		// my_chat_member попробует снова.
		b.log.Warn("get chat approval", "err", err, "chat", upd.Chat.ID)
		return nil
	}
	switch {
	case !exists:
		// Новый чат. Добавил сам владелец — его действие и есть согласие:
		// апрувим без переспроса. Иначе — спрашиваем владельцев ЛС.
		if b.isOwner(upd.From.ID) {
			b.log.Info("chat added by owner, auto-approved", "chat", upd.Chat.ID)
			if err := b.db.SetChatApproval(b.runCtx, upd.Chat.ID, storage.ChatApproved); err != nil {
				b.log.Warn("approve chat added by owner", "err", err, "chat", upd.Chat.ID)
				return nil
			}
			b.setApprovalCache(upd.Chat.ID, true)
			b.rememberChat(b.runCtx, info)
			b.markBotAddedAt(upd.Chat.ID)
			b.checkAdminRights(upd)
			return nil
		}
		b.askOwnerApproval(upd)
		return nil
	case status == storage.ChatApproved:
		// Существующий/повторно подтверждённый чат: как раньше.
		b.setApprovalCache(upd.Chat.ID, true)
		b.rememberChat(b.runCtx, info)
		b.markBotAddedAt(upd.Chat.ID)
		b.checkAdminRights(upd)
		return nil
	case status == storage.ChatPending:
		// Уже спросили — ждём решения владельца. Без повторного ЛС и без
		// подсказки по правам (любая активность в pending запрещена).
		b.setApprovalCache(upd.Chat.ID, false)
		b.rememberChat(b.runCtx, info)
		b.markBotAddedAt(upd.Chat.ID)
		return nil
	default:
		// rejected: leave не прошёл или событие догнало выход — чат инертен.
		b.setApprovalCache(upd.Chat.ID, false)
		b.rememberChat(b.runCtx, info)
		b.markBotAddedAt(upd.Chat.ID)
		return nil
	}
}

// markBotAddedAt штампует дату первого появления бота в чате (для /info).
// Вызывается строго ПОСЛЕ rememberChat: строка реестра уже должна существовать,
// иначе write-once UPDATE промахнётся мимо несуществующей строки. Write-once:
// повторные события (повышение, рестарт) дату не сдвигают; для чатов, живших
// до введения колонки, остаётся NULL и /info показывает фолбэк по самому
// раннему событию чата.
func (b *Bot) markBotAddedAt(chatID int64) {
	if err := b.db.SetChatBotAddedAtIfEmpty(b.runCtx, chatID, time.Now()); err != nil {
		b.log.Warn("mark bot added at", "err", err, "chat", chatID)
	}
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
	// Голосования тоже: оставшаяся плашка жила бы до суточного свипа, и
	// золотой голос в уже не обслуживаемом чате выдал бы глобальный бан.
	if err := b.db.DeleteChatSpamVotes(ctx, chatID); err != nil {
		b.log.Warn("delete chat spam votes", "err", err, "chat", chatID)
	}
	if err := b.db.DeleteChat(ctx, chatID); err != nil {
		b.log.Warn("delete chat", "err", err, "chat", chatID)
	}
	b.cacheMu.Lock()
	delete(b.chatCache, chatID)
	b.cacheMu.Unlock()
	b.delApprovalCache(chatID)
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
			// Ре-чек дедупа УЖЕ под замком: дубль-доставка могла прочитать
			// joined_at ДО нашей записи (свежести ещё не было), а дойти до
			// BeginKickoff — уже после освобождения замка. Без повторной
			// проверки она задвоила бы join+pass и отправила второе
			// приветствие.
			if joinedAt, ok, jerr := b.db.MemberJoinedAt(b.runCtx, chatID, user.ID); jerr == nil && ok &&
				time.Since(joinedAt) < time.Minute {
				b.store.FinishKickoff(chatID, user.ID)
				released = true
				return
			}
			// Имя в кэш до приветствия — путь сообщений его ещё не заполнял.
			b.rememberUser(b.runCtx, storage.UserInfo{
				UserID:    user.ID,
				FirstName: user.FirstName,
				LastName:  user.LastName,
				Username:  user.Username,
			})
			if err := b.db.RecordEvent(b.runCtx, chatID, user.ID, storage.EventJoin, time.Now(), ""); err != nil {
				b.log.Warn("record join event (trusted)", "err", err)
			}
			if err := b.db.RecordEvent(b.runCtx, chatID, user.ID, storage.EventPass, time.Now(), ""); err != nil {
				b.log.Warn("record pass event (trusted)", "err", err)
			}
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
					if err := b.db.RecordEvent(b.runCtx, chatID, user.ID, storage.EventJoin, time.Now(), ""); err != nil {
						b.log.Warn("record join event (spammer fallback)", "err", err)
					}
				}
				return
			}
			b.log.Info("banned known spammer on join", "chat", chatID, "user", user.ID)
			if err := b.db.RecordEvent(b.runCtx, chatID, user.ID, storage.EventSpamBan, time.Now(), storage.ReasonGlobal); err != nil {
				b.log.Warn("record spamban event (join)", "err", err)
			}
			b.notifyModAction(chatID, user.ID, storage.EventSpamBan, storage.ReasonGlobal)
			// Замок держим ещё минуту: у капчи роль «маркера после kickoff»
			// играет Pending в store, у бана ничего не остаётся — а дубль
			// new_chat_members может прийти следующим poll'ом и задвоить
			// событие spamban в статистике.
			handedOff = true
			// Голый AfterFunc сознательно вне runCtx: замок должен сняться
			// и после шатдауна/рестарта. Тело завёрнуто в goSafe — паника
			// не должна убивать процесс (recover не пересекает горутины).
			time.AfterFunc(time.Minute, func() {
				b.goSafe("finishKickoffLinger", func() {
					b.store.FinishKickoff(chatID, user.ID)
				})
			})
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

	// Чат необслуживаемый (pending/rejected, вне ALLOWED_CHATS): капча в нём
	// не живёт, а в стартовом окне после рестарта могла и остаться — не даём
	// ни решать её, ни наказывать.
	if !b.chatServiceable(chatID) {
		_ = b.api.AnswerCallbackQuery(ctx,
			tu.CallbackQuery(query.ID).
				WithText("Эта капча уже не активна.").
				WithShowAlert())
		return nil
	}

	capMsg := query.Message.Message()
	// Stale-guard и Take — атомарно (TakeMatch): клик по устаревшей
	// клавиатуре (старая капча, чей delete не прошёл) не должен разрешать
	// живую капчу — иначе optIdx с мёртвой клавиатуры сравнивается с
	// CorrectIdx живой и решает её исход. Проверка под тем же мьютом, что и
	// изъятие: между раздельными Get и Take могла улечься новая капча того
	// же юзера. Недоступное сообщение (capMsg == nil) проверить нечем — а
	// живая капча живёт минуты, так что клик почти наверняка с чужой/мёртвой
	// клавиатуры: считаем его устаревшим, а не пропускаем наудачу.
	p, ok := b.store.TakeMatch(chatID, query.From.ID, func(live *captcha.Pending) bool {
		return capMsg != nil && !staleCaptchaClick(live, capMsg)
	})
	if !ok {
		text := "Время вышло."
		stale := false
		if live, still := b.store.Get(chatID, query.From.ID); still &&
			(capMsg == nil || staleCaptchaClick(live, capMsg)) {
			text, stale = "Эта капча уже не активна.", true
		}
		answer := tu.CallbackQuery(query.ID).WithText(text)
		if stale {
			answer = answer.WithShowAlert()
		}
		_ = b.api.AnswerCallbackQuery(ctx, answer)
		return nil
	}
	p.Cancel()
	// Success-путь чистит pending СРАЗУ: onSuccess тянется секунды (release
	// с ретраями, приветствие), и рестарт в этом окне иначе воскресил бы
	// капчу для уже прошедшего и размьюченного юзера — с киком от таймера
	// серии. Fail-путь, наоборот, держит строку до успешного
	// наказания: она и есть механизм повтора при рестарте (см. onFail).

	if optIdx == p.CorrectIdx {
		if err := b.db.DeletePendingIfMsg(b.runCtx, chatID, query.From.ID, p.MessageID, p.EphemeralID); err != nil {
			b.log.Warn("delete pending on captcha pass", "err", err,
				"chat", chatID, "user", query.From.ID)
		}
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
			// Pending-строка оставлена: рестарт поднимет капчу и повторит
			// наказание (см. captchaStageLoop) — mute не останется навсегда.
			b.log.Error("on fail", "err", err, "chat", chatID, "user", query.From.ID)
			return nil
		}
		if err := b.db.DeletePendingIfMsg(b.runCtx, chatID, query.From.ID, p.MessageID, p.EphemeralID); err != nil {
			b.log.Warn("delete pending on captcha fail", "err", err,
				"chat", chatID, "user", query.From.ID)
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

	// Тот же гейт необслуживаемых чатов, что в handleCallback: до живой
	// проверки админства — чтобы не тратить API-запросы на мёртвый чат.
	if !b.chatServiceable(chatID) {
		_ = b.api.AnswerCallbackQuery(ctx,
			tu.CallbackQuery(query.ID).
				WithText("Эта капча уже не активна.").
				WithShowAlert())
		return nil
	}

	if !b.canManageChat(ctx, query.From.ID, chatID) {
		_ = b.api.AnswerCallbackQuery(ctx,
			tu.CallbackQuery(query.ID).
				WithText("Эта кнопка только для админов чата.").
				WithShowAlert())
		return nil
	}
	// Тот же атомарный stale-guard, что в handleCallback: «Впустить» с
	// мёртвой клавиатуры не должно апрувить живую капчу того же юзера.
	// Недоступное сообщение проверке не поддаётся и считается устаревшим
	// (см. комментарий там же).
	p, ok := b.store.TakeMatch(chatID, targetUserID, func(live *captcha.Pending) bool {
		msg := query.Message.Message()
		return msg != nil && !staleCaptchaClick(live, msg)
	})
	if !ok {
		msg := query.Message.Message()
		text := "Капча уже не активна."
		stale := false
		if live, still := b.store.Get(chatID, targetUserID); still &&
			(msg == nil || staleCaptchaClick(live, msg)) {
			text, stale = "Эта капча уже не активна.", true
		}
		answer := tu.CallbackQuery(query.ID).WithText(text)
		if stale {
			answer = answer.WithShowAlert()
		}
		_ = b.api.AnswerCallbackQuery(ctx, answer)
		return nil
	}
	// Pending — сразу (см. handleCallback: success-путь не оставляет строку).
	p.Cancel()
	if err := b.db.DeletePendingIfMsg(b.runCtx, chatID, targetUserID, p.MessageID, p.EphemeralID); err != nil {
		b.log.Warn("delete pending on admin approve", "err", err,
			"chat", chatID, "user", targetUserID)
	}
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

	if _, err := b.api.SendMessage(ctx, tu.Message(tu.ID(message.Chat.ID), text).
		WithParseMode(telego.ModeHTML).
		WithReplyMarkup(b.mainMenuKeyboard(userID))); err != nil {
		b.log.Debug("start menu send", "err", err, "user", userID)
	}
	// Владелец мог не получить ЛС-вопрос о pending-чате (закрытая личка на
	// момент добавления): его /start — единственная дверь, которая
	// открывается со стороны владельца. Переспрашиваем все ждущие чаты.
	// Гейт обязателен: /start в ЛС может послать кто угодно, а переспрос
	// рассылает кнопки appr: всем владельцам и раскрывает титулы
	// pending-чатов — чужакам туда хода нет.
	if b.isOwner(userID) {
		b.reAskOwnerApprovals(message.Chat.ID)
	}
	return nil
}

// reAskOwnerApprovals повторно рассылает вопросы по всем pending-чатам —
// выполняет обещание подсказки «напиши мне /start». Вызывается из
// handlePrivateStart ПОД гейтом isOwner: чужой /start не должен ни
// генерировать владельцам кнопки, ни видеть титулы ожидающих чатов.
func (b *Bot) reAskOwnerApprovals(dmChatID int64) {
	pending, err := b.db.PendingChats(b.runCtx)
	if err != nil {
		b.log.Warn("re-ask approvals: list pending", "err", err)
		return
	}
	for _, c := range pending {
		info := storage.ChatInfo{
			ChatID: c.ChatID, Title: c.Title, Type: c.Type, Username: c.Username,
		}
		adderLabel := "Кто добавил: неизвестно (переспрос по команде /start)"
		configured, delivered := b.sendOwnerApprovalPrompt(b.runCtx, c.ChatID, info, adderLabel)
		if configured && !delivered {
			// Личка по-прежнему закрыта — честно скажем об этом в ЛС-меню.
			if _, err := b.api.SendMessage(b.runCtx, tu.Message(tu.ID(dmChatID),
				fmt.Sprintf("🤖 Чат «%s» всё ещё ждёт подтверждения, но я не могу доставить тебе вопрос в этот диалог.",
					html.EscapeString(titleOrID(c))))); err != nil {
				b.log.Debug("re-ask delivery notice", "err", err)
			}
			continue
		}
		b.log.Info("approval prompt re-sent on start", "chat", c.ChatID)
	}
}

// migrateChatState переносит всё состояние чата при апгрейде basic-группы в
// супергруппу (оба сервис-сообщения ведут сюда — двойное срабатывание
// безвредно, MigrateChat идемпотентен). Статус подтверждения читается ДО
// MigrateChat — она удаляет старую строку реестра; иначе мигрировавший
// approved-чат молча ушёл бы в ожидание подтверждения как «новый».
func (b *Bot) migrateChatState(oldID, newID int64) {
	b.log.Info("chat migrating to supergroup", "old", oldID, "new", newID)
	b.carryApprovalOnMigrate(b.runCtx, oldID, newID)
	// Дату появления бота читаем ДО MigrateChat (удаляет старую строку
	// реестра) и пишем ПОСЛЕ: carryApprovalOnMigrate уже апсертил новую
	// строку, так что write-once UPDATE найдёт куда писать. Не перенесли —
	// /info покажет «примерно» по раннему событию, не драма.
	addedAt, hasAdded, err := b.db.GetChatBotAddedAt(b.runCtx, oldID)
	if err != nil {
		b.log.Warn("carry bot_added_at: read", "err", err, "old", oldID)
	}
	if err := b.db.MigrateChat(b.runCtx, oldID, newID); err != nil {
		b.log.Error("migrate chat data", "err", err, "old", oldID, "new", newID)
		// releaseMigratedCaptchas всё равно: таймеры капч целятся в мёртвый
		// старый chat_id, и после неудавшейся миграции они обязаны разрядиться
		// тихо, а не стрелять kick/noreply в несуществующий чат.
	} else if hasAdded {
		if err := b.db.SetChatBotAddedAtIfEmpty(b.runCtx, newID, addedAt); err != nil {
			b.log.Warn("carry bot_added_at: write", "err", err, "new", newID)
		}
	}
	b.releaseMigratedCaptchas(oldID, newID)
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
		b.migrateChatState(message.Chat.ID, message.MigrateToChatID)
		return nil
	}
	if message.MigrateFromChatID != 0 {
		b.migrateChatState(message.MigrateFromChatID, message.Chat.ID)
		return nil
	}

	// Всё дальше — обслуживание чата: реестр, капчи по сервис-сообщениям,
	// статистика, анонсы, спам-чек. Посторонние чаты (вне ALLOWED_CHATS) и
	// чаты, не подтверждённые владельцем (pending/rejected), не обслуживаем —
	// иначе первое же сообщение заносит чат в реестр и открывает его админам
	// DM-меню (вплоть до включения ИИ-антиспама за счёт владельца). Ветки
	// миграции выше гейта: MigrateFromChatID приходит уже с НОВЫМ chat_id,
	// которого в ALLOWED_CHATS ещё нет.
	if !b.chatServiceable(message.Chat.ID) {
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
	// Сервис-сообщения (смена названия/фото, пины, варианты опросов, старты
	// видеочатов, создание групп и т.п.) — не пользовательский контент: в
	// статистику, детектор тишины и LLM они идти не должны с пустым текстом.
	// Позитивный фильтр надёжнее перечисления того, что пропустить: новый вид
	// сервис-сообщения Telegram не проскочит молча.
	if !messageHasUserContent(&message) {
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
	if !b.chatServiceable(message.Chat.ID) {
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
	// Сервис-сообщения об изменении вариантов опроса (если придут как правка) —
	// проверять нечего.
	if message.PollOptionAdded != nil || message.PollOptionDeleted != nil {
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
		// Воронку закрываем: join уже записан, а капчи не будет — иначе юзер
		// навсегда зависнет в «В процессе». Это abort (вина инфраструктуры),
		// не «вышли сами»: юзер никуда не уходил и будет размучен ниже.
		// releaseOnAbort — страховка от частично применённого restriction
		// (таймаут после применения).
		b.recordAbortEvent(ctx, chatID, user.ID, "restrict failed")
		b.releaseOnAbort(ctx, chatID, user.ID)
		return
	}

	// Теперь даём клиенту юзера время полностью открыть чат. Без этого
	// сообщение капчи иногда не подклеивается в уже отрисованную ленту, и
	// юзер видит его только после повторного открытия чата. Задержка нужна
	// только перед ПЕРВОЙ отправкой: напоминания уходят минуты спустя.
	if b.cfg.CaptchaDelay > 0 {
		select {
		case <-ctx.Done():
			// Shutdown в окне задержки: рестрикт уже применён, а pending ещё
			// не записан — рестарт юзера не восстановит. Снимаем мут и
			// закрываем воронку abort'ом (join уже записан), иначе юзер
			// навсегда висел бы в «В процессе». Событие — на detached-бюджете:
			// RecordEvent на отменённом runCtx был бы гарантированный no-op.
			b.recordAbortDetached(chatID, user.ID, "shutdown during captcha delay")
			b.releaseOnAbort(ctx, chatID, user.ID)
			return
		case <-time.After(b.cfg.CaptchaDelay):
		}
	}

	settings := b.chatSettings(ctx, chatID)
	p := b.sendCaptchaStage(ctx, settings, chatID, user.ID, threadID, 1)
	if p == nil {
		// Серия оборвалась при отправке первой стадии (юзер ушёл / сбой
		// инфраструктуры): воронка и мьют уже закрыты внутри.
		return
	}
	// Вся серия живёт в своей горутине (как прежний waitTimeout): runCaptcha
	// возвращается сразу после взведения первой стадии.
	b.goSafe("captchaStageLoop", func() { b.captchaStageLoop(chatID, user.ID, p) })
}

// captchaStages — сколько сообщений капчи получает молчащий юзер за одну
// сессию: обычное, напоминание, последнее предупреждение. Вся серия — ОДНА
// попытка: промежуточные таймауты не пишут событий и не двигают attempts,
// punishAttempt вызывается один раз после исчерпания серии.
const captchaStages = 3

// sendCaptchaStage собирает и отправляет сообщение капчи стадии stage
// (1..captchaStages). Возвращает живой Pending (уже в store и в БД) или nil,
// если серия оборвалась здесь: все abort-пути внутри сами закрывают воронку
// (left/abort) и снимают капча-мьют (releaseOnAbort).
func (b *Bot) sendCaptchaStage(ctx context.Context, settings storage.ChatSettings,
	chatID, userID int64, threadID, stage int,
) *captcha.Pending {
	interval := b.effectiveStageInterval(settings)
	mode := effectiveCaptchaMode(settings)
	ch := captcha.New(mode)
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

	// Эфемерный режим — на ВСЮ серию без исключений: капча, напоминание и
	// последнее предупреждение видны только вступившему (и боту). Ряд
	// «Впустить» на эфемерных стадиях не рисуется вовсе: админы сообщение
	// не видят. Осознанная плата режима: оффлайн-юзер может не увидеть ни
	// одного сообщения серии, а админы не могут впустить его вручную.
	ephemeral := settings.EphemeralEnabled

	// Контракт раскладки: ряд 0 — варианты в порядке индексов, ряд 1 — кнопка
	// админа. buttonLabel читает row 0 по этому контракту — при перестановке
	// рядов обнови и его.
	buttons := make([]telego.InlineKeyboardButton, 0, len(ch.Options))
	for i, c := range ch.Options {
		buttons = append(buttons,
			tu.InlineKeyboardButton(c.Emoji).
				WithCallbackData(fmt.Sprintf("cap:%d:%d", userID, i)))
	}
	rows := [][]telego.InlineKeyboardButton{tu.InlineKeyboardRow(buttons...)}
	if !ephemeral {
		rows = append(rows, tu.InlineKeyboardRow(
			tu.InlineKeyboardButton("✅ Впустить (для админов)").
				WithCallbackData(fmt.Sprintf("capok:%d", userID))))
	}
	kb := tu.InlineKeyboard(rows...)

	// Имя берём из кэша user_info (runCaptcha заполнил его при старте,
	// рестарты читают ту же таблицу) — единообразно для всех стадий.
	infos, ierr := b.db.GetUserInfos(ctx, []int64{userID})
	if ierr != nil {
		b.log.Warn("fetch user info for captcha", "err", ierr, "chat", chatID, "user", userID)
	}
	mention := mentionOrID(infos, userID)
	minutes := int(interval.Minutes())

	// Отправка ретраится: 429 прилетает ровно во время масс-джойна, а
	// single-shot фейл здесь release'ил юзера БЕЗ капчи — щит отключался
	// как раз под флудом.
	var msg *telego.Message
	var err error
	if photo != nil {
		caption := captchaPhotoCaption(stage, mention, minutes)
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
				p = p.WithReceiverUserID(userID)
			}
			var e error
			msg, e = b.api.SendPhoto(ctx, p)
			return e
		})
	} else {
		text := captchaStageText(stage, mention, correct.Prompt, minutes)
		params := tu.Message(tu.ID(chatID), text).
			WithParseMode(telego.ModeHTML).
			WithReplyMarkup(kb)
		if threadID != 0 {
			params = params.WithMessageThreadID(threadID)
		}
		if ephemeral {
			params = params.WithReceiverUserID(userID)
		}
		err = retryTG(ctx, func() error {
			var e error
			msg, e = b.api.SendMessage(ctx, params)
			return e
		})
	}
	if err != nil {
		// USER_NOT_PARTICIPANT — юзер вышел до доставки капчи (в store её ещё
		// нет, ветка «left mid-captcha» в handleChatMember не сработала).
		// Не провал капчи: kick не пишем, а EventLeft — чтобы воронка в
		// статистике сходилась, а не копила «В процессе» навсегда.
		if isUserNotParticipant(err) {
			b.recordLeftEvent(ctx, chatID, userID, "user not participant")
			b.log.Info("captcha aborted — user not participant",
				"chat", chatID, "user", userID, "stage", stage)
		} else {
			b.log.Error("send captcha", "err", err, "chat", chatID, "user", userID, "stage", stage)
			// Не USER_NOT_PARTICIPANT — про юзера неизвестно ничего, но капча
			// сорвалась после ретраев. releaseOnAbort ниже впустит его; здесь
			// закрываем воронку как abort (инфраструктура), не «вышли сами».
			b.recordAbortEvent(ctx, chatID, userID, "captcha send failed")
		}
		// releaseOnAbort: при живом ctx (сетевой фейл) — полный бюджет ретраев,
		// при отменённом (shutdown) — detached, иначе мут не снялся бы.
		b.releaseOnAbort(ctx, chatID, userID)
		return nil
	}

	// Юзер мог выйти, пока капча готовилась и уходила: окно задержки плюс
	// лестница ретраев отправки тянуться десятки секунд под 429. То же верно
	// для напоминаний: между стадиями проходят минуты. Публичная отправка
	// отсутствующему проходит успешно (USER_NOT_PARTICIPANT ловит только
	// эфемерка), и без этой проверки цикл выдал бы фантомные напоминания и
	// кик/бан тому, кто давно вышел. Проверяем последним действием перед Put,
	// чтобы гонка «ушёл после проверки» была микросекундной. Ошибка чтения —
	// не повод не выдавать капчу (старое поведение, fail-open).
	if m, merr := b.api.GetChatMember(ctx, &telego.GetChatMemberParams{
		ChatID: tu.ID(chatID),
		UserID: userID,
	}); merr == nil {
		if s := m.MemberStatus(); s == "left" || s == "kicked" {
			if derr := b.deleteBotMessage(ctx, chatID, msg.MessageID, msg.EphemeralMessageID, userID); derr != nil {
				b.log.Debug("delete captcha of departed user", "err", derr, "chat", chatID)
			}
			b.recordLeftEvent(ctx, chatID, userID, "left before captcha")
			b.log.Info("captcha aborted — user left before captcha",
				"chat", chatID, "user", userID, "stage", stage)
			b.releaseOnAbort(ctx, chatID, userID)
			return nil
		}
	} else {
		b.log.Debug("captcha liveness check", "err", merr, "chat", chatID, "user", userID)
	}

	expires := time.Now().Add(interval)
	p := b.store.Put(chatID, userID, msg.MessageID, ch.CorrectIdx, expires, threadID, msg.EphemeralMessageID, stage)

	if err := b.db.PutPending(ctx, storage.PendingRow{
		ChatID:      chatID,
		UserID:      userID,
		MessageID:   msg.MessageID,
		CorrectIdx:  ch.CorrectIdx,
		ExpiresAt:   expires,
		ThreadID:    threadID,
		EphemeralID: msg.EphemeralMessageID,
		Stage:       stage,
	}); err != nil {
		// Третий pre-persist обрыв (после shutdown-в-задержке и фейла
		// отправки): капча только в памяти не переживёт рестарт — юзер
		// остался бы замьючен навсегда, причём сбой БД как раз коррелирует
		// со скорым рестартом. Fail-open, как и при неудачной отправке:
		// снимаем капчу и впускаем. Take с проверкой — юзер мог успеть
		// ответить за эти миллисекунды, тогда исход уже решён без нас.
		b.log.Warn("persist pending — dropping captcha, letting user in (fail-open)",
			"err", err, "chat", chatID, "user", userID, "stage", stage)
		if taken, ok := b.store.Take(chatID, userID); ok && taken == p {
			taken.Cancel()
			if derr := b.deleteBotMessage(ctx, chatID, msg.MessageID, msg.EphemeralMessageID, userID); derr != nil {
				b.log.Warn("delete captcha after persist failure",
					"err", derr, "chat", chatID, "msg", msg.MessageID)
			}
			// Воронку закрываем abort'ом: join уже записан, а капчи больше не
			// будет. Detached-бюджет — на случай, если фейл был вызван именно
			// отменой ctx (shutdown), а не самой БД.
			b.recordAbortDetached(chatID, userID, "pending persist failed")
			b.releaseOnAbort(ctx, chatID, userID)
		}
		return nil
	}
	return p
}

// captchaStageLoop ведёт серию капчи: ждёт дедлайн текущей стадии, а по его
// истечении либо показывает следующую стадию (удалив предыдущее сообщение),
// либо — после последнего предупреждения — исполняет штатную лестницу
// наказаний. Решившие капчу раньше срока (ответ юзера, админ-апрув, выход,
// тихая отмена) изымают pending через store.Take*/TakeMatch и гасят Cancel'ом:
// цикл выходит по Done без единого события. Промежуточные таймауты событий НЕ
// пишут — вся серия попадёт в статистику одним kick/ban. Настройки
// перечитываются на каждой смене стадии: редкое событие (минуты, не горячий
// путь), зато правки интервала из меню подхватывают следующие стадии.
func (b *Bot) captchaStageLoop(chatID, userID int64, p *captcha.Pending) {
	ctx := b.runCtx
	for {
		timer := time.NewTimer(time.Until(p.ExpiresAt))
		select {
		case <-p.Done():
			timer.Stop()
			return
		case <-ctx.Done():
			// Shutdown: строка pending_captchas уже персистентна (с текущей
			// стадией и дедлайном) — рестарт продолжит серию с этого места.
			timer.Stop()
			return
		case <-timer.C:
		}

		// Единственный победитель дедлайна: identity-проверка отсекает гонку
		// «колбэк успел изъять и решить капчу между таймером и Take».
		taken, ok := b.store.Take(chatID, userID)
		if !ok || taken != p {
			return
		}

		// Последняя стадия истекла: вся серия — одна попытка, наказание как
		// у прежнего одиночного таймаута. Cleanup на detached 10-секундном
		// контексте, чтобы shutdown не оборвал кик/бан на полпути;
		// pending-строка держится до успешного наказания — она и есть
		// механизм повтора при рестарте (иначе бессрочный капча-мьют остался
		// бы с юзером навсегда).
		if p.Stage >= captchaStages {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := b.onFail(cleanupCtx, p, "таймаут"); err != nil {
				b.log.Error("on fail timeout", "err", err, "chat", chatID, "user", userID)
				return
			}
			if err := b.db.DeletePendingIfMsg(cleanupCtx, chatID, userID, p.MessageID, p.EphemeralID); err != nil {
				b.log.Warn("delete pending row after timeout punish",
					"err", err, "chat", chatID, "user", userID)
			}
			return
		}

		// Промежуточная стадия истекла. Переход персистим ДО исполнения:
		// между Take и следующим PutPending строка в БД описывала бы уже
		// истёкшую стадию, и крэш в этом окне (деплои тут рутинны) клампил
		// бы рестарт к финальной стадии с грейс-киком юзера, у которого
		// оставались стадии. message_id в строке до конца перехода — старый;
		// после успешной отправки sendCaptchaStage перезапишет его своим.
		settings := b.chatSettings(ctx, chatID)
		if err := b.db.PutPending(ctx, storage.PendingRow{
			ChatID:      chatID,
			UserID:      userID,
			MessageID:   p.MessageID,
			CorrectIdx:  p.CorrectIdx,
			ExpiresAt:   time.Now().Add(b.effectiveStageInterval(settings)),
			ThreadID:    p.ThreadID,
			EphemeralID: p.EphemeralID,
			Stage:       p.Stage + 1,
		}); err != nil {
			b.log.Warn("persist stage transition", "err", err,
				"chat", chatID, "user", userID, "stage", p.Stage+1)
		}

		// Убираем истёкшее сообщение и показываем следующее (новый челлендж —
		// старая клавиатура удалена вместе с сообщением, stale-guard по
		// message_id не даст решиться живой стадии кликом по мёртвой).
		if derr := b.deleteBotMessage(ctx, chatID, p.MessageID, p.EphemeralID, userID); derr != nil {
			b.log.Debug("delete expired captcha stage",
				"err", derr, "chat", chatID, "user", userID, "msg", p.MessageID)
		}
		// Замок kickoff на время отправки: между Take и следующим Put в store
		// окно, куда дубль-доставка входа (chat_member + new_chat_members)
		// запустила бы вторую серию с дублем сообщения капчи.
		if !b.store.BeginKickoff(chatID, userID) {
			// Дубль-доставка успела раньше — её серия подхватит юзера.
			return
		}
		next := b.sendCaptchaStage(ctx, settings, chatID, userID, p.ThreadID, p.Stage+1)
		b.store.FinishKickoff(chatID, userID)
		if next == nil {
			// Серия оборвалась (юзер ушёл / сбой): воронка закрыта внутри.
			// Страховка от осиротевшего перехода: pre-persist выше мог успеть
			// записать следующую стадию под СТАРЫМ message_id — гасим её по
			// этому guard'у, иначе рестарт поднял бы призрачную капчу.
			if err := b.db.DeletePendingIfMsg(ctx, chatID, userID, p.MessageID, p.EphemeralID); err != nil {
				b.log.Warn("delete orphaned stage transition",
					"err", err, "chat", chatID, "user", userID)
			}
			return
		}
		// Капча могла быть решена колбэком, пока уходила следующая стадия
		// (секунды ретраев под 429): тогда Get промахнётся, а только что
		// отправленное сообщение останется висеть с живой клавиатурой.
		// Проверка через Get, НЕ Take: изымать живую стадию здесь нельзя.
		if cur, ok := b.store.Get(chatID, userID); !ok || cur != next {
			if derr := b.deleteBotMessage(ctx, chatID, next.MessageID, next.EphemeralID, userID); derr != nil {
				b.log.Debug("delete captcha of already-resolved stage",
					"err", derr, "chat", chatID, "user", userID, "msg", next.MessageID)
			}
			return
		}
		p = next
	}
}

// releaseMigratedCaptchas — basic-группа апгрейднулась до супергруппы:
// chat_id сменился на лету, и живые капчи/reply-wait старого id остались без
// колбэков (клики теперь приходят с новым chat_id, Take промахивается), а их
// таймауты стреляли бы наказаниями в мёртвый id. Telegram переносит
// участника вместе с restriction, поэтому гасим всё и снимаем капча-мьют в
// НОВОМ чате (семантика releaseOnAbort). Воронка каждого выпущенного юзера
// закрывается пассом: пройти проверку по старой капче/якорю он больше не
// может — невозможность ответить не его вина (прецедент /mute). Вызывается
// ПОСЛЕ MigrateChat, чтобы события легли сразу под новый id. Fail-open
// сознательно: человек входит без повторной капчи — миграция редкое админское
// действие, а перевзвести капчу под новым id нечем (таймеры захвати старый).
func (b *Bot) releaseMigratedCaptchas(oldID, newID int64) {
	for _, p := range b.store.TakeChat(oldID) {
		p.Cancel()
		// MigrateChat уже снёс строки старого чата; удаление здесь —
		// страховка от гонки с параллельным рестартом.
		if err := b.db.DeletePending(b.runCtx, oldID, p.UserID); err != nil {
			b.log.Debug("delete migrated pending (already gone?)",
				"err", err, "old", oldID, "user", p.UserID)
		}
		if err := b.db.RecordEvent(b.runCtx, newID, p.UserID, storage.EventPass, time.Now(), ""); err != nil {
			b.log.Warn("record pass event (migrated captcha)", "err", err)
		}
		b.releaseOnAbort(b.runCtx, newID, p.UserID)
		b.log.Info("captcha released across chat migration",
			"old", oldID, "new", newID, "user", p.UserID)
	}
	for _, p := range b.replies.TakeChat(oldID) {
		p.Cancel()
		if err := b.db.RecordEvent(b.runCtx, newID, p.UserID, storage.EventPass, time.Now(), ""); err != nil {
			b.log.Warn("record pass event (migrated reply wait)", "err", err)
		}
	}
	if err := b.db.DeletePendingRepliesChat(b.runCtx, oldID); err != nil {
		b.log.Debug("delete migrated pending replies (already gone?)",
			"err", err, "old", oldID)
	}
}

// cancelCaptchaSilent тихо гасит активную капчу юзера после успешного
// наказания инициатором (/kick, /ban, спам-вердикт): таймаут серии иначе
// записал бы СВОЙ kick/ban поверх уже учтённого события инициатора, задваивая
// воронку статистики. Без событий — их пишет инициатор.
func (b *Bot) cancelCaptchaSilent(chatID, userID int64) {
	p, ok := b.store.Take(chatID, userID)
	if !ok {
		return
	}
	p.Cancel()
	if err := b.db.DeletePending(b.runCtx, chatID, userID); err != nil {
		b.log.Warn("delete pending of punished user", "err", err, "chat", chatID, "user", userID)
	}
	if err := b.deleteBotMessage(b.runCtx, chatID, p.MessageID, p.EphemeralID, userID); err != nil {
		b.log.Debug("delete captcha of punished user (already gone?)", "err", err, "chat", chatID)
	}
}

// recordLeftEvent — терминальное «вышли сами»: юзер действительно ушёл до
// доставки капчи (USER_NOT_PARTICIPANT, liveness-проверка). Join уже записан,
// и без закрывающего события юзер навсегда остался бы в «В процессе».
func (b *Bot) recordLeftEvent(ctx context.Context, chatID, userID int64, why string) {
	if err := b.db.RecordEvent(ctx, chatID, userID, storage.EventLeft, time.Now(), ""); err != nil {
		b.log.Warn("record left event", "err", err)
		return
	}
	b.log.Info("captcha funnel closed with left event", "chat", chatID, "user", userID, "why", why)
}

// recordAbortEvent — терминальный abort для сорванной по нашей вине капчи
// (restrict/send упали после ретраев): юзер остался в чате, и «вышли сами»
// было бы ложью в статистике. Воронка закрывается тем же счётом.
func (b *Bot) recordAbortEvent(ctx context.Context, chatID, userID int64, why string) {
	if err := b.db.RecordEvent(ctx, chatID, userID, storage.EventAbort, time.Now(), ""); err != nil {
		b.log.Warn("record abort event", "err", err)
		return
	}
	b.log.Info("captcha funnel closed with abort event", "chat", chatID, "user", userID, "why", why)
}

// recordAbortDetached — abort на detached-бюджете: для ветвей, где ctx мог
// умереть вместе с процессом (shutdown в окне задержки, фейл персистенса из-за
// отмены ctx). RecordEvent на мёртвом ctx был бы гарантированный no-op, и
// воронка осталась бы открытой («В процессе» навсегда).
func (b *Bot) recordAbortDetached(chatID, userID int64, why string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b.recordAbortEvent(ctx, chatID, userID, why)
}

// onSuccess завершает капчу победой. answer — «выбрал N-й (эмодзи)» с кнопки,
// которую нажал юзер; пустая строка на admin-approve пути (выбора не было —
// уведомление владельцам пропускается, лог-поле answer остаётся пустым).
func (b *Bot) onSuccess(ctx context.Context, p *captcha.Pending, answer string) error {
	if err := b.db.ResetAttempts(ctx, p.ChatID, p.UserID); err != nil {
		// Потерянный сброс = следующий провал посчитает попытки с прошлого
		// захода и недосчитается до пермабана — не молчим.
		b.log.Warn("reset attempts", "err", err, "chat", p.ChatID, "user", p.UserID)
	}
	if err := b.db.UpsertMember(ctx, p.ChatID, p.UserID, time.Now()); err != nil {
		b.log.Warn("upsert member", "err", err)
	}
	s := b.chatSettings(ctx, p.ChatID)
	// «Прошёл» засчитывается только после ВСЕХ включённых проверок: при
	// однофакторной (одна капча) — сразу здесь; при включённом reply-check —
	// когда юзер реально ответил на приветствие (replyWaitSatisfied). Иначе
	// пасс до таймаута задваивался последующим киком за молчание
	// (join→pass→kick/noreply в воронке статистики).
	if !s.ReplyCheckEnabled {
		if err := b.db.RecordEvent(ctx, p.ChatID, p.UserID, storage.EventPass, time.Now(), ""); err != nil {
			b.log.Warn("record pass event", "err", err)
		}
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
		// Проверённого юзера нельзя оставлять за бессрочным капча-мьютом:
		// pending уже удалён (или удаляется вызывающим после успеха), и
		// повторной капчи не будет. Догоняем на detached-бюджете — как
		// releaseOnAbort на shutdown: 15 c покрывают обе лестницы release
		// (getChat + restrict). Провал и его — только Error в логе.
		b.log.Error("release after captcha — retrying detached",
			"err", err, "chat", p.ChatID, "user", p.UserID)
		rctx, rcancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer rcancel()
		if rerr := b.release(rctx, p.ChatID, p.UserID); rerr != nil {
			return rerr
		}
	}
	// Ожидание ответа взводим СРАЗУ после размьюта, до сетевой отправки
	// приветствия: юзер уже может писать, и его первое сообщение должно
	// застать ожидание активным (иначе гонка → кик написавшего).
	b.maybeArmReplyWait(s, p.ChatID, p.UserID, p.ThreadID)
	if !b.maybeSendGreeting(ctx, s, p.ChatID, p.UserID, p.ThreadID) && s.ReplyCheckEnabled {
		// Приветствие-якорь с требованием не ушёл даже после ретраев
		// (429-шторм масс-джойна, сеть): требование «напиши что-нибудь»
		// юзер выполнить не может — он его никогда не видел. Снимаем
		// ожидание и закрываем воронку пассом: невозможность ответить —
		// не вина юзера (та же логика, что у /mute замьюченного), иначе
		// таймаут серии кикнул бы его за молчание по вине инфраструктуры.
		if b.cancelReplyWait(p.ChatID, p.UserID) {
			if err := b.db.RecordEvent(ctx, p.ChatID, p.UserID, storage.EventPass, time.Now(), ""); err != nil {
				b.log.Warn("record pass event", "err", err)
			}
			b.log.Warn("reply wait disarmed — greeting send failed",
				"chat", p.ChatID, "user", p.UserID)
		}
	}
	// Затем — ИИ-оценка профиля новичка (асинхронная внутри).
	b.maybeProfileCheck(p.ChatID, p.UserID, p.ThreadID)
	return nil
}

// punishAttempt — общий карательный хвост капчи (onFail) и reply-wait
// (replyWaitLoop): инкремент попыток с гвардией «ошибка счётчика запрещает
// эскалацию до пермабана», порог effectiveMaxAttempts, banShort/kick, запись
// события с причиной. notify вызывается только ПОСЛЕ успешного действия —
// бан, которого не было, не должен попадать в статистику. dbReason уходит в
// events (константа Reason*), logReason — в логи и уведомления.
func (b *Bot) punishAttempt(ctx context.Context, chatID, userID int64,
	dbReason, logReason string, notify func(event storage.EventKind, count int),
) error {
	count, err := b.db.IncrementAttempt(ctx, chatID, userID, attemptsTTL)
	incFailed := false
	if err != nil {
		b.log.Warn("increment attempt", "err", err, "chat", chatID, "user", userID)
		count = 1        // считаем первой попыткой и едем дальше
		incFailed = true // счётчику верить нельзя — эскалация до бана запрещена
	}
	if !incFailed && count >= b.effectiveMaxAttempts(b.chatSettings(ctx, chatID)) {
		b.log.Info("banning user", "chat", chatID, "user", userID,
			"reason", logReason, "attempts", count)
		if err := b.banShort(ctx, chatID, userID); err != nil {
			return err
		}
		if err := b.db.RecordEvent(ctx, chatID, userID, storage.EventBan, time.Now(), dbReason); err != nil {
			b.log.Warn("record ban event", "err", err)
		}
		notify(storage.EventBan, count)
		return nil
	}
	b.log.Info("kicking user", "chat", chatID, "user", userID,
		"reason", logReason, "attempts", count)
	if err := b.kick(ctx, chatID, userID); err != nil {
		return err
	}
	if err := b.db.RecordEvent(ctx, chatID, userID, storage.EventKick, time.Now(), dbReason); err != nil {
		b.log.Warn("record kick event", "err", err)
	}
	notify(storage.EventKick, count)
	return nil
}

func (b *Bot) onFail(ctx context.Context, p *captcha.Pending, reason string) error {
	if err := b.deleteBotMessage(ctx, p.ChatID, p.MessageID, p.EphemeralID, p.UserID); err != nil {
		b.log.Warn("delete captcha on fail/timeout",
			"err", err, "chat", p.ChatID, "msg", p.MessageID, "reason", reason)
	}

	// Событие и уведомление — ПОСЛЕ успешного действия: упавший бан не должен
	// оставлять в статистике бан, которого не было. Провал действия при уже
	// удалённом pending невосстановим, поэтому вызывающие удаляют pending-строку
	// только после успеха — тогда рестарт поднимает капчу и повторяет наказание.
	return b.punishAttempt(ctx, p.ChatID, p.UserID, storage.ReasonCaptcha, reason,
		func(event storage.EventKind, count int) {
			b.notifyCaptchaFail(p.ChatID, p.UserID, event, reason, count)
		})
}

// captchaStageText — текст сообщения капчи для стадии серии: 1 — обычный
// промпт, 2 — напоминание, 3 — последнее предупреждение. minutes — интервал
// стадии; упоминание и верный эмодзи подставляются вызывающим.
func captchaStageText(stage int, mention, prompt string, minutes int) string {
	switch stage {
	case 2:
		return fmt.Sprintf("⏳ %s, напоминаю: капча ещё не пройдена.\n"+
			"Выбери <b>%s</b>.\n⏱ У тебя ещё %s.",
			mention, prompt, minutesNom(minutes))
	case 3:
		return fmt.Sprintf("⚠️ ПОСЛЕДНЕЕ ПРЕДУПРЕЖДЕНИЕ, %s!\n"+
			"Выбери <b>%s</b>, иначе через %s тебя исключат из чата.",
			mention, prompt, minutesAcc(minutes))
	default:
		return fmt.Sprintf("Привет, %s!\nДля защиты от спама выбери <b>%s</b>.\n⏱ У тебя %s.",
			mention, prompt, minutesNom(minutes))
	}
}

// captchaPhotoCaption — подпись к картинке капчи по стадиям серии.
func captchaPhotoCaption(stage int, mention string, minutes int) string {
	switch stage {
	case 2:
		return fmt.Sprintf("⏳ %s, напоминаю: капча ещё не пройдена.\n"+
			"Выбери эмодзи, наиболее похожее на картинку, в течение %s.",
			mention, minutesGen(minutes))
	case 3:
		return fmt.Sprintf("⚠️ ПОСЛЕДНЕЕ ПРЕДУПРЕЖДЕНИЕ, %s!\n"+
			"Выбери эмодзи, наиболее похожее на картинку, — через %s тебя исключат из чата.",
			mention, minutesAcc(minutes))
	default:
		return fmt.Sprintf("Привет, %s!\nДля защиты от спама выбери эмодзи, наиболее похожее на картинку, в течение %s.",
			mention, minutesGen(minutes))
	}
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

// staleCaptchaClick — клик по устаревшей клавиатуре: сообщение клика не
// совпадает с живой капчей (у эфемерной свой id, обычные message_id там
// нулевые). nil-сообщение (недоступное) — чистый компаратор не может его
// идентифицировать; вызывающие (handleCallback/handleApproveCallback)
// трактуют такой клик как stale сами, до вызова.
func staleCaptchaClick(live *captcha.Pending, capMsg *telego.Message) bool {
	if live == nil || capMsg == nil {
		return false
	}
	if live.EphemeralID != 0 {
		return capMsg.EphemeralMessageID != live.EphemeralID
	}
	return capMsg.MessageID != live.MessageID
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
