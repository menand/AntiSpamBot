package archive

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	uid := int64(42)
	rid := int64(7)
	entries := []Entry{
		{Chat: -1001, User: 10, TGMsgID: 1, TS: 1760000000, Kind: "text", Text: "Как твои руки?"},
		{Chat: -1001, User: 11, TGMsgID: 2, TS: 1760000060, Kind: "caption", HasMedia: true,
			ReplyUID: &uid, ReplyMsgID: &rid, ReplyAuthor: "Alina", ReplyText: "Ребёнку два месяца"},
	}
	zb, err := Encode(entries)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	// zstd должен реально сжать пачку: две строки ~100 байт → заметно меньше.
	if len(zb) == 0 {
		t.Fatal("empty compressed output")
	}

	got, err := Decode(zb)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2", len(got))
	}
	if got[0].Text != "Как твои руки?" || got[0].Kind != "text" || got[0].HasMedia {
		t.Fatalf("unexpected first: %+v", got[0])
	}
	if got[1].ReplyUID == nil || *got[1].ReplyUID != uid {
		t.Fatalf("reply uid: %+v", got[1].ReplyUID)
	}
	if got[1].ReplyAuthor != "Alina" || got[1].ReplyText != "Ребёнку два месяца" {
		t.Fatalf("reply fields: %+v", got[1])
	}
	if got[1].Text != "" {
		t.Fatalf("caption entry should have empty text, got %q", got[1].Text)
	}
}

func TestReadDayAndListDays(t *testing.T) {
	dir := t.TempDir()
	for _, day := range []string{"2026-10-12", "2026-10-14"} {
		zb, err := Encode([]Entry{{Chat: 1, User: 2, TGMsgID: 3, TS: 1, Kind: "text", Text: day}})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, FileNameForDay(day)), zb, 0o640); err != nil {
			t.Fatal(err)
		}
	}
	// Мусорный файл должен быть проигнорирован ListDays.
	if err := os.WriteFile(filepath.Join(dir, "not-a-day.jsonl.zst"), []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}

	days, err := ListDays(dir)
	if err != nil {
		t.Fatalf("list days: %v", err)
	}
	if len(days) != 2 || days[0] != "2026-10-12" || days[1] != "2026-10-14" {
		t.Fatalf("days: %v", days)
	}

	entries, err := ReadDay(dir, "2026-10-14")
	if err != nil {
		t.Fatalf("read day: %v", err)
	}
	if len(entries) != 1 || entries[0].Text != "2026-10-14" {
		t.Fatalf("read day entries: %+v", entries)
	}

	if _, err := ReadDay(dir, "2026-10-13"); err == nil {
		t.Fatal("expected error reading missing day")
	}
}

func TestDecodeRejectsCorrupt(t *testing.T) {
	if _, err := Decode([]byte("не zstd")); err == nil {
		t.Fatal("expected error decoding corrupt input")
	}
}

func TestDecodeRejectsBadJSONLInZstd(t *testing.T) {
	// Валидный zstd-фрейм, но внутри — не-JSON строка: Decode обязан упасть,
	// а не вернуть молча частичный результат (иначе msgtool скрыл бы порчу).
	zb, err := zstdEncode([]byte("{\"c\":1}\nэто не json\n"))
	if err != nil {
		t.Fatalf("zstd encode: %v", err)
	}
	if _, err := Decode(zb); err == nil {
		t.Fatal("expected error decoding bad JSONL line")
	}
}

// Пустые строки между записями (хвостовой \n) пропускаются, не роняя разбор.
func TestDecodeSkipsBlankLines(t *testing.T) {
	zb, err := zstdEncode([]byte("{\"c\":1}\n\n{\"c\":2}\n"))
	if err != nil {
		t.Fatalf("zstd encode: %v", err)
	}
	got, err := Decode(zb)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("entries = %d, want 2 (blank line skipped)", len(got))
	}
}

func TestMergeEntriesDedupsAndKeepsOriginals(t *testing.T) {
	existing := []Entry{
		{Chat: -1001, User: 10, TGMsgID: 1, TS: 100, Kind: "text", Text: "оригинал"},
		{Chat: -1001, User: 11, TGMsgID: 2, TS: 200, Kind: "text", Text: "второй"},
	}
	// Поздняя строка: дубликат TGMsgID=1/ts=100 (реплей) + новая правка.
	fresh := []Entry{
		{Chat: -1001, User: 10, TGMsgID: 1, TS: 100, Kind: "text", Text: "оригинал"}, // dup
		{Chat: -1001, User: 10, TGMsgID: 1, TS: 300, Kind: "edit", Text: "правка"},   // новая
	}
	got := MergeEntries(existing, fresh)
	if len(got) != 3 {
		t.Fatalf("merged = %d, want 3 (dedup dup, keep original + edit)", len(got))
	}
	if got[0].Text != "оригинал" || got[1].Text != "второй" || got[2].Kind != "edit" {
		t.Fatalf("unexpected order/content: %+v", got)
	}
	// Пустой existing → fresh как есть.
	if n := len(MergeEntries(nil, fresh)); n != 2 {
		t.Fatalf("empty existing = %d, want 2", n)
	}
}

// Две правки одного сообщения в ТУ ЖЕ секунду (Telegram гранулярность 1с) с
// разным текстом обязаны обе выжить: ронять позднюю нельзя. Ключ дедупа
// включает text — иначе вторая версия молча затёрла бы первую.
func TestMergeEntriesKeepsSameSecondDifferentText(t *testing.T) {
	existing := []Entry{
		{Chat: -1001, User: 10, TGMsgID: 5, TS: 100, Kind: "edit", Text: "первая версия"},
	}
	fresh := []Entry{
		{Chat: -1001, User: 10, TGMsgID: 5, TS: 100, Kind: "edit", Text: "вторая версия"},
		{Chat: -1001, User: 10, TGMsgID: 5, TS: 100, Kind: "edit", Text: "первая версия"}, // точный реплей
	}
	got := MergeEntries(existing, fresh)
	if len(got) != 2 {
		t.Fatalf("merged = %d, want 2 (different text kept, exact replay deduped)", len(got))
	}
	if got[0].Text != "первая версия" || got[1].Text != "вторая версия" {
		t.Fatalf("content/order: %+v", got)
	}

	// Дубли и ВНУТРИ existing схлопываются (страховка от самодублей в файле,
	// накопленных крашем между записью файла и удалением hot-строк).
	withDup := []Entry{existing[0], existing[0], existing[0]}
	if n := len(MergeEntries(withDup, nil)); n != 1 {
		t.Fatalf("dup inside existing = %d, want 1", n)
	}
}
