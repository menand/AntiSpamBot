package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const (
	AdmissionCaptcha    = "captcha"
	AdmissionUnban      = "u"
	AdmissionUnbanChat  = "f"
	AdmissionLocalUnban = "local_unban"
	AdmissionWhitelist  = "w"
)

type PendingAdmission struct {
	ChatID, UserID                        int64
	Generation, Action                    string
	MessageID, EphemeralID, ThreadID      int
	ReleaseNeeded, ClosesCheck, UnbanDone bool
	BanPending                            bool
	QuarantineUntil                       int64
	PardonID                              int64
}

const admissionColumns = `chat_id, user_id, generation, action, message_id, ephemeral_msg_id,
	thread_id, release_needed, closes_check, quarantine_until, unban_done, ban_pending, pardon_id`

func scanAdmission(row interface{ Scan(...any) error }) (PendingAdmission, error) {
	var p PendingAdmission
	err := row.Scan(&p.ChatID, &p.UserID, &p.Generation, &p.Action, &p.MessageID,
		&p.EphemeralID, &p.ThreadID, &p.ReleaseNeeded, &p.ClosesCheck, &p.QuarantineUntil, &p.UnbanDone, &p.BanPending, &p.PardonID)
	return p, err
}

func (d *DB) GetPendingAdmission(ctx context.Context, chatID, userID int64) (PendingAdmission, bool, error) {
	p, err := scanAdmission(d.sql.QueryRowContext(ctx, `SELECT `+admissionColumns+
		` FROM pending_admissions WHERE chat_id = ? AND user_id = ?`, chatID, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return p, false, nil
	}
	return p, err == nil, err
}

func (d *DB) LoadPendingAdmissions(ctx context.Context) ([]PendingAdmission, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT `+admissionColumns+` FROM pending_admissions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var out []PendingAdmission
	for rows.Next() {
		p, err := scanAdmission(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// AcceptCaptcha атомарно заменяет проверку принятым решением.
func (d *DB) AcceptCaptcha(ctx context.Context, p PendingAdmission) (bool, error) {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback() //nolint:errcheck
	res, err := tx.ExecContext(ctx, `
		DELETE FROM pending_captchas WHERE chat_id = ? AND user_id = ?
		AND message_id = ? AND ephemeral_msg_id = ? AND suspended = 0
	`, p.ChatID, p.UserID, p.MessageID, p.EphemeralID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return false, err
	}
	if err := putAdmission(ctx, tx, p); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// AcceptManualAdmission фиксирует ручной допуск вместе с прекращением
// текущих проверок и, для /whitelist, добавлением в доверенные.
func (d *DB) AcceptManualAdmission(ctx context.Context, p PendingAdmission, otherPardonChats []int64) (PendingAdmission, error) {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return p, err
	}
	defer tx.Rollback() //nolint:errcheck
	old, err := scanAdmission(tx.QueryRowContext(ctx, `SELECT `+admissionColumns+
		` FROM pending_admissions WHERE chat_id = ? AND user_id = ?`, p.ChatID, p.UserID))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return p, err
	}
	if err == nil {
		p.ReleaseNeeded, p.ClosesCheck = old.ReleaseNeeded, old.ClosesCheck
		p.MessageID, p.EphemeralID = old.MessageID, old.EphemeralID
		p.UnbanDone = old.UnbanDone && !old.BanPending
	}
	if p.Action == AdmissionUnban || p.Action == AdmissionLocalUnban {
		p.PardonID, err = nextSpamDecision(ctx, tx)
		if err != nil {
			return p, err
		}
		chats := []int64{p.ChatID}
		if p.Action == AdmissionUnban {
			chats = append(append(chats, 0), otherPardonChats...)
		}
		for _, chatID := range chats {
			if _, err := tx.ExecContext(ctx, rememberSpamDecisionSQL, chatID, p.UserID, 0, p.PardonID); err != nil {
				return p, err
			}
		}
	}
	err = tx.QueryRowContext(ctx, `
		DELETE FROM pending_captchas WHERE chat_id = ? AND user_id = ? RETURNING message_id, ephemeral_msg_id
	`, p.ChatID, p.UserID).Scan(&p.MessageID, &p.EphemeralID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return p, err
	}
	p.ReleaseNeeded = p.ReleaseNeeded || err == nil
	p.ClosesCheck = p.ClosesCheck || err == nil
	res, err := tx.ExecContext(ctx, `DELETE FROM pending_replies WHERE chat_id = ? AND user_id = ?`, p.ChatID, p.UserID)
	if err != nil {
		return p, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return p, err
	}
	p.ClosesCheck = p.ClosesCheck || n > 0
	var quarantined bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM chat_quarantine WHERE chat_id = ? AND user_id = ? AND until_at > ?
	)`, p.ChatID, p.UserID, time.Now().Unix()).Scan(&quarantined); err != nil {
		return p, err
	}
	p.ReleaseNeeded = p.ReleaseNeeded || quarantined
	if p.Action == AdmissionWhitelist {
		if _, err := tx.ExecContext(ctx, `INSERT INTO trusted_users (chat_id, user_id, at)
			VALUES (?, ?, ?) ON CONFLICT(chat_id, user_id) DO NOTHING`, p.ChatID, p.UserID, time.Now().Unix()); err != nil {
			return p, err
		}
	}
	res, err = tx.ExecContext(ctx, `DELETE FROM pending_punishments WHERE chat_id = ? AND user_id = ?`, p.ChatID, p.UserID)
	if err != nil {
		return p, err
	}
	n, err = res.RowsAffected()
	if err != nil {
		return p, err
	}
	p.UnbanDone = p.UnbanDone && n == 0
	if err := putAdmission(ctx, tx, p); err != nil {
		return p, err
	}
	return p, tx.Commit()
}

func putAdmission(ctx context.Context, tx *sql.Tx, p PendingAdmission) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO pending_admissions (`+admissionColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(chat_id, user_id) DO UPDATE SET generation = excluded.generation,
			action = excluded.action, message_id = excluded.message_id, ephemeral_msg_id = excluded.ephemeral_msg_id,
			thread_id = excluded.thread_id, release_needed = excluded.release_needed,
			closes_check = excluded.closes_check, quarantine_until = excluded.quarantine_until, unban_done = excluded.unban_done,
			ban_pending = excluded.ban_pending, pardon_id = excluded.pardon_id
	`, p.ChatID, p.UserID, p.Generation, p.Action, p.MessageID, p.EphemeralID, p.ThreadID,
		p.ReleaseNeeded, p.ClosesCheck, p.QuarantineUntil, p.UnbanDone, p.BanPending, p.PardonID)
	return err
}

func (d *DB) UpdateAdmissionRights(ctx context.Context, p PendingAdmission) error {
	r, err := d.sql.ExecContext(ctx, `UPDATE pending_admissions SET quarantine_until = ?, unban_done = ?, ban_pending = ?
		WHERE chat_id = ? AND user_id = ? AND generation = ?`, p.QuarantineUntil, p.UnbanDone, p.BanPending,
		p.ChatID, p.UserID, p.Generation)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err == nil && n != 1 {
		return fmt.Errorf("admission changed")
	}
	return err
}

func (d *DB) DeletePendingAdmission(ctx context.Context, p PendingAdmission) error {
	_, err := d.sql.ExecContext(ctx, `DELETE FROM pending_admissions WHERE chat_id = ? AND user_id = ? AND generation = ?`,
		p.ChatID, p.UserID, p.Generation)
	return err
}

// FinishAdmission подтверждает права, обновляет локальное состояние и
// закрывает воронку либо создаёт единственное ожидание ответа.
func (d *DB) FinishAdmission(ctx context.Context, p PendingAdmission, replyDeadline time.Time) (bool, error) {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback() //nolint:errcheck
	r, err := tx.ExecContext(ctx, `DELETE FROM pending_admissions
		WHERE chat_id = ? AND user_id = ? AND generation = ?`, p.ChatID, p.UserID, p.Generation)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	if err != nil || n == 0 {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM attempts WHERE chat_id = ? AND user_id = ?`, p.ChatID, p.UserID); err != nil {
		return false, err
	}
	now := time.Now().Unix()
	if p.Action == AdmissionUnban {
		if _, err := tx.ExecContext(ctx, `DELETE FROM spam_banned WHERE user_id = ? AND decision_id <= ?`, p.UserID, p.PardonID); err != nil {
			return false, err
		}
	}
	if p.Action == AdmissionCaptcha {
		if _, err := tx.ExecContext(ctx, `INSERT INTO members (chat_id, user_id, joined_at) VALUES (?, ?, ?)
			ON CONFLICT(chat_id, user_id) DO UPDATE SET joined_at = excluded.joined_at`, p.ChatID, p.UserID, now); err != nil {
			return false, err
		}
	}
	if p.QuarantineUntil > now {
		if _, err := tx.ExecContext(ctx, `INSERT INTO chat_quarantine (chat_id, user_id, until_at, warned)
			VALUES (?, ?, ?, 0) ON CONFLICT(chat_id, user_id) DO UPDATE SET until_at = excluded.until_at, warned = 0`,
			p.ChatID, p.UserID, p.QuarantineUntil); err != nil {
			return false, err
		}
	} else if _, err := tx.ExecContext(ctx, `DELETE FROM chat_quarantine WHERE chat_id = ? AND user_id = ?`, p.ChatID, p.UserID); err != nil {
		return false, err
	}
	if !replyDeadline.IsZero() {
		if _, err := tx.ExecContext(ctx, `INSERT INTO pending_replies (chat_id, user_id, expires_at, thread_id, welcome_claimed)
			VALUES (?, ?, ?, ?, 1) ON CONFLICT(chat_id, user_id) DO UPDATE SET expires_at = excluded.expires_at,
			thread_id = excluded.thread_id, stage = 1, greeting_msg_id = 0, suspended = 0, welcome_claimed = 1`,
			p.ChatID, p.UserID, replyDeadline.Unix(), p.ThreadID); err != nil {
			return false, err
		}
	} else if p.ClosesCheck {
		if _, err := tx.ExecContext(ctx, `INSERT INTO events (chat_id, user_id, kind, at) VALUES (?, ?, ?, ?)`,
			p.ChatID, p.UserID, EventPass, now); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

func (d *DB) DeleteChatAdmissions(ctx context.Context, chatID int64) error {
	_, err := d.sql.ExecContext(ctx, `DELETE FROM pending_admissions WHERE chat_id = ?`, chatID)
	return err
}
