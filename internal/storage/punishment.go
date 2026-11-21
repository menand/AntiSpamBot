package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type PendingPunishment struct {
	ChatID, UserID             int64
	Generation, Reason, Detail string
	Kind                       EventKind
	Attempt                    int
	BanUntil                   int64
	BanConfirmed               bool
}

const punishmentColumns = `chat_id, user_id, generation, kind, reason, detail, attempt, ban_until, ban_confirmed`

func scanPunishment(scanner interface{ Scan(...any) error }) (PendingPunishment, error) {
	var p PendingPunishment
	err := scanner.Scan(&p.ChatID, &p.UserID, &p.Generation, &p.Kind, &p.Reason, &p.Detail,
		&p.Attempt, &p.BanUntil, &p.BanConfirmed)
	return p, err
}

func (d *DB) GetPunishment(ctx context.Context, chatID, userID int64) (PendingPunishment, bool, error) {
	p, err := scanPunishment(d.sql.QueryRowContext(ctx, `SELECT `+punishmentColumns+
		` FROM pending_punishments WHERE chat_id = ? AND user_id = ?`, chatID, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return p, false, nil
	}
	return p, err == nil, err
}

func (d *DB) LoadPunishments(ctx context.Context) ([]PendingPunishment, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT `+punishmentColumns+` FROM pending_punishments`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var out []PendingPunishment
	for rows.Next() {
		p, err := scanPunishment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeferPunishment запоминает исход проверки до подтверждения членства.
func (d *DB) DeferPunishment(ctx context.Context, p PendingPunishment) error {
	_, err := d.sql.ExecContext(ctx, `INSERT INTO pending_punishments (`+punishmentColumns+`)
		VALUES (?, ?, ?, '', ?, ?, 0, 0, 0) ON CONFLICT(chat_id, user_id) DO NOTHING`,
		p.ChatID, p.UserID, p.Generation, p.Reason, p.Detail)
	return err
}

// BeginPunishment связывает попытку и выбор санкции одной транзакцией.
// Нулевой порог обозначает ручной кик, который не входит в счётчик капчи.
func (d *DB) BeginPunishment(ctx context.Context, p PendingPunishment, limit int, ttl time.Duration) (PendingPunishment, bool, error) {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return p, false, err
	}
	defer tx.Rollback() //nolint:errcheck
	old, err := scanPunishment(tx.QueryRowContext(ctx, `SELECT `+punishmentColumns+
		` FROM pending_punishments WHERE chat_id = ? AND user_id = ?`, p.ChatID, p.UserID))
	deferred := err == nil && old.Kind == "" && old.Reason == p.Reason && limit > 0
	if err == nil && !deferred {
		return old, false, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return p, false, err
	}
	p.Kind = EventKick
	if limit > 0 {
		p.Attempt, err = incrementAttempt(ctx, tx, p.ChatID, p.UserID, ttl)
		if err != nil {
			return p, false, err
		}
		if p.Attempt >= limit {
			p.Kind, p.BanUntil = EventBan, 0
		}
	}
	if deferred {
		p.Generation, p.Detail = old.Generation, old.Detail
		if _, err := tx.ExecContext(ctx, `UPDATE pending_punishments SET kind = ?, attempt = ?, ban_until = ?
			WHERE chat_id = ? AND user_id = ? AND generation = ?`, p.Kind, p.Attempt, p.BanUntil,
			p.ChatID, p.UserID, p.Generation); err != nil {
			return p, false, err
		}
		return p, true, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO pending_punishments (`+punishmentColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0)`, p.ChatID, p.UserID, p.Generation, p.Kind, p.Reason,
		p.Detail, p.Attempt, p.BanUntil); err != nil {
		return p, false, err
	}
	return p, true, tx.Commit()
}

func (d *DB) MarkPunishmentBanConfirmed(ctx context.Context, p PendingPunishment) error {
	result, err := d.sql.ExecContext(ctx, `UPDATE pending_punishments SET ban_confirmed = 1
		WHERE chat_id = ? AND user_id = ? AND generation = ?`, p.ChatID, p.UserID, p.Generation)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n != 1 {
		return fmt.Errorf("punishment generation changed")
	}
	return err
}

func (d *DB) DeletePunishment(ctx context.Context, p PendingPunishment) error {
	_, err := d.sql.ExecContext(ctx, `DELETE FROM pending_punishments
		WHERE chat_id = ? AND user_id = ? AND generation = ?`, p.ChatID, p.UserID, p.Generation)
	return err
}

// FinishPunishment одновременно закрывает решение, проверку и её событие.
func (d *DB) FinishPunishment(ctx context.Context, p PendingPunishment, kind EventKind) (bool, error) {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback() //nolint:errcheck
	result, err := tx.ExecContext(ctx, `DELETE FROM pending_punishments
		WHERE chat_id = ? AND user_id = ? AND generation = ?`, p.ChatID, p.UserID, p.Generation)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil || n == 0 {
		return false, err
	}
	for _, query := range []string{
		`DELETE FROM pending_captchas WHERE chat_id = ? AND user_id = ?`,
		`DELETE FROM pending_replies WHERE chat_id = ? AND user_id = ?`,
		`DELETE FROM pending_admissions WHERE chat_id = ? AND user_id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, query, p.ChatID, p.UserID); err != nil {
			return false, err
		}
	}
	if kind != "" {
		reason := p.Reason
		if kind != EventBan && kind != EventKick {
			reason = ""
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO events (chat_id, user_id, kind, at, reason)
			VALUES (?, ?, ?, ?, ?)`, p.ChatID, p.UserID, kind, time.Now().Unix(), nullableString(reason)); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

func (d *DB) DeleteChatPunishments(ctx context.Context, chatID int64) error {
	_, err := d.sql.ExecContext(ctx, `DELETE FROM pending_punishments WHERE chat_id = ?`, chatID)
	return err
}
