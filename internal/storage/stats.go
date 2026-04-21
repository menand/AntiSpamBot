package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type EventKind string

const (
	EventJoin EventKind = "join"
	EventPass EventKind = "pass"
	EventKick EventKind = "kick"
	EventBan  EventKind = "ban"
)

func (d *DB) RecordEvent(ctx context.Context, chatID, userID int64, kind EventKind, at time.Time) error {
	_, err := d.sql.ExecContext(ctx,
		`INSERT INTO events (chat_id, user_id, kind, at) VALUES (?, ?, ?, ?)`,
		chatID, userID, string(kind), at.Unix())
	if err != nil {
		return fmt.Errorf("record event: %w", err)
	}
	return nil
}

// UpsertMember records (or refreshes) the join timestamp for a passed user.
// Returns the stored joined_at — which is the existing one if present, else new.
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

// MemberJoinedAt returns the join time for a user. Returns (zero, false, nil) if
// the user has no record (pre-existing member).
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

// IncMessage bumps the per-day counter for the given classification.
// day is formatted 'YYYY-MM-DD' UTC.
func (d *DB) IncMessage(ctx context.Context, chatID int64, when time.Time, newcomer bool) error {
	day := when.UTC().Format("2006-01-02")
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
	Joined         int
	Passed         int
	Kicked         int
	Banned         int
	MsgNewcomer    int
	MsgOldtimer    int
	PeriodFrom     time.Time
	PeriodUntil    time.Time
}

func (d *DB) QueryStats(ctx context.Context, chatID int64, from, until time.Time) (Stats, error) {
	s := Stats{PeriodFrom: from, PeriodUntil: until}

	rows, err := d.sql.QueryContext(ctx, `
		SELECT kind, COUNT(*) FROM events
		WHERE chat_id = ? AND at >= ? AND at < ?
		GROUP BY kind
	`, chatID, from.Unix(), until.Unix())
	if err != nil {
		return s, fmt.Errorf("query events: %w", err)
	}
	for rows.Next() {
		var kind string
		var n int
		if err := rows.Scan(&kind, &n); err != nil {
			rows.Close()
			return s, fmt.Errorf("scan event: %w", err)
		}
		switch EventKind(kind) {
		case EventJoin:
			s.Joined = n
		case EventPass:
			s.Passed = n
		case EventKick:
			s.Kicked = n
		case EventBan:
			s.Banned = n
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return s, fmt.Errorf("events rows: %w", err)
	}

	fromDay := from.UTC().Format("2006-01-02")
	untilDay := until.UTC().Format("2006-01-02")
	err = d.sql.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(newcomer_count), 0), COALESCE(SUM(oldtimer_count), 0)
		FROM message_counts
		WHERE chat_id = ? AND day >= ? AND day <= ?
	`, chatID, fromDay, untilDay).Scan(&s.MsgNewcomer, &s.MsgOldtimer)
	if err != nil {
		return s, fmt.Errorf("query messages: %w", err)
	}

	return s, nil
}
