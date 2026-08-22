package bot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mymmrac/telego"
	"github.com/mymmrac/telego/telegoapi"
	tu "github.com/mymmrac/telego/telegoutil"

	"github.com/menand/AntiSpamBot/internal/storage"
)

// dailyDigestLoop — фоновая горутина на всё время жизни бота. Раз в 5 минут
// проверяет, есть ли чаты с включённой ежедневной сводкой, ещё не получившие
// сегодняшнюю, и постит её. Сводка охватывает последние сутки: топ писателей,
// топ провалов капчи и т.д.
//
// Время первой отправки задаёт DAILY_STATS_UTC_HOUR (дефолт 06:00 UTC ≈
// 09:00 МСК). Тикер крутится безусловно; гейтом служит проверка часа внутри.
func (b *Bot) dailyDigestLoop(ctx context.Context) {
	// Один быстрый прогон на старте, чтобы свежевключённые чаты не ждали
	// первого тика до 5 минут.
	b.maybeSendDigests(ctx)
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.maybeSendDigests(ctx)
		}
	}
}

func (b *Bot) maybeSendDigests(ctx context.Context) {
	// Час хранится в UTC, но гейт сравнивается в МСК-часах — почему это
	// обязательно, объяснено у SQL-сдвига в ChatsNeedingDailyStats.
	now := time.Now().In(storage.StatsLocation)
	today := storage.DayOf(now)
	// Окно дайджеста — ровно то же «вчера», что у кнопки «Вчера» в меню:
	// один источник, расхождение цифр структурно невозможно.
	from, until := statsRange(periodYesterday, now)
	chatIDs, err := b.db.ChatsNeedingDailyStats(ctx, now.Hour(), b.cfg.DailyStatsUTCHour, today)
	if err != nil {
		b.log.Warn("daily digest: query chats", "err", err)
		return
	}
	for _, chatID := range chatIDs {
		// Сводка — пер-чатовая активность: только для обслуживаемых чатов
		// (ALLOWED_CHATS + подтверждён владельцем). Защита от призрачных
		// рассылок, если chat_settings пережили уход бота или запись
		// отключения сводки при reject не прошла.
		if !b.chatServiceable(chatID) {
			continue
		}
		b.sendDailyDigest(ctx, chatID, from, until)
	}
	b.maybeSendDMReports(ctx, now, from, until, today)
}

// maybeSendDMReports — утренняя ЛС-сводка за вчера подписчикам (тумблер
// «📬 Итог дня в ЛС»): владельцу — по всем чатам, админу — по его чатам.
// Час общий с чат-дайджестом (DAILY_STATS_UTC_HOUR), окно то же «вчера».
func (b *Bot) maybeSendDMReports(ctx context.Context, now time.Time, from, until time.Time, today string) {
	if now.Hour() < (b.cfg.DailyStatsUTCHour+storage.MSKOffsetHours)%24 {
		return
	}
	subs, err := b.db.DailyReportSubscribers(ctx)
	if err != nil {
		b.log.Warn("dm report: query subscribers", "err", err)
		return
	}
	for _, sub := range subs {
		if sub.LastDay == today {
			continue
		}
		b.sendDMReport(ctx, sub.UserID, from, until, today)
	}
}

// reportRuneBudget — потолок длины ЛС-отчёта (лимит Telegram 4096; запас на
// заголовок и хвост «и ещё N»). Без него владелец с десятками чатов ловил бы
// MESSAGE_TOO_LONG, а неотмеченный день ретраил бы ту же ошибку весь день.
const reportRuneBudget = 3500

func (b *Bot) sendDMReport(ctx context.Context, userID int64, from, until time.Time, today string) {
	chats, err := b.userChats(ctx, userID)
	if err != nil {
		b.log.Warn("dm report: user chats", "err", err, "user", userID)
		return
	}
	if len(chats) == 0 {
		// Пусто = либо юзера правда разжаловали везде, либо getChatMember
		// временно падал (userChats не различает). День НЕ закрываем: ретрай
		// на следующем тике дёшев (кэш админства), а закрытие по сбою API
		// молча съело бы отчёт.
		b.log.Debug("dm report: no chats for subscriber", "user", userID)
		return
	}

	var sb strings.Builder
	sb.WriteString("📬 <b>Итог дня по чатам</b> (вчера)\n")
	budget := reportRuneBudget
	for i, c := range chats {
		s, err := b.db.QueryStats(ctx, c.ChatID, from, until)
		if err != nil {
			b.log.Warn("dm report: query stats", "err", err, "chat", c.ChatID)
			continue
		}
		line := reportLine(c, s)
		if n := len([]rune(line)); n > budget {
			fmt.Fprintf(&sb, "\n… и ещё %d %s", len(chats)-i,
				pluralRU(len(chats)-i, "чат", "чата", "чатов"))
			break
		} else {
			budget -= n
		}
		sb.WriteString("\n" + line)
	}

	if _, err := b.api.SendMessage(ctx, tu.Message(tu.ID(userID), sb.String()).
		WithParseMode(telego.ModeHTML).
		WithLinkPreviewOptions(&telego.LinkPreviewOptions{IsDisabled: true})); err != nil {
		// 403 = ЛС закрыта навсегда (юзер заблокировал бота / аккаунт удалён) —
		// отписываем, иначе ретрай долбился бы в закрытую ЛС каждый тик до
		// скончания веков.
		var apiErr *telegoapi.Error
		if errors.As(err, &apiErr) && apiErr.ErrorCode == 403 {
			b.log.Info("dm report: user blocked bot, unsubscribing", "user", userID)
			if err := b.db.SetDailyReport(ctx, userID, false); err != nil {
				b.log.Warn("dm report: unsubscribe", "err", err, "user", userID)
			}
			return
		}
		// Прочие ошибки — не помечаем: ретрай на следующем тике до конца дня
		// (семантика чат-дайджеста).
		b.log.Warn("dm report: send", "err", err, "user", userID)
		return
	}
	if err := b.db.MarkDailyReportSent(ctx, userID, today); err != nil {
		b.log.Warn("dm report: mark sent", "err", err, "user", userID)
	}
	b.log.Info("dm report sent", "user", userID, "chats", len(chats))
}

// reportLine — однострочная сводка чата для ЛС-отчёта: только счётчики, без
// имён и топов («без излишних деталей»). Бан = обычные + спам-вердикты.
// Пустота — как в digestHasContent: день с сообщениями, но без событий
// воронки не называется «без событий».
func reportLine(c storage.ChatInfo, s storage.Stats) string {
	c.Title = truncateLabel(titleOrID(c), 60) // рун-безопасно; простыня в названии чата не съест бюджет отчёта
	link := chatLinkHTML(c)
	banned := s.Banned + s.SpamBanned
	modActs := s.ModKicked + s.ModBanned
	msgs := s.MsgNewcomer + s.MsgOldtimer
	funnel := s.Joined + s.Passed + s.Kicked + s.Left + banned + modActs + s.Aborted
	switch {
	case funnel+msgs == 0:
		return fmt.Sprintf("%s — без событий", link)
	case funnel == 0:
		return fmt.Sprintf("%s — сообщений: %d, событий воронки нет", link, msgs)
	}
	line := fmt.Sprintf("%s — вступило %d, прошло %d, вышли сами %d, кик %d, бан %d",
		link, s.Joined, s.Passed, s.Left, s.Kicked, banned)
	if modActs > 0 {
		line += fmt.Sprintf(", админ-команды %d", modActs)
	}
	if s.Aborted > 0 {
		line += fmt.Sprintf(", не проверено %d", s.Aborted)
	}
	if s.SpamBanned > 0 {
		line += fmt.Sprintf(" (из них спам %d)", s.SpamBanned)
	}
	return line
}

// sendDailyDigest постит сводку за московские сутки [from, until) —
// окно вычислено ОДИН раз в maybeSendDigests из того же показания часов,
// что и гейт-маркер (повторный time.Now() здесь когда-то отправлял один
// день дважды, если 5-минутный тик пересекал полночь). Маркер отправки —
// день `until`: полночь МСК принадлежит наступившим суткам.
func (b *Bot) sendDailyDigest(ctx context.Context, chatID int64, from, until time.Time) {
	// In-process дедуп: сводка за этот день уже уходила в текущем процессе.
	b.digestMu.Lock()
	sent := b.digestSent[chatID]
	b.digestMu.Unlock()
	today := storage.DayOf(until)
	if sent == today {
		return
	}

	v, err := b.loadStatsView(ctx, chatID, from, until)
	if err != nil {
		b.log.Warn("daily digest: query stats", "err", err, "chat", chatID)
		return
	}

	// Нечего рассказывать — чат затих, не спамим пустой сводкой.
	if !digestHasContent(v.s, v.topWriters, v.topFailers, v.newMembers, v.banned) {
		// Но помечаем отправленной, чтобы не перепроверять десятки раз за день.
		if err := b.db.MarkDailyStatsSent(ctx, chatID, today); err != nil {
			b.log.Warn("daily digest: mark sent (empty)", "err", err, "chat", chatID)
		}
		b.digestMu.Lock()
		b.digestSent[chatID] = today
		b.digestMu.Unlock()
		return
	}

	header := "🌅 <b>Сводка за сутки</b>\n\n"
	body := renderStats(periodYesterday, "вчерашний день", v.s, b.cfg.NewcomerDays,
		v.newMembers, v.topWriters, v.topFailers, v.banned, v.infos)

	_, err = b.api.SendMessage(ctx,
		tu.Message(tu.ID(chatID), header+body).
			WithParseMode(telego.ModeHTML))
	if err != nil {
		b.log.Warn("daily digest: send", "err", err, "chat", chatID)
		// Не помечаем отправленной — ретрай на следующем тике. Если чат
		// навсегда заблокировал бота, сработает чистка по my_chat_member.
		return
	}
	if err := b.db.MarkDailyStatsSent(ctx, chatID, today); err != nil {
		b.log.Warn("daily digest: mark sent", "err", err, "chat", chatID)
	}
	// In-process маркер поверх БД: если запись маркера упала после успешной
	// отправки, следующий тик (5 мин) без него разослал бы тот же дайджест
	// заново — до полуночи. БД остаётся авторитетом между рестартами.
	b.digestMu.Lock()
	b.digestSent[chatID] = today
	b.digestMu.Unlock()
	b.log.Info("daily digest sent",
		"chat", chatID,
		"messages", v.s.MsgNewcomer+v.s.MsgOldtimer,
		"joined", v.s.Joined)
}

// digestHasContent отвечает, покажет ли сводка хоть что-нибудь. Обязан
// покрывать каждый счётчик и список, которые печатает renderStats: день, где
// единственное событие — прохождение капчи или спам-бан, всё равно
// заслуживает сводки (join мог остаться за окном: вошёл в 23:59, прошёл
// капчу в 00:01).
func digestHasContent(s storage.Stats, topWriters, topFailers, newMembers, banned []storage.UserCount) bool {
	return s.Joined+s.Passed+s.Kicked+s.Banned+s.ModKicked+s.ModBanned+s.SpamBanned+s.Left+s.Aborted+
		s.MsgNewcomer+s.MsgOldtimer+
		len(topWriters)+len(topFailers)+len(newMembers)+len(banned) > 0
}
