package storage

import (
	"context"
	"fmt"
	"time"
)

// IncrementAttempt returns the new count. If the last update was older than ttl,
// the counter resets to 1.
func (d *DB) IncrementAttempt(ctx context.Context, chatID, userID int64, ttl time.Duration) (int, error) {
	now := time.Now().Unix()
	ttlSec := int64(ttl.Seconds())

	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	var existingCount int
	var existingUpdated int64
	row := tx.QueryRowContext(ctx,
		`SELECT count, updated_at FROM attempts WHERE chat_id = ? AND user_id = ?`,
		chatID, userID)
	err = row.Scan(&existingCount, &existingUpdated)

	newCount := 1
	if err == nil && (now-existingUpdated) <= ttlSec {
		newCount = existingCount + 1
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO attempts (chat_id, user_id, count, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(chat_id, user_id) DO UPDATE SET
			count = excluded.count,
			updated_at = excluded.updated_at
	`, chatID, userID, newCount, now)
	if err != nil {
		return 0, fmt.Errorf("upsert attempt: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return newCount, nil
}

func (d *DB) ResetAttempts(ctx context.Context, chatID, userID int64) error {
	_, err := d.sql.ExecContext(ctx,
		`DELETE FROM attempts WHERE chat_id = ? AND user_id = ?`,
		chatID, userID)
	if err != nil {
		return fmt.Errorf("reset attempts: %w", err)
	}
	return nil
}

// SweepAttempts removes records older than ttl. Safe to call periodically.
func (d *DB) SweepAttempts(ctx context.Context, ttl time.Duration) error {
	cutoff := time.Now().Add(-ttl).Unix()
	_, err := d.sql.ExecContext(ctx,
		`DELETE FROM attempts WHERE updated_at < ?`, cutoff)
	if err != nil {
		return fmt.Errorf("sweep attempts: %w", err)
	}
	return nil
}
