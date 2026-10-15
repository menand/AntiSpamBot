package textwidth

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncate(t *testing.T) {
	tests := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{
			name: "короткое проходит насквозь",
			in:   "Чат двора",
			max:  30,
			want: "Чат двора",
		},
		{
			name: "точно в бюджет без изменений",
			in:   strings.Repeat("б", 30),
			max:  30,
			want: strings.Repeat("б", 30),
		},
		{
			name: "режется по ширине, многоточие в счёт",
			in:   strings.Repeat("б", 40),
			max:  10,
			want: strings.Repeat("б", 9) + "…",
		},
		{
			// Многоточие занимает единицу: 20 = 19 текста + «…».
			name: "хвостовой пробел подчищается",
			in:   "Беговой чат жителей Измайловского района",
			max:  21,
			want: "Беговой чат жителей…",
		},
		{
			// max=2: 1 единица текста + многоточие. Инвариант width≤2 держится.
			name: "max=2 не превышает бюджет",
			in:   "абвгде",
			max:  2,
			want: "а…",
		},
		{
			// max=1 усечённое многоточие: единственное, что влезает в 1.
			name: "max=1 не превышает бюджет",
			in:   "абвгде",
			max:  1,
			want: "…",
		},
		{
			// max<1 не обязан ничего вмещать, но и не должен паниковать.
			name: "max=0 не паникует",
			in:   "абвгде",
			max:  0,
			want: "…",
		},
		{
			name: "отрицательный max не паникует",
			in:   "абвгде",
			max:  -1,
			want: "…",
		},
		{
			// NBSP перед многоточием так же виден как дырка, как и пробел.
			name: "хвостовой NBSP подчищается",
			in:   "Беговой чат жителей",
			max:  12,
			want: "Беговой чат…",
		},
		{
			// Семья = 7 рун, но ОДНА графема: режется только после неё.
			name: "эмодзи-кластер цел",
			in:   "👨‍👩‍👧‍👦" + strings.Repeat("б", 40),
			max:  10,
			want: "👨‍👩‍👧‍👦" + strings.Repeat("б", 7) + "…",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Truncate(tc.in, tc.max)

			if !utf8.ValidString(got) {
				t.Fatalf("invalid UTF-8: %q", got)
			}
			// Главный инвариант: результат не шире бюджета.
			if tc.max >= 2 {
				if w := Width(got); w > tc.max {
					t.Errorf("width %d exceeds budget %d: %q", w, tc.max, got)
				}
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExact(t *testing.T) {
	// Ровно в бюджет — насквозь. «Беговой ча» = 10 рун = 10 единиц ширины.
	if got := Exact("Беговой чат жителей", 10); got != "Беговой ча" {
		t.Errorf("got %q, want %q", got, "Беговой ча")
	}
	if got := Exact("абвгде", 0); got != "" {
		t.Errorf("max=0 must give empty, got %q", got)
	}
	if got := Exact("абвгде", -1); got != "" {
		t.Errorf("negative max must give empty, got %q", got)
	}
	// Ровно в бюджет — насквозь.
	if got := Exact("абв", 3); got != "абв" {
		t.Errorf("got %q, want %q", got, "абв")
	}
}

func TestWidth(t *testing.T) {
	tests := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"а", 1},
		{"пробел", 6},
		{"ё", 1},       // многобайтовая руна = 1 единица
		{"🚀", 2},       // эмодзи = 2
		{"👨‍👩‍👧‍👦", 2}, // кластер из 7 рун = 2
		{"🇷🇺", 2},      // флаг = 2
		{"👍🏽", 2},      // тон кожи = 2
	}
	for _, tc := range tests {
		if got := Width(tc.in); got != tc.want {
			t.Errorf("Width(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// Клавиша-эмодзи («1️⃣» = цифра + VS16 + U+20E3) клиент рисует КВАДРАТНОЙ,
// а uniseg по основе (ASCII-цифра) даёт 1. Замерено вживую через
// cmd/btnprobe -mode kseq: при ширине 1 восьмая клавиша уже резалась.
//
// Это единственная спец-ветка пакета, поэтому закрепляем её явно: откат
// graphemeWidth на uniseg-значение 1 должен упасть здесь, а не молча
// вернуть обрезку на проде.
func TestWidthKeycap(t *testing.T) {
	tests := []struct {
		in   string
		want int
	}{
		{"1️⃣", 2},
		{"8️⃣", 2},
		{"1️⃣2️⃣", 4},
		{"#️⃣", 2},  // keycap бывает не только на цифре
		{"1️⃣а", 3}, // соседняя кириллица не схлопывает клавишу
	}
	for _, tc := range tests {
		if got := Width(tc.in); got != tc.want {
			t.Errorf("Width(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
	// Обрезка идёт по границе КЛАВИШИ, а не по её основе: 1 клавиша (2) + «…».
	if got := Truncate("1️⃣2️⃣3️⃣", 3); got != "1️⃣…" {
		t.Errorf("Truncate(keycaps) = %q, want %q", got, "1️⃣…")
	}
}

// Бюджет обязан падать с ростом числа кнопок в ряду: ширина ряда делится
// между кнопками, и по 30 на каждую в ряду из двух не влезет ни одна.
func TestLabelBudget(t *testing.T) {
	want := map[int]int{
		-1: MaxLabelWidth, // мусорный ввод = одна кнопка
		0:  MaxLabelWidth,
		1:  MaxLabelWidth,
		2:  17,
		3:  10,
		4:  7,
		5:  5,
		9:  5, // дальше — тот же потолок
	}
	for per, w := range want {
		if got := LabelBudget(per); got != w {
			t.Errorf("LabelBudget(%d) = %d, want %d", per, got, w)
		}
	}
	// Монотонность: больше кнопок — бюджет не растёт.
	prev := LabelBudget(1)
	for per := 2; per <= 8; per++ {
		cur := LabelBudget(per)
		if cur > prev {
			t.Errorf("LabelBudget(%d) = %d вырос относительно %d", per, cur, prev)
		}
		prev = cur
	}
	// Бюджет любого ряда обязан быть достижим Truncate без превышения.
	for per := 1; per <= 6; per++ {
		b := LabelBudget(per)
		if w := Width(Truncate(strings.Repeat("б", 100), b)); w > b {
			t.Errorf("per=%d: Truncate дал ширину %d при бюджете %d", per, w, b)
		}
	}
}
