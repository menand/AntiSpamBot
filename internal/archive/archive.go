// Package archive описывает формат дневных zstd-JSONL архивов сообщений и
// держит кодек (zstd-сжатие + JSONL). Общий для записи (бот, internal/bot/
// archive.go) и чтения (msgtool): если бы формат жил в двух местах, они бы
// разъехались.
package archive

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/menand/AntiSpamBot/internal/storage"
)

// Entry — одна строка дневного JSONL. Поля короткие; сообщение самодостаточно
// (цитата денормализована), чтобы архив читался без joins.
type Entry struct {
	ArchiveID   string `json:"store_id,omitempty"`
	RowID       int64  `json:"row_id,omitempty"` // отсутствует в старых файлах
	Chat        int64  `json:"c"`
	User        int64  `json:"u"`
	TGMsgID     int64  `json:"mid"`
	TS          int64  `json:"ts"`
	Kind        string `json:"k"`
	Text        string `json:"t,omitempty"`
	HasMedia    bool   `json:"media,omitempty"`
	ReplyMsgID  *int64 `json:"rmid,omitempty"`
	ReplyUID    *int64 `json:"ruid,omitempty"`
	ReplyAuthor string `json:"ra,omitempty"`
	ReplyText   string `json:"rt,omitempty"`
}

// FileExt — расширение дневного файла архива.
const FileExt = ".jsonl.zst"

// FromStored preserves the identity and content used on both sides of the archive.
func FromStored(m storage.StoredMessage) Entry {
	e := Entry{
		RowID: m.ID, Chat: m.ChatID, User: m.UserID, TGMsgID: m.TGMsgID,
		TS: m.TS.Unix(), Kind: m.Kind, Text: m.Text, HasMedia: m.HasMedia,
		ReplyMsgID: m.ReplyMsgID, ReplyUID: m.ReplyUID,
	}
	if m.ReplyAuthor != nil {
		e.ReplyAuthor = *m.ReplyAuthor
	}
	if m.ReplyText != nil {
		e.ReplyText = *m.ReplyText
	}
	return e
}

// EntryKey matches exact snapshots, including the denormalized quote.
// RowID is intentionally omitted to support files that predate it.
type EntryKey struct {
	chat, user, msg, ts                int64
	kind, text, replyAuthor, replyText string
	media, hasReplyID, hasReplyUser    bool
	replyID, replyUser                 int64
}

func (e Entry) Key() EntryKey {
	k := EntryKey{
		chat: e.Chat, user: e.User, msg: e.TGMsgID, ts: e.TS, kind: e.Kind, text: e.Text,
		replyAuthor: e.ReplyAuthor, replyText: e.ReplyText, media: e.HasMedia,
	}
	if e.ReplyMsgID != nil {
		k.hasReplyID, k.replyID = true, *e.ReplyMsgID
	}
	if e.ReplyUID != nil {
		k.hasReplyUser, k.replyUser = true, *e.ReplyUID
	}
	return k
}

// MergeEntries keeps the existing archive before late arrivals and removes
// exact replays while preserving distinct edits made within the same second.
func MergeEntries(existing, fresh []Entry) []Entry {
	seen := make(map[EntryKey]struct{}, len(existing)+len(fresh))
	out := make([]Entry, 0, len(existing)+len(fresh))
	for _, entries := range [][]Entry{existing, fresh} {
		for _, e := range entries {
			key := e.Key()
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, e)
		}
	}
	return out
}

// FileNameForDay — имя файла архива для дня ('YYYY-MM-DD'). Имя = день, чтобы
// msgtool мог отфильтровать нужные месяца без распаковки лишнего.
func FileNameForDay(day string) string { return day + FileExt }

// Encode сжимает записи в один zstd-файл (JSONL → zstd). Сжатие пачкой за день
// даёт ~18× против 1.2× по одному сообщению (словарь на всю пачку).
func Encode(entries []Entry) ([]byte, error) {
	var jl bytes.Buffer
	w := bufio.NewWriter(&jl)
	enc := json.NewEncoder(w)
	for _, e := range entries {
		if err := enc.Encode(e); err != nil {
			return nil, fmt.Errorf("encode entry: %w", err)
		}
	}
	if err := w.Flush(); err != nil {
		return nil, fmt.Errorf("flush jsonl: %w", err)
	}
	return zstdEncode(jl.Bytes())
}

// Decode распаковывает дневной файл в записи. Используется msgtool.
func Decode(zb []byte) ([]Entry, error) {
	raw, err := zstdDecode(zb)
	if err != nil {
		return nil, fmt.Errorf("zstd decode: %w", err)
	}
	var out []Entry
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024) // до 16 МБ на строку
	lineNumber := 0
	for sc.Scan() {
		lineNumber++
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		if line[0] != '{' {
			return nil, fmt.Errorf("JSONL line %d: expected a JSON object", lineNumber)
		}
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			var syntax *json.SyntaxError
			if errors.As(err, &syntax) {
				return nil, fmt.Errorf("JSONL line %d: invalid JSON at byte %d", lineNumber, syntax.Offset)
			}
			return nil, fmt.Errorf("JSONL line %d: invalid archive entry fields", lineNumber)
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("JSONL line %d: %w", lineNumber+1, err)
	}
	return out, nil
}

// ListDays возвращает отсортированные дни ('YYYY-MM-DD'), чьи файлы есть в
// каталоге архива. Дни берутся из имён файлов — без распаковки.
func ListDays(dir string) ([]string, error) {
	names, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list archive: %w", err)
	}
	days := make([]string, 0, len(names))
	for _, n := range names {
		if n.IsDir() || !strings.HasSuffix(n.Name(), FileExt) {
			continue
		}
		d := strings.TrimSuffix(n.Name(), FileExt)
		if len(d) == len("2006-01-02") {
			days = append(days, d)
		}
	}
	sort.Strings(days)
	return days, nil
}

// ReadDay читает один дневной файл архива.
func ReadDay(dir, day string) ([]Entry, error) {
	zb, err := os.ReadFile(filepath.Join(dir, FileNameForDay(day))) //nolint:gosec // local archive directory and day from DB/ListDays
	if err != nil {
		return nil, fmt.Errorf("read archive day %s: %w", day, err)
	}
	return Decode(zb)
}

// VerifyDirectory checks every daily file, including days outside query filters.
func VerifyDirectory(dir string) error {
	files, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, f := range files {
		day, archiveFile := strings.CutSuffix(f.Name(), FileExt)
		if !archiveFile {
			continue
		}
		if _, err := time.Parse("2006-01-02", day); err != nil || !f.Type().IsRegular() {
			return fmt.Errorf("invalid daily archive %q", f.Name())
		}
		info, err := f.Info()
		if err != nil {
			return err
		}
		if info.Size() == 0 {
			return fmt.Errorf("empty archive %q", f.Name())
		}
		if _, err := ReadDay(dir, day); err != nil {
			return fmt.Errorf("%s: %w", f.Name(), err)
		}
	}
	return nil
}

func zstdEncode(b []byte) ([]byte, error) {
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		return nil, err
	}
	defer enc.Close()
	return enc.EncodeAll(b, nil), nil
}

func zstdDecode(b []byte) ([]byte, error) {
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	return dec.DecodeAll(b, nil)
}
