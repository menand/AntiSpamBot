package storage

import (
	"context"
	"fmt"
	"time"
)

type PendingRow struct {
	ChatID     int64
	UserID     int64
	MessageID  int
	CorrectIdx int
	ExpiresAt  time.Time
}

func (d *DB) PutPending(ctx context.Context, p PendingRow) error {
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO pending_captchas (chat_id, user_id, message_id, correct_idx, expires_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(chat_id, user_id) DO UPDATE SET
			message_id = excluded.message_id,
			correct_idx = excluded.correct_idx,
			expires_at = excluded.expires_at
	`, p.ChatID, p.UserID, p.MessageID, p.CorrectIdx, p.ExpiresAt.Unix())
	if err != nil {
		return fmt.Errorf("put pending: %w", err)
	}
	return nil
}

func (d *DB) DeletePending(ctx context.Context, chatID, userID int64) error {
	_, err := d.sql.ExecContext(ctx,
		`DELETE FROM pending_captchas WHERE chat_id = ? AND user_id = ?`,
		chatID, userID)
	if err != nil {
		return fmt.Errorf("delete pending: %w", err)
	}
	return nil
}

func (d *DB) LoadAllPending(ctx context.Context) ([]PendingRow, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT chat_id, user_id, message_id, correct_idx, expires_at FROM pending_captchas`)
	if err != nil {
		return nil, fmt.Errorf("load pending: %w", err)
	}
	defer rows.Close()

	var out []PendingRow
	for rows.Next() {
		var p PendingRow
		var expiresUnix int64
		if err := rows.Scan(&p.ChatID, &p.UserID, &p.MessageID, &p.CorrectIdx, &expiresUnix); err != nil {
			return nil, fmt.Errorf("scan pending: %w", err)
		}
		p.ExpiresAt = time.Unix(expiresUnix, 0)
		out = append(out, p)
	}
	return out, rows.Err()
}
