package bot

import "testing"

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
