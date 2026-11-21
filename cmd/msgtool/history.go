package main

import (
	"cmp"
	"context"
	"fmt"
	"iter"
	"slices"

	"github.com/menand/AntiSpamBot/internal/archive"
	"github.com/menand/AntiSpamBot/internal/storage"
)

type messageHistory struct {
	hot       iter.Seq2[storage.StoredMessage, error]
	closeHot  func()
	archiveID string
	through   int64
	chatIDs   map[int64]bool
}

func readHotHistory(ctx context.Context, dbPath string, f storage.MessageFilter) (messageHistory, error) {
	db, err := storage.OpenReadOnly(ctx, dbPath)
	if err != nil {
		return messageHistory{}, err
	}
	snapshot, err := db.OpenMessageSnapshot(ctx, f)
	if err != nil {
		_ = db.Close()
		return messageHistory{}, err
	}
	h := messageHistory{
		hot: snapshot.Messages(ctx), archiveID: snapshot.ArchiveID, through: snapshot.ThroughID,
		closeHot: func() { _ = snapshot.Close(); _ = db.Close() },
	}
	if len(snapshot.ChatIDs) > 0 {
		h.chatIDs = make(map[int64]bool, len(snapshot.ChatIDs))
		for _, id := range snapshot.ChatIDs {
			h.chatIDs[id] = true
		}
	}
	return h, nil
}

func (h messageHistory) close() {
	if h.closeHot != nil {
		h.closeHot()
	}
}

func (h messageHistory) visit(dir string, f storage.MessageFilter, warn func(error), emit func(wireEntry)) {
	days, err := archive.ListDays(dir)
	if err != nil {
		warn(err)
	}
	days = slices.DeleteFunc(days, func(day string) bool { return !dayInRange(day, f.From, f.Until) })

	var hot archive.Entry
	var hotDay string
	advanceHot := func() {}
	if h.hot != nil {
		next, stop := iter.Pull2(h.hot)
		defer stop()
		advanceHot = func() {
			m, err, ok := next()
			if err != nil {
				warn(fmt.Errorf("hot window skipped: %w", err))
			}
			if !ok || err != nil {
				hotDay = ""
				return
			}
			hot, hotDay = archive.FromStored(m), storage.DayOf(m.TS)
		}
		advanceHot()
	}

	emitted := 0
	send := func(e archive.Entry, source string) bool {
		emit(fromEntry(e, source))
		emitted++
		return f.Limit > 0 && emitted == f.Limit
	}
	for len(days) > 0 || hotDay != "" {
		day := hotDay
		var entries []archive.Entry
		seen := make(map[archive.EntryKey]bool)
		if len(days) > 0 && (day == "" || days[0] <= day) {
			day, days = days[0], days[1:]
			entries, err = h.archiveDay(dir, day, f, seen)
			if err != nil {
				warn(err)
			}
		}
		for len(entries) > 0 || hotDay == day {
			if len(entries) > 0 && (hotDay != day || compareEntries(entries[0], hot) <= 0) {
				if send(entries[0], "archive") {
					return
				}
				entries = entries[1:]
				continue
			}
			key := hot.Key()
			if !seen[key] {
				seen[key] = true
				if send(hot, "hot") {
					return
				}
			}
			advanceHot()
		}
	}
}

func (h messageHistory) archiveDay(dir, day string, f storage.MessageFilter, seen map[archive.EntryKey]bool) ([]archive.Entry, error) {
	entries, err := archive.ReadDay(dir, day)
	if err != nil {
		return nil, err
	}
	kept := entries[:0]
	for _, e := range entries {
		// Legacy and independent archives can only be matched by content.
		if h.archiveID != "" && e.ArchiveID == h.archiveID && e.RowID > h.through {
			continue
		}
		filterChat := f.ChatID
		if h.chatIDs[e.Chat] {
			filterChat = 0
		}
		if !entryMatches(e.Chat, e.User, e.Text, e.ReplyText, filterChat, f.UserID, f.Word) {
			continue
		}
		key := e.Key()
		if !seen[key] {
			seen[key] = true
			kept = append(kept, e)
		}
	}
	slices.SortStableFunc(kept, compareEntries)
	return kept, nil
}

func compareEntries(a, b archive.Entry) int {
	if byTime := cmp.Compare(a.TS, b.TS); byTime != 0 {
		return byTime
	}
	return cmp.Compare(a.RowID, b.RowID)
}
