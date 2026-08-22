package bot

import (
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	antispam "github.com/menand/AntiSpamBot"
)

func TestBaseVersion(t *testing.T) {
	cases := map[string]string{
		"":                      "",
		"dev":                   "",
		"v1.6.0":                "v1.6.0",
		"v1.6.0-dirty":          "v1.6.0",
		"v1.6.0-39-g7ca5ded":    "v1.6.0",
		"v1.6.0-3-gabc12-dirty": "v1.6.0",
		// Голый хеш (describe без тегов) базовой версией не является, но и
		// ломаться не должен — вернётся как есть и просто не совпадёт с
		// чейнджлогом.
		"7ca5ded": "7ca5ded",
	}
	for in, want := range cases {
		if got := baseVersion(in); got != want {
			t.Errorf("baseVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseChangelog(t *testing.T) {
	md := "# Заголовок\nпрочий текст\n\n## v1.2.0 — 2026-07-10\n- первое\n- второе\n\n## v1.1.0 — 2026-07-08\n- одно\n"
	rels := parseChangelog(md)
	if len(rels) != 2 {
		t.Fatalf("want 2 releases, got %+v", rels)
	}
	if rels[0].Version != "v1.2.0" || rels[0].Date != "2026-07-10" || len(rels[0].Items) != 2 {
		t.Fatalf("first release parsed wrong: %+v", rels[0])
	}
	if rels[1].Version != "v1.1.0" || len(rels[1].Items) != 1 || rels[1].Items[0] != "одно" {
		t.Fatalf("second release parsed wrong: %+v", rels[1])
	}
}

// TestEmbeddedChangelogParses сторожит формат реального CHANGELOG.md: битый
// заголовок версии или пункты вне записей молча выпадали бы из /whatsnew и
// рассылки о новой версии.
func TestEmbeddedChangelogParses(t *testing.T) {
	rels := parseChangelog(antispam.ChangelogMD)
	if len(rels) == 0 {
		t.Fatal("embedded CHANGELOG.md parsed to zero releases")
	}
	for _, r := range rels {
		if !strings.HasPrefix(r.Version, "v") || r.Date == "" || len(r.Items) == 0 {
			t.Errorf("malformed release entry: %+v", r)
		}
	}
	// Верхняя запись — самая свежая; renderWhatsNew на реальном файле влезает
	// в лимит Telegram.
	text := renderWhatsNew(rels, whatsNewVersions)
	if !strings.Contains(text, rels[0].Version) {
		t.Errorf("render must include the latest version %s", rels[0].Version)
	}
	if utf8.RuneCountInString(text) > 4096 {
		t.Errorf("rendered /whatsnew exceeds Telegram limit: %d runes", utf8.RuneCountInString(text))
	}
}

func TestRenderWhatsNewBudget(t *testing.T) {
	long := release{Version: "v9.9.9", Date: "2026-01-01",
		Items: []string{strings.Repeat("х", whatsNewRuneBudget)}}
	short := release{Version: "v9.9.8", Date: "2026-01-01", Items: []string{"пункт"}}
	text := renderWhatsNew([]release{long, short}, 10)
	if !strings.HasSuffix(text, "…") {
		t.Fatalf("overflow must be folded into …, got tail %q", text[len(text)-20:])
	}
	if strings.Contains(text, "v9.9.8") {
		t.Fatal("versions past the budget must be dropped")
	}
}

func TestBuildAnnounceTextBudget(t *testing.T) {
	// Короткий релиз: без усечения, с хвостом.
	short := []release{{Version: "v1.0.0", Items: []string{"фикс"}}}
	got := buildAnnounceText("v1.0.0", short, "\n\nхвост")
	if strings.Contains(got, "…") || !strings.HasSuffix(got, "хвост") || !strings.Contains(got, "фикс") {
		t.Fatalf("short release must fit intact: %q", got)
	}

	// Огромный релиз: усечение до бюджета, хвост на месте, итог < 4096.
	var items []string
	for i := 0; i < 200; i++ {
		items = append(items, strings.Repeat("пункт ", 30)+strconv.Itoa(i))
	}
	big := []release{{Version: "v2.0.0", Items: items}}
	got = buildAnnounceText("v2.0.0", big, "\n\nхвост")
	if n := utf8.RuneCountInString(got); n >= 4096 {
		t.Fatalf("announcement exceeds Telegram limit: %d runes", n)
	}
	if !strings.Contains(got, "…") {
		t.Fatal("overflow must fold into ellipsis")
	}
	if !strings.HasSuffix(got, "хвост") {
		t.Fatal("tail hint must survive truncation")
	}

	// Версии нет в чейнджлоге — заголовок и хвост без пунктов.
	got = buildAnnounceText("v9.9.9", big, "\n\nхвост")
	if strings.Contains(got, "пункт") || strings.Contains(got, "…") {
		t.Fatalf("unknown version must produce header+tail only: %q", got)
	}
}
