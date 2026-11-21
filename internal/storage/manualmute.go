package storage

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type ManualMute struct {
	ChatID, UserID int64
	Generation     string
	UntilDate      int64
	Reason         string
}

func (d *DB) GetManualMute(ctx context.Context, chatID, userID int64) (ManualMute, bool, error) {
	row := ManualMute{ChatID: chatID, UserID: userID}
	err := d.sql.QueryRowContext(ctx, `SELECT generation, until_date, reason
		FROM pending_manual_mutes WHERE chat_id = ? AND user_id = ?`, chatID, userID).
		Scan(&row.Generation, &row.UntilDate, &row.Reason)
	if errors.Is(err, sql.ErrNoRows) {
		return row, false, nil
	}
	return row, err == nil, err
}

func (d *DB) LoadManualMutes(ctx context.Context) ([]ManualMute, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT chat_id, user_id, generation, until_date, reason FROM pending_manual_mutes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var out []ManualMute
	for rows.Next() {
		var m ManualMute
		if err := rows.Scan(&m.ChatID, &m.UserID, &m.Generation, &m.UntilDate, &m.Reason); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (d *DB) PutManualMute(ctx context.Context, m ManualMute) error {
	_, err := d.sql.ExecContext(ctx, `INSERT INTO pending_manual_mutes (chat_id, user_id, generation, until_date, reason)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT(chat_id, user_id) DO UPDATE SET
		generation = excluded.generation, until_date = excluded.until_date, reason = excluded.reason`,
		m.ChatID, m.UserID, m.Generation, m.UntilDate, m.Reason)
	return err
}

// RejectManualMute откатывает только отклонённое поколение. Если раньше был
// другой неподтверждённый мут, снова действующим становится он.
func (d *DB) RejectManualMute(ctx context.Context, m, previous ManualMute) error {
	if previous.Generation == "" {
		_, err := d.sql.ExecContext(ctx, `DELETE FROM pending_manual_mutes
			WHERE chat_id = ? AND user_id = ? AND generation = ?`, m.ChatID, m.UserID, m.Generation)
		return err
	}
	_, err := d.sql.ExecContext(ctx, `UPDATE pending_manual_mutes SET generation = ?, until_date = ?, reason = ?
		WHERE chat_id = ? AND user_id = ? AND generation = ?`,
		previous.Generation, previous.UntilDate, previous.Reason, m.ChatID, m.UserID, m.Generation)
	return err
}

// FinishManualMute гасит более ранние решения и таймеры после подтверждения
// мута. Допуск, уже извлечённый из памяти, тоже закрывает одну проверку.
func (d *DB) FinishManualMute(ctx context.Context, m ManualMute, liveCheck, confirmed, keepQuarantine bool) (PendingRow, bool, error) {
	var card PendingRow
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return card, false, err
	}
	defer tx.Rollback() //nolint:errcheck
	chatID, userID := m.ChatID, m.UserID
	res, err := tx.ExecContext(ctx, `DELETE FROM pending_manual_mutes WHERE chat_id = ? AND user_id = ? AND generation = ?`,
		chatID, userID, m.Generation)
	if err != nil {
		return card, false, err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return card, false, err
	}

	err = tx.QueryRowContext(ctx, `DELETE FROM pending_captchas WHERE chat_id = ? AND user_id = ?
		RETURNING message_id, ephemeral_msg_id`, chatID, userID).Scan(&card.MessageID, &card.EphemeralID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return card, false, err
	}
	liveCheck = liveCheck || err == nil
	var admitted PendingRow
	var closesCheck bool
	err = tx.QueryRowContext(ctx, `DELETE FROM pending_admissions WHERE chat_id = ? AND user_id = ?
		RETURNING message_id, ephemeral_msg_id, closes_check`, chatID, userID).
		Scan(&admitted.MessageID, &admitted.EphemeralID, &closesCheck)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return card, false, err
	}
	if card.MessageID == 0 && card.EphemeralID == 0 {
		card = admitted
	}
	replies, err := tx.ExecContext(ctx, `DELETE FROM pending_replies WHERE chat_id = ? AND user_id = ?`, chatID, userID)
	if err != nil {
		return card, false, err
	}
	repliesRemoved, err := replies.RowsAffected()
	if err != nil {
		return card, false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM pending_punishments WHERE chat_id = ? AND user_id = ?`, chatID, userID); err != nil {
		return card, false, err
	}
	if !keepQuarantine {
		if _, err := tx.ExecContext(ctx, `DELETE FROM chat_quarantine WHERE chat_id = ? AND user_id = ?`, chatID, userID); err != nil {
			return card, false, err
		}
	}
	now := time.Now().Unix()
	if liveCheck || closesCheck || repliesRemoved > 0 {
		kind := EventAbort
		if confirmed {
			kind = EventPass
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO events (chat_id, user_id, kind, at)
			VALUES (?, ?, ?, ?)`, chatID, userID, kind, now); err != nil {
			return card, false, err
		}
	}
	if confirmed {
		if _, err := tx.ExecContext(ctx, `INSERT INTO events (chat_id, user_id, kind, at, reason)
			VALUES (?, ?, ?, ?, ?)`, chatID, userID, EventMute, now, m.Reason); err != nil {
			return card, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return card, false, err
	}
	return card, true, nil
}

func (d *DB) DeleteChatManualMutes(ctx context.Context, chatID int64) error {
	_, err := d.sql.ExecContext(ctx, `DELETE FROM pending_manual_mutes WHERE chat_id = ?`, chatID)
	return err
}
