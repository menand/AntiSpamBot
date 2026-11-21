package storage

import (
	"context"
	"database/sql"
	"fmt"
	"iter"
	"net/url"

	_ "modernc.org/sqlite"
)

// OpenReadOnly открывает БД строго на чтение — для инструментов вроде msgtool,
// которые дёргаются на живой проде параллельно с ботом. Не выполняет миграций
// (никакой записи) и не трогает схему. WAL даёт конкурентное чтение с работающим
// писателем. Если файл не существует — ошибка (читать нечего).
//
// DSN строится через url.URL, а не sprintf: путь с «?» или «#» иначе ломал бы
// разбор URI и молча снимал mode=ro. OmitHost обязателен: без него url.String
// добавил бы «//» перед первым сегментом относительного пути («file://bot.db»),
// и SQLite разобрал бы «bot.db» как authority — «invalid uri authority», дефолт
// msgtool -db bot.db перестал бы открываться. query_only — вторая страховка,
// если mode=ro когда-нибудь не применился.
func OpenReadOnly(ctx context.Context, path string) (*DB, error) {
	u := url.URL{Scheme: "file", Path: path, OmitHost: true}
	q := url.Values{}
	q.Set("mode", "ro")
	// Именно Add: Set дважды по одному ключу оставляет только последнюю
	// прагму — query_only молча терялся бы.
	q.Add("_pragma", "query_only(1)")
	q.Add("_pragma", "busy_timeout(5000)")
	u.RawQuery = q.Encode()

	raw, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("open sqlite read-only: %w", err)
	}
	// Ограничение одним соединением — инструмент однопоточный, пул не нужен.
	raw.SetMaxOpenConns(1)
	if err := raw.PingContext(ctx); err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("ping sqlite read-only: %w", err)
	}
	return &DB{sql: raw}, nil
}

type MessageSnapshot struct {
	ArchiveID string
	ThroughID int64
	ChatIDs   []int64
	tx        *sql.Tx
	filter    MessageFilter
}

// OpenMessageSnapshot pins a read transaction before opening the message cursor.
// The ID boundary applies only to archives of this database.
func (d *DB) OpenMessageSnapshot(ctx context.Context, f MessageFilter) (*MessageSnapshot, error) {
	tx, err := d.sql.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	// The caller applies the limit after merging and deduplicating both sources.
	f.Limit = 0
	snapshot := &MessageSnapshot{tx: tx, filter: f}
	if err := snapshot.readMetadata(ctx); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return snapshot, nil
}

func (s *MessageSnapshot) readMetadata(ctx context.Context) error {
	var hasArchiveMetadata bool
	if err := s.tx.QueryRowContext(ctx, `
		SELECT COUNT(*) = 2 FROM sqlite_schema WHERE name IN ('bot_meta', 'sqlite_sequence')
	`).Scan(&hasArchiveMetadata); err != nil {
		return fmt.Errorf("pin message snapshot: %w", err)
	}
	if hasArchiveMetadata {
		if err := s.tx.QueryRowContext(ctx, `
			SELECT COALESCE((SELECT value FROM bot_meta WHERE key = ?), ''),
			       COALESCE((SELECT seq FROM sqlite_sequence WHERE name = 'messages'), 0)
		`, messageArchiveIDKey).Scan(&s.ArchiveID, &s.ThroughID); err != nil {
			return fmt.Errorf("read message snapshot boundary: %w", err)
		}
	}
	if s.filter.ChatID != 0 {
		ids, err := messageChatHistory(ctx, s.tx, s.filter.ChatID)
		if err != nil {
			return err
		}
		s.ChatIDs = ids
	}
	return nil
}

func (s *MessageSnapshot) Messages(ctx context.Context) iter.Seq2[StoredMessage, error] {
	return func(yield func(StoredMessage, error) bool) {
		rows, err := queryMessageRows(ctx, s.tx, s.filter, s.ChatIDs)
		if err != nil {
			yield(StoredMessage{}, err)
			return
		}
		defer rows.Close() //nolint:errcheck
		for rows.Next() {
			m, err := scanStoredMessage(rows)
			if !yield(m, err) || err != nil {
				return
			}
		}
		if err := rows.Err(); err != nil {
			yield(StoredMessage{}, err)
		}
	}
}

func (s *MessageSnapshot) Close() error {
	return s.tx.Rollback()
}
