package storage

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// QuarantineRow — активный карантин одного (chat, user).
type QuarantineRow struct {
	ChatID  int64
	UserID  int64
	UntilAt time.Time
	Warned  bool
}

// PutQuarantine записывает карантинную строку (upsert): рестарт-безопасность
// бот-сайд цензора и флаг разового эфемерного пояснения.
func (d *DB) PutQuarantine(ctx context.Context, chatID, userID int64, untilAt time.Time, warned bool) error {
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO chat_quarantine (chat_id, user_id, until_at, warned)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(chat_id, user_id) DO UPDATE SET
			until_at = excluded.until_at,
			warned = excluded.warned
	`, chatID, userID, untilAt.Unix(), boolToInt(warned))
	if err != nil {
		return fmt.Errorf("put quarantine: %w", err)
	}
	return nil
}

// SetQuarantineWarned помечает, что эфемерное пояснение показано (write-only
// флаг, обратно не сбрасывается).
func (d *DB) SetQuarantineWarned(ctx context.Context, chatID, userID int64) error {
	_, err := d.sql.ExecContext(ctx,
		`UPDATE chat_quarantine SET warned = 1 WHERE chat_id = ? AND user_id = ?`,
		chatID, userID)
	if err != nil {
		return fmt.Errorf("set quarantine warned: %w", err)
	}
	return nil
}

// AllQuarantines возвращает активные строки карантина чата — питает отпуск
// всех ограниченных при выключении карантина. Истёкшие не отдаёт (их снимет
// Telegram, отпускать нечего).
func (d *DB) AllQuarantines(ctx context.Context, chatID, now int64) ([]QuarantineRow, error) {
	return scanQuarantineRows(ctx, d.sql, `
		SELECT chat_id, user_id, until_at, warned
		FROM chat_quarantine WHERE chat_id = ? AND until_at > ?
	`, chatID, now)
}

// DeleteChatQuarantine удаляет все строки карантина чата (бот покинул чат /
// выключил карантин).
func (d *DB) DeleteChatQuarantine(ctx context.Context, chatID int64) error {
	if _, err := d.sql.ExecContext(ctx,
		`DELETE FROM chat_quarantine WHERE chat_id = ?`, chatID); err != nil {
		return fmt.Errorf("delete chat quarantine: %w", err)
	}
	return nil
}

// DeleteQuarantine удаляет карантинную строку одного (chat, user): /trust,
// промоушен в админы, эскалация, точечный отпуск reconcile-свипа.
func (d *DB) DeleteQuarantine(ctx context.Context, chatID, userID int64) error {
	if _, err := d.sql.ExecContext(ctx,
		`DELETE FROM chat_quarantine WHERE chat_id = ? AND user_id = ?`, chatID, userID); err != nil {
		return fmt.Errorf("delete quarantine: %w", err)
	}
	return nil
}

// RestoreQuarantines поднимает все активные карантины после рестарта для
// сида in-memory зеркала цензора.
func (d *DB) RestoreQuarantines(ctx context.Context, now int64) ([]QuarantineRow, error) {
	return scanQuarantineRows(ctx, d.sql, `
		SELECT chat_id, user_id, until_at, warned
		FROM chat_quarantine WHERE until_at > ?
	`, now)
}

// DisabledChatQuarantines — активные карантины чатов, где фича СЕЙЧАС
// выключена (chat_settings.quarantine_enabled != 1 или строки настроек нет).
// Питает reconcile-свип: незавершённый отпуск (крэш между тогглом и релизом,
// БД-правка) не должен удерживать серверный рестрикт до старого until_at.
func (d *DB) DisabledChatQuarantines(ctx context.Context, now int64) ([]QuarantineRow, error) {
	return scanQuarantineRows(ctx, d.sql, `
		SELECT q.chat_id, q.user_id, q.until_at, q.warned
		FROM chat_quarantine q
		LEFT JOIN chat_settings s ON s.chat_id = q.chat_id
		WHERE q.until_at > ? AND COALESCE(s.quarantine_enabled, 0) = 0
	`, now)
}

// scanQuarantineRows — общий сканер строк карантина для трёх чтений выше.
// args должны покрывать все «?» запроса.
func scanQuarantineRows(ctx context.Context, q *sql.DB, query string, args ...any) ([]QuarantineRow, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("quarantine scan: %w", err)
	}
	defer rows.Close() //nolint:errcheck // cleanup, error intentionally ignored
	var out []QuarantineRow
	for rows.Next() {
		var r QuarantineRow
		var until int64
		var warned int
		if err := rows.Scan(&r.ChatID, &r.UserID, &until, &warned); err != nil {
			return nil, fmt.Errorf("scan quarantine: %w", err)
		}
		r.UntilAt = time.Unix(until, 0)
		r.Warned = warned != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// PruneQuarantines подрезает истёкшие строки (срок истёк, рестрикт снял
// Telegram) — столбик не должен расти вечно.
func (d *DB) PruneQuarantines(ctx context.Context, before int64) error {
	if _, err := d.sql.ExecContext(ctx,
		`DELETE FROM chat_quarantine WHERE until_at <= ?`, before); err != nil {
		return fmt.Errorf("prune quarantines: %w", err)
	}
	return nil
}

// HasQuarantine — быстрый «есть ли активный карантин». Не на горячем пути
// (in-memory зеркало для этого есть), нужен тестам и редким проверкам.
func (d *DB) HasQuarantine(ctx context.Context, chatID, userID, now int64) (bool, error) {
	var one int
	err := d.sql.QueryRowContext(ctx,
		`SELECT 1 FROM chat_quarantine WHERE chat_id = ? AND user_id = ? AND until_at > ?`,
		chatID, userID, now).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("has quarantine: %w", err)
	}
	return true, nil
}
