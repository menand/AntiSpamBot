package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

type UserInfo struct {
	UserID    int64
	FirstName string
	LastName  string
	Username  string
}

type MessageRecord struct {
	Silence         time.Duration // время с last_message_at или members.joined_at
	HasBaseline     bool          // false = нет надёжного baseline для вычисления тишины
	WasFirstMessage bool          // true, если это первое сообщение юзера в этом чате
}

// RecordMessage делает upsert в user_activity + user_message_counts внутри
// одной транзакции и возвращает данные о тишине относительно последнего
// сообщения (или времени входа, если юзер раньше не писал). Если нет ни того
// ни другого baseline, HasBaseline = false.
func (d *DB) RecordMessage(ctx context.Context, chatID, userID int64, at time.Time) (MessageRecord, error) {
	var mr MessageRecord
	atUnix := at.Unix()

	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return mr, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	var firstMsg, lastMsg sql.NullInt64
	err = tx.QueryRowContext(ctx,
		`SELECT first_message_at, last_message_at FROM user_activity WHERE chat_id = ? AND user_id = ?`,
		chatID, userID).Scan(&firstMsg, &lastMsg)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return mr, fmt.Errorf("query user_activity: %w", err)
	}

	var joinedAt sql.NullInt64
	// Best-effort: нет строки members — нет baseline тишины, это штатно; ошибка чтения даёт то же поведение.
	_ = tx.QueryRowContext(ctx,
		`SELECT joined_at FROM members WHERE chat_id = ? AND user_id = ?`,
		chatID, userID).Scan(&joinedAt)

	wasFirst := !lastMsg.Valid
	if lastMsg.Valid && atUnix > lastMsg.Int64 {
		mr.Silence = time.Duration(atUnix-lastMsg.Int64) * time.Second
		mr.HasBaseline = true
	} else if wasFirst && joinedAt.Valid && atUnix > joinedAt.Int64 {
		mr.Silence = time.Duration(atUnix-joinedAt.Int64) * time.Second
		mr.HasBaseline = true
		mr.WasFirstMessage = true
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO user_activity (chat_id, user_id, first_message_at, last_message_at, message_count)
		VALUES (?, ?, ?, ?, 1)
		ON CONFLICT(chat_id, user_id) DO UPDATE SET
			first_message_at = COALESCE(user_activity.first_message_at, excluded.first_message_at),
			last_message_at = excluded.last_message_at,
			message_count = user_activity.message_count + 1
	`, chatID, userID, atUnix, atUnix)
	if err != nil {
		return mr, fmt.Errorf("upsert user_activity: %w", err)
	}

	day := DayOf(at)
	_, err = tx.ExecContext(ctx, `
		INSERT INTO user_message_counts (chat_id, user_id, day, count)
		VALUES (?, ?, ?, 1)
		ON CONFLICT(chat_id, user_id, day) DO UPDATE SET count = count + 1
	`, chatID, userID, day)
	if err != nil {
		return mr, fmt.Errorf("upsert user_message_counts: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return mr, fmt.Errorf("commit: %w", err)
	}
	return mr, nil
}

// RememberUser делает upsert отображаемого имени юзера. Идемпотентен;
// безопасно вызывать на каждом сообщении.
func (d *DB) RememberUser(ctx context.Context, info UserInfo) error {
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO user_info (user_id, first_name, last_name, username, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET
			first_name = excluded.first_name,
			last_name = excluded.last_name,
			username = excluded.username,
			updated_at = excluded.updated_at
	`, info.UserID,
		nullableString(info.FirstName),
		nullableString(info.LastName),
		nullableString(info.Username),
		time.Now().Unix())
	if err != nil {
		return fmt.Errorf("remember user: %w", err)
	}
	return nil
}

// UserIDByUsername ищет user_id по @username в кэше user_info (без «@»,
// регистронезависимо). Bot API не умеет резолвить юзерские @username, но все
// писавшие/прошедшие капчу у нас закэшированы. (0, false, nil) — не найден.
func (d *DB) UserIDByUsername(ctx context.Context, username string) (int64, bool, error) {
	var id int64
	err := d.sql.QueryRowContext(ctx,
		`SELECT user_id FROM user_info WHERE username = ? COLLATE NOCASE`,
		username).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("user id by username: %w", err)
	}
	return id, true, nil
}

// GetUserInfos достаёт закэшированные отображаемые данные сразу многих
// юзеров. Юзеры без записи в результирующей map отсутствуют.
func (d *DB) GetUserInfos(ctx context.Context, userIDs []int64) (map[int64]UserInfo, error) {
	result := make(map[int64]UserInfo, len(userIDs))
	if len(userIDs) == 0 {
		return result, nil
	}
	placeholders := strings.Repeat("?,", len(userIDs))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, len(userIDs))
	for i, id := range userIDs {
		args[i] = id
	}
	query := fmt.Sprintf(`SELECT user_id, first_name, last_name, username FROM user_info WHERE user_id IN (%s)`, placeholders)
	rows, err := d.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query user_info: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var info UserInfo
		var fn, ln, un sql.NullString
		if err := rows.Scan(&info.UserID, &fn, &ln, &un); err != nil {
			return nil, fmt.Errorf("scan user_info: %w", err)
		}
		info.FirstName = fn.String
		info.LastName = ln.String
		info.Username = un.String
		result[info.UserID] = info
	}
	return result, rows.Err()
}

type UserCount struct {
	UserID int64
	Count  int
	// Secs — сколько секунд заняло прохождение капчи (join → pass).
	// Заполняется только PassedUsers; -1 = неизвестно (join не записан).
	// Остальные выборки (writers/failers/EventUsers) оставляют 0 — их
	// рендеры это поле не читают.
	Secs int
	// LastReason — причина ПОСЛЕДНЕГО провального события юзера за период
	// (events.reason). Заполняется TopFailers/EventUsers; "" = нет причины
	// (старые строки). Разворачивает в текст humanReason в internal/bot.
	LastReason string
}

// TopFailers возвращает юзеров с наибольшим числом событий kick+ban в
// [from, until), по убыванию; LastReason — причина их последнего провала.
func (d *DB) TopFailers(ctx context.Context, chatID int64, from, until time.Time, limit int) ([]UserCount, error) {
	rows, err := d.sql.QueryContext(ctx, `
		SELECT e.user_id, COUNT(*) AS n,
		       (SELECT reason FROM events e2
		        WHERE e2.chat_id = e.chat_id AND e2.user_id = e.user_id
		          AND e2.kind IN ('kick', 'ban') AND e2.at >= ? AND e2.at < ?
		        ORDER BY e2.at DESC LIMIT 1) AS last_reason
		FROM events e
		WHERE e.chat_id = ? AND e.kind IN ('kick', 'ban') AND e.at >= ? AND e.at < ?
		GROUP BY e.user_id
		ORDER BY n DESC, e.user_id ASC
		LIMIT ?
	`, from.Unix(), until.Unix(), chatID, from.Unix(), until.Unix(), limit)
	if err != nil {
		return nil, fmt.Errorf("query top failers: %w", err)
	}
	defer rows.Close()
	return scanUserCountsWithReason(rows)
}

// PassedUsers возвращает всех, кто прошёл капчу в [from, until), в порядке
// первого прохождения, со временем решения капчи в секундах (лучшее за
// период, если юзер проходил не один раз). Поиск join намеренно без нижней
// границы по времени — вход мог случиться до начала периода (вошёл в 23:59,
// прошёл в 00:00). Secs = -1, когда join не записан (старые данные).
// Без лимита — список обрезает renderStats.
func (d *DB) PassedUsers(ctx context.Context, chatID int64, from, until time.Time) ([]UserCount, error) {
	rows, err := d.sql.QueryContext(ctx, `
		SELECT user_id, COUNT(*) AS n, COALESCE(MIN(dur), -1) AS secs
		FROM (
			SELECT p.user_id, p.at,
			       p.at - (SELECT MAX(j.at) FROM events j
			               WHERE j.chat_id = p.chat_id AND j.user_id = p.user_id
			                 AND j.kind = 'join' AND j.at <= p.at) AS dur
			FROM events p
			WHERE p.chat_id = ? AND p.kind = 'pass' AND p.at >= ? AND p.at < ?
		)
		GROUP BY user_id
		ORDER BY MIN(at) ASC, user_id ASC
	`, chatID, from.Unix(), until.Unix())
	if err != nil {
		return nil, fmt.Errorf("query passed users: %w", err)
	}
	defer rows.Close()
	var out []UserCount
	for rows.Next() {
		var uc UserCount
		if err := rows.Scan(&uc.UserID, &uc.Count, &uc.Secs); err != nil {
			return nil, fmt.Errorf("scan passed user: %w", err)
		}
		out = append(out, uc)
	}
	return out, rows.Err()
}

// EventUsers возвращает всех, у кого есть хотя бы одно событие указанных
// видов в [from, until), в порядке первого события. Без лимита — вызывающий
// (renderStats) обрезает список под допустимую длину сообщения Telegram.
func (d *DB) EventUsers(ctx context.Context, chatID int64, from, until time.Time, kinds ...EventKind) ([]UserCount, error) {
	if len(kinds) == 0 {
		return nil, nil
	}
	placeholders := strings.Repeat("?,", len(kinds))
	placeholders = placeholders[:len(placeholders)-1]

	// Один и тот же набор параметров нужен дважды: сперва для коррелированного
	// подзапроса last_reason, затем для основного WHERE.
	whereArgs := func() []any {
		a := []any{chatID}
		for _, k := range kinds {
			a = append(a, string(k))
		}
		return append(a, from.Unix(), until.Unix())
	}
	args := append(whereArgs(), whereArgs()...)
	rows, err := d.sql.QueryContext(ctx, fmt.Sprintf(`
		SELECT e.user_id, COUNT(*) AS n,
		       (SELECT reason FROM events e2
		        WHERE e2.chat_id = ? AND e2.user_id = e.user_id
		          AND e2.kind IN (%s) AND e2.at >= ? AND e2.at < ?
		        ORDER BY e2.at DESC LIMIT 1) AS last_reason
		FROM events e
		WHERE e.chat_id = ? AND e.kind IN (%s) AND e.at >= ? AND e.at < ?
		GROUP BY e.user_id
		ORDER BY MIN(e.at) ASC, e.user_id ASC
	`, placeholders, placeholders), args...)
	if err != nil {
		return nil, fmt.Errorf("query event users: %w", err)
	}
	defer rows.Close()
	return scanUserCountsWithReason(rows)
}

// TopWriters возвращает юзеров с наибольшим числом сообщений в [from, until)
// (по дням, верхняя граница исключается — семантика та же, что у QueryStats),
// по убыванию.
func (d *DB) TopWriters(ctx context.Context, chatID int64, from, until time.Time, limit int) ([]UserCount, error) {
	fromDay := DayOf(from)
	untilDay := DayOf(until)
	rows, err := d.sql.QueryContext(ctx, `
		SELECT user_id, SUM(count) AS n FROM user_message_counts
		WHERE chat_id = ? AND day >= ? AND day < ?
		GROUP BY user_id
		ORDER BY n DESC, user_id ASC
		LIMIT ?
	`, chatID, fromDay, untilDay, limit)
	if err != nil {
		return nil, fmt.Errorf("query top writers: %w", err)
	}
	defer rows.Close()
	return scanUserCounts(rows)
}

// scanUserCountsWithReason — как scanUserCounts, но с колонкой last_reason
// (nullable) третьей.
func scanUserCountsWithReason(rows *sql.Rows) ([]UserCount, error) {
	var out []UserCount
	for rows.Next() {
		var uc UserCount
		var reason sql.NullString
		if err := rows.Scan(&uc.UserID, &uc.Count, &reason); err != nil {
			return nil, err
		}
		uc.LastReason = reason.String
		out = append(out, uc)
	}
	return out, rows.Err()
}

func scanUserCounts(rows *sql.Rows) ([]UserCount, error) {
	var out []UserCount
	for rows.Next() {
		var uc UserCount
		if err := rows.Scan(&uc.UserID, &uc.Count); err != nil {
			return nil, err
		}
		out = append(out, uc)
	}
	return out, rows.Err()
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
