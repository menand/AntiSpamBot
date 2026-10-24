package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ReplyWatch — сообщение, которым юзер прошёл «приветствие с ответом», и
// состояние его отслеживания. Проба копирует сообщение в служебный чат через
// 1/5/15/60 минут: успех = сообщение живо, «message to copy not found» =
// удалено. Text — снимок на момент ответа/последней правки, DeletedBy — метка
// «это удалил бот» (иначе пусто: юзер/админ/неизвестно).
type ReplyWatch struct {
	ChatID     int64
	UserID     int64
	MessageID  int
	Text       string
	AnsweredAt time.Time
	Stage      int // индекс следующей пробы в replyWatchOffsets (0..3)
	NextAt     time.Time
	DeletedBy  string
	// EditReports — сколько карточек о правках уже отправлено (не больше
	// replyWatchEditMaxReports), EditNotifiedAt — время последней карточки.
	// Ограничение живёт в БД, а не в памяти: рестарт не должен открывать
	// юзеру новый лимит карточек.
	EditReports    int
	EditNotifiedAt time.Time
	// ReportAttempts — неудачные попытки доставить карточку об удалении;
	// после исчерпания лимита наблюдение снимается (отчёт потерян — в логе).
	ReportAttempts int
}

// ReplyWatchEdit — результат атомарного клейма правки. OldText — версия ДО
// этой правки (даже если предыдущие правки были проглочены троттлингом),
// ShouldNotify — пора ли отправлять карточку. Встроенный ReplyWatch несёт уже
// НОВЫЙ текст и обновлённый EditReports.
type ReplyWatchEdit struct {
	ReplyWatch
	OldText      string
	ShouldNotify bool
}

const replyWatchColumns = `chat_id, user_id, message_id, text, answered_at, stage, next_at, deleted_by, edit_reports, edit_notified_at, report_attempts`

// PutReplyWatch создаёт (или перезаписывает) наблюдение за сообщением.
// Перезапись используется и для возврата карточки об удалении в очередь
// (report_attempts + 1): все поля, включая счётчики правок, переносятся
// вызывающим как есть.
func (d *DB) PutReplyWatch(ctx context.Context, w ReplyWatch) error {
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO reply_watches (`+replyWatchColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(chat_id, message_id) DO UPDATE SET
			user_id = excluded.user_id,
			text = excluded.text,
			answered_at = excluded.answered_at,
			stage = excluded.stage,
			next_at = excluded.next_at,
			deleted_by = excluded.deleted_by,
			edit_reports = excluded.edit_reports,
			edit_notified_at = excluded.edit_notified_at,
			report_attempts = excluded.report_attempts
	`, w.ChatID, w.UserID, w.MessageID, w.Text, w.AnsweredAt.Unix(),
		w.Stage, w.NextAt.Unix(), w.DeletedBy, w.EditReports,
		unixOrZero(w.EditNotifiedAt), w.ReportAttempts)
	if err != nil {
		return fmt.Errorf("put reply watch: %w", err)
	}
	return nil
}

// unixOrZero — «нулевое время» в SQLite хранится как 0, а не как огромное
// отрицательное число: иначе edit_notified_at пришлось бы трактовать как
// валидную метку в прошлом.
func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// DueReplyWatches отдаёт наблюдения, время пробы которых наступило.
func (d *DB) DueReplyWatches(ctx context.Context, now time.Time, limit int) ([]ReplyWatch, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := d.sql.QueryContext(ctx, `
		SELECT `+replyWatchColumns+`
		FROM reply_watches
		WHERE next_at <= ?
		ORDER BY next_at, message_id
		LIMIT ?
	`, now.Unix(), limit)
	if err != nil {
		return nil, fmt.Errorf("due reply watches: %w", err)
	}
	defer rows.Close() //nolint:errcheck // cleanup, ошибка неинтересна
	var out []ReplyWatch
	for rows.Next() {
		w, _, err := scanReplyWatch(rows)
		if err != nil {
			return nil, fmt.Errorf("scan due reply watch: %w", err)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("due reply watches rows: %w", err)
	}
	return out, nil
}

// ReplyWatchChats — чаты, где сейчас есть наблюдения (на старте наполняет
// дешёвый предфильтр правок в памяти: правка в чате без наблюдений не должна
// открывать транзакцию).
func (d *DB) ReplyWatchChats(ctx context.Context) ([]int64, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT DISTINCT chat_id FROM reply_watches`)
	if err != nil {
		return nil, fmt.Errorf("reply watch chats: %w", err)
	}
	defer rows.Close() //nolint:errcheck // cleanup, ошибка неинтересна
	var out []int64
	for rows.Next() {
		var chatID int64
		if err := rows.Scan(&chatID); err != nil {
			return nil, fmt.Errorf("scan reply watch chat: %w", err)
		}
		out = append(out, chatID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reply watch chats rows: %w", err)
	}
	return out, nil
}

// GetReplyWatch возвращает наблюдение по сообщению.
func (d *DB) GetReplyWatch(ctx context.Context, chatID int64, messageID int) (ReplyWatch, bool, error) {
	row := d.sql.QueryRowContext(ctx, `
		SELECT `+replyWatchColumns+`
		FROM reply_watches
		WHERE chat_id = ? AND message_id = ?
	`, chatID, messageID)
	w, ok, err := scanReplyWatch(row)
	if err != nil {
		return ReplyWatch{}, false, fmt.Errorf("get reply watch: %w", err)
	}
	return w, ok, nil
}

// TakeReplyWatch атомарно изымает наблюдение (победитель гонки):
// удаление сообщения детектируется ровно один раз, даже если проба и
// рестарт наложились. Возвращает изъятую строку — в ней свежий DeletedBy.
func (d *DB) TakeReplyWatch(ctx context.Context, chatID int64, messageID int) (ReplyWatch, bool, error) {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return ReplyWatch{}, false, fmt.Errorf("take reply watch begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	w, ok, err := scanReplyWatch(tx.QueryRowContext(ctx, `
		SELECT `+replyWatchColumns+`
		FROM reply_watches
		WHERE chat_id = ? AND message_id = ?
	`, chatID, messageID))
	if err != nil {
		return ReplyWatch{}, false, fmt.Errorf("take reply watch select: %w", err)
	}
	if !ok {
		return ReplyWatch{}, false, nil
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM reply_watches WHERE chat_id = ? AND message_id = ?`,
		chatID, messageID); err != nil {
		return ReplyWatch{}, false, fmt.Errorf("take reply watch delete: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return ReplyWatch{}, false, fmt.Errorf("take reply watch commit: %w", err)
	}
	return w, true, nil
}

// AdvanceReplyWatch переводит наблюдение на следующую стадию. Guard по
// stage: строку мог изъять /del-хук или финальная проба — тогда 0 строк и
// тихий no-op. Возвращает, применилось ли обновление.
func (d *DB) AdvanceReplyWatch(ctx context.Context, chatID int64, messageID, fromStage, toStage int, nextAt time.Time) (bool, error) {
	res, err := d.sql.ExecContext(ctx, `
		UPDATE reply_watches
		SET stage = ?, next_at = ?
		WHERE chat_id = ? AND message_id = ? AND stage = ?
	`, toStage, nextAt.Unix(), chatID, messageID, fromStage)
	if err != nil {
		return false, fmt.Errorf("advance reply watch: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("advance reply watch rows: %w", err)
	}
	return n > 0, nil
}

// ClaimReplyWatchEdit атомарно применяет новую версию текста и решает,
// отправлять ли карточку о правке. Фильтр по user_id — защита от чужой
// edited_message: текст в снимок и карточку попадает только от автора
// наблюдения. Guard по edit_reports в UPDATE — второй победитель гонки
// (два параллельных соединения) не перезапишет счётчик.
//
// Политика: первая правка — сразу; дальше не чаще cooldown и не больше
// maxReports карточек. Текст обновляется ВСЕГДА (в том числе когда карточку
// проглотил троттлинг): отчёт об удалении должен показать последнюю версию.
// Исключение — правка, СТЁРШАЯ текст: снимок-улику не затираем (иначе юзер
// мог бы спрятать содержимое, а потом удалить сообщение), а карточку шлём,
// игнорируя кулдаун: «стёр текст» само по себе сигнал.
func (d *DB) ClaimReplyWatchEdit(ctx context.Context, chatID int64, messageID int, userID int64, text string, now time.Time, cooldown time.Duration, maxReports int) (ReplyWatchEdit, bool, error) {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return ReplyWatchEdit{}, false, fmt.Errorf("claim reply watch edit begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	w, ok, err := scanReplyWatch(tx.QueryRowContext(ctx, `
		SELECT `+replyWatchColumns+`
		FROM reply_watches
		WHERE chat_id = ? AND message_id = ? AND user_id = ?
	`, chatID, messageID, userID))
	if err != nil {
		return ReplyWatchEdit{}, false, fmt.Errorf("claim reply watch edit select: %w", err)
	}
	if !ok {
		return ReplyWatchEdit{}, false, nil
	}
	edit := ReplyWatchEdit{ReplyWatch: w, OldText: w.Text}
	if text == w.Text {
		return edit, true, nil
	}
	erased := strings.TrimSpace(text) == ""
	notify := w.EditReports < maxReports &&
		(erased || w.EditReports == 0 || w.EditNotifiedAt.IsZero() ||
			now.Sub(w.EditNotifiedAt) >= cooldown)
	fromReports := w.EditReports
	if notify {
		w.EditReports++
		w.EditNotifiedAt = now
		if !erased {
			w.Text = text
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE reply_watches
			SET text = ?, edit_reports = ?, edit_notified_at = ?
			WHERE chat_id = ? AND message_id = ? AND edit_reports = ?
		`, w.Text, w.EditReports, w.EditNotifiedAt.Unix(), chatID, messageID,
			fromReports); err != nil {
			return ReplyWatchEdit{}, false, fmt.Errorf("claim reply watch edit update: %w", err)
		}
		edit.ReplyWatch = w
		if erased {
			// Карточка должна показать «стало: текст убран», а снимок в БД
			// остаётся прежним (улика для отчёта об удалении).
			edit.ReplyWatch.Text = ""
		}
		edit.ShouldNotify = true
	} else if !erased {
		w.Text = text
		if _, err := tx.ExecContext(ctx, `
			UPDATE reply_watches SET text = ?
			WHERE chat_id = ? AND message_id = ? AND edit_reports = ?
		`, text, chatID, messageID, fromReports); err != nil {
			return ReplyWatchEdit{}, false, fmt.Errorf("claim reply watch edit snapshot: %w", err)
		}
		edit.ReplyWatch = w
	}
	if err := tx.Commit(); err != nil {
		return ReplyWatchEdit{}, false, fmt.Errorf("claim reply watch edit commit: %w", err)
	}
	return edit, true, nil
}

// DeferReplyWatch переносит следующую пробу, не меняя стадию. Нужен на
// транзиентных сбоях (429, ошибка БД, недоступность статуса чата): строка
// остаётся в наблюдении, но следующий тик не долбит API/БД тем же вызовом.
func (d *DB) DeferReplyWatch(ctx context.Context, chatID int64, messageID int, nextAt time.Time) error {
	_, err := d.sql.ExecContext(ctx, `
		UPDATE reply_watches SET next_at = ?
		WHERE chat_id = ? AND message_id = ?
	`, nextAt.Unix(), chatID, messageID)
	if err != nil {
		return fmt.Errorf("defer reply watch: %w", err)
	}
	return nil
}

// DeleteStaleReplyWatches снимает строки, пережившие максимальное окно
// наблюдения (60 минут) с большим запасом. Это страховка от «зависшего»
// наблюдения (цикл умер, чат недоступен, БД-ошибки): такие строки не должны
// копиться вечно и не должны разом «созреть» после рестарта.
func (d *DB) DeleteStaleReplyWatches(ctx context.Context, before time.Time) (int64, error) {
	res, err := d.sql.ExecContext(ctx,
		`DELETE FROM reply_watches WHERE answered_at < ?`, before.Unix())
	if err != nil {
		return 0, fmt.Errorf("delete stale reply watches: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("delete stale reply watches rows: %w", err)
	}
	return n, nil
}

// MarkReplyWatchDeletedByMessage помечает, что сообщение удалил бот (команда
// /del). Пустой reason не пишем: «не бот» — это и есть пустое значение.
// Первая непустая метка побеждает: если /del уже пометил сообщение, поздний
// revoke-бан не перепишет причину.
func (d *DB) MarkReplyWatchDeletedByMessage(ctx context.Context, chatID int64, messageID int, reason string) error {
	if reason == "" {
		return nil
	}
	_, err := d.sql.ExecContext(ctx, `
		UPDATE reply_watches SET deleted_by = ?
		WHERE chat_id = ? AND message_id = ? AND deleted_by = ''
	`, reason, chatID, messageID)
	if err != nil {
		return fmt.Errorf("mark reply watch deleted by message: %w", err)
	}
	return nil
}

// MarkReplyWatchesDeletedByUser помечает все наблюдения юзера в чате: /kick,
// /ban и спам-бан стирают все его сообщения одним revoke. Первая непустая
// метка побеждает (см. выше).
func (d *DB) MarkReplyWatchesDeletedByUser(ctx context.Context, chatID, userID int64, reason string) error {
	if reason == "" {
		return nil
	}
	_, err := d.sql.ExecContext(ctx, `
		UPDATE reply_watches SET deleted_by = ?
		WHERE chat_id = ? AND user_id = ? AND deleted_by = ''
	`, reason, chatID, userID)
	if err != nil {
		return fmt.Errorf("mark reply watches deleted by user: %w", err)
	}
	return nil
}

// DeleteReplyWatchesChat сносит наблюдения чата: бот вышел или basic group
// стал supergroup (message ID старого чата умерли — копировать нечего).
func (d *DB) DeleteReplyWatchesChat(ctx context.Context, chatID int64) error {
	_, err := d.sql.ExecContext(ctx,
		`DELETE FROM reply_watches WHERE chat_id = ?`, chatID)
	if err != nil {
		return fmt.Errorf("delete reply watches chat: %w", err)
	}
	return nil
}

// DeleteAllReplyWatches очищает наблюдения целиком: фича выключена, строки
// короткоживущие и при повторном включении не должны разом «созреть».
func (d *DB) DeleteAllReplyWatches(ctx context.Context) error {
	_, err := d.sql.ExecContext(ctx, `DELETE FROM reply_watches`)
	if err != nil {
		return fmt.Errorf("delete all reply watches: %w", err)
	}
	return nil
}

func scanReplyWatch(row interface{ Scan(dest ...any) error }) (ReplyWatch, bool, error) {
	var w ReplyWatch
	var answeredAt, nextAt, editNotifiedAt int64
	err := row.Scan(&w.ChatID, &w.UserID, &w.MessageID, &w.Text, &answeredAt,
		&w.Stage, &nextAt, &w.DeletedBy, &w.EditReports, &editNotifiedAt,
		&w.ReportAttempts)
	if errors.Is(err, sql.ErrNoRows) {
		return ReplyWatch{}, false, nil
	}
	if err != nil {
		return ReplyWatch{}, false, err
	}
	w.AnsweredAt = time.Unix(answeredAt, 0)
	w.NextAt = time.Unix(nextAt, 0)
	if editNotifiedAt > 0 {
		w.EditNotifiedAt = time.Unix(editNotifiedAt, 0)
	}
	return w, true, nil
}
