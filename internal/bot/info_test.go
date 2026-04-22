package bot

import (
	"testing"
	"time"
)

func TestFormatUptimeRU(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{0, "1 секунда"}, // clamp to >=1s
		{500 * time.Millisecond, "1 секунда"},
		{1 * time.Second, "1 секунда"},
		{2 * time.Second, "2 секунды"},
		{5 * time.Second, "5 секунд"},
		{45 * time.Second, "45 секунд"},
		{1 * time.Minute, "1 минута"},
		{2 * time.Minute, "2 минуты"},
		{5 * time.Minute, "5 минут"},
		{1*time.Minute + 30*time.Second, "1 минута"},
		{1 * time.Hour, "1 час"},
		{1*time.Hour + 1*time.Minute, "1 час, 1 минута"},
		{3*time.Hour + 42*time.Minute, "3 часа, 42 минуты"},
		{24 * time.Hour, "1 день"},
		{3*24*time.Hour + 5*time.Hour + 42*time.Minute, "3 дня, 5 часов, 42 минуты"},
		{7 * 24 * time.Hour, "7 дней"},
		{21 * 24 * time.Hour, "21 день"},
	}
	for _, tc := range tests {
		if got := formatUptimeRU(tc.d); got != tc.want {
			t.Errorf("formatUptimeRU(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}
