// Package archive описывает формат дневных zstd-JSONL архивов сообщений и
// держит кодек (zstd-сжатие + JSONL). Общий для записи (бот, internal/bot/
// archive.go) и чтения (msgtool): если бы формат жил в двух местах, они бы
// разъехались.
package archive

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// Entry — одна строка дневного JSONL. Поля короткие; сообщение самодостаточно
// (цитата денормализована), чтобы архив читался без joins.
type Entry struct {
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

// MergeEntries объединяет уже заархивированные записи дня с новыми (только что
// прочитанными из hot) и дедуплицирует по (chat, tg_msg_id, kind, ts). Нужно,
// когда в заархивированный день пришли поздние строки — правки или реплеи
// апдейтов от Telegram: без слияния следующий прогон архиватора перезаписал бы
// файл дня одними поздними записями, уничтожив оригиналы. Существующие идут
// первыми, новые добавляются в конец — хронология сохраняется.
func MergeEntries(existing, fresh []Entry) []Entry {
	if len(existing) == 0 {
		return fresh
	}
	// Ключ включает text: две правки одного сообщения в ту же секунду (у
	// Telegram гранулярность 1с) с РАЗНЫМ текстом обязаны обе остаться в
	// архиве — ронять позднюю нельзя. Совпадают по ключу только точные
	// реплеи (тот же контент) — их и дедупим.
	type key struct {
		chat, msg, ts int64
		kind, text    string
	}
	seen := make(map[key]struct{}, len(existing)+len(fresh))
	k := func(e Entry) key { return key{e.Chat, e.TGMsgID, e.TS, e.Kind, e.Text} }
	out := make([]Entry, 0, len(existing)+len(fresh))
	for _, e := range existing {
		kk := k(e)
		if _, dup := seen[kk]; dup {
			continue
		}
		seen[kk] = struct{}{}
		out = append(out, e)
	}
	for _, e := range fresh {
		kk := k(e)
		if _, dup := seen[kk]; dup {
			continue
		}
		seen[kk] = struct{}{}
		out = append(out, e)
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
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("unmarshal entry: %w", err)
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// ListDays возвращает отсортированные дни ('YYYY-MM-DD'), чьи файлы есть в
// каталоге архива. Дни берутся из имён файлов — без распаковки.
func ListDays(dir string) ([]string, error) {
	names, err := filepath.Glob(filepath.Join(dir, "*"+FileExt))
	if err != nil {
		return nil, fmt.Errorf("glob archive: %w", err)
	}
	days := make([]string, 0, len(names))
	for _, n := range names {
		base := filepath.Base(n)
		d := strings.TrimSuffix(base, FileExt)
		if len(d) == len("2006-01-02") {
			days = append(days, d)
		}
	}
	sort.Strings(days)
	return days, nil
}

// ReadDay читает один дневной файл архива.
func ReadDay(dir, day string) ([]Entry, error) {
	zb, err := os.ReadFile(filepath.Join(dir, FileNameForDay(day)))
	if err != nil {
		return nil, fmt.Errorf("read archive day %s: %w", day, err)
	}
	return Decode(zb)
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
