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
	Silence         time.Duration // time since last_message_at or members.joined_at
	HasBaseline     bool          // false = no reliable baseline to compute silence from
	WasFirstMessage bool          // true if this is user's first message in this chat
}

// RecordMessage upserts user_activity + user_message_counts inside a transaction
// and returns silence information relative to the last message (or join time if
// the user never wrote before). If neither baseline exists, HasBaseline is false.
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

	day := at.UTC().Format("2006-01-02")
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

// RememberUser upserts display-name info for a user. Idempotent; safe to call
// on every message.
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

// GetUserInfos looks up cached display info for many users at once. Missing
// users are absent from the result map.
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
}

// TopFailers returns users with the most kick+ban events in [from, until), sorted desc.
func (d *DB) TopFailers(ctx context.Context, chatID int64, from, until time.Time, limit int) ([]UserCount, error) {
	rows, err := d.sql.QueryContext(ctx, `
		SELECT user_id, COUNT(*) AS n FROM events
		WHERE chat_id = ? AND kind IN ('kick', 'ban') AND at >= ? AND at < ?
		GROUP BY user_id
		ORDER BY n DESC, user_id ASC
		LIMIT ?
	`, chatID, from.Unix(), until.Unix(), limit)
	if err != nil {
		return nil, fmt.Errorf("query top failers: %w", err)
	}
	defer rows.Close()
	return scanUserCounts(rows)
}

// TopWriters returns users with the most messages in [from, until] (by day), sorted desc.
func (d *DB) TopWriters(ctx context.Context, chatID int64, from, until time.Time, limit int) ([]UserCount, error) {
	fromDay := from.UTC().Format("2006-01-02")
	untilDay := until.UTC().Format("2006-01-02")
	rows, err := d.sql.QueryContext(ctx, `
		SELECT user_id, SUM(count) AS n FROM user_message_counts
		WHERE chat_id = ? AND day >= ? AND day <= ?
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
