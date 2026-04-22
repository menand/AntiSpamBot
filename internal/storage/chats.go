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

// ChatSettings is the per-chat configuration row. Nullable fields mean
// "use global default" — callers should fall back to b.cfg.* when the
// field is not set.
type ChatSettings struct {
	ChatID                int64
	GreetingEnabled       bool          // defaults to true when no row exists
	MaxAttempts           sql.NullInt64 // NULL = use global
	CaptchaTimeoutSeconds sql.NullInt64 // NULL = use global
	DailyStatsEnabled     bool          // defaults to false when no row exists
	LastDailyStatsDay     sql.NullString
}

// GetChatSettings loads the full settings row for a chat, applying defaults
// when the row is absent.
func (d *DB) GetChatSettings(ctx context.Context, chatID int64) (ChatSettings, error) {
	s := ChatSettings{ChatID: chatID, GreetingEnabled: true}

	var greetingInt, dailyInt int
	err := d.sql.QueryRowContext(ctx, `
		SELECT greeting_enabled, max_attempts, captcha_timeout_seconds,
		       daily_stats_enabled, last_daily_stats_day
		FROM chat_settings WHERE chat_id = ?
	`, chatID).Scan(&greetingInt,
		&s.MaxAttempts, &s.CaptchaTimeoutSeconds,
		&dailyInt, &s.LastDailyStatsDay)
	if errors.Is(err, sql.ErrNoRows) {
		return s, nil
	}
	if err != nil {
		return s, fmt.Errorf("get chat settings: %w", err)
	}
	s.GreetingEnabled = greetingInt != 0
	s.DailyStatsEnabled = dailyInt != 0
	return s, nil
}

// GetGreetingEnabled is a thin convenience over GetChatSettings, kept to
// avoid churn at existing call sites.
func (d *DB) GetGreetingEnabled(ctx context.Context, chatID int64) (bool, error) {
	s, err := d.GetChatSettings(ctx, chatID)
	return s.GreetingEnabled, err
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

// SetMaxAttempts overrides the global MaxAttempts for this chat. Pass nil to
// clear the override (falls back to global default again).
func (d *DB) SetMaxAttempts(ctx context.Context, chatID int64, value *int) error {
	var v any
	if value != nil {
		v = int64(*value)
	}
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO chat_settings (chat_id, max_attempts)
		VALUES (?, ?)
		ON CONFLICT(chat_id) DO UPDATE SET max_attempts = excluded.max_attempts
	`, chatID, v)
	if err != nil {
		return fmt.Errorf("set max_attempts: %w", err)
	}
	return nil
}

// SetCaptchaTimeoutSec overrides the global captcha timeout for this chat.
// Pass nil to clear the override.
func (d *DB) SetCaptchaTimeoutSec(ctx context.Context, chatID int64, seconds *int) error {
	var v any
	if seconds != nil {
		v = int64(*seconds)
	}
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO chat_settings (chat_id, captcha_timeout_seconds)
		VALUES (?, ?)
		ON CONFLICT(chat_id) DO UPDATE SET captcha_timeout_seconds = excluded.captcha_timeout_seconds
	`, chatID, v)
	if err != nil {
		return fmt.Errorf("set captcha_timeout_seconds: %w", err)
	}
	return nil
}

// SetDailyStatsEnabled toggles whether the bot posts a daily digest to this
// chat. Default is off.
func (d *DB) SetDailyStatsEnabled(ctx context.Context, chatID int64, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO chat_settings (chat_id, daily_stats_enabled)
		VALUES (?, ?)
		ON CONFLICT(chat_id) DO UPDATE SET daily_stats_enabled = excluded.daily_stats_enabled
	`, chatID, v)
	if err != nil {
		return fmt.Errorf("set daily_stats_enabled: %w", err)
	}
	return nil
}

// MarkDailyStatsSent records that the daily digest for `day` was posted to
// `chatID`. Used to skip chats already handled today.
func (d *DB) MarkDailyStatsSent(ctx context.Context, chatID int64, day string) error {
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO chat_settings (chat_id, last_daily_stats_day)
		VALUES (?, ?)
		ON CONFLICT(chat_id) DO UPDATE SET last_daily_stats_day = excluded.last_daily_stats_day
	`, chatID, day)
	if err != nil {
		return fmt.Errorf("mark daily sent: %w", err)
	}
	return nil
}

// ChatsNeedingDailyStats returns chat IDs where daily stats are enabled and
// the last digest wasn't sent today.
func (d *DB) ChatsNeedingDailyStats(ctx context.Context, day string) ([]int64, error) {
	rows, err := d.sql.QueryContext(ctx, `
		SELECT chat_id FROM chat_settings
		WHERE daily_stats_enabled = 1
		  AND (last_daily_stats_day IS NULL OR last_daily_stats_day != ?)
	`, day)
	if err != nil {
		return nil, fmt.Errorf("chats needing daily: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
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
