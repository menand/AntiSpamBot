package bot

import (
	"context"
	"errors"
	"fmt"
	"html"
	"math/rand/v2"
	"sort"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegoapi"
	tu "github.com/mymmrac/telego/telegoutil"

	"github.com/menand/AntiSpamBot/internal/storage"
)

// replyWatchOffsets — расписание проб: через 1, 5, 15 и 60 минут после
// осмысленного ответа. Абсолютные смещения от answered_at, а не интервалы:
// «прожило 60 минут» — конец наблюдения, а не ещё одна пауза.
var replyWatchOffsets = []time.Duration{
	time.Minute,
	5 * time.Minute,
	15 * time.Minute,
	60 * time.Minute,
}

const (
	// replyWatchTick — как часто фоновая проба смотрит созревшие строки.
	// 10 c: первая проба приходит на 60–70-й секунде, точность важнее
	// дешёвого SELECT по индексу next_at.
	replyWatchTick = 10 * time.Second
	// replyWatchProbeTimeout — потолок одной пробы (copy + короткий delete).
	replyWatchProbeTimeout = 15 * time.Second
	// replyWatchQuoteLimit — сколько UTF-16 единиц текста показываем в одной
	// цитате. Telegram считает лимит сообщения в UTF-16, а не в рунах: две
	// цитаты по 1400 + заголовки укладываются в 4096 даже на эмодзи.
	replyWatchQuoteLimit = 1400

	// Правки одного сообщения не должны заваливать ЛС владельца: первая
	// карточка уходит сразу, следующие — не чаще раза в 5 минут, максимум
	// три на наблюдение. Снимок текста обновляется при каждой правке, так что
	// отчёт об удалении всегда показывает последнюю версию.
	replyWatchEditCooldown   = 5 * time.Minute
	replyWatchEditMaxReports = 3
	// Недоставленная карточка об удалении возвращается в очередь: +5 минут,
	// максимум три попытки. Дальше — Error в лог (доставить некому/некуда),
	// строка снимается, чтобы не ретраить вечно.
	replyWatchReportRetryDelay  = 5 * time.Minute
	replyWatchReportMaxAttempts = 3
	replyWatchTransientDefer    = time.Minute
	replyWatchStaleAfter        = 2 * time.Hour
	replyWatchPruneInterval     = time.Hour
	replyWatchBrokenCauseLimit  = 300

	// replyWatchBatchLimit — сколько наблюдений проверяем за один тик.
	replyWatchBatchLimit = 20

	// replyWatchDeleteFailureLimit — сколько подряд неудачных удалений копии
	// терпим. Право на удаление могли не выдать/отобрать: тогда каждая проба
	// оставляла бы текст юзера в служебном чате навсегда. После лимита
	// наблюдение останавливается целиком (до перезапуска) — приватность
	// важнее, чем ещё несколько карточек.
	replyWatchDeleteFailureLimit = 3

	replyWatchDeletedByDel        = "bot:del"
	replyWatchDeletedByKick       = "bot:kick"
	replyWatchDeletedByBan        = "bot:ban"
	replyWatchDeletedBySpam       = "bot:spam"
	replyWatchDeletedByQuarantine = "bot:quarantine"
	replyWatchDeletedByCaptcha    = "bot:captcha"
)

// setupReplyWatch — стартовая диагностика фичи: куда копируем, кому
// отчитываемся, умеем ли удалять копию и что накопилось в БД. Ошибка getChat
// не валит запуск — проба сама переключится на ЛС владельца и пришлёт одно
// уведомление. Вызывается из goSafe ДО первого тика: старт long polling не
// должен ждать сетевые проверки.
func (b *Bot) setupReplyWatch(ctx context.Context) {
	if len(b.cfg.OwnerIDs) == 0 {
		b.log.Warn("reply watch enabled but OWNER_IDS is empty; no watches will be created")
		return
	}
	if chats, err := b.db.ReplyWatchChats(ctx); err != nil {
		b.log.Warn("reply watch: load watched chats", "err", err)
	} else {
		for _, chatID := range chats {
			b.replyWatchChats.Store(chatID, struct{}{})
		}
	}
	if target := b.cfg.ReplyWatchChatID; target == 0 {
		b.log.Info("reply watch enabled", "target", "owner DM",
			"owner", b.replyWatchOwnerID(), "owners", len(b.cfg.OwnerIDs))
	} else {
		b.checkReplyWatchTarget(ctx, target)
	}
	pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if n, err := b.db.DeleteStaleReplyWatches(pctx,
		time.Now().Add(-replyWatchStaleAfter)); err != nil {
		b.log.Warn("reply watch: prune stale", "err", err)
	} else if n > 0 {
		b.log.Info("reply watch: stale watches pruned", "count", n)
	}
}

// checkReplyWatchTarget — проверка служебного чата на старте: существует ли,
// виден ли копии посторонним и есть ли у бота право удалять копию. Без права
// удаления каждый отчёт оставлял бы текст юзера в чате навсегда — такую
// конфигурацию честно отключаем, а не «предупреждаем в лог».
func (b *Bot) checkReplyWatchTarget(ctx context.Context, target int64) {
	gctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	chat, err := b.api.GetChat(gctx, &telego.GetChatParams{ChatID: tu.ID(target)})
	if err != nil {
		b.log.Warn("reply watch: probe chat unavailable at startup",
			"target", target, "err", err)
		return
	}
	b.log.Info("reply watch: probe chat", "target", target,
		"type", chat.Type, "title", chat.Title)
	if chat.Type == "private" {
		return
	}
	b.log.Warn("reply watch: probe chat is not private; copies of user messages are briefly visible to its members",
		"target", target, "type", chat.Type, "username", chat.Username)
	if b.me == nil {
		return
	}
	member, err := b.api.GetChatMember(gctx, &telego.GetChatMemberParams{
		ChatID: tu.ID(target),
		UserID: b.me.ID,
	})
	if err != nil {
		b.log.Warn("reply watch: cannot verify delete rights in probe chat",
			"target", target, "err", err)
		return
	}
	if replyWatchCanDeleteCopies(member) {
		return
	}
	b.log.Error("reply watch: no right to delete probe copies — probes disabled until restart",
		"target", target, "type", chat.Type)
	b.replyWatchBlocked.Store(true)
	b.notifyReplyWatchDisabled(
		"В служебном чате для проверки ответов у бота нет права удалять сообщения — " +
			"копии пользовательских сообщений остались бы там навсегда. " +
			"Проверки ответов выключены до перезапуска: выдай право \"Удалять сообщения\" " +
			"или укажи другой REPLY_WATCH_CHAT_ID.")
}

// replyWatchCanDeleteCopies — может ли бот удалять сообщения в служебном чате.
// В обычной группе/канале это право администратора; в ЛС проверка не нужна
// (чат private сюда не доходит).
func replyWatchCanDeleteCopies(member telego.ChatMember) bool {
	switch m := member.(type) {
	case *telego.ChatMemberAdministrator:
		return m.CanDeleteMessages
	case *telego.ChatMemberOwner:
		return true
	}
	return false
}

// notifyReplyWatchDisabled — разовое ЛС владельцам о выключенной фиче.
func (b *Bot) notifyReplyWatchDisabled(reason string) {
	ctx, cancel := b.liveCtx()
	defer cancel()
	b.sendNotificationHTMLCtx(ctx, b.sortedOwnerIDs(),
		"\u26a0\ufe0f <b>Наблюдение за ответами выключено</b>\n\n"+html.EscapeString(reason),
		"reply watch disabled")
}

// startReplyWatch ставит сообщение на наблюдение. Вызывается ровно в
// победивший момент «ответ на приветствие засчитан» (middleware обычного
// сообщения и ветка правки), поэтому снимок текста — это именно та версия,
// которую увидел чат. Синхронная запись: последующий /del в ту же секунду не
// должен опередить строку.
func (b *Bot) startReplyWatch(m *telego.Message) {
	if !b.cfg.ReplyWatchEnabled || m == nil || m.From == nil || m.MessageID == 0 {
		return
	}
	if b.replyWatchBlocked.Load() {
		// Служебный чат не даёт удалять копии (или право отобрали): копировать
		// пользовательский текст без гарантии удаления нельзя.
		return
	}
	if len(b.cfg.OwnerIDs) == 0 {
		// Некому отправлять отчёты — наблюдение бессмысленно и только
		// копило бы строки/ошибки "no probe target".
		return
	}
	text := m.Text
	if text == "" {
		text = m.Caption
	}
	if text == "" {
		return
	}
	if !b.chatServiceable(m.Chat.ID) {
		return
	}
	now := time.Now()
	w := storage.ReplyWatch{
		ChatID:     m.Chat.ID,
		UserID:     m.From.ID,
		MessageID:  m.MessageID,
		Text:       text,
		AnsweredAt: now,
		Stage:      0,
		// Джиттер разводит пробы волны ответов не совпадали до
		// секунды.
		NextAt: now.Add(replyWatchOffsets[0] + replyWatchJitter()),
	}
	ctx, cancel := b.liveCtx()
	defer cancel()
	if err := b.db.PutReplyWatch(ctx, w); err != nil {
		b.log.Warn("reply watch: start", "err", err,
			"chat", w.ChatID, "user", w.UserID, "msg", w.MessageID)
		return
	}
	// Дешёвый предфильтр правок: чат, где наблюдений не было ни разу, не
	// открывает транзакцию на каждую правку своего сообщения.
	b.replyWatchChats.Store(w.ChatID, struct{}{})
	b.log.Info("reply watch: started", "chat", w.ChatID, "user", w.UserID,
		"msg", w.MessageID)
}

// replyWatchJitter — до 10 секунд, чтобы пробы волны ответов не совпадали до
// секунды.
func replyWatchJitter() time.Duration {
	return rand.N(10 * time.Second)
}

// maybeHandleReplyWatchEdit — наблюдаемое сообщение отредактировали: атомарно
// обновляем снимок и, если позволяет троттлинг, уведомляем владелеца «было →
// стало». Вызывается из handleEditedGroupMessage до reply-wait-логики: правка
// уже прошедшего ответа не должна теряться из-за фильтра осмысленности.
func (b *Bot) maybeHandleReplyWatchEdit(m *telego.Message) {
	b.log.Info("maybeHandleReplyWatchEdit called", "chat", m.Chat.ID, "msg", m.MessageID, "text", m.Text, "from", m.From.ID)
	if !b.cfg.ReplyWatchEnabled || m == nil || m.From == nil || m.MessageID == 0 {
		b.log.Info("early return - basic checks", "enabled", b.cfg.ReplyWatchEnabled, "nilFrom", m.From == nil, "nilMsgID", m.MessageID == 0)
		return
	}
	b.log.Info("passed basic checks")
	// Дешёвый предфильтр: правки в чатах, где наблюдений не было, не должны
	// открывать транзакцию (единственное соединение SQLite сериализует все
	// записи бота).
	if _, watched := b.replyWatchChats.Load(m.Chat.ID); !watched {
		b.log.Info("early return - not watched", "chat", m.Chat.ID)
		return
	}
	b.log.Info("chat is watched")
	if !b.chatServiceable(m.Chat.ID) {
		b.log.Info("early return: not serviceable", "chat", m.Chat.ID)
		b.log.Info("early return: not serviceable", "chat", m.Chat.ID)
		return
	}
	b.log.Info("passing all checks, calling ClaimReplyWatchEdit")
	text := m.Text
	if text == "" {
		text = m.Caption
	}
	ctx, cancel := b.liveCtx()
	edit, ok, err := b.db.ClaimReplyWatchEdit(ctx, m.Chat.ID, m.MessageID,
		m.From.ID, text, time.Now(), replyWatchEditCooldown,
		replyWatchEditMaxReports)
	cancel()
	if err != nil {
		b.log.Warn("reply watch: claim edit", "err", err,
			"chat", m.Chat.ID, "msg", m.MessageID)
		return
	}
	if !ok {
		// Строки нет (не наблюдаем) или правку прислал не автор — тихий no-op.
		return
	}
	if !edit.ShouldNotify {
		// Троттлинг/лимит карточек: снимок уже обновлён, отчёт об удалении
		// покажет последнюю версию.
		b.log.Info("REPLYWATCH DEBUG: edit shouldNotify=false, skipping notify")
		return
	}
	b.log.Info("reply watch: edited", "chat", edit.ChatID, "user", edit.UserID,
		"msg", edit.MessageID, "report", edit.EditReports)
	// Отправка — в фоне: отчёт о правке не должен тормозить цепочку
	// хендлеров (тот же приём, что у notifyReplyAnswered).
	b.goSafe("notifyReplyWatchEdited", func() { b.notifyReplyWatchEdited(edit) })
}

// replyWatchLoop — фоновая проба созревших наблюдений. Один тикер на весь бот:
// строк десятки, а не тысячи; последовательность проб исключает гонки и
// лишние API-вызовы из-за параллельных таймеров. Раз в час —insurance
// свип строк, переживших максимальное окно наблюдения.
func (b *Bot) replyWatchLoop(ctx context.Context) {
	ticker := time.NewTicker(replyWatchTick)
	defer ticker.Stop()
	lastPrune := time.Now()
	for {
		if time.Since(lastPrune) >= replyWatchPruneInterval {
			lastPrune = time.Now()
			if n, err := b.db.DeleteStaleReplyWatches(ctx,
				time.Now().Add(-replyWatchStaleAfter)); err != nil {
				b.log.Warn("reply watch: prune stale", "err", err)
			} else if n > 0 {
				b.log.Info("reply watch: stale watches pruned", "count", n)
			}
		}
		b.probeDueReplyWatches(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (b *Bot) probeDueReplyWatches(ctx context.Context) {
	if b.replyWatchBlocked.Load() {
		// Копии не удаляются (нет права) — пробы остановлены до перезапуска.
		return
	}
	rows, err := b.db.DueReplyWatches(ctx, time.Now(), replyWatchBatchLimit)
	if err != nil {
		b.log.Warn("reply watch: due rows", "err", err)
		return
	}
	for i := range rows {
		if ctx.Err() != nil || b.replyWatchBlocked.Load() {
			return
		}
		b.probeOneReplyWatch(ctx, rows[i])
	}
}

func (b *Bot) probeOneReplyWatch(ctx context.Context, w storage.ReplyWatch) {
	if w.Stage < 0 || w.Stage >= len(replyWatchOffsets) {
		b.finishReplyWatch(w, "stale stage")
		return
	}
	serviceable, serr := b.chatServiceableChecked(ctx, w.ChatID)
	if serr != nil {
		b.log.Warn("reply watch: serviceable check", "err", serr,
			"chat", w.ChatID, "msg", w.MessageID)
		b.deferReplyWatch(w, replyWatchTransientDefer)
		return
	}
	if !serviceable {
		b.finishReplyWatch(w, "chat unserviceable")
		return
	}
	pctx, cancel := context.WithTimeout(ctx, replyWatchProbeTimeout)
	res, delay := b.probeReplyMessage(pctx, w)
	if res == replyProbeDeleted {
		sctx, scancel := context.WithTimeout(ctx, replyWatchProbeTimeout/3)
		accessible := b.confirmSourceAccessible(sctx, w.ChatID)
		scancel()
		if !accessible {
			res = replyProbeUnknown
		}
	}
	cancel()

	switch res {
	case replyProbeRetry:
		b.deferReplyWatch(w, delay)
	case replyProbeDeleted:
		lctx, lcancel := b.liveCtx()
		taken, ok, err := b.db.TakeReplyWatch(lctx, w.ChatID, w.MessageID)
		if err != nil {
			b.log.Warn("reply watch: take deleted", "err", err,
				"chat", w.ChatID, "msg", w.MessageID)
			if err := b.db.DeferReplyWatch(lctx, w.ChatID, w.MessageID,
				time.Now().Add(replyWatchTransientDefer)); err != nil {
				b.log.Warn("reply watch: defer after take error", "err", err,
					"chat", w.ChatID, "msg", w.MessageID)
			}
			lcancel()
			return
		}
		if !ok {
			lcancel()
			return
		}
		if b.notifyReplyWatchDeleted(lctx, taken) {
			lcancel()
			return
		}
		if taken.ReportAttempts < replyWatchReportMaxAttempts {
			taken.ReportAttempts++
			taken.NextAt = time.Now().Add(replyWatchReportRetryDelay)
			if err := b.db.PutReplyWatch(lctx, taken); err != nil {
				b.log.Warn("reply watch: requeue undelivered report", "err", err,
					"chat", taken.ChatID, "msg", taken.MessageID)
			} else {
				b.log.Warn("reply watch: deletion report undelivered, retry scheduled",
					"chat", taken.ChatID, "msg", taken.MessageID,
					"attempt", taken.ReportAttempts)
			}
		} else {
			b.log.Error("reply watch: deletion report undeliverable, giving up",
				"chat", taken.ChatID, "msg", taken.MessageID,
				"attempts", taken.ReportAttempts)
		}
		lcancel()
	case replyProbeAlive, replyProbeUnknown:
		b.advanceReplyWatch(w)
	default:
		b.deferReplyWatch(w, replyWatchTransientDefer)
	}
}

func (b *Bot) deferReplyWatch(w storage.ReplyWatch, d time.Duration) {
	ctx, cancel := b.liveCtx()
	defer cancel()
	if err := b.db.DeferReplyWatch(ctx, w.ChatID, w.MessageID,
		time.Now().Add(d)); err != nil {
		b.log.Warn("reply watch: defer", "err", err,
			"chat", w.ChatID, "msg", w.MessageID)
	}
}

func (b *Bot) advanceReplyWatch(w storage.ReplyWatch) {
	nextStage, nextAt, ok := nextReplyWatchProbe(w.AnsweredAt, w.Stage, time.Now())
	ctx, cancel := b.liveCtx()
	defer cancel()
	if !ok {
		taken, took, err := b.db.TakeReplyWatch(ctx, w.ChatID, w.MessageID)
		if err != nil {
			b.log.Warn("reply watch: take finished", "err", err,
				"chat", w.ChatID, "msg", w.MessageID)
			b.deferReplyWatch(w, replyWatchTransientDefer)
			return
		}
		if took {
			b.log.Info("reply watch: survived all probes",
				"chat", w.ChatID, "user", w.UserID, "msg", w.MessageID,
				"deleted_by", taken.DeletedBy)
		}
		return
	}
	advanced, err := b.db.AdvanceReplyWatch(ctx, w.ChatID, w.MessageID,
		w.Stage, nextStage, nextAt)
	if err != nil {
		b.log.Warn("reply watch: advance", "err", err,
			"chat", w.ChatID, "msg", w.MessageID)
		b.deferReplyWatch(w, replyWatchTransientDefer)
		return
	}
	if advanced {
		b.log.Debug("reply watch: advanced", "chat", w.ChatID,
			"msg", w.MessageID, "stage", nextStage, "next_at", nextAt)
	}
}

func (b *Bot) finishReplyWatch(w storage.ReplyWatch, why string) {
	ctx, cancel := b.liveCtx()
	defer cancel()
	taken, ok, err := b.db.TakeReplyWatch(ctx, w.ChatID, w.MessageID)
	if err != nil {
		b.log.Warn("reply watch: finish", "err", err,
			"chat", w.ChatID, "msg", w.MessageID, "why", why)
		b.deferReplyWatch(w, replyWatchTransientDefer)
		return
	}
	if ok {
		b.log.Info("reply watch: finished", "chat", w.ChatID, "user", w.UserID,
			"msg", w.MessageID, "why", why, "deleted_by", taken.DeletedBy)
	}
}

// nextReplyWatchProbe — чистая логика расписания. currentStage — проба,
// которую только что исполнили. Ранний тик (секундное округление next_at)
// перевзводит ту же стадию, опоздание/рестарт перескакивает через уже
// прошедшие контрольные точки к следующей будущей.
func nextReplyWatchProbe(answeredAt time.Time, currentStage int, now time.Time) (int, time.Time, bool) {
	if currentStage < 0 || currentStage >= len(replyWatchOffsets) {
		return 0, time.Time{}, false
	}
	elapsed := now.Sub(answeredAt)
	if elapsed < 0 {
		elapsed = 0
	}
	if elapsed < replyWatchOffsets[currentStage] {
		return currentStage, answeredAt.Add(replyWatchOffsets[currentStage]), true
	}
	for i := currentStage + 1; i < len(replyWatchOffsets); i++ {
		if replyWatchOffsets[i] > elapsed {
			return i, answeredAt.Add(replyWatchOffsets[i]), true
		}
	}
	return 0, time.Time{}, false
}

// confirmSourceAccessible — вторая проверка перед вердиктом «удалено»: бот
// всё ещё состоит в чате, где жило сообщение. Если бота оттуда выгнали,
// copyMessage может ответить тем же «message to copy not found», что и на
// удалённое сообщение; членство различает эти случаи.
func (b *Bot) confirmSourceAccessible(ctx context.Context, chatID int64) bool {
	if b.me == nil {
		return false // не знаем свой id — консервативно unknown
	}
	member, err := b.api.GetChatMember(ctx, &telego.GetChatMemberParams{
		ChatID: tu.ID(chatID),
		UserID: b.me.ID,
	})
	if err != nil {
		return false
	}
	return member.MemberIsMember()
}

// replyProbeResult — исход одной пробы. targetUnavailable — отдельный
// внутренний исход: служебный чат сломан, его надо отличить от «сообщения
// нет» (иначе удаление припишется юзеру) и от прочих ошибок. retry — 429:
// пробу нельзя считать ни живой, ни удалённой, строку надо отложить.
type replyProbeResult int

const (
	replyProbeUnknown replyProbeResult = iota
	replyProbeAlive
	replyProbeDeleted
	replyProbeTargetUnavailable
	replyProbeRetry
)

func (b *Bot) probeReplyMessage(ctx context.Context, w storage.ReplyWatch) (replyProbeResult, time.Duration) {
	target := b.replyWatchTarget()
	if target == 0 {
		b.log.Warn("reply watch: no probe target", "chat", w.ChatID, "msg", w.MessageID)
		return replyProbeUnknown, 0
	}
	res, delay, err := b.copyProbe(ctx, w, target)
	if res != replyProbeTargetUnavailable {
		if res == replyProbeUnknown && err != nil {
			b.log.Warn("reply watch: probe failed (not deleted)", "err", err,
				"chat", w.ChatID, "msg", w.MessageID, "target", target)
		}
		return res, delay
	}
	if b.cfg.ReplyWatchChatID == 0 || target != b.cfg.ReplyWatchChatID {
		if b.replyWatchWarnPending.Load() && !b.replyWatchBrokenNotified.Load() &&
			b.notifyReplyWatchProbeBroken(err) {
			b.replyWatchBrokenNotified.Store(true)
			b.replyWatchWarnPending.Store(false)
		}
		b.log.Warn("reply watch: fallback target unavailable", "err", err,
			"chat", w.ChatID, "msg", w.MessageID, "target", target)
		return replyProbeUnknown, 0
	}
	if !b.confirmProbeTargetGone(ctx, target, err) {
		b.log.Warn("reply watch: destination error is not probe-chat outage", "err", err,
			"chat", w.ChatID, "msg", w.MessageID, "target", target)
		return replyProbeUnknown, 0
	}
	b.replyWatchFallback.Store(true)
	if !b.replyWatchBrokenNotified.Load() {
		if b.notifyReplyWatchProbeBroken(err) {
			b.replyWatchBrokenNotified.Store(true)
		} else {
			b.replyWatchWarnPending.Store(true)
		}
	}
	owner := b.replyWatchOwnerID()
	if owner == 0 {
		return replyProbeUnknown, 0
	}
	res, delay, ferr := b.copyProbe(ctx, w, owner)
	if res == replyProbeTargetUnavailable {
		b.log.Warn("reply watch: owner-DM fallback unavailable", "err", ferr,
			"chat", w.ChatID, "msg", w.MessageID, "owner", owner)
		return replyProbeUnknown, 0
	}
	if res == replyProbeUnknown && ferr != nil {
		b.log.Warn("reply watch: fallback probe failed (not deleted)", "err", ferr,
			"chat", w.ChatID, "msg", w.MessageID, "owner", owner)
	}
	return res, delay
}

func (b *Bot) copyProbe(ctx context.Context, w storage.ReplyWatch, target int64) (replyProbeResult, time.Duration, error) {
	copied, err := b.api.CopyMessage(ctx, &telego.CopyMessageParams{
		ChatID:              tu.ID(target),
		FromChatID:          tu.ID(w.ChatID),
		MessageID:           w.MessageID,
		DisableNotification: true,
	})
	if err != nil {
		if ra, ok := retryAfterDelay(err); ok {
			return replyProbeRetry, probeRetryDelay(ra), err
		}
		res := classifyReplyProbeError(err)
		if res == replyProbeRetry {
			return res, probeRetryDelay(0), err
		}
		return res, 0, err
	}
	if copied != nil && copied.MessageID != 0 {
		if derr := retryWith(ctx, kickUnbanBackoffs, func() error {
			return b.deleteMessage(ctx, target, copied.MessageID)
		}); derr != nil {
			fails := b.replyWatchDeleteFailures.Add(1)
			b.log.Warn("reply watch: delete probe copy", "err", derr,
				"target", target, "msg", copied.MessageID, "fails", fails)
			if fails >= replyWatchDeleteFailureLimit && !b.replyWatchBlocked.Swap(true) {
				b.log.Error("reply watch: probe copies are not being deleted — probes disabled until restart",
					"target", target, "fails", fails)
				b.notifyReplyWatchDisabled(
					"Бот не может удалять копии сообщений в служебном чате — " +
						"пользовательский текст оставался бы там навсегда. " +
						"Проверки ответов выключены до перезапуска: проверь право " +
						"«Удалять сообщения» или смени REPLY_WATCH_CHAT_ID.")
			}
		} else {
			b.replyWatchDeleteFailures.Store(0)
		}
	}
	return replyProbeAlive, 0, nil
}

func probeRetryDelay(retryAfter time.Duration) time.Duration {
	d := replyWatchTransientDefer
	if retryAfter > d {
		d = retryAfter
	}
	return d + replyWatchJitter()
}

// classifyReplyProbeError отделяет ИМЕННО удаление от всего остального.

// CopyMessage на удалённое сообщение отвечает "message to copy not found";

// protected content — unknown (контрольную точку двигаем: содержимое не

// скопировать никогда), сеть/5xx/таймаут — retry (точку НЕ сжигаем).

func classifyReplyProbeError(err error) replyProbeResult {

	if err == nil {

		return replyProbeAlive

	}

	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {

		return replyProbeRetry

	}

	desc := err.Error()

	var apiErr *telegoapi.Error

	if errors.As(err, &apiErr) {

		if apiErr.ErrorCode == 429 || apiErr.ErrorCode >= 500 {

			return replyProbeRetry

		}

		if apiErr.Description != "" {

			desc = apiErr.Description

		}

	}

	s := strings.ToLower(desc)

	switch {

	case strings.Contains(s, "message to copy not found"),

		strings.Contains(s, "message to forward not found"):

		return replyProbeDeleted

	case strings.Contains(s, "not enough rights"),

		strings.Contains(s, "chat_write_forbidden"),

		strings.Contains(s, "have no rights to send"),

		strings.Contains(s, "chat not found"),

		strings.Contains(s, "bot was kicked"),

		strings.Contains(s, "not a member of the chat"),

		strings.Contains(s, "peer_id_invalid"):

		return replyProbeTargetUnavailable

	case strings.Contains(s, "timeout"),

		strings.Contains(s, "timed out"),

		strings.Contains(s, "too many requests"),

		strings.Contains(s, "connection reset"),

		strings.Contains(s, "connection refused"),

		strings.Contains(s, "temporary failure"),

		strings.Contains(s, "eof"):

		return replyProbeRetry

	default:

		return replyProbeUnknown

	}

}

func (b *Bot) confirmProbeTargetGone(ctx context.Context, target int64, cause error) bool {
	s := strings.ToLower(cause.Error())
	if strings.Contains(s, "not enough rights") ||
		strings.Contains(s, "chat_write_forbidden") ||
		strings.Contains(s, "have no rights to send") {
		return true
	}
	_, err := b.api.GetChat(ctx, &telego.GetChatParams{ChatID: tu.ID(target)})
	if err == nil {
		return false
	}
	es := strings.ToLower(err.Error())
	for _, marker := range []string{"chat not found", "bot was kicked", "not a member of the chat", "peer_id_invalid"} {
		if strings.Contains(es, marker) {
			return true
		}
	}
	return false
}

func (b *Bot) notifyReplyWatchProbeBroken(cause error) bool {
	ctx, cancel := b.liveCtx()
	defer cancel()
	if b.replyWatchOwnerID() == 0 {
		b.log.Warn("reply watch: probe chat broken and no owner to report to", "err", cause)
		return false
	}
	text := fmt.Sprintf("\u26a0\ufe0f <b>Служебный чат для проверки ответов недоступен</b>\n"+
		"<code>%s</code>\n\nПробы переключены на ЛС владельца до перезапуска бота.\n"+
		"Проверь права бота и REPLY_WATCH_CHAT_ID.",
		html.EscapeString(truncateLabel(cause.Error(), replyWatchBrokenCauseLimit)))
	return b.sendNotificationHTMLCtx(ctx, b.sortedOwnerIDs(), text, "reply watch probe broken")
}

func (b *Bot) replyWatchTarget() int64 {
	owner := b.replyWatchOwnerID()
	if b.cfg.ReplyWatchChatID == 0 {
		return owner
	}
	if b.replyWatchFallback.Load() && owner != 0 {
		return owner
	}
	return b.cfg.ReplyWatchChatID
}

func (b *Bot) replyWatchOwnerID() int64 {
	var out int64
	for id := range b.cfg.OwnerIDs {
		if out == 0 || id < out {
			out = id
		}
	}
	return out
}

func (b *Bot) sortedOwnerIDs() []int64 {
	out := make([]int64, 0, len(b.cfg.OwnerIDs))
	for id := range b.cfg.OwnerIDs {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (b *Bot) notifyReplyWatchDeleted(ctx context.Context, w storage.ReplyWatch) bool {
	text := fmt.Sprintf("\U0001F5D1 <b>Сообщение удалено</b>\n\n"+
		"<b>Чат:</b> %s\n<b>Автор:</b> %s\n"+
		"<b>Обнаружено:</b> %s (когда именно удалили, Telegram не сообщает).\n"+
		"<b>Удалил:</b> %s\n\n<b>Текст:</b>\n%s",
		b.chatLink(ctx, w.ChatID), b.replyWatchMention(ctx, w.UserID),
		replyWatchElapsed(w.AnsweredAt, time.Now()),
		replyWatchDeletedBy(w.DeletedBy), replyWatchQuote(w.Text))
	delivered := b.sendNotificationHTMLCtx(ctx, b.sortedOwnerIDs(), text, "reply watch deleted")
	b.log.Info("reply watch: deleted", "chat", w.ChatID, "user", w.UserID,
		"msg", w.MessageID, "deleted_by", w.DeletedBy,
		"elapsed", time.Since(w.AnsweredAt).Round(time.Second),
		"delivered", delivered)
	return delivered
}

func (b *Bot) replyWatchMention(ctx context.Context, userID int64) string {
	infos, err := b.db.GetUserInfos(ctx, []int64{userID})
	if err != nil {
		b.log.Warn("reply watch: user info", "err", err, "user", userID)
		return mentionWithUsername(nil, userID)
	}
	return mentionWithUsername(infos, userID)
}

func (b *Bot) notifyReplyWatchEdited(edit storage.ReplyWatchEdit) {
	ctx, cancel := b.liveCtx()
	defer cancel()
	tail := ""
	if edit.EditReports >= replyWatchEditMaxReports {
		tail = "\n\n<i>Дальнейшие правки этого сообщения не показываю — " +
			"если его удалят, итог будет в отчёте об удалении.</i>"
	}
	text := fmt.Sprintf("\U0001F4DD <b>Сообщение отредактировано</b>\n\n"+
		"<b>Чат:</b> %s\n<b>Автор:</b> %s\n<b>Обнаружено:</b> %s\n\n"+
		"<b>Было:</b>\n%s\n\n<b>Стало:</b>\n%s%s",
		b.chatLink(ctx, edit.ChatID), b.replyWatchMention(ctx, edit.UserID),
		replyWatchElapsed(edit.AnsweredAt, time.Now()),
		replyWatchQuote(edit.OldText), replyWatchQuote(edit.Text), tail)
	if !b.sendNotificationHTMLCtx(ctx, b.sortedOwnerIDs(), text, "reply watch edited") {
		b.log.Warn("reply watch: edit report undelivered", "chat", edit.ChatID,
			"user", edit.UserID, "msg", edit.MessageID)
	}
}

func replyWatchElapsed(answeredAt, detectedAt time.Time) string {
	d := detectedAt.Sub(answeredAt)
	if d < time.Minute {
		return "сразу после ответа (меньше минуты)"
	}
	m := int(d / time.Minute)
	return fmt.Sprintf("через %d %s после ответа", m,
		pluralRU(m, "минуту", "минуты", "минут"))
}

func replyWatchDeletedBy(reason string) string {
	switch reason {
	case replyWatchDeletedByDel:
		return "бот (команда /del)"
	case replyWatchDeletedByKick:
		return "бот (команда /kick)"
	case replyWatchDeletedByBan:
		return "бот (команда /ban)"
	case replyWatchDeletedBySpam:
		return "бот (спам-бан)"
	case replyWatchDeletedByQuarantine:
		return "бот (карантин)"
	case replyWatchDeletedByCaptcha:
		return "бот (капча)"
	default:
		if strings.HasPrefix(reason, "bot:") {
			return "бот (неизвестное действие)"
		}
		return "не наш бот (юзер, админ или другой бот — неразличимо)"
	}
}

func replyWatchQuote(text string) string {
	if strings.TrimSpace(text) == "" {
		return "<blockquote><i>— (текст убран)</i></blockquote>"
	}
	esc := html.EscapeString(text)
	if utf16Units(esc) > replyWatchQuoteLimit {
		esc = truncateUTF16(esc, replyWatchQuoteLimit)
		if amp := strings.LastIndex(esc, "&"); amp >= 0 {
			if semi := strings.LastIndex(esc, ";"); semi < amp {
				esc = esc[:amp] + "…"
			}
		}
	}
	return "<blockquote>" + esc + "</blockquote>"
}

func truncateUTF16(s string, max int) string {
	if max <= 0 {
		return ""
	}
	units := utf16.Encode([]rune(s))
	if len(units) <= max {
		return s
	}
	units = units[:max-1]
	if len(units) > 0 {
		last := units[len(units)-1]
		if last >= 0xD800 && last <= 0xDBFF {
			units = units[:len(units)-1]
		}
	}
	return string(utf16.Decode(units)) + "…"
}

func (b *Bot) markReplyWatchDeletedByMessage(chatID int64, messageID int, reason string) {
	if !b.cfg.ReplyWatchEnabled {
		return
	}
	ctx, cancel := b.liveCtx()
	defer cancel()
	if err := b.db.MarkReplyWatchDeletedByMessage(ctx, chatID, messageID, reason); err != nil {
		b.log.Warn("reply watch: mark deleted by message", "err", err, "chat", chatID,
			"msg", messageID, "reason", reason)
	}
}

func (b *Bot) markReplyWatchesDeletedByUser(chatID, userID int64, reason string) {
	if !b.cfg.ReplyWatchEnabled {
		return
	}
	ctx, cancel := b.liveCtx()
	defer cancel()
	if err := b.db.MarkReplyWatchesDeletedByUser(ctx, chatID, userID, reason); err != nil {
		b.log.Warn("reply watch: mark watches deleted by user", "err", err, "chat", chatID,
			"user", userID, "reason", reason)
	}
}
