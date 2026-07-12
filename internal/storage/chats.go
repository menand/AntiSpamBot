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

// GetChat возвращает строку реестра для одного чата, если он известен.
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

// ChatSettings — строка пер-чатовых настроек. Nullable-поля означают
// «использовать глобальный дефолт» — вызывающий должен откатываться к
// b.cfg.*, когда поле не задано.
type ChatSettings struct {
	ChatID                int64
	GreetingEnabled       bool          // по умолчанию true, когда строки нет
	MaxAttempts           sql.NullInt64 // NULL = глобальный дефолт
	CaptchaTimeoutSeconds sql.NullInt64 // NULL = глобальный дефолт
	DailyStatsEnabled     bool          // по умолчанию false, когда строки нет
	DailyStatsUTCHour     sql.NullInt64 // NULL = глобальный DAILY_STATS_UTC_HOUR
	LastDailyStatsDay     sql.NullString
	CaptchaMode           sql.NullString // NULL = дефолт (circles)
	GreetingText          sql.NullString // NULL = встроенное приветствие по умолчанию
	SilentAnnounceEnabled bool           // по умолчанию true, когда строки нет
	SpamCheckEnabled      bool           // по умолчанию false, когда строки нет
	SpamThreshold         sql.NullInt64  // NULL = 90 (%)
	SpamWhitelistMsgs     sql.NullInt64  // NULL = 5 сообщений до белого списка
	SpamVoteMargin        sql.NullInt64  // NULL = 3 голоса перевеса
}

// defaultChatSettings — строка настроек для чата без сохранённой строки —
// и безопасный фолбек, когда загрузка не удалась.
func defaultChatSettings(chatID int64) ChatSettings {
	return ChatSettings{ChatID: chatID, GreetingEnabled: true, SilentAnnounceEnabled: true}
}

// GetChatSettings загружает полную строку настроек чата, применяя дефолты,
// когда строки нет. При ошибке возвращаемая структура всё равно несёт
// дефолты, так что вызывающий может резолвить по ней, не проверяя err.
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
		// Неудавшийся Scan мог частично заполнить s — отдаём чистые дефолты.
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

// SetSilentAnnounceEnabled включает/выключает объявления «вернулся после
// долгого молчания» для этого чата.
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

// SetSpamCheckEnabled включает/выключает ИИ-анализ спама для этого чата.
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

// SetSpamThreshold переопределяет порог вероятности спама (%). nil сбрасывает.
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

// SetSpamWhitelistMsgs переопределяет, сколько всего сообщений выводит юзера
// в белый список (без анализа спама). nil сбрасывает.
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

// SetSpamVoteMargin переопределяет перевес голосов, решающий спам-вердикт. nil сбрасывает.
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

// SetMaxAttempts переопределяет глобальный MaxAttempts для этого чата. nil
// снимает переопределение (снова действует глобальный дефолт).
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

// SetCaptchaTimeoutSec переопределяет глобальный таймаут капчи для этого
// чата. nil снимает переопределение.
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

// SetCaptchaMode сохраняет стиль капчи для этого чата. nil снимает
// переопределение (возврат к режиму по умолчанию). Известные значения бот
// валидирует до вызова; неизвестные строки сохраняются и читаются как есть,
// но в момент использования бот откатывается к дефолту.
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

// SetGreetingText сохраняет кастомный шаблон приветствия для этого чата.
// Шаблон может содержать плейсхолдер {name}, который при отправке заменяется
// упоминанием нового участника. nil возвращает встроенный дефолт.
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

// SetDailyStatsHour переопределяет час UTC (0-23), в который в этот чат
// постится ежедневный дайджест. nil снимает переопределение (возврат к
// глобальному дефолту).
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

// SetDailyStatsEnabled включает/выключает ежедневный дайджест в этом чате.
// По умолчанию выключено.
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

// MarkDailyStatsSent записывает, что ежедневный дайджест за `day` отправлен в
// `chatID`. Нужен, чтобы пропускать чаты, уже обработанные сегодня.
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

// ChatsNeedingDailyStats возвращает ID чатов, у которых:
//   - ежедневная статистика включена,
//   - действующий для чата час уже наступил (currentMSKHour >= действующего),
//   - сегодняшний (МСК) дайджест ещё не отправлен.
//
// Час ХРАНИТСЯ в UTC (совместимость с пресетами меню/mskHourLabel), а гейт
// сравнивается в МСК — SQL-сдвиг (+MSKOffsetHours)%24 ниже. Гейт-час и
// day-маркер обязаны жить в одном поясе: при UTC-часе с МСК-днём маркер
// переворачивался бы в 21:00 UTC при уже открытом гейте, и дайджесты
// сползали бы на полночь МСК.
func (d *DB) ChatsNeedingDailyStats(ctx context.Context, currentMSKHour, defaultHour int, day string) ([]int64, error) {
	rows, err := d.sql.QueryContext(ctx, `
		SELECT chat_id FROM chat_settings
		WHERE daily_stats_enabled = 1
		  AND (COALESCE(daily_stats_utc_hour, ?) + ?) % 24 <= ?
		  AND (last_daily_stats_day IS NULL OR last_daily_stats_day != ?)
	`, defaultHour, MSKOffsetHours, currentMSKHour, day)
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

// ListChats возвращает все чаты, которые бот видел, отсортированные по названию.
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
