package bot

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/menand/AntiSpamBot/internal/storage"
)

func TestTruncateLabel(t *testing.T) {
	// 25 кириллических рун = 50 байт: старая побайтовая обрезка сломалась бы.
	long := "Очень длинное название чата для проверки обрезки"
	got := truncateLabel(long, 40)
	if !utf8.ValidString(got) {
		t.Fatalf("invalid UTF-8 after truncation: %q", got)
	}
	if r := []rune(got); len(r) != 40 {
		t.Errorf("got %d runes, want 40", len(r))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("expected ellipsis suffix, got %q", got)
	}

	short := "Чат"
	if truncateLabel(short, 40) != short {
		t.Error("short labels must pass through unchanged")
	}
}

func TestTruncateLabelWidth(t *testing.T) {
	tests := []struct {
		name string
		in   string
		max  int
		// wantEmpty — строка вообще не должна обрезаться.
		wantNoEllipsis bool
		// wantRunePrefix — срез ровно по этим рунам плюс многоточие.
		wantRunePrefix string
		// wantContains — подстрока, обязана сохраниться в результате
		// (инварианты вроде «эмодзи-кластер цел»).
		wantContains string
	}{
		{
			name:           "короткое не трогаем",
			in:             "129 роддом, июль-сентябрь",
			max:            labelMaxWidth,
			wantNoEllipsis: true,
		},
		{
			name: "длинное кириллическое режем по ширине",
			in:   "Беговой чат жителей Измайловского района",
			max:  labelMaxWidth,
			// 33 единицы бюджета: 32 на текст + 1 на многоточие.
			// Кейс писался под старый бюджет 30 — при откате константы
			// назад он упадёт здесь, а не молча на проде.
			wantRunePrefix: "Беговой чат жителей Измайловског",
		},
		{
			// Семья из 7 рун = ОДНА графема шириной 2. Рунная обрезка
			// дала бы мусорный half-эмодзи, по графемам — целый кластер.
			// Проверяем не только UTF-8, но и сам инвариант: кластер в
			// результате обязан присутствовать целиком, иначе регрессия,
			// ради которой тест писался, пройдёт молча.
			name:           "эмодзи-кластер не режется пополам",
			in:             "👨‍👩‍👧‍👦" + strings.Repeat("б", 40),
			max:            labelMaxWidth,
			wantNoEllipsis: false,
			wantContains:   "👨‍👩‍👧‍👦",
		},
		{
			// 35 «х» + 3 эмодзи = 41 единица. Под бюджет 33 текст занимает
			// весь лимит (32 + многоточие), и все три эмодзи уходят ЦЕЛИКОМ,
			// а не порезанными пополам. При старом бюджете 30 вход был 31
			// и кейс ничего не проверял.
			name:           "эмодзи в конце просто отваливаются",
			in:             strings.Repeat("х", 35) + "🚀🔥🎉",
			max:            labelMaxWidth,
			wantNoEllipsis: false,
		},
		{
			// 30 × 2 = 60 единиц, режется, но каждый 🚀 остаётся целым.
			name:           "сплошные эмодзи режутся по ширине, не по рунам",
			in:             strings.Repeat("🚀", 30),
			max:            labelMaxWidth,
			wantNoEllipsis: false,
		},
		{
			// Бюджет 21: 20 на текст + многоточие, и 20-я позиция — пробел
			// после «жителей». Без подчистки получилось бы «жителей …» с
			// дыркой перед многоточием.
			name:           "хвостовой пробел подчищается",
			in:             "Беговой чат жителей Измайловского района",
			max:            21,
			wantRunePrefix: "Беговой чат жителей",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := truncateLabelWidth(tc.in, tc.max)

			if !utf8.ValidString(got) {
				t.Fatalf("invalid UTF-8: %q", got)
			}
			if w := labelWidth(got); w > tc.max {
				t.Errorf("width %d exceeds budget %d: %q", w, tc.max, got)
			}
			if tc.wantContains != "" && !strings.Contains(got, tc.wantContains) {
				t.Errorf("invariant lost: %q not in %q", tc.wantContains, got)
			}
			if tc.wantNoEllipsis {
				if strings.HasSuffix(got, "…") {
					t.Errorf("must not be truncated, got %q", got)
				}
				return
			}
			if !strings.HasSuffix(got, "…") {
				t.Errorf("expected ellipsis, got %q", got)
			}
			// Обрезанный хвост не должен оставлять висячего пробела.
			if strings.HasSuffix(strings.TrimSuffix(got, "…"), " ") {
				t.Errorf("trailing space before ellipsis: %q", got)
			}
			if tc.wantRunePrefix != "" {
				if want := tc.wantRunePrefix + "…"; got != want {
					t.Errorf("got %q, want %q", got, want)
				}
			}
		})
	}
}

// Семья должна считаться за ОДНУ графему шириной 2, а не за 7 рун.
func TestLabelWidthCountsEmojiClusters(t *testing.T) {
	if w := labelWidth("👨‍👩‍👧‍👦"); w != 2 {
		t.Errorf("family emoji width = %d, want 2", w)
	}
	if w := labelWidth("б"); w != 1 {
		t.Errorf("letter width = %d, want 1", w)
	}
	if w := labelWidth("🚀"); w != 2 {
		t.Errorf("rocket width = %d, want 2", w)
	}
}

func TestRenderGreeting(t *testing.T) {
	mention := `<a href="tg://user?id=1">Вася</a>`

	// Дефолт, когда кастомного шаблона нет.
	if got := renderGreeting("", "", mention); !strings.Contains(got, mention) {
		t.Errorf("default greeting must mention the user: %q", got)
	}

	// Кастомный шаблон: {name} подставляется, пользовательский HTML экранируется.
	got := renderGreeting("Привет, {name}! <b>Прочти правила</b>", "", mention)
	if !strings.Contains(got, mention) {
		t.Errorf("{name} not substituted: %q", got)
	}
	if strings.Contains(got, "<b>") {
		t.Errorf("user-supplied HTML must be escaped: %q", got)
	}
	if !strings.Contains(got, "&lt;b&gt;") {
		t.Errorf("expected escaped tags in output: %q", got)
	}
}

func TestPluralRU(t *testing.T) {
	tests := []struct {
		n    int
		want string
	}{
		{0, "дней"},
		{1, "день"},
		{2, "дня"},
		{4, "дня"},
		{5, "дней"},
		{10, "дней"},
		{11, "дней"},
		{12, "дней"},
		{14, "дней"},
		{15, "дней"},
		{21, "день"},
		{22, "дня"},
		{25, "дней"},
		{101, "день"},
		{111, "дней"},
		{121, "день"},
	}
	for _, tc := range tests {
		if got := pluralRU(tc.n, "день", "дня", "дней"); got != tc.want {
			t.Errorf("pluralRU(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

func TestHumanDaysRU(t *testing.T) {
	tests := []struct {
		days int
		want string
	}{
		{0, "0 дней"},
		{1, "1 день"},
		{5, "5 дней"},
		{29, "29 дней"},
		{30, "1 месяц"},
		{60, "2 месяца"},
		{150, "5 месяцев"},
		{365, "1 год"},
		{730, "2 года"},
		{2000, "5 лет"},
	}
	for _, tc := range tests {
		if got := humanDaysRU(tc.days); got != tc.want {
			t.Errorf("humanDaysRU(%d) = %q, want %q", tc.days, got, tc.want)
		}
	}
}

func TestHumanDaysGenRU(t *testing.T) {
	// Родительный падеж для фраз «после N ...».
	tests := []struct {
		days int
		want string
	}{
		{1, "1 дня"},
		{2, "2 дней"},
		{21, "21 дня"},
		{29, "29 дней"},
		{30, "1 месяца"},
		{60, "2 месяцев"},
		{150, "5 месяцев"},
		{365, "1 года"},
		{730, "2 лет"},
		{2000, "5 лет"},
	}
	for _, tc := range tests {
		if got := humanDaysGenRU(tc.days); got != tc.want {
			t.Errorf("humanDaysGenRU(%d) = %q, want %q", tc.days, got, tc.want)
		}
	}
}

func TestChatLinkHTML(t *testing.T) {
	tests := []struct {
		name string
		c    storage.ChatInfo
		want string
	}{
		{"public username", storage.ChatInfo{ChatID: -1001234567890, Title: "Бег", Username: "run_chat"},
			`<a href="https://t.me/run_chat">«Бег»</a>`},
		{"private supergroup", storage.ChatInfo{ChatID: -1001234567890, Title: "Бег"},
			`<a href="https://t.me/c/1234567890/999999999">«Бег»</a>`},
		{"basic group plain", storage.ChatInfo{ChatID: -12345, Title: "Бег"},
			"«Бег»"},
		{"title escaped", storage.ChatInfo{ChatID: -12345, Title: "A<b>&"},
			"«A&lt;b&gt;&amp;»"},
		{"no title", storage.ChatInfo{ChatID: -12345},
			"«Чат -12345»"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := chatLinkHTML(tc.c); got != tc.want {
				t.Errorf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestMentionWithUsername(t *testing.T) {
	infos := map[int64]storage.UserInfo{
		1: {UserID: 1, FirstName: "Andrey", LastName: "Menshov", Username: "menshovandrey"},
		2: {UserID: 2, FirstName: "Вася"},
		3: {UserID: 3, Username: "onlynick"},
	}
	if got := mentionWithUsername(infos, 1); !strings.Contains(got, "</a> - @menshovandrey") {
		t.Fatalf("want username tail after the mention, got %q", got)
	}
	if got := mentionWithUsername(infos, 2); strings.Contains(got, " - @") {
		t.Fatalf("user without username must have no tail, got %q", got)
	}
	// Имени нет — меншен уже показывает @username, хвост бы его дублировал.
	if got := mentionWithUsername(infos, 3); strings.Count(got, "@onlynick") != 1 {
		t.Fatalf("username-only user must not duplicate the nick, got %q", got)
	}
	if got := mentionWithUsername(infos, 99); strings.Contains(got, " - @") {
		t.Fatalf("unknown user must have no tail, got %q", got)
	}
}

// Бюджет обязан падать с ростом числа кнопок в ряду: ширина ряда делится
// между кнопками, и по 30 на каждую в ряду из двух не влезет ни одна.
func TestLabelBudget(t *testing.T) {
	want := map[int]int{
		-1: labelMaxWidth, // мусорный ввод = одна кнопка
		0:  labelMaxWidth,
		1:  labelMaxWidth,
		2:  17,
		3:  10,
		4:  7,
		5:  5,
		9:  5, // дальше — тот же потолок
	}
	for per, w := range want {
		if got := labelBudget(per); got != w {
			t.Errorf("labelBudget(%d) = %d, want %d", per, got, w)
		}
	}
	// Монотонность: больше кнопок — бюджет не растёт.
	prev := labelBudget(1)
	for per := 2; per <= 8; per++ {
		cur := labelBudget(per)
		if cur > prev {
			t.Errorf("labelBudget(%d) = %d вырос относительно %d", per, cur, prev)
		}
		prev = cur
	}
}
