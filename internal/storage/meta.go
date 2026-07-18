package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// GetMeta читает служебную метку бота (таблица bot_meta); отсутствие ключа —
// пустая строка без ошибки.
func (d *DB) GetMeta(ctx context.Context, key string) (string, error) {
	var v string
	err := d.sql.QueryRowContext(ctx,
		`SELECT value FROM bot_meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get meta %s: %w", key, err)
	}
	return v, nil
}

// SetMeta пишет служебную метку бота.
func (d *DB) SetMeta(ctx context.Context, key, value string) error {
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO bot_meta (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value
	`, key, value)
	if err != nil {
		return fmt.Errorf("set meta %s: %w", key, err)
	}
	return nil
}
