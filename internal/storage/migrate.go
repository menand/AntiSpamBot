package storage

import (
	"context"
	"fmt"
)

// MigrateChat переносит все строки с ключом oldID на newID во всех таблицах,
// сливая их с уже существующими строками newID. Идемпотентен — безопасно
// вызывать, когда с одной из сторон данных нет.
//
// Нужен, когда Telegram апгрейдит basic group до supergroup, что переназначает
// chat_id. Без этого статистика и настройки раскололись бы между двумя
// логическими чатами, а старый chat_id продолжал бы висеть в меню «Мои чаты».
func (d *DB) MigrateChat(ctx context.Context, oldID, newID int64) error {
	if oldID == newID {
		return nil
	}

	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("migrate begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// events — PK здесь id; chat_id можно просто переписать.
	if _, err := tx.ExecContext(ctx,
		`UPDATE events SET chat_id = ? WHERE chat_id = ?`, newID, oldID); err != nil {
		return fmt.Errorf("migrate events: %w", err)
	}

	// members — PK (chat_id, user_id). Оставляем самый ранний joined_at юзера.
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

	// message_counts — PK (chat_id, day). Счётчики суммируем.
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

	// user_activity — PK (chat_id, user_id). Nullable first/last_message_at
	// усложняют слияние: предпочитаем non-null, иначе берём самое
	// раннее/позднее.
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

	// user_message_counts — PK (chat_id, user_id, day). Счётчики суммируем.
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

	// chat_settings — PK chat_id. Если у нового чата уже есть настройки, оставляем их.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO chat_settings (chat_id, greeting_enabled, max_attempts,
			captcha_timeout_seconds, daily_stats_enabled, daily_stats_utc_hour,
			last_daily_stats_day, captcha_mode, greeting_text, greeting_entities, silent_announce_enabled,
			spam_check_enabled, spam_threshold, spam_whitelist_msgs, spam_vote_margin,
			reply_check_enabled, reply_check_seconds)
		SELECT ?, greeting_enabled, max_attempts,
			captcha_timeout_seconds, daily_stats_enabled, daily_stats_utc_hour,
			last_daily_stats_day, captcha_mode, greeting_text, greeting_entities, silent_announce_enabled,
			spam_check_enabled, spam_threshold, spam_whitelist_msgs, spam_vote_margin,
			reply_check_enabled, reply_check_seconds
		FROM chat_settings WHERE chat_id = ?
		ON CONFLICT(chat_id) DO NOTHING
	`, newID, oldID); err != nil {
		return fmt.Errorf("migrate chat_settings: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM chat_settings WHERE chat_id = ?`, oldID); err != nil {
		return fmt.Errorf("drop old chat_settings: %w", err)
	}

	// pending_captchas — старого чата больше нет, всё «pending» там ссылается
	// на мёртвые message ID. Удаляем, а не копируем.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM pending_captchas WHERE chat_id = ?`, oldID); err != nil {
		return fmt.Errorf("drop old pending_captchas: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM pending_replies WHERE chat_id = ?`, oldID); err != nil {
		return fmt.Errorf("drop old pending_replies: %w", err)
	}

	// spam_votes / spam_ballots / greetings — то же самое: message ID умерли
	// вместе со старым чатом, переносить нечего.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM spam_votes WHERE chat_id = ?`, oldID); err != nil {
		return fmt.Errorf("drop old spam_votes: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM spam_ballots WHERE chat_id = ?`, oldID); err != nil {
		return fmt.Errorf("drop old spam_ballots: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM greetings WHERE chat_id = ?`, oldID); err != nil {
		return fmt.Errorf("drop old greetings: %w", err)
	}

	// attempts — PK (chat_id, user_id). Берём max count + самое позднее время обновления.
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

	// chats — удаляем старую строку реестра. Новая либо уже существует
	// (от более ранних событий в supergroup), либо создастся на следующем
	// событии; синтезировать её здесь незачем.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM chats WHERE chat_id = ?`, oldID); err != nil {
		return fmt.Errorf("drop old chats: %w", err)
	}

	return tx.Commit()
}

// DeleteChat удаляет чат только из реестра известных чатов. Исторические
// строки событий/сообщений/участников остаются как архив — их удаление
// потеряло бы статистику для тех, кто позже снова добавит бота в тот же чат,
// и для миграции, если окажется, что это был переход basic group → supergroup,
// триггер которого мы не увидели.
func (d *DB) DeleteChat(ctx context.Context, chatID int64) error {
	if _, err := d.sql.ExecContext(ctx,
		`DELETE FROM chats WHERE chat_id = ?`, chatID); err != nil {
		return fmt.Errorf("delete chat: %w", err)
	}
	return nil
}
