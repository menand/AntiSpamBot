package storage

import (
	"context"
	"fmt"
)

// MigrateChat moves all rows keyed by oldID to newID across every table,
// merging with existing rows at newID. Idempotent — safe to call when one
// side has no data.
//
// Used when Telegram upgrades a basic group to a supergroup, which reassigns
// the chat_id. Without this, stats and settings would be split across two
// logical chats and the old chat_id would linger in the "Мои чаты" menu.
func (d *DB) MigrateChat(ctx context.Context, oldID, newID int64) error {
	if oldID == newID {
		return nil
	}

	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("migrate begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// events — id is PK; chat_id can just be rewritten.
	if _, err := tx.ExecContext(ctx,
		`UPDATE events SET chat_id = ? WHERE chat_id = ?`, newID, oldID); err != nil {
		return fmt.Errorf("migrate events: %w", err)
	}

	// members — (chat_id, user_id) PK. Keep earliest joined_at per user.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO members (chat_id, user_id, joined_at)
		SELECT ?, user_id, joined_at FROM members WHERE chat_id = ?
		ON CONFLICT(chat_id, user_id) DO UPDATE SET
			joined_at = min(members.joined_at, excluded.joined_at)
	`, newID, oldID); err != nil {
		return fmt.Errorf("migrate members: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM members WHERE chat_id = ?`, oldID); err != nil {
		return fmt.Errorf("drop old members: %w", err)
	}

	// message_counts — (chat_id, day) PK. Sum counts.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO message_counts (chat_id, day, newcomer_count, oldtimer_count)
		SELECT ?, day, newcomer_count, oldtimer_count FROM message_counts WHERE chat_id = ?
		ON CONFLICT(chat_id, day) DO UPDATE SET
			newcomer_count = newcomer_count + excluded.newcomer_count,
			oldtimer_count = oldtimer_count + excluded.oldtimer_count
	`, newID, oldID); err != nil {
		return fmt.Errorf("migrate message_counts: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM message_counts WHERE chat_id = ?`, oldID); err != nil {
		return fmt.Errorf("drop old message_counts: %w", err)
	}

	// user_activity — (chat_id, user_id) PK. Nullable first/last_message_at
	// complicate the merge: prefer non-null, else take earliest/latest.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO user_activity (chat_id, user_id, first_message_at, last_message_at, message_count)
		SELECT ?, user_id, first_message_at, last_message_at, message_count FROM user_activity WHERE chat_id = ?
		ON CONFLICT(chat_id, user_id) DO UPDATE SET
			first_message_at = CASE
				WHEN user_activity.first_message_at IS NULL THEN excluded.first_message_at
				WHEN excluded.first_message_at IS NULL THEN user_activity.first_message_at
				ELSE min(user_activity.first_message_at, excluded.first_message_at)
			END,
			last_message_at = CASE
				WHEN user_activity.last_message_at IS NULL THEN excluded.last_message_at
				WHEN excluded.last_message_at IS NULL THEN user_activity.last_message_at
				ELSE max(user_activity.last_message_at, excluded.last_message_at)
			END,
			message_count = user_activity.message_count + excluded.message_count
	`, newID, oldID); err != nil {
		return fmt.Errorf("migrate user_activity: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM user_activity WHERE chat_id = ?`, oldID); err != nil {
		return fmt.Errorf("drop old user_activity: %w", err)
	}

	// user_message_counts — (chat_id, user_id, day) PK. Sum counts.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO user_message_counts (chat_id, user_id, day, count)
		SELECT ?, user_id, day, count FROM user_message_counts WHERE chat_id = ?
		ON CONFLICT(chat_id, user_id, day) DO UPDATE SET
			count = count + excluded.count
	`, newID, oldID); err != nil {
		return fmt.Errorf("migrate user_message_counts: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM user_message_counts WHERE chat_id = ?`, oldID); err != nil {
		return fmt.Errorf("drop old user_message_counts: %w", err)
	}

	// chat_settings — chat_id PK. Prefer existing new-chat setting if any.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO chat_settings (chat_id, greeting_enabled)
		SELECT ?, greeting_enabled FROM chat_settings WHERE chat_id = ?
		ON CONFLICT(chat_id) DO NOTHING
	`, newID, oldID); err != nil {
		return fmt.Errorf("migrate chat_settings: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM chat_settings WHERE chat_id = ?`, oldID); err != nil {
		return fmt.Errorf("drop old chat_settings: %w", err)
	}

	// pending_captchas — the old chat is gone, any "pending" there refers to
	// dead message IDs. Drop rather than copy.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM pending_captchas WHERE chat_id = ?`, oldID); err != nil {
		return fmt.Errorf("drop old pending_captchas: %w", err)
	}

	// attempts — (chat_id, user_id) PK. Take max count + latest update time.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO attempts (chat_id, user_id, count, updated_at)
		SELECT ?, user_id, count, updated_at FROM attempts WHERE chat_id = ?
		ON CONFLICT(chat_id, user_id) DO UPDATE SET
			count = max(attempts.count, excluded.count),
			updated_at = max(attempts.updated_at, excluded.updated_at)
	`, newID, oldID); err != nil {
		return fmt.Errorf("migrate attempts: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM attempts WHERE chat_id = ?`, oldID); err != nil {
		return fmt.Errorf("drop old attempts: %w", err)
	}

	// chats — delete old registry row. The new one either already exists
	// (from earlier events in the supergroup) or will be created on the
	// next event; no need to synthesize one here.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM chats WHERE chat_id = ?`, oldID); err != nil {
		return fmt.Errorf("drop old chats: %w", err)
	}

	return tx.Commit()
}

// DeleteChat removes the chat from the known-chats registry only. Historical
// event/message/member rows are kept for archival — deleting them would lose
// stats for anyone re-adding the bot to the same chat later, or for migration
// if this turns out to have been a basic-group → supergroup transition we
// didn't see the trigger for.
func (d *DB) DeleteChat(ctx context.Context, chatID int64) error {
	if _, err := d.sql.ExecContext(ctx,
		`DELETE FROM chats WHERE chat_id = ?`, chatID); err != nil {
		return fmt.Errorf("delete chat: %w", err)
	}
	return nil
}
