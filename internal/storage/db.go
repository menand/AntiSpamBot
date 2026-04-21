package storage

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"

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
	// Serialize writes; SQLite has a single writer and this avoids "database is locked"
	// churn at low traffic. Raise + use WAL if traffic ever grows.
	raw.SetMaxOpenConns(1)
	if err := raw.PingContext(ctx); err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	if _, err := raw.ExecContext(ctx, schemaSQL); err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &DB{sql: raw}, nil
}

func (d *DB) Close() error {
	return d.sql.Close()
}
