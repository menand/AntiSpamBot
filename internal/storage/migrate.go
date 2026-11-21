package storage

import (
	"context"
	"fmt"
)

// MigrateChat переносит настройки и состояние с oldID на newID и
// сохраняет связь для чтения истории сообщений. Повтор той же миграции
// безопасен, в том числе при отсутствии данных с одной из сторон.
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

	// Message rows (including later writes from the capture queue) retain
	// their source chat ID. The same lookup covers hot and archived rows.
	if err := recordMessageChatMove(ctx, tx, oldID, newID); err != nil {
		return err
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

	// trusted_users — PK (chat_id, user_id). При конфликте остаётся строка
	// нового чата (время добавления информационное, сливать нечего).
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO trusted_users (chat_id, user_id, at)
		SELECT ?, user_id, at FROM trusted_users WHERE chat_id = ?
		ON CONFLICT(chat_id, user_id) DO NOTHING
	`, newID, oldID); err != nil {
		return fmt.Errorf("migrate trusted_users: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM trusted_users WHERE chat_id = ?`, oldID); err != nil {
		return fmt.Errorf("drop old trusted_users: %w", err)
	}

	// chat_quarantine — PK (chat_id, user_id). Ограничение Telegram при апгрейде
	// basic group → supergroup переносится автоматически, остаётся перенести
	// наши строки. При конфликте остаётся строка нового чата.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO chat_quarantine (chat_id, user_id, until_at, warned)
		SELECT ?, user_id, until_at, warned FROM chat_quarantine WHERE chat_id = ?
		ON CONFLICT(chat_id, user_id) DO NOTHING
	`, newID, oldID); err != nil {
		return fmt.Errorf("migrate chat_quarantine: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM chat_quarantine WHERE chat_id = ?`, oldID); err != nil {
		return fmt.Errorf("drop old chat_quarantine: %w", err)
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
	// captcha_timeout_seconds / reply_check_seconds — легаси-колонки: копируются,
	// чтобы архив не терялся, но нигде не читаются; живая — captcha_interval_minutes.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO chat_settings (chat_id, greeting_enabled, max_attempts,
			captcha_timeout_seconds, captcha_interval_minutes, daily_stats_enabled, daily_stats_utc_hour,
			last_daily_stats_day, captcha_mode, greeting_text, greeting_entities,
			welcome_mode, reply_text, reply_entities, silent_announce_enabled,
			spam_check_enabled, spam_threshold, spam_whitelist_msgs, spam_vote_margin,
			reply_check_enabled, reply_check_seconds, ephemeral_enabled,
			quarantine_enabled, quarantine_hours)
		SELECT ?, greeting_enabled, max_attempts,
			captcha_timeout_seconds, captcha_interval_minutes, daily_stats_enabled, daily_stats_utc_hour,
			last_daily_stats_day, captcha_mode, greeting_text, greeting_entities,
			welcome_mode, reply_text, reply_entities, silent_announce_enabled,
			spam_check_enabled, spam_threshold, spam_whitelist_msgs, spam_vote_margin,
			reply_check_enabled, reply_check_seconds, ephemeral_enabled,
			quarantine_enabled, quarantine_hours
		FROM chat_settings WHERE chat_id = ?
		ON CONFLICT(chat_id) DO NOTHING
	`, newID, oldID); err != nil {
		return fmt.Errorf("migrate chat_settings: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM chat_settings WHERE chat_id = ?`, oldID); err != nil {
		return fmt.Errorf("drop old chat_settings: %w", err)
	}

	// owner_dm_links — message ID старого basic-чата после апгрейда умерли
	// (та же причина, что у pending_captchas/reply_watches/greetings): перенос
	// chat_id оставил бы source_msg_id, который в новой нумерации указывает на
	// ЧУЖОЕ сообщение — ответ владельца приклеился бы не туда. Карточки
	// старого чата просто перестают находиться: честное «связка устарела».
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM owner_dm_links WHERE chat_id = ?`, oldID); err != nil {
		return fmt.Errorf("drop old owner_dm_links: %w", err)
	}

	// Миграция прерывает капчу, включая подготовку. Старые message ID
	// недействительны, а live-обработчик снимает ограничение в новом чате.
	// Конечный restrict_until страхует подготовку при обрыве обработчика.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM pending_captchas WHERE chat_id = ?`, oldID); err != nil {
		return fmt.Errorf("drop old pending_captchas: %w", err)
	}
	// Допуск привязан к участнику; клавиатура и топик старого чата устарели.
	if _, err := tx.ExecContext(ctx, `UPDATE OR IGNORE pending_admissions
		SET chat_id = ?, message_id = 0, ephemeral_msg_id = 0, thread_id = 0 WHERE chat_id = ?`, newID, oldID); err != nil {
		return fmt.Errorf("move pending admissions: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM pending_admissions WHERE chat_id = ?`, oldID); err != nil {
		return fmt.Errorf("drop migrated admissions: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM pending_punishments WHERE chat_id = ?`, oldID); err != nil {
		return fmt.Errorf("drop migrated punishments: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE OR IGNORE pending_manual_mutes SET chat_id = ? WHERE chat_id = ?`, newID, oldID); err != nil {
		return fmt.Errorf("move pending manual mutes: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM pending_manual_mutes WHERE chat_id = ?`, oldID); err != nil {
		return fmt.Errorf("drop migrated manual mutes: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM pending_replies WHERE chat_id = ?`, oldID); err != nil {
		return fmt.Errorf("drop old pending_replies: %w", err)
	}
	// reply_watches — message ID сообщений старого чата после апгрейда умерли;
	// проба по ним вернула бы «not found» и соврала бы об удалении.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM reply_watches WHERE chat_id = ?`, oldID); err != nil {
		return fmt.Errorf("drop old reply_watches: %w", err)
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

	// Принятое решение переживает миграцию чата. Старые message ID недействительны.
	if _, err := tx.ExecContext(ctx,
		`UPDATE spam_verdicts SET chat_id = ?, bot_msg_id = 0, target_msg_id = 0 WHERE chat_id = ?`, newID, oldID); err != nil {
		return fmt.Errorf("migrate spam verdicts: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO spam_verdict_targets (verdict_id, chat_id, status)
		SELECT verdict_id, ?, status FROM spam_verdict_targets WHERE chat_id = ?
		ON CONFLICT (verdict_id, chat_id) DO UPDATE SET status = max(status, excluded.status)`, newID, oldID); err != nil {
		return fmt.Errorf("migrate spam verdict targets: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM spam_verdict_targets WHERE chat_id = ?`, oldID); err != nil {
		return fmt.Errorf("drop migrated spam verdict targets: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO chat_spam_decisions (chat_id, user_id, verdict_id, pardon_id)
		SELECT ?, user_id, verdict_id, pardon_id FROM chat_spam_decisions WHERE chat_id = ?
		ON CONFLICT(chat_id, user_id) DO UPDATE SET verdict_id = max(verdict_id, excluded.verdict_id),
			pardon_id = max(pardon_id, excluded.pardon_id)`, newID, oldID); err != nil {
		return fmt.Errorf("migrate spam decisions: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM chat_spam_decisions WHERE chat_id = ?`, oldID); err != nil {
		return fmt.Errorf("drop migrated spam decisions: %w", err)
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
