package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type ChatInfo struct {
	ChatID   int64
	Title    string
	Type     string
	Username string // публичный @username чата; "" = приватный
}

// Статусы подтверждения чата владельцем бота (chats.approval_status).
// 'approved' — бот работает в чате (значение по умолчанию: строки,
// созданные до введения подтверждения, считаются approved); 'pending' —
// ждёт решения владельца, чат полностью инертен; 'rejected' — владелец
// отклонил (обычно бот выходит из чата, строка удаляется).
const (
	ChatApproved = "approved"
	ChatPending  = "pending"
	ChatRejected = "rejected"
)

func (d *DB) RememberChat(ctx context.Context, info ChatInfo) error {
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO chats (chat_id, title, type, username, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(chat_id) DO UPDATE SET
			title = excluded.title,
			type = excluded.type,
			username = excluded.username,
			updated_at = excluded.updated_at
	`, info.ChatID,
		nullableString(info.Title),
		nullableString(info.Type),
		nullableString(info.Username),
		time.Now().Unix())
	if err != nil {
		return fmt.Errorf("remember chat: %w", err)
	}
	return nil
}

// GetChat возвращает строку реестра для одного чата, если он известен.
func (d *DB) GetChat(ctx context.Context, chatID int64) (ChatInfo, bool, error) {
	var c ChatInfo
	var title, ctype, uname sql.NullString
	err := d.sql.QueryRowContext(ctx,
		`SELECT chat_id, title, type, username FROM chats WHERE chat_id = ?`,
		chatID).Scan(&c.ChatID, &title, &ctype, &uname)
	if errors.Is(err, sql.ErrNoRows) {
		return ChatInfo{}, false, nil
	}
	if err != nil {
		return ChatInfo{}, false, fmt.Errorf("get chat: %w", err)
	}
	c.Title = title.String
	c.Type = ctype.String
	c.Username = uname.String
	return c, true, nil
}

// GetChatApproval возвращает статус подтверждения чата. exists=false — чат
// не в реестре (никогда не регистрировался).
func (d *DB) GetChatApproval(ctx context.Context, chatID int64) (status string, exists bool, err error) {
	err = d.sql.QueryRowContext(ctx,
		`SELECT approval_status FROM chats WHERE chat_id = ?`, chatID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("get chat approval: %w", err)
	}
	return status, true, nil
}

// SetChatApproval записывает статус подтверждения чата (upsert). Вызывается
// только из точек, владеющих статусом: handleMyChatMember и
// carryApprovalOnMigrate. Восстановление отклонённого чата — отдельный
// условный путь (ReapproveChat), чтобы не воскресить мёртвый чат гонкой с
// dropChat.
func (d *DB) SetChatApproval(ctx context.Context, chatID int64, status string) error {
	if status != ChatApproved && status != ChatPending && status != ChatRejected {
		return fmt.Errorf("set chat approval: invalid status %q", status)
	}
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO chats (chat_id, approval_status, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(chat_id) DO UPDATE SET
			approval_status = excluded.approval_status,
			updated_at = excluded.updated_at
	`, chatID, status, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("set chat approval: %w", err)
	}
	return nil
}

// ReapproveChat — страховочный повторный апрув отклонённого чата: переводит
// его из rejected в approved ТОЛЬКО если строка ещё существует. Возвращает
// false, когда чат не был rejected — например, строка уже удалена (бот всё же
// вышел из чата после отклонения, dropChat). В отличие от SetChatApproval
// (слепой upsert), ReapproveChat не может воскресить мёртвый чат гонкой с
// dropChat: условный UPDATE по 0 строк ничего не создаёт.
func (d *DB) ReapproveChat(ctx context.Context, chatID int64) (bool, error) {
	res, err := d.sql.ExecContext(ctx, `
		UPDATE chats SET approval_status = ?, updated_at = ?
		WHERE chat_id = ? AND approval_status = ?
	`, ChatApproved, time.Now().Unix(), chatID, ChatRejected)
	if err != nil {
		return false, fmt.Errorf("reapprove chat: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("reapprove chat rows: %w", err)
	}
	return n == 1, nil
}

// ClaimChatApproval атомарно переводит чат из pending в status. Возвращает
// true, только если переход сделал ИМЕННО этот вызывающий. Нужен, чтобы
// первое нажатие кнопки владельца выигрывало гонку между владельцами
// (telego обрабатывает callback'и параллельно): два владельца, жмущие «Да»
// и «Нет» одновременно, не могут перезаписать друг друга.
func (d *DB) ClaimChatApproval(ctx context.Context, chatID int64, status string) (bool, error) {
	if status != ChatApproved && status != ChatRejected {
		return false, fmt.Errorf("claim chat approval: invalid status %q", status)
	}
	res, err := d.sql.ExecContext(ctx, `
		UPDATE chats SET approval_status = ?, updated_at = ?
		WHERE chat_id = ? AND approval_status = ?
	`, status, time.Now().Unix(), chatID, ChatPending)
	if err != nil {
		return false, fmt.Errorf("claim chat approval: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("claim chat approval rows: %w", err)
	}
	return n == 1, nil
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
	GreetingEntities      sql.NullString // JSON entities шаблона; NULL = плоский текст
	SilentAnnounceEnabled bool           // по умолчанию true, когда строки нет
	SpamCheckEnabled      bool           // по умолчанию false, когда строки нет
	SpamThreshold         sql.NullInt64  // NULL = 90 (%)
	SpamWhitelistMsgs     sql.NullInt64  // NULL = 5 сообщений до белого списка
	SpamVoteMargin        sql.NullInt64  // NULL = 3 голоса перевеса
	ReplyCheckEnabled     bool           // режим «требовать ответа»; по умолчанию false
	ReplyCheckSeconds     sql.NullInt64  // NULL = 60 секунд на ответ
	EphemeralEnabled      bool           // служебные сообщения эфемерно; по умолчанию false
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

	var greetingInt, dailyInt, silentInt, spamInt, replyInt, ephInt int
	err := d.sql.QueryRowContext(ctx, `
		SELECT greeting_enabled, max_attempts, captcha_timeout_seconds,
		       daily_stats_enabled, daily_stats_utc_hour, last_daily_stats_day,
		       captcha_mode, greeting_text, greeting_entities, silent_announce_enabled,
		       spam_check_enabled, spam_threshold, spam_whitelist_msgs,
		       spam_vote_margin, reply_check_enabled, reply_check_seconds,
		       ephemeral_enabled
		FROM chat_settings WHERE chat_id = ?
	`, chatID).Scan(&greetingInt,
		&s.MaxAttempts, &s.CaptchaTimeoutSeconds,
		&dailyInt, &s.DailyStatsUTCHour, &s.LastDailyStatsDay,
		&s.CaptchaMode, &s.GreetingText, &s.GreetingEntities, &silentInt,
		&spamInt, &s.SpamThreshold, &s.SpamWhitelistMsgs,
		&s.SpamVoteMargin, &replyInt, &s.ReplyCheckSeconds,
		&ephInt)
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
	s.ReplyCheckEnabled = replyInt != 0
	s.EphemeralEnabled = ephInt != 0
	return s, nil
}

// setChatSetting апсертит одну колонку chat_settings — общее тело всех
// Set*-сеттеров ниже. col — всегда константа вызывающего, не пользовательский
// ввод.
func (d *DB) setChatSetting(ctx context.Context, chatID int64, col string, v any) error {
	_, err := d.sql.ExecContext(ctx, fmt.Sprintf(`
		INSERT INTO chat_settings (chat_id, %[1]s)
		VALUES (?, ?)
		ON CONFLICT(chat_id) DO UPDATE SET %[1]s = excluded.%[1]s
	`, col), chatID, v)
	if err != nil {
		return fmt.Errorf("set %s: %w", col, err)
	}
	return nil
}

// nullableInt — *int в значение nullable-колонки: nil = NULL (снять
// пер-чатовый override, вернуться к глобальному дефолту).
func nullableInt(v *int) any {
	if v == nil {
		return nil
	}
	return int64(*v)
}

// SetEphemeralEnabled тогглит эфемерные служебные сообщения.
func (d *DB) SetEphemeralEnabled(ctx context.Context, chatID int64, enabled bool) error {
	return d.setChatSetting(ctx, chatID, "ephemeral_enabled", boolToInt(enabled))
}

// SetReplyCheckEnabled тогглит режим «требовать ответа на приветствие».
func (d *DB) SetReplyCheckEnabled(ctx context.Context, chatID int64, enabled bool) error {
	return d.setChatSetting(ctx, chatID, "reply_check_enabled", boolToInt(enabled))
}

// SetReplyCheckSeconds переопределяет срок ожидания ответа. nil снимает.
func (d *DB) SetReplyCheckSeconds(ctx context.Context, chatID int64, seconds *int) error {
	return d.setChatSetting(ctx, chatID, "reply_check_seconds", nullableInt(seconds))
}

func (d *DB) SetGreetingEnabled(ctx context.Context, chatID int64, enabled bool) error {
	return d.setChatSetting(ctx, chatID, "greeting_enabled", boolToInt(enabled))
}

// SetSilentAnnounceEnabled включает/выключает объявления «вернулся после
// долгого молчания» для этого чата.
func (d *DB) SetSilentAnnounceEnabled(ctx context.Context, chatID int64, enabled bool) error {
	return d.setChatSetting(ctx, chatID, "silent_announce_enabled", boolToInt(enabled))
}

// SetSpamCheckEnabled включает/выключает ИИ-анализ спама для этого чата.
func (d *DB) SetSpamCheckEnabled(ctx context.Context, chatID int64, enabled bool) error {
	return d.setChatSetting(ctx, chatID, "spam_check_enabled", boolToInt(enabled))
}

// SetSpamWhitelistMsgs переопределяет, сколько всего сообщений выводит юзера
// в белый список (без анализа спама). nil сбрасывает.
func (d *DB) SetSpamWhitelistMsgs(ctx context.Context, chatID int64, value *int) error {
	return d.setChatSetting(ctx, chatID, "spam_whitelist_msgs", nullableInt(value))
}

// SetSpamVoteMargin переопределяет перевес голосов, решающий спам-вердикт. nil сбрасывает.
func (d *DB) SetSpamVoteMargin(ctx context.Context, chatID int64, value *int) error {
	return d.setChatSetting(ctx, chatID, "spam_vote_margin", nullableInt(value))
}

// SetMaxAttempts переопределяет глобальный MaxAttempts для этого чата. nil
// снимает переопределение (снова действует глобальный дефолт).
func (d *DB) SetMaxAttempts(ctx context.Context, chatID int64, value *int) error {
	return d.setChatSetting(ctx, chatID, "max_attempts", nullableInt(value))
}

// SetCaptchaTimeoutSec переопределяет глобальный таймаут капчи для этого
// чата. nil снимает переопределение.
func (d *DB) SetCaptchaTimeoutSec(ctx context.Context, chatID int64, seconds *int) error {
	return d.setChatSetting(ctx, chatID, "captcha_timeout_seconds", nullableInt(seconds))
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
	return d.setChatSetting(ctx, chatID, "captcha_mode", v)
}

// SetGreetingText сохраняет кастомный шаблон приветствия для этого чата.
// Шаблон может содержать плейсхолдер {name}, который при отправке заменяется
// упоминанием нового участника. entitiesJSON — сериализованные
// telego.MessageEntity форматирования (nil = плоский текст). nil-текст
// возвращает встроенный дефолт (entities при этом тоже сбрасываются).
func (d *DB) SetGreetingText(ctx context.Context, chatID int64, text, entitiesJSON *string) error {
	var v, ve any
	if text != nil {
		v = *text
	}
	if entitiesJSON != nil {
		ve = *entitiesJSON
	}
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO chat_settings (chat_id, greeting_text, greeting_entities)
		VALUES (?, ?, ?)
		ON CONFLICT(chat_id) DO UPDATE SET
			greeting_text = excluded.greeting_text,
			greeting_entities = excluded.greeting_entities
	`, chatID, v, ve)
	if err != nil {
		return fmt.Errorf("set greeting_text: %w", err)
	}
	return nil
}

// SetDailyStatsHour переопределяет час UTC (0-23), в который в этот чат
// постится ежедневный дайджест. nil снимает переопределение (возврат к
// глобальному дефолту).
func (d *DB) SetDailyStatsHour(ctx context.Context, chatID int64, utcHour *int) error {
	return d.setChatSetting(ctx, chatID, "daily_stats_utc_hour", nullableInt(utcHour))
}

// SetDailyStatsEnabled включает/выключает ежедневный дайджест в этом чате.
// По умолчанию выключено.
func (d *DB) SetDailyStatsEnabled(ctx context.Context, chatID int64, enabled bool) error {
	return d.setChatSetting(ctx, chatID, "daily_stats_enabled", boolToInt(enabled))
}

// MarkDailyStatsSent записывает, что ежедневный дайджест за `day` отправлен в
// `chatID`. Нужен, чтобы пропускать чаты, уже обработанные сегодня.
func (d *DB) MarkDailyStatsSent(ctx context.Context, chatID int64, day string) error {
	return d.setChatSetting(ctx, chatID, "last_daily_stats_day", day)
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
		`SELECT chat_id, title, type, username FROM chats ORDER BY COALESCE(title, ''), chat_id`)
	if err != nil {
		return nil, fmt.Errorf("list chats: %w", err)
	}
	defer rows.Close()
	var out []ChatInfo
	for rows.Next() {
		var c ChatInfo
		var title, ctype, uname sql.NullString
		if err := rows.Scan(&c.ChatID, &title, &ctype, &uname); err != nil {
			return nil, fmt.Errorf("scan chat: %w", err)
		}
		c.Title = title.String
		c.Type = ctype.String
		c.Username = uname.String
		out = append(out, c)
	}
	return out, rows.Err()
}
