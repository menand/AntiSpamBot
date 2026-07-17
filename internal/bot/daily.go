package bot

import (
	"context"
	"errors"
	"fmt"
	"html"
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
		line := reportLine(titleOrID(c), s)
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
		WithParseMode(telego.ModeHTML)); err != nil {
		// 403 = ЛС закрыта навсегда (юзер заблокировал бота / аккаунт удалён) —
		// отписываем, иначе ретрай долбился бы в закрытую ЛС каждый тик до
		// скончания веков.
		var apiErr *telegoapi.Error
		if errors.As(err, &apiErr) && apiErr.ErrorCode == 403 {
			b.log.Info("dm report: user blocked bot, unsubscribing", "user", userID)
			_ = b.db.SetDailyReport(ctx, userID, false)
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
func reportLine(title string, s storage.Stats) string {
	title = truncateLabel(title, 60) // рун-безопасно; простыня в названии чата не съест бюджет отчёта
	banned := s.Banned + s.SpamBanned
	if s.Joined+s.Passed+s.Kicked+banned == 0 {
		return fmt.Sprintf("«%s» — без событий", html.EscapeString(title))
	}
	line := fmt.Sprintf("«%s» — вступило %d, прошло %d, кик %d, бан %d",
		html.EscapeString(title), s.Joined, s.Passed, s.Kicked, banned)
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
	today := storage.DayOf(until)

	s, err := b.db.QueryStats(ctx, chatID, from, until)
	if err != nil {
		b.log.Warn("daily digest: query stats", "err", err, "chat", chatID)
		return
	}
	topWriters, err := b.db.TopWriters(ctx, chatID, from, until, 5)
	if err != nil {
		b.log.Warn("daily digest: top writers", "err", err, "chat", chatID)
	}
	// -1 = без лимита (SQLite: LIMIT -1); длину сообщения режет renderStats.
	topFailers, err := b.db.TopFailers(ctx, chatID, from, until, -1)
	if err != nil {
		b.log.Warn("daily digest: top failers", "err", err, "chat", chatID)
	}
	newMembers, err := b.db.PassedUsers(ctx, chatID, from, until)
	if err != nil {
		b.log.Warn("daily digest: new members", "err", err, "chat", chatID)
	}
	banned, err := b.db.EventUsers(ctx, chatID, from, until, storage.EventBan, storage.EventSpamBan)
	if err != nil {
		b.log.Warn("daily digest: banned users", "err", err, "chat", chatID)
	}

	// Нечего рассказывать — чат затих, не спамим пустой сводкой.
	if !digestHasContent(s, topWriters, topFailers, newMembers, banned) {
		// Но помечаем отправленной, чтобы не перепроверять десятки раз за день.
		_ = b.db.MarkDailyStatsSent(ctx, chatID, today)
		return
	}

	infos, err := b.db.GetUserInfos(ctx,
		collectUserIDs(topWriters, topFailers, newMembers, banned))
	if err != nil {
		b.log.Warn("daily digest: user infos", "err", err, "chat", chatID)
		infos = map[int64]storage.UserInfo{}
	}

	header := "🌅 <b>Сводка за сутки</b>\n\n"
	body := renderStats(periodYesterday, "вчера", s, b.cfg.NewcomerDays,
		newMembers, topWriters, topFailers, banned, infos)

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
	b.log.Info("daily digest sent",
		"chat", chatID,
		"messages", s.MsgNewcomer+s.MsgOldtimer,
		"joined", s.Joined)
}

// digestHasContent отвечает, покажет ли сводка хоть что-нибудь. Обязан
// покрывать каждый счётчик и список, которые печатает renderStats: день, где
// единственное событие — прохождение капчи или спам-бан, всё равно
// заслуживает сводки (join мог остаться за окном: вошёл в 23:59, прошёл
// капчу в 00:01).
func digestHasContent(s storage.Stats, topWriters, topFailers, newMembers, banned []storage.UserCount) bool {
	return s.Joined+s.Passed+s.Kicked+s.Banned+s.SpamBanned+
		s.MsgNewcomer+s.MsgOldtimer+
		len(topWriters)+len(topFailers)+len(newMembers)+len(banned) > 0
}
