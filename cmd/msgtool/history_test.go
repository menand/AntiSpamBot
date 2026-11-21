package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/menand/AntiSpamBot/internal/archive"
	"github.com/menand/AntiSpamBot/internal/storage"
)

func TestShortHistoryReadsOnlyOneHotRow(t *testing.T) {
	for _, size := range []int{128, 4096} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			ctx := t.Context()
			dir := t.TempDir()
			path := filepath.Join(dir, "messages.db")
			db, err := storage.Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			at := time.Date(2026, 11, 21, 0, 0, 0, 0, storage.StatsLocation)
			rows := make([]storage.StoredMessage, size)
			for i := range rows {
				rows[i] = storage.StoredMessage{ChatID: 1, UserID: 2, TS: at.Add(time.Duration(i) * time.Second), TGMsgID: int64(i + 1), Kind: "text", Text: "hot"}
			}
			if err := db.AppendMessages(ctx, rows); err != nil {
				t.Fatal(err)
			}
			filter := storage.MessageFilter{Limit: 1}
			for _, source := range []string{"hot", "archive"} {
				if source == "archive" {
					first := rows[0]
					first.TS = at.Add(-time.Second)
					writeHistoryDay(t, dir, storage.DayOf(first.TS), []archive.Entry{archive.FromStored(first)})
				}
				h, err := readHotHistory(ctx, path, filter)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(h.close)
				got, count := countHistoryReads(t, h, dir, filter)
				if count != 1 || len(got) != 1 || got[0].Source != source {
					t.Fatalf("limit=1 loaded %d hot rows with %d stored, got %+v, want %s", count, size, got, source)
				}
				t.Logf("stored=%d, limit=1, first=%s, hot rows read=%d", size, source, count)
			}
		})
	}
}

func TestHistoryReadsThroughOverlapForNextUniqueResult(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	path := filepath.Join(dir, "messages.db")
	db, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	at := time.Date(2026, 11, 21, 0, 0, 0, 0, storage.StatsLocation)
	duplicate := storage.StoredMessage{ChatID: 1, UserID: 2, TGMsgID: 1, TS: at, Kind: "text", Text: "overlap"}
	writeHistoryDay(t, dir, storage.DayOf(at), []archive.Entry{archive.FromStored(duplicate)})
	const copies = 64
	rows := make([]storage.StoredMessage, 512)
	for i := range rows {
		rows[i] = duplicate
		if i >= copies {
			rows[i].TGMsgID = int64(i + 2)
			rows[i].TS = at.Add(time.Duration(i) * time.Second)
		}
	}
	if err := db.AppendMessages(ctx, rows); err != nil {
		t.Fatal(err)
	}
	f := storage.MessageFilter{Limit: 2}
	h, err := readHotHistory(ctx, path, f)
	if err != nil {
		t.Fatal(err)
	}
	defer h.close()
	got, count := countHistoryReads(t, h, dir, f)
	if count != copies+1 || len(got) != 2 || got[0].Source != "archive" || got[1].Source != "hot" || got[1].TGMsgID != copies+2 {
		t.Fatalf("results after overlap = %+v, read %d hot rows", got, count)
	}
	t.Logf("stored=%d, duplicates=%d, limit=2, hot rows read=%d", len(rows), copies, count)
}

func countHistoryReads(t *testing.T, h messageHistory, dir string, f storage.MessageFilter) ([]wireEntry, int) {
	t.Helper()
	rows := h.hot
	count := 0
	h.hot = func(yield func(storage.StoredMessage, error) bool) {
		for m, err := range rows {
			if err == nil {
				count++
			}
			if !yield(m, err) {
				return
			}
		}
	}
	var got []wireEntry
	h.visit(dir, f, func(err error) { t.Error(err) }, func(e wireEntry) { got = append(got, e) })
	return got, count
}

func TestHistoryAcrossArchivePublication(t *testing.T) {
	for _, removeHot := range []bool{true, false} {
		name := "published and removed"
		if !removeHot {
			name = "published before interruption"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			dir := t.TempDir()
			path := filepath.Join(dir, "messages.db")
			db, err := storage.Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			archiveID, err := db.MessageArchiveID(ctx)
			if err != nil || archiveID == "" {
				t.Fatalf("archive identity missing: %q / %v", archiveID, err)
			}
			when := time.Date(2026, 11, 21, 0, 0, 0, 0, storage.StatsLocation)
			first, second := "match first quote", "match second quote"
			msgs := []storage.StoredMessage{
				{ChatID: -3, UserID: 7, TGMsgID: 3, TS: when, Kind: "edit", Text: "same text", ReplyText: &first},
				{ChatID: -3, UserID: 7, TGMsgID: 3, TS: when, Kind: "edit", Text: "same text", ReplyText: &second},
				{ChatID: -9, UserID: 7, TGMsgID: 3, TS: when, Kind: "edit", Text: "match elsewhere"},
			}
			if err := db.AppendMessages(ctx, msgs); err != nil {
				t.Fatal(err)
			}
			f := storage.MessageFilter{ChatID: -3, UserID: 7, Word: "match", From: when, Until: when.AddDate(0, 0, 1)}
			h, err := readHotHistory(ctx, path, f)
			if err != nil {
				t.Fatal(err)
			}
			defer h.close()

			// The second connection appends another row after the snapshot.
			// Archive all rows, optionally simulating a stop before deletion.
			later := storage.StoredMessage{ChatID: -3, UserID: 7, TGMsgID: 4, TS: when, Kind: "text", Text: "match after snapshot"}
			if err := db.AppendMessages(ctx, []storage.StoredMessage{later}); err != nil {
				t.Fatal(err)
			}
			day := storage.DayOf(when)
			rows, err := db.MessagesForDay(ctx, day)
			if err != nil || len(rows) != 4 {
				t.Fatalf("source rows = %v / %v", rows, err)
			}
			legacy := archive.FromStored(rows[0])
			legacy.RowID = 0
			entries := []archive.Entry{legacy}
			for _, r := range rows {
				e := archive.FromStored(r)
				e.ArchiveID = archiveID
				entries = append(entries, e)
			}
			writeHistoryDay(t, dir, day, entries)
			if removeHot {
				if err := db.DeleteDayThrough(ctx, day, rows[len(rows)-1].ID); err != nil {
					t.Fatal(err)
				}
			}

			want := []string{first, second}
			if got := readHistoryQuotes(t, h, dir, f); !reflect.DeepEqual(got, want) {
				t.Fatalf("snapshot output = %q, want %q", got, want)
			}
			f.Limit = 1
			if got := readHistoryQuotes(t, h, dir, f); !reflect.DeepEqual(got, want[:1]) {
				t.Fatalf("limited snapshot output = %q, want %q", got, want[:1])
			}
		})
	}
}

func TestHistoryUsesEitherAvailableSource(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	path := filepath.Join(root, "messages.db")
	db, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	archiveDir := filepath.Join(root, "archive")
	if err := os.Mkdir(archiveDir, 0o700); err != nil {
		t.Fatal(err)
	}
	f := storage.MessageFilter{Limit: 1}
	now := time.Now()
	quote := "matching record"
	m := storage.StoredMessage{ChatID: -3, UserID: 7, TS: now, TGMsgID: 3, Kind: "text", ReplyText: &quote}
	if err := db.AppendMessages(ctx, []storage.StoredMessage{m}); err != nil {
		t.Fatal(err)
	}
	h, err := readHotHistory(ctx, path, f)
	if err != nil {
		t.Fatal(err)
	}
	defer h.close()
	if got := readHistoryQuotes(t, h, filepath.Join(root, "no-archive"), f); !reflect.DeepEqual(got, []string{quote}) {
		t.Fatalf("hot-only output = %q", got)
	}
	archiveEntry := archive.FromStored(m)
	archiveEntry.ArchiveID = "independent archive"
	archiveEntry.RowID = 5000
	writeHistoryDay(t, archiveDir, storage.DayOf(now), []archive.Entry{archiveEntry})
	missing, err := readHotHistory(ctx, filepath.Join(root, "missing.db"), f)
	if err == nil {
		t.Fatal("nonexistent DB unexpectedly opened")
	}
	if got := readHistoryQuotes(t, missing, archiveDir, f); !reflect.DeepEqual(got, []string{quote}) {
		t.Fatalf("archive-only output = %q", got)
	}
	if got := readHistoryQuotes(t, h, archiveDir, f); !reflect.DeepEqual(got, []string{quote}) {
		t.Fatalf("independent archive output = %q", got)
	}
}

func readHistoryQuotes(t *testing.T, history messageHistory, dir string, f storage.MessageFilter) []string {
	t.Helper()
	var got []string
	history.visit(dir, f, func(err error) { t.Error(err) }, func(e wireEntry) {
		got = append(got, e.ReplyText)
	})
	return got
}

func writeHistoryDay(t *testing.T, dir, day string, entries []archive.Entry) {
	t.Helper()
	data, err := archive.Encode(entries)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, archive.FileNameForDay(day)), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestNewChatFilterIncludesHistoryAndLateCapture(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	path := filepath.Join(dir, "messages.db")
	db, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	const oldChat, middleChat, newChat, otherChat int64 = -10001, -10002, -10003, -10004
	when := time.Date(2026, 11, 20, 10, 0, 0, 0, storage.StatsLocation)
	makeMessage := func(chat, mid int64, at time.Time, text string) storage.StoredMessage {
		return storage.StoredMessage{ChatID: chat, UserID: 7, TGMsgID: mid, TS: at, Kind: "text", Text: text}
	}
	cold := makeMessage(oldChat, 21, when.AddDate(0, 0, -1), "archived original")
	if err := db.AppendMessages(ctx, []storage.StoredMessage{cold}); err != nil {
		t.Fatal(err)
	}
	day := storage.DayOf(cold.TS)
	rows, err := db.MessagesForDay(ctx, day)
	if err != nil || len(rows) != 1 {
		t.Fatalf("pack old day: %v / %v", rows, err)
	}
	archived := archive.FromStored(rows[0])
	archived.ArchiveID, err = db.MessageArchiveID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	writeHistoryDay(t, dir, day, []archive.Entry{archived})
	if err := db.DeleteDayThrough(ctx, day, rows[0].ID); err != nil {
		t.Fatal(err)
	}

	late := makeMessage(oldChat, 23, when, "same ID and content")
	if err := db.AppendMessages(ctx, []storage.StoredMessage{
		makeMessage(oldChat, 22, when, "hot original"),
		makeMessage(newChat, 23, when, "same ID and content"),
		makeMessage(otherChat, 24, when, "unrelated"),
	}); err != nil {
		t.Fatal(err)
	}
	for _, move := range [][2]int64{{oldChat, middleChat}, {middleChat, newChat}, {oldChat, middleChat}} {
		if err := db.MigrateChat(ctx, move[0], move[1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.AppendMessages(ctx, []storage.StoredMessage{late}); err != nil {
		t.Fatal(err)
	}
	if err := db.MigrateChat(ctx, oldChat, middleChat); err != nil {
		t.Fatal(err)
	}
	if err := db.MigrateChat(ctx, oldChat, otherChat); err == nil {
		t.Fatal("an inconsistent second migration must not expand the unrelated chat")
	}
	read := func(chatID int64) []wireEntry {
		t.Helper()
		f := storage.MessageFilter{ChatID: chatID}
		h, err := readHotHistory(ctx, path, f)
		if err != nil {
			t.Fatal(err)
		}
		defer h.close()
		var out []wireEntry
		h.visit(dir, f, func(err error) { t.Error(err) }, func(e wireEntry) { out = append(out, e) })
		return out
	}
	all := read(newChat)
	if len(all) != 4 || all[0].Source != "archive" || all[0].Chat != oldChat ||
		all[0].TGMsgID != 21 || all[1].Chat != oldChat || all[1].TGMsgID != 22 ||
		all[2].Chat != newChat || all[2].TGMsgID != 23 || all[3].Chat != oldChat || all[3].TGMsgID != 23 {
		t.Fatalf("history lost source identities or dropped a batch: %+v", all)
	}
	if only := read(otherChat); len(only) != 1 || only[0].Chat != otherChat || only[0].Text != "unrelated" {
		t.Fatalf("unrelated query expanded: %+v", only)
	}
}
