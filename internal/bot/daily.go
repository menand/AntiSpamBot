package bot

import (
	"context"
	"time"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"

	"github.com/menand/AntiSpamBot/internal/storage"
)

// dailyDigestLoop runs as a background goroutine for the lifetime of the bot.
// Every 5 minutes it checks for chats that have opted into daily stats and
// haven't yet received today's digest, then posts one. The digest summarises
// the last 24 hours and highlights top writers / top captcha-failers.
//
// The time of first post is controlled by DAILY_STATS_UTC_HOUR (default 06:00
// UTC ≈ 09:00 MSK). The ticker runs unconditionally; the hour-of-day check
// inside the handler acts as the gate.
func (b *Bot) dailyDigestLoop(ctx context.Context) {
	// Check quickly once at startup so freshly-enabled chats don't wait up to
	// 5 minutes for the first tick.
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
	if now.Hour() < b.cfg.DailyStatsUTCHour {
		return
	}
	today := now.Format("2006-01-02")
	chatIDs, err := b.db.ChatsNeedingDailyStats(ctx, today)
	if err != nil {
		b.log.Warn("daily digest: query chats", "err", err)
		return
	}
	for _, chatID := range chatIDs {
		b.sendDailyDigest(ctx, chatID, today)
	}
}

func (b *Bot) sendDailyDigest(ctx context.Context, chatID int64, today string) {
	until := time.Now()
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
	topFailers, err := b.db.TopFailers(ctx, chatID, from, until, 5)
	if err != nil {
		b.log.Warn("daily digest: top failers", "err", err, "chat", chatID)
	}

	// Skip entirely if nothing to report — chat went quiet, don't spam.
	total := s.Joined + s.MsgNewcomer + s.MsgOldtimer + len(topFailers)
	if total == 0 {
		// Still mark as sent so we don't re-check dozens of times today.
		_ = b.db.MarkDailyStatsSent(ctx, chatID, today)
		return
	}

	infos, err := b.db.GetUserInfos(ctx, collectUserIDs(topWriters, topFailers))
	if err != nil {
		b.log.Warn("daily digest: user infos", "err", err, "chat", chatID)
		infos = map[int64]storage.UserInfo{}
	}

	header := "🌅 <b>Сводка за сутки</b>\n\n"
	body := renderStats(periodDay, s, b.cfg.NewcomerDays, topWriters, topFailers, infos)

	_, err = b.api.SendMessage(ctx,
		tu.Message(tu.ID(chatID), header+body).
			WithParseMode(telego.ModeHTML))
	if err != nil {
		b.log.Warn("daily digest: send", "err", err, "chat", chatID)
		// Don't mark as sent — we'll retry on the next tick. If the chat
		// permanently blocks the bot, my_chat_member cleanup will kick in.
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

