package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOpenReadOnlyReadsExistingRows(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ro.db")
	// Сеем через обычное (write) открытие, потом переоткрываем read-only.
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	base := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	if err := db.AppendMessages(ctx, []StoredMessage{
		msg(-1001, 10, 1, base, "text", "только чтение"),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	ro, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatalf("open read-only: %v", err)
	}
	t.Cleanup(func() { _ = ro.Close() })

	got, err := ro.QueryMessages(ctx, MessageFilter{})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 1 || got[0].Text != "только чтение" {
		t.Fatalf("read-only rows: %+v", got)
	}
}

func TestOpenReadOnlyRejectsWrites(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ro-w.db")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_ = db.Close()

	ro, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatalf("open read-only: %v", err)
	}
	t.Cleanup(func() { _ = ro.Close() })

	// mode=ro + query_only(1): любая вставка обязана упасть.
	if err := ro.AppendMessages(ctx, []StoredMessage{
		msg(-1001, 10, 1, time.Now(), "text", "не должно записаться"),
	}); err == nil {
		t.Fatal("write on read-only handle must fail")
	}

	// query_only — вторая страховка (на случай, если mode=ro не применился):
	// обе прагмы DSN должны дойти до соединения, а не перезаписать друг друга.
	var qo int
	if err := ro.sql.QueryRowContext(ctx, `PRAGMA query_only`).Scan(&qo); err != nil {
		t.Fatalf("read query_only: %v", err)
	}
	if qo != 1 {
		t.Fatalf("query_only = %d, want 1 — прагма потерялась в DSN", qo)
	}
}

func TestOpenReadOnlyMissingFileErrors(t *testing.T) {
	ctx := context.Background()
	if _, err := OpenReadOnly(ctx, filepath.Join(t.TempDir(), "нет.db")); err == nil {
		t.Fatal("missing file must error, not open empty")
	}
}

// Относительный путь — форма дефолтного msgtool -db bot.db. url.URL без
// OmitHost собрал бы тут "file://bot.db?..." («//» + authority), и SQLite
// отверг бы DSN как invalid uri authority — hot-окно не читалось бы вовсе.
// Абсолютные пути в остальных тестах этот баг маскируют.
func TestOpenReadOnlyRelativePath(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(cwd) }() //nolint:errcheck
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	// Сеем через обычное открытие (относительным же путём) и переоткрываем ro.
	db, err := Open(ctx, "bot.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.AppendMessages(ctx, []StoredMessage{
		msg(-1001, 10, 1, time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC), "text", "относительный"),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	ro, err := OpenReadOnly(ctx, "bot.db")
	if err != nil {
		t.Fatalf("open read-only relative: %v", err)
	}
	t.Cleanup(func() { _ = ro.Close() })

	got, err := ro.QueryMessages(ctx, MessageFilter{})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 1 || got[0].Text != "относительный" {
		t.Fatalf("relative path rows: %+v", got)
	}
}
