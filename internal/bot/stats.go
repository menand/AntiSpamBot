package bot

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mymmrac/telego"
	th "github.com/mymmrac/telego/telegohandler"
	tu "github.com/mymmrac/telego/telegoutil"

	"github.com/menand/AntiSpamBot/internal/storage"
)

// handleStatsCommand — no-op. Статистика и настройки доступны только через
// DM-меню (/chats → выбрать чат → экран статистики). Хендлер зарегистрирован,
// чтобы /stats в группе проглатывался без ответа бота в чат.
func (b *Bot) handleStatsCommand(_ *th.Context, _ telego.Message) error {
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
	periodDay       statsPeriod = "day"
	periodYesterday statsPeriod = "yesterday"
	periodWeek      statsPeriod = "week"
	periodMonth     statsPeriod = "month"
	periodAll       statsPeriod = "all"
)

// parsePeriod валидирует токен периода из callback data. Всё неизвестное
// (устаревшие кнопки, подделанные данные) откатывается к periodWeek — сырая
// строка не должна дойти ни до statsRange, ни до рендеренного HTML.
func parsePeriod(s string) statsPeriod {
	switch p := statsPeriod(s); p {
	case periodDay, periodYesterday, periodWeek, periodMonth, periodAll:
		return p
	}
	return periodWeek
}

// statsRange возвращает выровненные по московским суткам окна [from, until).
// События считаются по unix-времени, сообщения — по календарным дням
// (storage.DayOf, тот же пояс storage.StatsLocation); только выравнивание
// обеих границ по одной и той же полуночи держит оба счёта в одном диапазоне
// (см. QueryStats). now — параметром, чтобы граница суток тестировалась:
//
//	day       — сегодня (с 00:00 МСК)
//	yesterday — вчерашние сутки [вчера 00:00, сегодня 00:00) МСК
//	week      — последние 7 календарных суток, включая сегодня
//	month     — последние 30 календарных суток, включая сегодня
//	all       — с начала эпохи
func statsRange(p statsPeriod, now time.Time) (from, until time.Time) {
	now = now.In(storage.StatsLocation)
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, storage.StatsLocation)
	until = midnight.AddDate(0, 0, 1) // не включается: следующая полночь
	switch p {
	case periodDay:
		from = midnight
	case periodYesterday:
		// Единственный период, чей until — СЕГОДНЯШНЯЯ полночь, а не завтрашняя.
		from, until = midnight.AddDate(0, 0, -1), midnight
	case periodWeek:
		from = midnight.AddDate(0, 0, -6)
	case periodMonth:
		from = midnight.AddDate(0, 0, -29)
	case periodAll:
		from = time.Unix(0, 0)
	}
	return from, until
}

func periodLabel(p statsPeriod) string {
	switch p {
	case periodDay:
		return "сегодня"
	case periodYesterday:
		return "вчера"
	case periodMonth:
		return "месяц"
	case periodAll:
		return "всё время"
	}
	// periodWeek — и он же для неожиданных значений: parsePeriod гарантирует,
	// что сырая строка из callback data сюда не доходит.
	return "неделю"
}

// statsRuneBudget ограничивает суммарную длину пофамильных списков в
// renderStats. Лимит Telegram — 4096 символов на сообщение, но снаружи к
// статистике дописывают заголовок (тайтл чата в меню, «Сводка за сутки» в
// дайджесте, ≤ ~150 рун), а после списков идёт футер (~100 рун) плюс хвосты
// «…и ещё N» у обрезанных блоков — отсюда запас.
const statsRuneBudget = 3600

// appendUserList выводит заголовок и пронумерованный список юзеров, обрезая
// его по statsRuneBudget: не влезшие сворачиваются в «…и ещё N человек».
// Бюджет общий на всё сообщение и жадный — при огромных списках (рейд)
// блоки, идущие позже, сворачиваются первыми.
func appendUserList(sb *strings.Builder, header string, list []storage.UserCount,
	line func(i int, uc storage.UserCount) string) {
	if len(list) == 0 {
		return
	}
	sb.WriteString(header)
	used := utf8.RuneCountInString(sb.String())
	for i, uc := range list {
		l := line(i, uc)
		used += utf8.RuneCountInString(l)
		if used > statsRuneBudget {
			rest := len(list) - i
			fmt.Fprintf(sb, "…и ещё %d %s\n",
				rest, pluralRU(rest, "человек", "человека", "человек"))
			return
		}
		sb.WriteString(l)
	}
}

func renderStats(
	p statsPeriod,
	label string,
	s storage.Stats,
	newcomerDays int,
	newMembers, topWriters, topFailers, banned []storage.UserCount,
	infos map[int64]storage.UserInfo,
) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "<b>📊 Статистика за %s</b>\n\n", label)

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
	// Спам-баны — вне воронки капчи (банят уже вошедших), поэтому отдельной
	// строкой и без процента от Joined.
	if s.SpamBanned > 0 {
		fmt.Fprintf(&sb, "🤖 <b>Забанено ИИ-антиспамом:</b> %d\n", s.SpamBanned)
	}

	appendUserList(&sb, "\n🆕 <b>Новые участники:</b>\n", newMembers,
		func(i int, uc storage.UserCount) string {
			// Secs за пределами суток — мусор из битых исторических данных,
			// показываем без времени.
			if uc.Secs >= 0 && uc.Secs <= 86400 {
				return fmt.Sprintf("%d. %s — за %d сек\n",
					i+1, mentionWithUsername(infos, uc.UserID), uc.Secs)
			}
			return fmt.Sprintf("%d. %s\n", i+1, mentionWithUsername(infos, uc.UserID))
		})

	total := s.MsgNewcomer + s.MsgOldtimer
	fmt.Fprintf(&sb, "\n💬 <b>Сообщений:</b> %d\n", total)
	if total > 0 {
		fmt.Fprintf(&sb, "• Новички: %d (%s)\n", s.MsgNewcomer, pct(s.MsgNewcomer, total))
		fmt.Fprintf(&sb, "• Старички: %d (%s)\n", s.MsgOldtimer, pct(s.MsgOldtimer, total))
	}

	appendUserList(&sb, "\n🔝 <b>Топ писателей:</b>\n", topWriters,
		func(i int, uc storage.UserCount) string {
			return fmt.Sprintf("%d. %s — %d %s\n",
				i+1, mentionWithUsername(infos, uc.UserID),
				uc.Count, pluralRU(uc.Count, "сообщение", "сообщения", "сообщений"))
		})

	appendUserList(&sb, "\n🚫 <b>Провалили капчу:</b>\n", topFailers,
		func(i int, uc storage.UserCount) string {
			return fmt.Sprintf("%d. %s — %d %s\n",
				i+1, mentionWithUsername(infos, uc.UserID),
				uc.Count, pluralRU(uc.Count, "раз", "раза", "раз"))
		})

	appendUserList(&sb, "\n⛔️ <b>Забанены:</b>\n", banned,
		func(i int, uc storage.UserCount) string {
			return fmt.Sprintf("%d. %s\n", i+1, mentionWithUsername(infos, uc.UserID))
		})

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
