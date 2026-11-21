package storage

import (
	"context"
	"database/sql"
)

// Старые вердикты уже имеют AUTOINCREMENT ID. Новые решения и прощения
// используют тот же порядок, включая промежутки после удаления вердиктов.
func nextSpamDecision(ctx context.Context, tx *sql.Tx) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `
		INSERT INTO bot_meta (key, value) VALUES ('spam_decision_id',
			1 + COALESCE((SELECT seq FROM sqlite_sequence WHERE name = 'spam_verdicts'), 0))
		ON CONFLICT(key) DO UPDATE SET value = max(1 + CAST(bot_meta.value AS INTEGER), CAST(excluded.value AS INTEGER))
		RETURNING CAST(value AS INTEGER)`).Scan(&id)
	return id, err
}

const rememberSpamDecisionSQL = `INSERT INTO chat_spam_decisions (chat_id, user_id, verdict_id, pardon_id)
	VALUES (?, ?, ?, ?) ON CONFLICT(chat_id, user_id) DO UPDATE SET
	verdict_id = max(verdict_id, excluded.verdict_id), pardon_id = max(pardon_id, excluded.pardon_id)`

func (d *DB) BeginDirectSpamDecision(ctx context.Context, v SpamVote, why, reason string) (int64, error) {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck
	id, err := nextSpamDecision(ctx, tx)
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, rememberSpamDecisionSQL, v.ChatID, v.AuthorID, id, 0); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO spam_verdicts
		(id, chat_id, bot_msg_id, target_msg_id, author_id, initiator_id, prob, created_at, is_spam, why, ballots, ban_reason)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?, '[]', ?)`, id, v.ChatID, v.BotMsgID, v.TargetMsgID,
		v.AuthorID, v.InitiatorID, v.Prob, v.CreatedAt.Unix(), why, reason); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

func (d *DB) SpamBanStillCurrent(ctx context.Context, source, target, userID, id int64) (bool, error) {
	var current bool
	err := d.sql.QueryRowContext(ctx, `SELECT NOT EXISTS(
		SELECT 1 FROM chat_spam_decisions WHERE chat_id IN (?, ?) AND user_id = ? AND pardon_id >= ?
	)`, source, target, userID, id).Scan(&current)
	return current, err
}

func (d *DB) IsSpamBannedInChat(ctx context.Context, chatID, userID int64) (bool, error) {
	var banned bool
	err := d.sql.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM spam_banned AS s WHERE s.user_id = ? AND NOT EXISTS (
			SELECT 1 FROM chat_spam_decisions AS c WHERE c.chat_id IN (s.chat_id, ?)
			AND c.user_id = s.user_id AND c.pardon_id > 0 AND c.pardon_id >= s.decision_id
		)
	)`, userID, chatID).Scan(&banned)
	return banned, err
}

func (d *DB) NewerSpamDecision(ctx context.Context, chatID, userID, pardonID int64) (bool, error) {
	var newer bool
	err := d.sql.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM chat_spam_decisions WHERE chat_id = ? AND user_id = ? AND verdict_id > ?
	)`, chatID, userID, pardonID).Scan(&newer)
	return newer, err
}

const rememberGlobalSpamBanSQL = `INSERT INTO spam_banned (user_id, chat_id, at, decision_id)
	SELECT ?, ?, ?, ? WHERE NOT EXISTS (
		SELECT 1 FROM chat_spam_decisions WHERE chat_id = ? AND user_id = ? AND pardon_id >= ?
	) ON CONFLICT(user_id) DO UPDATE SET decision_id = max(decision_id, excluded.decision_id)`
