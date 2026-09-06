package storage

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"strings"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

type DB struct {
	sql *sql.DB
}

func Open(ctx context.Context, path string) (*DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)", path)
	raw, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// Сериализуем записи: у SQLite один писатель, и это убирает возню с
	// «database is locked» на малом трафике. WAL (задан в DSN выше) не даёт
	// чтениям блокироваться за ними. Если трафик когда-нибудь вырастет —
	// поднять пул.
	raw.SetMaxOpenConns(1)
	if err := raw.PingContext(ctx); err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	if _, err := raw.ExecContext(ctx, schemaSQL); err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}

	// Аддитивные миграции для существующих БД. В свежих БД эти колонки уже
	// есть через schema.sql. Идемпотентность — по PRAGMA table_info, а не
	// по подстроке «duplicate column name» в тексте ошибки: переформулировка
	// драйвером не должна валить стартап, а чужая ошибка с той же подстрокой
	// не должна молча пропускать миграцию.
	migrations := []struct {
		table  string // только константы из этого списка (интерполяция в PRAGMA)
		column string
		stmt   string
	}{
		{"chat_settings", "max_attempts", `ALTER TABLE chat_settings ADD COLUMN max_attempts INTEGER`},
		{"chat_settings", "captcha_timeout_seconds", `ALTER TABLE chat_settings ADD COLUMN captcha_timeout_seconds INTEGER`},
		{"chat_settings", "daily_stats_enabled", `ALTER TABLE chat_settings ADD COLUMN daily_stats_enabled INTEGER NOT NULL DEFAULT 0`},
		{"chat_settings", "last_daily_stats_day", `ALTER TABLE chat_settings ADD COLUMN last_daily_stats_day TEXT`},
		{"chat_settings", "daily_stats_utc_hour", `ALTER TABLE chat_settings ADD COLUMN daily_stats_utc_hour INTEGER`},
		{"chat_settings", "captcha_mode", `ALTER TABLE chat_settings ADD COLUMN captcha_mode TEXT`},
		{"chat_settings", "greeting_text", `ALTER TABLE chat_settings ADD COLUMN greeting_text TEXT`},
		{"chat_settings", "silent_announce_enabled", `ALTER TABLE chat_settings ADD COLUMN silent_announce_enabled INTEGER NOT NULL DEFAULT 1`},
		{"pending_captchas", "thread_id", `ALTER TABLE pending_captchas ADD COLUMN thread_id INTEGER NOT NULL DEFAULT 0`},
		{"chat_settings", "spam_check_enabled", `ALTER TABLE chat_settings ADD COLUMN spam_check_enabled INTEGER NOT NULL DEFAULT 0`},
		{"chat_settings", "spam_threshold", `ALTER TABLE chat_settings ADD COLUMN spam_threshold INTEGER`},
		{"chat_settings", "spam_whitelist_msgs", `ALTER TABLE chat_settings ADD COLUMN spam_whitelist_msgs INTEGER`},
		{"chat_settings", "spam_vote_margin", `ALTER TABLE chat_settings ADD COLUMN spam_vote_margin INTEGER`},
		{"events", "reason", `ALTER TABLE events ADD COLUMN reason TEXT`},
		{"chat_settings", "greeting_entities", `ALTER TABLE chat_settings ADD COLUMN greeting_entities TEXT`},
		{"chat_settings", "reply_check_enabled", `ALTER TABLE chat_settings ADD COLUMN reply_check_enabled INTEGER NOT NULL DEFAULT 0`},
		{"chat_settings", "reply_check_seconds", `ALTER TABLE chat_settings ADD COLUMN reply_check_seconds INTEGER`},
		{"owner_settings", "mod_notify", `ALTER TABLE owner_settings ADD COLUMN mod_notify INTEGER NOT NULL DEFAULT 0`},
		{"owner_settings", "daily_report", `ALTER TABLE owner_settings ADD COLUMN daily_report INTEGER NOT NULL DEFAULT 0`},
		{"owner_settings", "last_report_day", `ALTER TABLE owner_settings ADD COLUMN last_report_day TEXT`},
		{"chat_settings", "ephemeral_enabled", `ALTER TABLE chat_settings ADD COLUMN ephemeral_enabled INTEGER NOT NULL DEFAULT 0`},
		{"pending_captchas", "ephemeral_msg_id", `ALTER TABLE pending_captchas ADD COLUMN ephemeral_msg_id INTEGER NOT NULL DEFAULT 0`},
		{"owner_settings", "captcha_notify", `ALTER TABLE owner_settings ADD COLUMN captcha_notify INTEGER NOT NULL DEFAULT 0`},
		{"owner_settings", "version_notify", `ALTER TABLE owner_settings ADD COLUMN version_notify INTEGER NOT NULL DEFAULT 1`},
		{"chats", "username", `ALTER TABLE chats ADD COLUMN username TEXT`},
		{"chats", "approval_status", `ALTER TABLE chats ADD COLUMN approval_status TEXT NOT NULL DEFAULT 'approved'`},
		{"owner_settings", "last_stats_period", `ALTER TABLE owner_settings ADD COLUMN last_stats_period TEXT`},
		{"chats", "bot_added_at", `ALTER TABLE chats ADD COLUMN bot_added_at INTEGER`},
		{"spam_votes", "initiator_id", `ALTER TABLE spam_votes ADD COLUMN initiator_id INTEGER NOT NULL DEFAULT 0`},
		{"chat_settings", "captcha_interval_minutes", `ALTER TABLE chat_settings ADD COLUMN captcha_interval_minutes INTEGER`},
		{"pending_captchas", "stage", `ALTER TABLE pending_captchas ADD COLUMN stage INTEGER NOT NULL DEFAULT 1`},
		{"pending_replies", "stage", `ALTER TABLE pending_replies ADD COLUMN stage INTEGER NOT NULL DEFAULT 1`},
		{"pending_replies", "thread_id", `ALTER TABLE pending_replies ADD COLUMN thread_id INTEGER NOT NULL DEFAULT 0`},
		// Новые таблицы (spam_votes, spam_ballots) и индексы миграций не
		// требуют: schema.sql идемпотентен и выполняется при каждом открытии.
	}
	for _, m := range migrations {
		exists, err := columnExists(ctx, raw, m.table, m.column)
		if err != nil {
			_ = raw.Close()
			return nil, fmt.Errorf("check column %s.%s: %w", m.table, m.column, err)
		}
		if exists {
			continue
		}
		if _, err := raw.ExecContext(ctx, m.stmt); err != nil {
			_ = raw.Close()
			return nil, fmt.Errorf("apply migration %q: %w", m.stmt, err)
		}
	}

	// Разовая конвертация легаси-пер-чатовых таймаутов (секунды одиночной
	// капчи) в минуты интервала серии: ceil(сек/60), минимум 1. Идемпотентно —
	// заполняет только NULL; старая колонка остаётся как архив (не читается).
	if _, err := raw.ExecContext(ctx, `
		UPDATE chat_settings
		SET captcha_interval_minutes = max(1, CAST((captcha_timeout_seconds + 59) / 60 AS INTEGER))
		WHERE captcha_interval_minutes IS NULL AND captcha_timeout_seconds IS NOT NULL
	`); err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("migrate captcha_timeout_seconds to interval minutes: %w", err)
	}

	return &DB{sql: raw}, nil
}

// columnExists читает PRAGMA table_info. table интерполируется в SQL —
// допустимо, потому что приходит только из констант списка миграций выше.
func columnExists(ctx context.Context, db *sql.DB, table, column string) (bool, error) {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return false, fmt.Errorf("pragma table_info(%s): %w", table, err)
	}
	defer rows.Close() //nolint:errcheck // cleanup, error intentionally ignored
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var dflt sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			return false, fmt.Errorf("scan pragma table_info(%s): %w", table, err)
		}
		if strings.EqualFold(name, column) {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (d *DB) Close() error {
	return d.sql.Close()
}
