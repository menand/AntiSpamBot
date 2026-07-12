package bot

import (
	"context"
	"time"

	"github.com/mymmrac/telego"
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
	now := time.Now().UTC()
	today := now.Format("2006-01-02")
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	chatIDs, err := b.db.ChatsNeedingDailyStats(ctx, now.Hour(), b.cfg.DailyStatsUTCHour, today)
	if err != nil {
		b.log.Warn("daily digest: query chats", "err", err)
		return
	}
	for _, chatID := range chatIDs {
		b.sendDailyDigest(ctx, chatID, midnight)
	}
}

// sendDailyDigest постит сводку за календарный день перед `until`
// (сегодняшняя полночь UTC, вычисленная ОДИН раз в maybeSendDigests из того
// же показания часов, что и гейт-маркер `today` — повторный time.Now() здесь
// когда-то отправлял один день дважды, если 5-минутный тик пересекал полночь).
func (b *Bot) sendDailyDigest(ctx context.Context, chatID int64, until time.Time) {
	today := until.Format("2006-01-02")
	from := until.Add(-24 * time.Hour)

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
	body := renderStats(periodDay, "вчера", s, b.cfg.NewcomerDays,
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
