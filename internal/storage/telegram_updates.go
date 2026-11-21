package storage

import "context"

func (d *DB) HandledTelegramUpdates(ctx context.Context, botID int64) (map[int]bool, error) {
	rows, err := d.sql.QueryContext(ctx, "SELECT update_id FROM handled_telegram_updates WHERE bot_id = ?", botID)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // read-only cursor
	handled := make(map[int]bool)
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		handled[id] = true
	}
	return handled, rows.Err()
}

func (d *DB) MarkTelegramUpdateHandled(ctx context.Context, botID int64, updateID int) error {
	_, err := d.sql.ExecContext(ctx, "INSERT OR IGNORE INTO handled_telegram_updates (bot_id, update_id) VALUES (?, ?)", botID, updateID)
	return err
}

// A zero offset is used only after Telegram reports that its queue is empty.
func (d *DB) ForgetConfirmedTelegramUpdates(ctx context.Context, botID int64, offset int) error {
	_, err := d.sql.ExecContext(ctx, "DELETE FROM handled_telegram_updates WHERE bot_id = ? AND (? = 0 OR update_id < ?)", botID, offset, offset)
	return err
}
