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

// GetChat returns the registry row for a single chat, if known.
func (d *DB) GetChat(ctx context.Context, chatID int64) (ChatInfo, bool, error) {
	var c ChatInfo
	var title, ctype sql.NullString
	err := d.sql.QueryRowContext(ctx,
		`SELECT chat_id, title, type FROM chats WHERE chat_id = ?`,
		chatID).Scan(&c.ChatID, &title, &ctype)
	if errors.Is(err, sql.ErrNoRows) {
		return ChatInfo{}, false, nil
	}
	if err != nil {
		return ChatInfo{}, false, fmt.Errorf("get chat: %w", err)
	}
	c.Title = title.String
	c.Type = ctype.String
	return c, true, nil
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
	DailyStatsUTCHour     sql.NullInt64 // NULL = use global DAILY_STATS_UTC_HOUR
	LastDailyStatsDay     sql.NullString
	CaptchaMode           sql.NullString // NULL = default (circles)
	GreetingText          sql.NullString // NULL = built-in default greeting
	SilentAnnounceEnabled bool           // defaults to true when no row exists
	SpamCheckEnabled      bool           // defaults to false when no row exists
	SpamThreshold         sql.NullInt64  // NULL = 90 (%)
	SpamWhitelistMsgs     sql.NullInt64  // NULL = 5 сообщений до белого списка
	SpamVoteMargin        sql.NullInt64  // NULL = 3 голоса перевеса
}

// defaultChatSettings is the settings row used when a chat has no stored row —
// and the safe fallback when loading fails.
func defaultChatSettings(chatID int64) ChatSettings {
	return ChatSettings{ChatID: chatID, GreetingEnabled: true, SilentAnnounceEnabled: true}
}

// GetChatSettings loads the full settings row for a chat, applying defaults
// when the row is absent. On error the returned struct still carries the
// defaults, so callers may resolve against it without checking err first.
func (d *DB) GetChatSettings(ctx context.Context, chatID int64) (ChatSettings, error) {
	s := defaultChatSettings(chatID)

	var greetingInt, dailyInt, silentInt, spamInt int
	err := d.sql.QueryRowContext(ctx, `
		SELECT greeting_enabled, max_attempts, captcha_timeout_seconds,
		       daily_stats_enabled, daily_stats_utc_hour, last_daily_stats_day,
		       captcha_mode, greeting_text, silent_announce_enabled,
		       spam_check_enabled, spam_threshold, spam_whitelist_msgs,
		       spam_vote_margin
		FROM chat_settings WHERE chat_id = ?
	`, chatID).Scan(&greetingInt,
		&s.MaxAttempts, &s.CaptchaTimeoutSeconds,
		&dailyInt, &s.DailyStatsUTCHour, &s.LastDailyStatsDay,
		&s.CaptchaMode, &s.GreetingText, &silentInt,
		&spamInt, &s.SpamThreshold, &s.SpamWhitelistMsgs,
		&s.SpamVoteMargin)
	if errors.Is(err, sql.ErrNoRows) {
		return s, nil
	}
	if err != nil {
		// A failed Scan may have partially filled s — hand back clean defaults.
		return defaultChatSettings(chatID), fmt.Errorf("get chat settings: %w", err)
	}
	s.GreetingEnabled = greetingInt != 0
	s.DailyStatsEnabled = dailyInt != 0
	s.SilentAnnounceEnabled = silentInt != 0
	s.SpamCheckEnabled = spamInt != 0
	return s, nil
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

// SetSilentAnnounceEnabled toggles the "returned after long silence"
// announcements for this chat.
func (d *DB) SetSilentAnnounceEnabled(ctx context.Context, chatID int64, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO chat_settings (chat_id, silent_announce_enabled)
		VALUES (?, ?)
		ON CONFLICT(chat_id) DO UPDATE SET silent_announce_enabled = excluded.silent_announce_enabled
	`, chatID, v)
	if err != nil {
		return fmt.Errorf("set silent_announce_enabled: %w", err)
	}
	return nil
}

// SetSpamCheckEnabled toggles the AI spam analysis for this chat.
func (d *DB) SetSpamCheckEnabled(ctx context.Context, chatID int64, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO chat_settings (chat_id, spam_check_enabled)
		VALUES (?, ?)
		ON CONFLICT(chat_id) DO UPDATE SET spam_check_enabled = excluded.spam_check_enabled
	`, chatID, v)
	if err != nil {
		return fmt.Errorf("set spam_check_enabled: %w", err)
	}
	return nil
}

// SetSpamThreshold overrides the spam probability threshold (%). Nil clears.
func (d *DB) SetSpamThreshold(ctx context.Context, chatID int64, value *int) error {
	var v any
	if value != nil {
		v = int64(*value)
	}
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO chat_settings (chat_id, spam_threshold)
		VALUES (?, ?)
		ON CONFLICT(chat_id) DO UPDATE SET spam_threshold = excluded.spam_threshold
	`, chatID, v)
	if err != nil {
		return fmt.Errorf("set spam_threshold: %w", err)
	}
	return nil
}

// SetSpamWhitelistMsgs overrides how many total messages whitelist a user
// from spam analysis. Nil clears.
func (d *DB) SetSpamWhitelistMsgs(ctx context.Context, chatID int64, value *int) error {
	var v any
	if value != nil {
		v = int64(*value)
	}
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO chat_settings (chat_id, spam_whitelist_msgs)
		VALUES (?, ?)
		ON CONFLICT(chat_id) DO UPDATE SET spam_whitelist_msgs = excluded.spam_whitelist_msgs
	`, chatID, v)
	if err != nil {
		return fmt.Errorf("set spam_whitelist_msgs: %w", err)
	}
	return nil
}

// SetSpamVoteMargin overrides the vote margin deciding a spam verdict. Nil clears.
func (d *DB) SetSpamVoteMargin(ctx context.Context, chatID int64, value *int) error {
	var v any
	if value != nil {
		v = int64(*value)
	}
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO chat_settings (chat_id, spam_vote_margin)
		VALUES (?, ?)
		ON CONFLICT(chat_id) DO UPDATE SET spam_vote_margin = excluded.spam_vote_margin
	`, chatID, v)
	if err != nil {
		return fmt.Errorf("set spam_vote_margin: %w", err)
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

// SetCaptchaMode stores the captcha style for this chat. Pass nil to clear
// the override (fall back to the default mode). The bot validates known
// values before calling this; unknown strings round-trip as-is but the bot
// falls back to default at use time.
func (d *DB) SetCaptchaMode(ctx context.Context, chatID int64, mode *string) error {
	var v any
	if mode != nil {
		v = *mode
	}
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO chat_settings (chat_id, captcha_mode)
		VALUES (?, ?)
		ON CONFLICT(chat_id) DO UPDATE SET captcha_mode = excluded.captcha_mode
	`, chatID, v)
	if err != nil {
		return fmt.Errorf("set captcha_mode: %w", err)
	}
	return nil
}

// SetGreetingText stores a custom greeting template for this chat. The
// template may contain the {name} placeholder, replaced with the new member's
// mention at send time. Pass nil to reset to the built-in default.
func (d *DB) SetGreetingText(ctx context.Context, chatID int64, text *string) error {
	var v any
	if text != nil {
		v = *text
	}
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO chat_settings (chat_id, greeting_text)
		VALUES (?, ?)
		ON CONFLICT(chat_id) DO UPDATE SET greeting_text = excluded.greeting_text
	`, chatID, v)
	if err != nil {
		return fmt.Errorf("set greeting_text: %w", err)
	}
	return nil
}

// SetDailyStatsHour overrides the UTC hour (0-23) at which the daily digest
// is posted for this chat. Pass nil to clear (fall back to the global default).
func (d *DB) SetDailyStatsHour(ctx context.Context, chatID int64, utcHour *int) error {
	var v any
	if utcHour != nil {
		v = int64(*utcHour)
	}
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO chat_settings (chat_id, daily_stats_utc_hour)
		VALUES (?, ?)
		ON CONFLICT(chat_id) DO UPDATE SET daily_stats_utc_hour = excluded.daily_stats_utc_hour
	`, chatID, v)
	if err != nil {
		return fmt.Errorf("set daily_stats_utc_hour: %w", err)
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

// ChatsNeedingDailyStats returns chat IDs where:
//   - daily stats are enabled,
//   - the chat's effective UTC hour (per-chat override or defaultHour) has
//     been reached (currentHour >= effective hour),
//   - today's digest hasn't been sent yet.
func (d *DB) ChatsNeedingDailyStats(ctx context.Context, currentHour, defaultHour int, day string) ([]int64, error) {
	rows, err := d.sql.QueryContext(ctx, `
		SELECT chat_id FROM chat_settings
		WHERE daily_stats_enabled = 1
		  AND COALESCE(daily_stats_utc_hour, ?) <= ?
		  AND (last_daily_stats_day IS NULL OR last_daily_stats_day != ?)
	`, defaultHour, currentHour, day)
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
