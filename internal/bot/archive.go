package bot

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/menand/AntiSpamBot/internal/archive"
	"github.com/menand/AntiSpamBot/internal/storage"
)

const (
	// archiveTick — как часто архиватор проверяет, есть ли дни старше окна
	// удержания. Часа достаточно: свежайший архивируемый день всегда старше
	// недели, спешить некуда.
	archiveTick = time.Hour
)

// archiveLoop — фоновая горутина архивации. Периодически ищет дни, старше окна
// удержания, и упаковывает каждый в дневной zstd-JSONL, после чего удаляет
// hot-строки дня. Идемпотентно: в существующий файл дня поздние строки
// ПЕРЕЛИВАЮТСЯ через MergeEntries (а не затирают его), удаление идёт только
// после успешной записи и только по прочитанной границе id.
func (b *Bot) archiveLoop(ctx context.Context) {
	// Первый прогон — сразу, чтобы свежеразвернутый бот не ждал тика.
	b.archiveOnce(ctx)
	t := time.NewTicker(archiveTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.archiveOnce(ctx)
		}
	}
}

// archiveDir резолвит каталог архива: из конфига или по умолчанию рядом с БД.
func (b *Bot) archiveDir() string {
	if b.cfg.ArchiveDir != "" {
		return b.cfg.ArchiveDir
	}
	return filepath.Join(filepath.Dir(b.cfg.DBPath), "archive")
}

func (b *Bot) archiveOnce(ctx context.Context) {
	retain := b.cfg.ArchiveHotDays
	if retain < 0 {
		retain = 0
	}
	// Отсечка: архивируем всё, что строже «сегодня минус retain». Границу
	// берём по МСК-дню, как и запись (storage.DayOf).
	cutoff := time.Now().In(storage.StatsLocation).AddDate(0, 0, -retain)

	days, err := b.db.UnarchivedDaysBefore(ctx, cutoff)
	if err != nil {
		b.log.Warn("archive: query days", "err", err)
		return
	}
	if len(days) == 0 {
		return
	}
	dir := b.archiveDir()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		b.log.Warn("archive: mkdir", "err", err, "dir", dir)
		return
	}
	for _, day := range days {
		if err := b.archiveDay(ctx, dir, day); err != nil {
			// Ошибка одного дня не должна блокировать остальные; день попробуем
			// на следующем тике (hot-строки не удалены — идемпотентно).
			b.log.Warn("archive: day", "err", err, "day", day)
			continue
		}
	}
}

// archiveDay упаковывает один день: читает hot-строки, ПЕРЕЛИВАЕТ их в уже
// существующий day.jsonl.zst (если он есть) через MergeEntries, атомарно пишет
// и удаляет hot-строки. Порядок критичен: сначала запись файла, потом удаление
// — иначе потеряли бы текст при краше между операциями.
//
// Слияние, а не перезапись, — страховка от уничтожения архива: в день, который
// уже упакован, могут прийти поздние строки (правка старого сообщения при
// EditDate-фикс всё равно может прийти реплей апдейта от Telegram). Простой
// перезапись файл бы урезал до этих поздних строк.
func (b *Bot) archiveDay(ctx context.Context, dir, day string) error {
	msgs, err := b.db.MessagesForDay(ctx, day)
	if err != nil {
		return fmt.Errorf("load day: %w", err)
	}
	if len(msgs) == 0 {
		// Пустой день: читать нечего — удалять нечего (границы id нет). Просто
		// выходим; файла не создаём. Поздняя строка, пришедшая после чтения,
		// переживёт и будет упакована следующим тиком.
		return nil
	}

	// Граница удаления: максимальный id прочитанного. Строка, вставленная
	// между этим чтением и DeleteDayThrough (поздняя правка/реплей), получит
	// id больше и переживёт прогон — её допишет следующий тик (слиянием).
	var maxID int64
	for _, m := range msgs {
		if m.ID > maxID {
			maxID = m.ID
		}
	}

	final := filepath.Join(dir, archive.FileNameForDay(day))

	// Считываем уже упакованный файл дня (если есть) для слияния. Любой
	// stat-кроме NotExist — ошибка: молча принять EACCES/EIO за «файла нет»
	// и перезаписать далее свежими строками значило бы урезать архив дня.
	var existing []archive.Entry
	if _, statErr := os.Stat(final); statErr == nil {
		existing, err = archive.ReadDay(dir, day)
		if err != nil {
			// Не перезаписываем вслепую: сломанный файл не трогаем, день
			// попробуем на следующем тике (hot-строки не удалены).
			return fmt.Errorf("read existing archive: %w", err)
		}
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return fmt.Errorf("stat existing archive: %w", statErr)
	}

	// Сливаем: существующие + новые, дедуп по (chat, tg_msg_id, kind, ts).
	fresh := make([]archive.Entry, 0, len(msgs))
	for _, m := range msgs {
		fresh = append(fresh, entryFromStored(m))
	}
	entries := archive.MergeEntries(existing, fresh)

	// Сжимаем одной zstd-пачкой: словарь на всю пачку даёт ~18× против 1.2×
	// по одному сообщению (замерено на корпусе бота).
	zb, err := archive.Encode(entries)
	if err != nil {
		return fmt.Errorf("encode day: %w", err)
	}

	// Атомарная запись: времянка рядом с целью + rename.
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, zb, 0o640); err != nil {
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename: %w", err)
	}

	// Файл доехал — теперь удаляем прочитанные hot-строки дня (по границе id,
	// чтобы вставленные после чтения строки пережили прогон). Идемпотентно.
	if err := b.db.DeleteDayThrough(ctx, day, maxID); err != nil {
		return fmt.Errorf("delete hot rows: %w", err)
	}
	b.log.Info("archive: day packed", "day", day, "messages", len(msgs),
		"total", len(entries), "compressed", len(zb), "file", final)
	return nil
}

func entryFromStored(m storage.StoredMessage) archive.Entry {
	e := archive.Entry{
		Chat:     m.ChatID,
		User:     m.UserID,
		TGMsgID:  m.TGMsgID,
		TS:       m.TS.Unix(),
		Kind:     m.Kind,
		Text:     m.Text,
		HasMedia: m.HasMedia,
	}
	if m.ReplyMsgID != nil {
		e.ReplyMsgID = m.ReplyMsgID
	}
	if m.ReplyUID != nil {
		e.ReplyUID = m.ReplyUID
	}
	if m.ReplyAuthor != nil {
		e.ReplyAuthor = *m.ReplyAuthor
	}
	if m.ReplyText != nil {
		e.ReplyText = *m.ReplyText
	}
	return e
}
