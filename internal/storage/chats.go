package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type ChatInfo struct {
	ChatID int64
	Title  string
	Type   string
}

func (d *DB) RememberChat(ctx context.Context, info ChatInfo) error {
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO chats (chat_id, title, type, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(chat_id) DO UPDATE SET
			title = excluded.title,
			type = excluded.type,
			updated_at = excluded.updated_at
	`, info.ChatID,
		nullableString(info.Title),
		nullableString(info.Type),
		time.Now().Unix())
	if err != nil {
		return fmt.Errorf("remember chat: %w", err)
	}
	return nil
}

// GetGreetingEnabled returns whether the bot should greet new members after
// they pass captcha in this chat. Defaults to true when no row exists.
func (d *DB) GetGreetingEnabled(ctx context.Context, chatID int64) (bool, error) {
	var v int
	err := d.sql.QueryRowContext(ctx,
		`SELECT greeting_enabled FROM chat_settings WHERE chat_id = ?`,
		chatID).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return true, fmt.Errorf("get greeting: %w", err)
	}
	return v != 0, nil
}

func (d *DB) SetGreetingEnabled(ctx context.Context, chatID int64, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO chat_settings (chat_id, greeting_enabled)
		VALUES (?, ?)
		ON CONFLICT(chat_id) DO UPDATE SET greeting_enabled = excluded.greeting_enabled
	`, chatID, v)
	if err != nil {
		return fmt.Errorf("set greeting: %w", err)
	}
	return nil
}

// ListChats returns all chats the bot has seen, sorted by title.
func (d *DB) ListChats(ctx context.Context) ([]ChatInfo, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT chat_id, title, type FROM chats ORDER BY COALESCE(title, ''), chat_id`)
	if err != nil {
		return nil, fmt.Errorf("list chats: %w", err)
	}
	defer rows.Close()
	var out []ChatInfo
	for rows.Next() {
		var c ChatInfo
		var title, ctype sql.NullString
		if err := rows.Scan(&c.ChatID, &title, &ctype); err != nil {
			return nil, fmt.Errorf("scan chat: %w", err)
		}
		c.Title = title.String
		c.Type = ctype.String
		out = append(out, c)
	}
	return out, rows.Err()
}
