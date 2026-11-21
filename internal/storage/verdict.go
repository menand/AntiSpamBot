package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type SpamVerdict struct {
	ID int64
	SpamVote
	IsSpam      bool
	Why         string
	Ballots     []Ballot
	Outcome     SpamOutcome
	FanoutReady bool
	BanReason   string
}

type SpamOutcome string

const (
	SpamBanApplied  SpamOutcome = "banned"
	SpamBanDenied   SpamOutcome = "denied"
	SpamCleared     SpamOutcome = "clear"
	SpamUnavailable SpamOutcome = "unavailable"
)

const spamVerdictColumns = "id, " + spamVoteColumns + ", is_spam, why, ballots, outcome, fanout_ready, ban_reason"

// AcceptSpamVerdict freezes the vote and its ballots before any Telegram
// request. The decision function uses the exact ballot set in the transaction.
func (d *DB) AcceptSpamVerdict(ctx context.Context, chatID int64, botMsgID int,
	decide func(yes, no int) (spam bool, why string, decided bool)) (int64, error) {
	if botMsgID <= 0 {
		return 0, nil
	}
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("accept verdict: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	vote, err := scanSpamVote(tx.QueryRowContext(ctx,
		`SELECT `+spamVoteColumns+` FROM spam_votes WHERE chat_id = ? AND bot_msg_id = ?`, chatID, botMsgID))
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read vote for verdict: %w", err)
	}

	ballots, err := readBallots(ctx, tx, chatID, botMsgID)
	if err != nil {
		return 0, err
	}
	yes := 0
	for _, ballot := range ballots {
		if ballot.IsSpam {
			yes++
		}
	}
	spam, why, decided := decide(yes, len(ballots)-yes)
	if !decided {
		return 0, nil
	}
	payload, err := json.Marshal(ballots)
	if err != nil {
		return 0, fmt.Errorf("encode verdict ballots: %w", err)
	}
	id, err := nextSpamDecision(ctx, tx)
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO spam_verdicts (id, chat_id, bot_msg_id, target_msg_id, author_id,
			initiator_id, prob, created_at, is_spam, why, ballots)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, vote.ChatID, vote.BotMsgID, vote.TargetMsgID, vote.AuthorID, vote.InitiatorID,
		vote.Prob, vote.CreatedAt.Unix(), spam, why, string(payload)); err != nil {
		return 0, fmt.Errorf("save accepted verdict: %w", err)
	}
	if spam {
		if _, err := tx.ExecContext(ctx, rememberSpamDecisionSQL, chatID, vote.AuthorID, id, 0); err != nil {
			return 0, err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM spam_votes WHERE chat_id = ? AND bot_msg_id = ?`, chatID, botMsgID); err != nil {
		return 0, fmt.Errorf("close accepted vote: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM spam_ballots WHERE chat_id = ? AND bot_msg_id = ?`, chatID, botMsgID); err != nil {
		return 0, fmt.Errorf("close accepted ballots: %w", err)
	}
	return id, tx.Commit()
}

func scanSpamVerdict(row interface{ Scan(...any) error }) (SpamVerdict, error) {
	var v SpamVerdict
	var payload string
	var created int64
	err := row.Scan(&v.ID, &v.ChatID, &v.BotMsgID, &v.TargetMsgID, &v.AuthorID,
		&v.InitiatorID, &v.Prob, &created, &v.IsSpam, &v.Why, &payload, &v.Outcome, &v.FanoutReady, &v.BanReason)
	if err != nil {
		return v, err
	}
	v.CreatedAt = time.Unix(created, 0)
	return v, json.Unmarshal([]byte(payload), &v.Ballots)
}

func (d *DB) SpamVerdict(ctx context.Context, id int64) (SpamVerdict, bool, error) {
	v, err := scanSpamVerdict(d.sql.QueryRowContext(ctx,
		`SELECT `+spamVerdictColumns+` FROM spam_verdicts WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return v, false, nil
	}
	return v, err == nil, err
}

func (d *DB) ListSpamVerdicts(ctx context.Context) ([]SpamVerdict, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT `+spamVerdictColumns+` FROM spam_verdicts ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var out []SpamVerdict
	for rows.Next() {
		v, err := scanSpamVerdict(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// RecordSpamOutcome persists local success together with the one ban event
// and global entry. All three writes roll back if any one fails.
func (d *DB) RecordSpamOutcome(ctx context.Context, v SpamVerdict, outcome SpamOutcome, at time.Time, reason string) (bool, error) {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback() //nolint:errcheck
	r, err := tx.ExecContext(ctx, `UPDATE spam_verdicts SET outcome = ? WHERE id = ? AND chat_id = ? AND outcome = ''`,
		outcome, v.ID, v.ChatID)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	if err != nil || n == 0 {
		return false, err
	}
	if outcome == SpamBanApplied {
		if err := recordLocalSpamBan(ctx, tx, v, at, reason); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

func recordLocalSpamBan(ctx context.Context, tx *sql.Tx, v SpamVerdict, at time.Time, reason string) error {
	if _, err := tx.ExecContext(ctx, rememberGlobalSpamBanSQL,
		v.AuthorID, v.ChatID, at.Unix(), v.ID, v.ChatID, v.AuthorID, v.ID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO events (chat_id, user_id, kind, at, reason) VALUES (?, ?, ?, ?, ?)`,
		v.ChatID, v.AuthorID, EventSpamBan, at.Unix(), nullableString(reason))
	return err
}

func (d *DB) CancelSpamVerdict(ctx context.Context, id int64) error {
	_, err := d.sql.ExecContext(ctx, `DELETE FROM spam_verdicts WHERE id = ?`, id)
	return err
}

func (d *DB) FinishSpamVerdict(ctx context.Context, id, chatID int64) (bool, error) {
	r, err := d.sql.ExecContext(ctx, `DELETE FROM spam_verdicts WHERE id = ? AND chat_id = ? AND outcome <> ''
		AND (outcome <> ? OR (fanout_ready = 1 AND NOT EXISTS (
			SELECT 1 FROM spam_verdict_targets WHERE verdict_id = ? AND status < ?
		)))`, id, chatID, SpamBanApplied, id, CrossBanSkipped)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n == 1, err
}
