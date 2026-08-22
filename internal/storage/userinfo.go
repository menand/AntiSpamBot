package storage

import (
	"context"
	"fmt"
)

// MessageWindows — счётчики сообщений юзера за окна статистики /info.
type MessageWindows struct {
	Today     int
	Yesterday int
	Week      int
	Month     int
	Total     int
}

// UserMessageWindowCounts суммирует user_message_counts за пять окон.
// fromDays — ключи дней по DayOf (МСК): today = сегодняшний день, yesterday =
// вчерашний (верхняя граница — today, как в statsRange), weekFrom/monthFrom —
// начала скользящих окон ВКЛЮЧАЯ сегодня (statsRange(periodWeek/periodMonth)),
// поэтому верхней границы у них нет: строк «из будущего» в таблице не бывает.
func (d *DB) UserMessageWindowCounts(ctx context.Context, chatID, userID int64,
	today, yesterday, weekFrom, monthFrom string) (MessageWindows, error) {
	var w MessageWindows
	row := d.sql.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN day >= ? THEN count ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN day >= ? AND day < ? THEN count ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN day >= ? THEN count ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN day >= ? THEN count ELSE 0 END), 0),
			COALESCE(SUM(count), 0)
		FROM user_message_counts
		WHERE chat_id = ? AND user_id = ?
	`, today, yesterday, today, weekFrom, monthFrom, chatID, userID)
	if err := row.Scan(&w.Today, &w.Yesterday, &w.Week, &w.Month, &w.Total); err != nil {
		return w, fmt.Errorf("user message windows: %w", err)
	}
	return w, nil
}

// EventCounts — свёртка событий юзера в этом чате для карточки /info.
// Все счётчики живут в окне ретеншна events (180 дней) — подпись в карточке
// должна об этом честно говорить.
type EventCounts struct {
	CaptchaFails int // kick/ban с reason='captcha'
	NoReply      int // kick/ban с reason='noreply' (режим «требовать ответа»)
	GlobalBans   int // мгновенный бан по глобальной базе спамеров при входе
	ModKicked    int // кик командой админа (reason LIKE 'mod:%')
	ModBanned    int // бан командой админа
	SpamBanned   int // вердикты ИИ/голосований (kind=spamban)
	Mutes        int // мьюты командой /mute (kind=mute)
	Suspects     int // подозрения на спам (плашки ИИ и репорты /spam)
}

// UserEventCounts агрегирует events юзера одним проходом: kind группирует,
// внутри кика/бана причина раскладывается CASE-ами. Неизвестные kind-ы
// (например, join/pass/left) просто не попадают ни в один бакет.
func (d *DB) UserEventCounts(ctx context.Context, chatID, userID int64) (EventCounts, error) {
	var c EventCounts
	rows, err := d.sql.QueryContext(ctx, `
		SELECT kind,
			COALESCE(SUM(CASE WHEN reason = 'captcha' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN reason = 'noreply' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN reason = 'global' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN reason LIKE 'mod:%' THEN 1 ELSE 0 END), 0),
			COUNT(*)
		FROM events
		WHERE chat_id = ? AND user_id = ?
		GROUP BY kind
	`, chatID, userID)
	if err != nil {
		return c, fmt.Errorf("user event counts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		var captcha, noreply, global, mod, total int
		if err := rows.Scan(&kind, &captcha, &noreply, &global, &mod, &total); err != nil {
			return c, fmt.Errorf("scan user event counts: %w", err)
		}
		switch EventKind(kind) {
		case EventKick:
			c.CaptchaFails += captcha
			c.NoReply += noreply
			c.GlobalBans += global
			c.ModKicked = mod
		case EventBan:
			c.CaptchaFails += captcha
			c.NoReply += noreply
			c.GlobalBans += global
			c.ModBanned = mod
		case EventSpamBan:
			c.SpamBanned = total
		case EventMute:
			c.Mutes = total
		case EventSuspect:
			c.Suspects = total
		}
	}
	return c, rows.Err()
}
