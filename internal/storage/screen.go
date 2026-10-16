package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SwapDMScreen атомарно меняет запомненный ЛС-экран юзера (таблица dm_screens)
// и возвращает id ПРЕДЫДУЩЕГО сообщения этого вида — вызывающий удаляет его
// best-effort уже после успешного свопа. Порядок принципиален: упавшая запись
// не должна оставить новый экран неотслеженным (иначе он не удалится никогда),
// поэтому прошлое сообщение живёт до подтверждённого свопа. 0 — предыдущего
// экрана не было (или его id совпал с новым).
func (d *DB) SwapDMScreen(ctx context.Context, userID int64, kind string, messageID int, at time.Time) (int, error) {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("swap dm screen: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	old := 0
	err = tx.QueryRowContext(ctx,
		`SELECT message_id FROM dm_screens WHERE user_id = ? AND kind = ?`,
		userID, kind).Scan(&old)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("swap dm screen: read prev: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO dm_screens (user_id, kind, message_id, at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(user_id, kind) DO UPDATE SET
			message_id = excluded.message_id, at = excluded.at
	`, userID, kind, messageID, at.Unix()); err != nil {
		return 0, fmt.Errorf("swap dm screen: write new: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("swap dm screen: commit: %w", err)
	}
	if old == messageID {
		return 0, nil
	}
	return old, nil
}
