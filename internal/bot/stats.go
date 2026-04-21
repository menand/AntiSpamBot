package bot

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
	tu "github.com/mymmrac/telego/telegoutil"

	"github.com/menand/AntiSpamBot/internal/storage"
)

func (b *Bot) handleStatsCommand(ctx *th.Context, message telego.Message) error {
	if message.Chat.Type != "group" && message.Chat.Type != "supergroup" {
		return nil
	}
	if !b.chatAllowed(message.Chat.ID) {
		return nil
	}
	if message.From == nil {
		return nil
	}

	isAdmin, err := b.isChatAdmin(ctx, message.Chat.ID, message.From.ID)
	if err != nil {
		b.log.Warn("check admin", "err", err)
		return nil
	}
	if !isAdmin {
		_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(message.Chat.ID),
			"Команда доступна только администраторам чата.").
			WithReplyParameters(&telego.ReplyParameters{MessageID: message.MessageID}))
		return nil
	}

	period := parseStatsPeriod(message.Text)
	from, until := statsRange(period)

	s, err := b.db.QueryStats(ctx, message.Chat.ID, from, until)
	if err != nil {
		b.log.Error("query stats", "err", err)
		return nil
	}

	topWriters, err := b.db.TopWriters(ctx, message.Chat.ID, from, until, 5)
	if err != nil {
		b.log.Warn("top writers", "err", err)
	}
	topFailers, err := b.db.TopFailers(ctx, message.Chat.ID, from, until, 5)
	if err != nil {
		b.log.Warn("top failers", "err", err)
	}

	infos, err := b.db.GetUserInfos(ctx, collectUserIDs(topWriters, topFailers))
	if err != nil {
		b.log.Warn("get user infos", "err", err)
		infos = map[int64]storage.UserInfo{}
	}

	text := renderStats(period, s, b.cfg.NewcomerDays, topWriters, topFailers, infos)
	_, _ = b.api.SendMessage(ctx, tu.Message(tu.ID(message.Chat.ID), text).
		WithParseMode(telego.ModeHTML).
		WithReplyParameters(&telego.ReplyParameters{MessageID: message.MessageID}))
	return nil
}

func collectUserIDs(lists ...[]storage.UserCount) []int64 {
	seen := make(map[int64]struct{})
	var out []int64
	for _, l := range lists {
		for _, uc := range l {
			if _, ok := seen[uc.UserID]; ok {
				continue
			}
			seen[uc.UserID] = struct{}{}
			out = append(out, uc.UserID)
		}
	}
	return out
}

func (b *Bot) isChatAdmin(ctx context.Context, chatID, userID int64) (bool, error) {
	m, err := b.api.GetChatMember(ctx, &telego.GetChatMemberParams{
		ChatID: tu.ID(chatID),
		UserID: userID,
	})
	if err != nil {
		return false, err
	}
	status := m.MemberStatus()
	return status == "creator" || status == "administrator", nil
}

type statsPeriod string

const (
	periodDay   statsPeriod = "day"
	periodWeek  statsPeriod = "week"
	periodMonth statsPeriod = "month"
	periodAll   statsPeriod = "all"
)

func parseStatsPeriod(cmd string) statsPeriod {
	fields := strings.Fields(cmd)
	if len(fields) < 2 {
		return periodWeek
	}
	switch strings.ToLower(fields[1]) {
	case "day", "сутки", "день":
		return periodDay
	case "week", "неделя":
		return periodWeek
	case "month", "месяц":
		return periodMonth
	case "all", "все", "всё":
		return periodAll
	}
	return periodWeek
}

func statsRange(p statsPeriod) (from, until time.Time) {
	until = time.Now().Add(time.Minute) // exclusive upper bound, give a tiny buffer
	switch p {
	case periodDay:
		from = until.Add(-24 * time.Hour)
	case periodWeek:
		from = until.Add(-7 * 24 * time.Hour)
	case periodMonth:
		from = until.Add(-30 * 24 * time.Hour)
	case periodAll:
		from = time.Unix(0, 0)
	}
	return from, until
}

func periodLabel(p statsPeriod) string {
	switch p {
	case periodDay:
		return "сутки"
	case periodWeek:
		return "неделю"
	case periodMonth:
		return "месяц"
	case periodAll:
		return "всё время"
	}
	return string(p)
}

func renderStats(
	p statsPeriod,
	s storage.Stats,
	newcomerDays int,
	topWriters, topFailers []storage.UserCount,
	infos map[int64]storage.UserInfo,
) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "<b>📊 Статистика за %s</b>\n\n", periodLabel(p))

	fmt.Fprintf(&sb, "👋 <b>Новых участников:</b> %d\n", s.Joined)
	if s.Joined > 0 {
		fmt.Fprintf(&sb, "• Прошли капчу: %d (%s)\n", s.Passed, pct(s.Passed, s.Joined))
		fmt.Fprintf(&sb, "• Кикнуты: %d (%s)\n", s.Kicked, pct(s.Kicked, s.Joined))
		fmt.Fprintf(&sb, "• Забанены: %d (%s)\n", s.Banned, pct(s.Banned, s.Joined))
		pending := s.Joined - s.Passed - s.Kicked - s.Banned
		if pending > 0 {
			fmt.Fprintf(&sb, "• В процессе: %d\n", pending)
		}
	}

	total := s.MsgNewcomer + s.MsgOldtimer
	fmt.Fprintf(&sb, "\n💬 <b>Сообщений:</b> %d\n", total)
	if total > 0 {
		fmt.Fprintf(&sb, "• Новички: %d (%s)\n", s.MsgNewcomer, pct(s.MsgNewcomer, total))
		fmt.Fprintf(&sb, "• Старички: %d (%s)\n", s.MsgOldtimer, pct(s.MsgOldtimer, total))
	}

	if len(topWriters) > 0 {
		sb.WriteString("\n🔝 <b>Топ писателей:</b>\n")
		for i, uc := range topWriters {
			fmt.Fprintf(&sb, "%d. %s — %d %s\n",
				i+1, mentionOrID(infos, uc.UserID),
				uc.Count, pluralRU(uc.Count, "сообщение", "сообщения", "сообщений"))
		}
	}

	if len(topFailers) > 0 {
		sb.WriteString("\n🚫 <b>Топ провалов капчи:</b>\n")
		for i, uc := range topFailers {
			fmt.Fprintf(&sb, "%d. %s — %d %s\n",
				i+1, mentionOrID(infos, uc.UserID),
				uc.Count, pluralRU(uc.Count, "раз", "раза", "раз"))
		}
	}

	if p != periodAll {
		fmt.Fprintf(&sb, "\n<i>Новичок — тот, кто прошёл капчу за последние %d дн.</i>", newcomerDays)
	}
	if p == periodAll {
		fmt.Fprintf(&sb, "\n<i>Статистика собирается с момента запуска бота в этом чате.</i>")
	}

	return sb.String()
}

func pct(part, total int) string {
	if total == 0 {
		return "0%"
	}
	return fmt.Sprintf("%d%%", part*100/total)
}
