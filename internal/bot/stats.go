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

// collectUserIDs — id всех юзеров из списков ПЛЮС id, спрятанные в причинах
// (админы команд, голосовавшие), чтобы renderStats резолвил их имена из
// одного infos-запроса.
func collectUserIDs(lists ...[]storage.UserCount) []int64 {
	seen := make(map[int64]struct{})
	var out []int64
	add := func(id int64) {
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	for _, l := range lists {
		for _, uc := range l {
			add(uc.UserID)
		}
	}
	for _, id := range reasonUserIDs(lists...) {
		add(id)
	}
	return out
}

// statsView — всё, что нужно отрисовке статистики за период: и меню
// (renderChatStats), и дайджест (sendDailyDigest) питаются из одной загрузки,
// чтобы новый блок не приходилось добавлять в два места.
type statsView struct {
	s                      storage.Stats
	topWriters, topFailers []storage.UserCount
	newMembers, banned     []storage.UserCount
	infos                  map[int64]storage.UserInfo
}

// loadStatsView грузит счётчики, списки и имена одним куском. Ошибка
// QueryStats возвращается вызывающему (оба прерывают отрисовку); ошибки
// остальных запросов лишь логируются — блоки с пустыми списками лучше
// частичной статистики. renderStats сам режет длину, поэтому лимит топов
// тот же, что был у обоих вызывающих: 5 писателей, все фейлеры (-1).
func (b *Bot) loadStatsView(ctx context.Context, chatID int64, from, until time.Time) (statsView, error) {
	v := statsView{infos: map[int64]storage.UserInfo{}}

	s, err := b.db.QueryStats(ctx, chatID, from, until)
	if err != nil {
		return v, fmt.Errorf("query stats: %w", err)
	}
	v.s = s

	if v.topWriters, err = b.db.TopWriters(ctx, chatID, from, until, 5); err != nil {
		b.log.Warn("stats view: top writers", "err", err, "chat", chatID)
	}
	// -1 = без лимита (SQLite: LIMIT -1); длину сообщения режет renderStats.
	if v.topFailers, err = b.db.TopFailers(ctx, chatID, from, until, -1); err != nil {
		b.log.Warn("stats view: top failers", "err", err, "chat", chatID)
	}
	if v.newMembers, err = b.db.PassedUsers(ctx, chatID, from, until); err != nil {
		b.log.Warn("stats view: passed users", "err", err, "chat", chatID)
	}
	if v.banned, err = b.db.EventUsers(ctx, chatID, from, until, storage.EventBan, storage.EventSpamBan); err != nil {
		b.log.Warn("stats view: banned users", "err", err, "chat", chatID)
	}
	if v.infos, err = b.db.GetUserInfos(ctx,
		collectUserIDs(v.topWriters, v.topFailers, v.newMembers, v.banned)); err != nil {
		b.log.Warn("stats view: user infos", "err", err, "chat", chatID)
		v.infos = map[int64]storage.UserInfo{}
	}
	return v, nil
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
	periodDayBefore statsPeriod = "daybefore"
	periodWeek      statsPeriod = "week"
	periodMonth     statsPeriod = "month"
	periodAll       statsPeriod = "all"
)

// parsePeriod валидирует токен периода из callback data. Всё неизвестное
// (устаревшие кнопки, подделанные данные) откатывается к periodWeek — сырая
// строка не должна дойти ни до statsRange, ни до рендеренного HTML.
func parsePeriod(s string) statsPeriod {
	switch p := statsPeriod(s); p {
	case periodDay, periodYesterday, periodDayBefore, periodWeek, periodMonth, periodAll:
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
//	daybefore — позавчерашние сутки [позавчера 00:00, вчера 00:00) МСК
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
		// У yesterday/daybefore until — не завтрашняя полночь, а конец их суток.
		from, until = midnight.AddDate(0, 0, -1), midnight
	case periodDayBefore:
		from, until = midnight.AddDate(0, 0, -2), midnight.AddDate(0, 0, -1)
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
		// «За вчера» — просторечие; родительный падеж с предлогом «за»
		// требует винительного: «за вчерашний день».
		return "вчерашний день"
	case periodDayBefore:
		// Тот же грамматический случай, что и «за вчера» выше.
		return "позавчерашний день"
	case periodMonth:
		return "месяц"
	case periodAll:
		return "всё время"
	case periodWeek:
		return "неделю"
	}
	// Для неожиданных значений: parsePeriod гарантирует,
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
		if s.Left > 0 {
			fmt.Fprintf(&sb, "• Вышли сами: %d (%s)\n", s.Left, pct(s.Left, s.Joined))
		}
		if s.Aborted > 0 {
			// Сорванные по вине бота проверки — не «вышли сами»: юзер остался
			// в чате и был выпущен без капчи.
			fmt.Fprintf(&sb, "• Не удалось проверить: %d\n", s.Aborted)
		}
		// Командные кики/баны тоже вычитаются: новичок, командой убранный в
		// том же окне, не должен остаться «в процессе». Перевычитка (команда
		// по старожилу) даёт отрицательное — гасится условием pending > 0.
		pending := s.Joined - s.Passed - s.Kicked - s.Banned -
			s.ModKicked - s.ModBanned - s.Left - s.Aborted
		if pending > 0 {
			fmt.Fprintf(&sb, "• В процессе: %d\n", pending)
		}
	}
	// Спам-баны — вне воронки капчи (банят уже вошедших), поэтому отдельной
	// строкой и без процента от Joined.
	if s.SpamBanned > 0 {
		fmt.Fprintf(&sb, "🤖 <b>Забанено ИИ-антиспамом:</b> %d\n", s.SpamBanned)
	}
	// Команды админов (/kick /ban) — тоже вне воронки: их жертва чаще всего
	// не из «новых участников» окна, процент от Joined был бы ложью.
	if s.ModKicked > 0 || s.ModBanned > 0 {
		fmt.Fprintf(&sb, "🛡 <b>Командами админов:</b> кикнуто %d, забанено %d\n",
			s.ModKicked, s.ModBanned)
	}

	appendUserList(&sb, "\n🆕 <b>Новые участники:</b>\n", newMembers,
		func(i int, uc storage.UserCount) string {
			// Ниже минуты — честные секунды («решил подозрительно быстро»
			// читается именно тут), выше — человеческие минуты/часы. Secs за
			// пределами суток — мусор из битых исторических данных, показываем
			// без времени.
			if uc.Secs >= 0 && uc.Secs <= 86400 {
				d := time.Duration(uc.Secs) * time.Second
				if d < time.Minute {
					return fmt.Sprintf("%d. %s — за %d сек\n",
						i+1, mentionWithUsername(infos, uc.UserID), uc.Secs)
				}
				return fmt.Sprintf("%d. %s — за %s\n",
					i+1, mentionWithUsername(infos, uc.UserID), humanDurationRU(d))
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

	appendUserList(&sb, "\n🚫 <b>Кикнуты/забанены:</b>\n", topFailers,
		func(i int, uc storage.UserCount) string {
			return fmt.Sprintf("%d. %s — %d %s%s\n",
				i+1, mentionWithUsername(infos, uc.UserID),
				uc.Count, pluralRU(uc.Count, "раз", "раза", "раз"),
				reasonSuffix(uc.LastReason, infos))
		})

	// Список мерджит ban+spamban (капча-баны + вердикты ИИ-антиспама) — суффикс
	// в заголовке, чтобы числа не «расходились» с пулей воронки выше (та
	// считает только капча-баны, и это осознанное разделение).
	appendUserList(&sb, "\n⛔️ <b>Забанены (вкл. ИИ-антиспам):</b>\n", banned,
		func(i int, uc storage.UserCount) string {
			return fmt.Sprintf("%d. %s%s\n", i+1,
				mentionWithUsername(infos, uc.UserID),
				reasonSuffix(uc.LastReason, infos))
		})

	if p != periodAll {
		fmt.Fprintf(&sb, "\n<i>Новичок — тот, кто прошёл капчу за последние %d дн.</i>", newcomerDays)
	}
	if p == periodAll {
		fmt.Fprintf(&sb, "\n<i>События хранятся за последние ~180 дней, счётчики сообщений — за всё время.</i>")
	}

	return sb.String()
}

// reasonSuffix — « — причина» для строки статистики, резолвя имена из уже
// загруженного infos (без похода в БД). Пустая причина → "".
func reasonSuffix(reason string, infos map[int64]storage.UserInfo) string {
	human := humanReasonWith(reason, func(ids []int64) map[int64]storage.UserInfo { return infos })
	if human == "" {
		return ""
	}
	return " — " + human
}

func pct(part, total int) string {
	if total == 0 {
		return "0%"
	}
	return fmt.Sprintf("%d%%", part*100/total)
}
