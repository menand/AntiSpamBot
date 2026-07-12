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
	// есть через schema.sql; ALTER TABLE здесь безвредно падает с
	// "duplicate column name", и мы это игнорируем. Записи держать
	// идемпотентными — add-column с дефолтом или NULL, без переписывания
	// данных.
	migrations := []string{
		`ALTER TABLE chat_settings ADD COLUMN max_attempts INTEGER`,
		`ALTER TABLE chat_settings ADD COLUMN captcha_timeout_seconds INTEGER`,
		`ALTER TABLE chat_settings ADD COLUMN daily_stats_enabled INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE chat_settings ADD COLUMN last_daily_stats_day TEXT`,
		`ALTER TABLE chat_settings ADD COLUMN daily_stats_utc_hour INTEGER`,
		`ALTER TABLE chat_settings ADD COLUMN captcha_mode TEXT`,
		`ALTER TABLE chat_settings ADD COLUMN greeting_text TEXT`,
		`ALTER TABLE chat_settings ADD COLUMN silent_announce_enabled INTEGER NOT NULL DEFAULT 1`,
		`ALTER TABLE pending_captchas ADD COLUMN thread_id INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE chat_settings ADD COLUMN spam_check_enabled INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE chat_settings ADD COLUMN spam_threshold INTEGER`,
		`ALTER TABLE chat_settings ADD COLUMN spam_whitelist_msgs INTEGER`,
		`ALTER TABLE chat_settings ADD COLUMN spam_vote_margin INTEGER`,
		`ALTER TABLE events ADD COLUMN reason TEXT`,
		`ALTER TABLE chat_settings ADD COLUMN greeting_entities TEXT`,
		`ALTER TABLE chat_settings ADD COLUMN reply_check_enabled INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE chat_settings ADD COLUMN reply_check_seconds INTEGER`,
		`ALTER TABLE owner_settings ADD COLUMN mod_notify INTEGER NOT NULL DEFAULT 0`,
		// Новые таблицы (spam_votes, spam_ballots) и индексы миграций не
		// требуют: schema.sql идемпотентен и выполняется при каждом открытии.
	}
	for _, stmt := range migrations {
		if _, err := raw.ExecContext(ctx, stmt); err != nil {
			if !strings.Contains(err.Error(), "duplicate column name") {
				_ = raw.Close()
				return nil, fmt.Errorf("apply migration %q: %w", stmt, err)
			}
		}
	}

	return &DB{sql: raw}, nil
}

func (d *DB) Close() error {
	return d.sql.Close()
}
