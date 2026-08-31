package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// MSKOffsetHours — смещение МСК от UTC. ЕДИНСТВЕННЫЙ источник «+3»: из него
// собираются StatsLocation, SQL-сдвиг гейта дайджеста (ChatsNeedingDailyStats)
// и подписи часов в меню (mskHourLabel/hourPresetRow). Разъезд этих мест —
// это «день переворачивается не в тот час, что открывается гейт», поэтому
// литерал 3 нигде больше не повторяется.
const MSKOffsetHours = 3

// StatsLocation — пояс, по которому режутся «календарные сутки» всей
// статистики: и запись дневных агрегатов (IncMessage/RecordMessage), и
// границы чтения (QueryStats/TopWriters), и окна statsRange/дайджеста в
// internal/bot. МСК как FixedZone: в РФ нет переводов часов с 2014 года, а
// системная tzdata в контейнере не нужна.
var StatsLocation = time.FixedZone("MSK", MSKOffsetHours*60*60)

// DayOf — календарный день момента t по StatsLocation, ключ дневных таблиц
// (message_counts.day, user_message_counts.day). ЕДИНСТВЕННОЕ место
// форматирования дня: события считаются по unix-времени, дни — этой функцией,
// и только общий пояс держит оба счёта в одном окне.
func DayOf(t time.Time) string {
	return t.In(StatsLocation).Format("2006-01-02")
}

type EventKind string

const (
	EventJoin    EventKind = "join"
	EventPass    EventKind = "pass"
	EventKick    EventKind = "kick"
	EventBan     EventKind = "ban"
	EventSpamBan EventKind = "spamban" // бан по вердикту ИИ-антиспама (вне воронки капчи)
	// EventLeft — юзер вышел (или был убран админом), не дождавшись конца
	// капчи: ни пасс, ни провал. Воронка становится честной: входило =
	// прошло + кикнуто + забанено + вышло само + сорвалось + в процессе. Без
	// него такие юзеры навсегда висели бы в «В процессе» в статистике.
	EventLeft EventKind = "left"
	// EventAbort — капча сорвалась по вине инфраструктуры (restrict/send не
	// прошли после ретраев): юзер остался в чате и размьючен, «вышли сами»
	// было бы ложью. Закрывает воронку так же, как left, но рендерится
	// отдельной строкой «не удалось проверить».
	EventAbort EventKind = "abort"
	// EventMute — мьют командой /mute. Питает список «10 последних» /unmute;
	// QueryStats его сознательно игнорирует — мьют не относится к воронке капчи.
	EventMute EventKind = "mute"
	// EventSuspect — появилась плашка «похоже на спам» (ИИ-спам-чек,
	// профиль-чек или ручной репорт /spam). В воронку НЕ входит и в
	// QueryStats сознательно не добавляется: подозрение ещё не вердикт.
	// Питает историю «подозрения на спам» в карточке /info; чистится общим
	// ретеншном events (180 дней).
	EventSuspect EventKind = "suspect"
)

// Причины киков/банов (events.reason). Префиксные форматы несут ID для
// рендера имён в статистике/уведомлениях; парсит их humanReason в internal/bot.
const (
	ReasonCaptcha      = "captcha"        // не прошёл капчу (неверный ответ или таймаут)
	ReasonNoReply      = "noreply"        // не ответил на приветствие (режим «требовать ответа»)
	ReasonGlobal       = "global"         // мгновенный бан: юзер в глобальной базе спамеров
	ReasonModPrefix    = "mod:"           // + adminID: команда /kick|/ban админа
	ReasonVotePrefix   = "vote:"          // + id,id,...: вердикт голосования (голоса «за»)
	ReasonReplyApprove = "reply_approve:" // + adminID: админ впустил вручную через кнопку на приветствии
)

// RecordEvent пишет событие; reason — причина для kick/ban/spamban (см.
// Reason*-константы), пустая строка для join/pass хранится как NULL.
func (d *DB) RecordEvent(ctx context.Context, chatID, userID int64, kind EventKind, at time.Time, reason string) error {
	_, err := d.sql.ExecContext(ctx,
		`INSERT INTO events (chat_id, user_id, kind, at, reason) VALUES (?, ?, ?, ?, ?)`,
		chatID, userID, string(kind), at.Unix(), nullableString(reason))
	if err != nil {
		return fmt.Errorf("record event: %w", err)
	}
	return nil
}

// UpsertMember записывает (или обновляет) время входа прошедшего капчу юзера.
func (d *DB) UpsertMember(ctx context.Context, chatID, userID int64, joinedAt time.Time) error {
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO members (chat_id, user_id, joined_at)
		VALUES (?, ?, ?)
		ON CONFLICT(chat_id, user_id) DO UPDATE SET joined_at = excluded.joined_at
	`, chatID, userID, joinedAt.Unix())
	if err != nil {
		return fmt.Errorf("upsert member: %w", err)
	}
	return nil
}

// MemberJoinedAt возвращает время входа юзера. Возвращает (zero, false, nil),
// если записи о юзере нет (участник состоял в чате ещё до бота).
func (d *DB) MemberJoinedAt(ctx context.Context, chatID, userID int64) (time.Time, bool, error) {
	var unix int64
	err := d.sql.QueryRowContext(ctx,
		`SELECT joined_at FROM members WHERE chat_id = ? AND user_id = ?`,
		chatID, userID).Scan(&unix)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("member joined_at: %w", err)
	}
	return time.Unix(unix, 0), true, nil
}

// IncMessage увеличивает дневной счётчик для данной классификации.
// День режется по StatsLocation (см. DayOf).
func (d *DB) IncMessage(ctx context.Context, chatID int64, when time.Time, newcomer bool) error {
	day := DayOf(when)
	var newInc, oldInc int
	if newcomer {
		newInc = 1
	} else {
		oldInc = 1
	}
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO message_counts (chat_id, day, newcomer_count, oldtimer_count)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(chat_id, day) DO UPDATE SET
			newcomer_count = newcomer_count + excluded.newcomer_count,
			oldtimer_count = oldtimer_count + excluded.oldtimer_count
	`, chatID, day, newInc, oldInc)
	if err != nil {
		return fmt.Errorf("inc message: %w", err)
	}
	return nil
}

type Stats struct {
	Joined      int
	Passed      int
	Kicked      int // воронка капчи: БЕЗ командных киков (mod:<id>)
	Banned      int // воронка капчи: БЕЗ командных банов (mod:<id>)
	ModKicked   int // /kick командой админа — вне воронки, см. renderStats
	ModBanned   int // /ban командой админа — аналогично
	SpamBanned  int // баны ИИ-антиспама; отдельно от воронки капчи
	Left        int // вышли сами посреди капчи (ни пасс, ни кик)
	Aborted     int // капча сорвалась по вине инфраструктуры (не «вышли сами»)
	MsgNewcomer int
	MsgOldtimer int
	PeriodFrom  time.Time
	PeriodUntil time.Time
}

// ChatEarliestEventAt — время самого раннего события чата. Фолбэк для
// /info, когда chats.bot_added_at пуст (чат жил до введения колонки):
// события начинаются там, где бот начал работать, так что дата честная
// нижняя граница «бот в чате примерно с …». ok=false — событий нет вовсе.
func (d *DB) ChatEarliestEventAt(ctx context.Context, chatID int64) (time.Time, bool, error) {
	var unix sql.NullInt64
	err := d.sql.QueryRowContext(ctx,
		`SELECT MIN(at) FROM events WHERE chat_id = ?`, chatID).Scan(&unix)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("chat earliest event: %w", err)
	}
	if !unix.Valid {
		return time.Time{}, false, nil
	}
	return time.Unix(unix.Int64, 0), true, nil
}

func (d *DB) QueryStats(ctx context.Context, chatID int64, from, until time.Time) (Stats, error) {
	s := Stats{PeriodFrom: from, PeriodUntil: until}

	// Командные события (/kick|/ban, reason = mod:<id>) считаются в
	// ModKicked/ModBanned и из воронки Kicked/Banned исключены: блок в
	// renderStats делит их на «Новых участников», и бан старожила командой
	// рендерился как «Забанены: N (100%)» у новичков. Списки юзеров
	// (TopFailers/EventUsers) причины не фильтруют — там видно всё.
	rows, err := d.sql.QueryContext(ctx, `
		SELECT kind, COUNT(*),
		       COALESCE(SUM(CASE WHEN reason LIKE 'mod:%' THEN 1 ELSE 0 END), 0)
		FROM events
		WHERE chat_id = ? AND at >= ? AND at < ?
		GROUP BY kind
	`, chatID, from.Unix(), until.Unix())
	if err != nil {
		return s, fmt.Errorf("query events: %w", err)
	}
	for rows.Next() {
		var kind string
		var n, modN int
		if err := rows.Scan(&kind, &n, &modN); err != nil {
			rows.Close()
			return s, fmt.Errorf("scan event: %w", err)
		}
		switch EventKind(kind) {
		case EventJoin:
			s.Joined = n
		case EventPass:
			s.Passed = n
		case EventKick:
			s.ModKicked = modN
			s.Kicked = n - modN
		case EventBan:
			s.ModBanned = modN
			s.Banned = n - modN
		case EventSpamBan:
			s.SpamBanned = n
		case EventLeft:
			s.Left = n
		case EventAbort:
			s.Aborted = n
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return s, fmt.Errorf("events rows: %w", err)
	}

	// Таблицы с дневной гранулярностью используют [fromDay, untilDay) —
	// верхняя граница исключается, как и в запросе событий выше. Вызывающие
	// передают выровненные по календарю диапазоны (см. statsRange /
	// sendDailyDigest), так что полуночный `until` исключает этот день.
	fromDay := DayOf(from)
	untilDay := DayOf(until)
	err = d.sql.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(newcomer_count), 0), COALESCE(SUM(oldtimer_count), 0)
		FROM message_counts
		WHERE chat_id = ? AND day >= ? AND day < ?
	`, chatID, fromDay, untilDay).Scan(&s.MsgNewcomer, &s.MsgOldtimer)
	if err != nil {
		return s, fmt.Errorf("query messages: %w", err)
	}

	return s, nil
}
