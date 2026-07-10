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
	// Serialize writes; SQLite has a single writer and this avoids "database
	// is locked" churn at low traffic. WAL (set in the DSN above) keeps reads
	// from blocking behind them. Raise the pool if traffic ever grows.
	raw.SetMaxOpenConns(1)
	if err := raw.PingContext(ctx); err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	if _, err := raw.ExecContext(ctx, schemaSQL); err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}

	// Additive migrations for existing DBs. Fresh DBs already have these
	// columns via schema.sql; the ALTER TABLE here harmlessly fails with
	// "duplicate column name" and we ignore it. Keep entries idempotent —
	// add-column with a default or NULL, no data rewrites.
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
