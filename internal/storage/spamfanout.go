package storage

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type CrossBanStatus int

const (
	CrossBanPending CrossBanStatus = iota
	CrossBanStarted
	CrossBanSkipped
	CrossBanApplied
)

type CrossBanTarget struct {
	ChatID int64
	Status CrossBanStatus
}

func (d *DB) InitSpamFanout(ctx context.Context, verdictID int64, chats []int64) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	r, err := tx.ExecContext(ctx, `UPDATE spam_verdicts SET fanout_ready = 1 WHERE id = ? AND outcome = ? AND fanout_ready = 0`, verdictID, SpamBanApplied)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil || n == 0 {
		return err
	}
	for _, chatID := range chats {
		if _, err := tx.ExecContext(ctx, `INSERT INTO spam_verdict_targets (verdict_id, chat_id) VALUES (?, ?)`, verdictID, chatID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (d *DB) CrossBanTargets(ctx context.Context, verdictID int64) ([]CrossBanTarget, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT chat_id, status FROM spam_verdict_targets WHERE verdict_id = ? ORDER BY chat_id`, verdictID)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var targets []CrossBanTarget
	for rows.Next() {
		var target CrossBanTarget
		if err := rows.Scan(&target.ChatID, &target.Status); err != nil {
			return nil, err
		}
		targets = append(targets, target)
	}
	return targets, rows.Err()
}

func (d *DB) CrossBanStatus(ctx context.Context, verdictID, chatID int64) (CrossBanStatus, bool, error) {
	var status CrossBanStatus
	err := d.sql.QueryRowContext(ctx, `SELECT status FROM spam_verdict_targets WHERE verdict_id = ? AND chat_id = ?`, verdictID, chatID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return status, false, nil
	}
	return status, err == nil, err
}

func (d *DB) StartCrossBan(ctx context.Context, verdictID, chatID int64) (bool, error) {
	r, err := d.sql.ExecContext(ctx, `UPDATE spam_verdict_targets SET status = ? WHERE verdict_id = ? AND chat_id = ? AND status = ?`,
		CrossBanStarted, verdictID, chatID, CrossBanPending)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n == 1, err
}

func (d *DB) FinishCrossBan(ctx context.Context, v SpamVerdict, chatID int64, status CrossBanStatus) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	r, err := tx.ExecContext(ctx, `UPDATE spam_verdict_targets SET status = ? WHERE verdict_id = ? AND chat_id = ? AND status < ?`,
		status, v.ID, chatID, CrossBanSkipped)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil || n == 0 {
		return err
	}
	if status == CrossBanApplied {
		if _, err := tx.ExecContext(ctx, rememberSpamDecisionSQL, chatID, v.AuthorID, v.ID, 0); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO events (chat_id, user_id, kind, at) VALUES (?, ?, ?, ?)`,
			chatID, v.AuthorID, EventFunnelClose, time.Now().Unix()); err != nil {
			return err
		}
	}
	return tx.Commit()
}
