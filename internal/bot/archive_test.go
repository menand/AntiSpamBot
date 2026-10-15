package bot

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/menand/AntiSpamBot/internal/archive"
	"github.com/menand/AntiSpamBot/internal/config"
	"github.com/menand/AntiSpamBot/internal/storage"
)

// seedDay вставляет n сообщений в указанный МСК-день и возвращает day-строку.
func seedDay(t *testing.T, db *storage.DB, base time.Time, chat int64, n int) string {
	t.Helper()
	msgs := make([]storage.StoredMessage, 0, n)
	for i := 0; i < n; i++ {
		msgs = append(msgs, storage.StoredMessage{
			ChatID:  chat,
			UserID:  10,
			TGMsgID: int64(i + 1),
			TS:      base.Add(time.Duration(i) * time.Minute),
			Kind:    "text",
			Text:    "оригинал",
		})
	}
	if err := db.AppendMessages(context.Background(), msgs); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return storage.DayOf(base)
}

func TestArchiveDayHappyPathPacksAndDeletes(t *testing.T) {
	ctx := context.Background()
	b, db := newTestBotWithDB(t)
	dir := t.TempDir()
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, storage.StatsLocation)
	day := seedDay(t, db, base, -1001, 3)

	if err := b.archiveDay(ctx, dir, day); err != nil {
		t.Fatalf("archiveDay: %v", err)
	}
	entries, err := archive.ReadDay(dir, day)
	if err != nil {
		t.Fatalf("read day: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("archive entries = %d, want 3", len(entries))
	}
	// hot-строки дня удалены.
	if hot, _ := db.MessagesForDay(ctx, day); len(hot) != 0 {
		t.Fatalf("hot rows after archive = %d, want 0", len(hot))
	}
}

// TestArchiveDayLateRowMergesNotOverwrites — регрессия критического бага
// уничтожения архива: поздняя строка (реплей апдейта / правка), пришедшая в
// УЖЕ заархивированный день, должна СЛИВАТЬся с файлом, а не перезаписывать его
// одними поздними записями (иначе оригиналы дня терялись бы навсегда).
func TestArchiveDayLateRowMergesNotOverwrites(t *testing.T) {
	ctx := context.Background()
	b, db := newTestBotWithDB(t)
	dir := t.TempDir()
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, storage.StatsLocation)
	day := seedDay(t, db, base, -1001, 2)

	// Первый прогон: день упакован, hot пуст.
	if err := b.archiveDay(ctx, dir, day); err != nil {
		t.Fatalf("first archive: %v", err)
	}
	if n := len(mustReadDay(t, dir, day)); n != 2 {
		t.Fatalf("after first archive = %d, want 2", n)
	}

	// Поздняя строка в уже заархивированный день (реплей правки): тот же
	// МСК-день, но более поздний час — попадает в тот же day.
	late := base.Add(time.Hour)
	if err := db.AppendMessages(ctx, []storage.StoredMessage{{
		ChatID: -1001, UserID: 10, TGMsgID: 99, TS: late,
		Kind: "edit", Text: "поздняя правка",
	}}); err != nil {
		t.Fatalf("late row: %v", err)
	}

	// Второй прогон: файл должен содержать 2 оригинала + 1 позднюю = 3.
	if err := b.archiveDay(ctx, dir, day); err != nil {
		t.Fatalf("second archive: %v", err)
	}
	entries := mustReadDay(t, dir, day)
	if len(entries) != 3 {
		t.Fatalf("after late row = %d entries, want 3 (originals must survive)", len(entries))
	}
}

func TestArchiveDayEmptyDayJustCleans(t *testing.T) {
	ctx := context.Background()
	b, _ := newTestBotWithDB(t)
	dir := t.TempDir()
	// Пустой день: нет строк → не файл, просто no-op.
	day := "2026-10-02"
	if err := b.archiveDay(ctx, dir, day); err != nil {
		t.Fatalf("empty day: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, archive.FileNameForDay(day))); err == nil {
		t.Fatal("empty day must not produce a file")
	}
}

func TestArchiveDayWriteFailureKeepsHotRows(t *testing.T) {
	ctx := context.Background()
	b, db := newTestBotWithDB(t)
	base := time.Date(2026, 10, 3, 12, 0, 0, 0, storage.StatsLocation)
	day := seedDay(t, db, base, -1001, 2)

	// dir — регулярный файл, не каталог: os.Stat файла дня вернёт ENOTDIR
	// (≠ fs.ErrNotExist) → archiveDay обязан упасть, а не «решить, что файла нет».
	blocked := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := b.archiveDay(ctx, blocked, day); err == nil {
		t.Fatal("expected error when archive dir is a file")
	}
	// hot-строки НЕ удалены (файл не записан) — данные не потеряны.
	if hot, _ := db.MessagesForDay(ctx, day); len(hot) != 2 {
		t.Fatalf("hot rows after failed archive = %d, want 2 (kept)", len(hot))
	}
}

// Путь WriteFile-ошибки: настоящий каталог, но на месте tmp-файла уже стоит
// КАТАЛОГ → os.WriteFile упадёт с EISDIR. Файл дня не появился, hot-строки целы
// (раньше этот тест упирался в stat-ветку выше и WriteFile не покрывал).
func TestArchiveDayTmpWriteFailureKeepsHotRows(t *testing.T) {
	ctx := context.Background()
	b, db := newTestBotWithDB(t)
	dir := t.TempDir()
	base := time.Date(2026, 10, 5, 12, 0, 0, 0, storage.StatsLocation)
	day := seedDay(t, db, base, -1001, 2)

	// Каталог вместо tmp-файла: WriteFile → EISDIR, Rename ниже — мимо.
	if err := os.Mkdir(filepath.Join(dir, archive.FileNameForDay(day)+".tmp"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := b.archiveDay(ctx, dir, day); err == nil {
		t.Fatal("expected tmp write error")
	}
	if _, err := os.Stat(filepath.Join(dir, archive.FileNameForDay(day))); err == nil {
		t.Fatal("final file must not exist after failed write")
	}
	if hot, _ := db.MessagesForDay(ctx, day); len(hot) != 2 {
		t.Fatalf("hot rows after tmp failure = %d, want 2 (kept)", len(hot))
	}
}

// Сломанный (не zstd) существующий файл дня: archiveDay обязан вернуть ошибку,
// НЕ перезаписав его свежими строками и НЕ удалив hot-строки. Молчаливая
// перезапись урезала бы архив дня до одного позднего вхождения (потеря данных).
func TestArchiveDayCorruptExistingKeepsFileAndHotRows(t *testing.T) {
	ctx := context.Background()
	b, db := newTestBotWithDB(t)
	dir := t.TempDir()
	base := time.Date(2026, 10, 6, 12, 0, 0, 0, storage.StatsLocation)
	day := seedDay(t, db, base, -1001, 2)

	final := filepath.Join(dir, archive.FileNameForDay(day))
	if err := os.WriteFile(final, []byte("мусор вместо zstd"), 0o640); err != nil {
		t.Fatal(err)
	}
	// Поздняя строка → второй прогон обязан споткнуться о сломанный файл.
	if err := db.AppendMessages(ctx, []storage.StoredMessage{{
		ChatID: -1001, UserID: 10, TGMsgID: 9, TS: base.Add(time.Hour), Kind: "edit", Text: "x",
	}}); err != nil {
		t.Fatalf("late row: %v", err)
	}

	if err := b.archiveDay(ctx, dir, day); err == nil {
		t.Fatal("expected error on corrupt existing archive")
	}
	// Сломанный файл НЕ тронут (единственный шанс ручного восстановления),
	// hot-строки не удалены.
	if raw, _ := os.ReadFile(final); string(raw) != "мусор вместо zstd" {
		t.Fatal("corrupt file must not be overwritten")
	}
	if hot, _ := db.MessagesForDay(ctx, day); len(hot) != 3 {
		t.Fatalf("hot rows = %d, want 3 (kept)", len(hot))
	}
}

// Политика окна удержания: archiveOnce пакует ТОЛЬКО дни строже окна, свежие
// остаются в hot. Ядро архиватора (отсечка по МСК-дню от now) до этого теста
// не было покрыто.
func TestArchiveOncePacksOnlyDaysOlderThanRetain(t *testing.T) {
	b, db := newTestBotWithDB(t)
	dir := t.TempDir()
	b.cfg = &config.Config{ArchiveDir: dir, ArchiveHotDays: 7}

	now := time.Now().In(storage.StatsLocation)
	oldDay := seedDay(t, db, now.AddDate(0, 0, -8), -1001, 2)   // строже окна
	freshDay := seedDay(t, db, now.AddDate(0, 0, -1), -1001, 2) // внутри окна

	b.archiveOnce(context.Background())

	if _, err := os.Stat(filepath.Join(dir, archive.FileNameForDay(oldDay))); err != nil {
		t.Fatalf("old day not archived: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, archive.FileNameForDay(freshDay))); !os.IsNotExist(err) {
		t.Fatal("fresh day must stay hot")
	}
	if hot, _ := db.MessagesForDay(context.Background(), freshDay); len(hot) != 2 {
		t.Fatalf("fresh hot rows = %d, want 2", len(hot))
	}
	if hot, _ := db.MessagesForDay(context.Background(), oldDay); len(hot) != 0 {
		t.Fatalf("old hot rows = %d, want 0 (packed)", len(hot))
	}
}

func TestArchiveDirResolution(t *testing.T) {
	// Дефолт (пустой конфиг) — каталог archive РЯДОМ С БД: msgtool по умолчанию
	// смотрит именно туда, расхождение сломало бы поиск архивов на проде.
	b, _ := newTestBotWithDB(t)
	b.cfg = &config.Config{DBPath: "/data/bot.db"}
	if got := b.archiveDir(); got != "/data/archive" {
		t.Fatalf("default dir = %q, want /data/archive", got)
	}
	b.cfg.ArchiveDir = "/custom/arch"
	if got := b.archiveDir(); got != "/custom/arch" {
		t.Fatalf("configured dir = %q", got)
	}
}

// Цитата (reply_*) денормализуется в архивный Entry, nil-поля остаются nil.
func TestEntryFromStoredCarriesReply(t *testing.T) {
	uid, rid := int64(42), int64(7)
	author, text := "Alina", "Ребёнку два месяца"
	with := storage.StoredMessage{
		ChatID: -1001, UserID: 10, TGMsgID: 5,
		TS: time.Unix(1760000000, 0), Kind: "text", Text: "Как твои руки?",
		ReplyMsgID: &rid, ReplyUID: &uid, ReplyAuthor: &author, ReplyText: &text,
	}
	e := entryFromStored(with)
	if e.ReplyMsgID == nil || *e.ReplyMsgID != 7 || e.ReplyUID == nil || *e.ReplyUID != 42 {
		t.Fatalf("reply ids: %+v", e)
	}
	if e.ReplyAuthor != "Alina" || e.ReplyText != "Ребёнку два месяца" {
		t.Fatalf("reply text fields: %+v", e)
	}
	if e.TS != 1760000000 {
		t.Fatalf("TS = %d", e.TS)
	}

	// Без цитаты — все reply-поля пустые, а не мусорные указатели.
	plain := entryFromStored(storage.StoredMessage{
		ChatID: -1001, TS: time.Unix(1760000000, 0), Kind: "text", Text: "t",
	})
	if plain.ReplyMsgID != nil || plain.ReplyUID != nil || plain.ReplyAuthor != "" || plain.ReplyText != "" {
		t.Fatalf("plain entry must have empty reply fields: %+v", plain)
	}
}

func mustReadDay(t *testing.T, dir, day string) []archive.Entry {
	t.Helper()
	entries, err := archive.ReadDay(dir, day)
	if err != nil {
		t.Fatalf("read day %s: %v", day, err)
	}
	return entries
}
