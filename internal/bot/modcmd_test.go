package bot

import (
	"testing"
	"time"
)

func TestParseMuteDuration(t *testing.T) {
	tests := []struct {
		text string
		want time.Duration
		ok   bool
	}{
		{"/mute 5", 5 * time.Minute, true},
		{"/mute 45m", 45 * time.Minute, true},
		{"/mute 3h", 3 * time.Hour, true},
		{"/mute 5d", 5 * 24 * time.Hour, true},
		{"/mute 45 @vasya", 45 * time.Minute, true},
		{"/mute @vasya 45", 45 * time.Minute, true},
		{"/mute@TestBot 10", 10 * time.Minute, true},
		{"/mute 400d", 365 * 24 * time.Hour, true},          // кап 365 дней
		{"/mute 999999999999d", 365 * 24 * time.Hour, true}, // overflow-защита до умножения
		{"/mute 9999999999999999999m", 0, false},            // за пределами int64 — отказ Atoi
		{"/mute", 0, false},
		{"/mute abc", 0, false},
		{"/mute 0", 0, false},
		{"/mute -5", 0, false},
		{"/mute m", 0, false},
		{"/mute 5x", 0, false},
	}
	for _, tt := range tests {
		got, ok := parseMuteDuration(tt.text)
		if got != tt.want || ok != tt.ok {
			t.Errorf("parseMuteDuration(%q) = (%v, %v), want (%v, %v)",
				tt.text, got, ok, tt.want, tt.ok)
		}
	}
}

func TestMuteLabel(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{5 * time.Minute, "5 мин"},
		{90 * time.Minute, "90 мин"},
		{3 * time.Hour, "3 ч"},
		{5 * 24 * time.Hour, "5 дн"},
	}
	for _, tt := range tests {
		if got := muteLabel(tt.d); got != tt.want {
			t.Errorf("muteLabel(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}
