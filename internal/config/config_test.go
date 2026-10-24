package config

import (
	"log/slog"
	"reflect"
	"testing"
	"time"
)

// clearEnv сбрасывает все переменные, которые читает Load, — экспортированные
// в шелле разработчика значения не должны протекать в тесты.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"BOT_TOKEN", "CAPTCHA_STAGE_MINUTES", "MAX_ATTEMPTS", "LOG_LEVEL",
		"ALLOWED_CHATS", "DB_PATH", "NEWCOMER_DAYS", "SILENT_ANNOUNCE_DAYS",
		"OWNER_IDS", "LOG_FILE", "CAPTCHA_DELAY_MS", "DAILY_STATS_UTC_HOUR",
		"GROQ_API_KEY", "GROQ_MODEL",
		"GEMINI_API_KEY", "GEMINI_MODEL",
		"GIGACHAT_AUTH_KEY", "GIGACHAT_SCOPE", "GIGACHAT_MODEL",
		"AI_PROVIDER_ORDER",
		"MESSAGE_CAPTURE_ENABLED", "ARCHIVE_DIR", "MESSAGE_ARCHIVE_HOT_DAYS",
		"REPLY_WATCH_ENABLED", "REPLY_WATCH_CHAT_ID",
	} {
		t.Setenv(name, "")
	}
}

func TestParseChatIDs(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    []int64 // nil = ожидаем nil-мапу
		wantErr bool
	}{
		{"empty", "", nil, false},
		{"single", "-1001234", []int64{-1001234}, false},
		{"list with spaces", " 1 , -2 ,3 ", []int64{1, -2, 3}, false},
		{"only commas", ", ,", nil, false},
		{"garbage", "1,abc", nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TEST_CHAT_IDS", tc.value)
			got, err := parseChatIDs("TEST_CHAT_IDS")
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if tc.want == nil {
				if got != nil {
					t.Fatalf("want nil map, got %v", got)
				}
				return
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want ids %v", got, tc.want)
			}
			for _, id := range tc.want {
				if _, ok := got[id]; !ok {
					t.Errorf("missing id %d in %v", id, got)
				}
			}
		})
	}
}

func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		value   string
		want    slog.Level
		wantErr bool
	}{
		{"", slog.LevelWarn, false}, // пусто → дефолт (передаём Warn как def)
		{"debug", slog.LevelDebug, false},
		{"info", slog.LevelInfo, false},
		{"WARN", slog.LevelWarn, false}, // регистронезависимо
		{"warning", slog.LevelWarn, false},
		{"error", slog.LevelError, false},
		{"verbose", 0, true},
	}
	for _, tc := range tests {
		t.Run("value="+tc.value, func(t *testing.T) {
			t.Setenv("TEST_LOG_LEVEL", tc.value)
			got, err := parseLogLevel("TEST_LOG_LEVEL", slog.LevelWarn)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseAIProviderOrder(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  []string // nil = ждём ошибку
	}{
		{"empty", "", DefaultAIProviders},
		{"whitespace", "  ", DefaultAIProviders},
		{"default order", "groq,gemini,gigachat", []string{"groq", "gemini", "gigachat"}},
		{"reordered", "gigachat,groq,gemini", []string{"gigachat", "groq", "gemini"}},
		{"single", "gemini", []string{"gemini"}},
		{"unknown provider", "groq,cerebras", nil},
		{"duplicate", "groq,gemini,groq", nil},
		{"empty entry", "groq,,gemini", nil},
		{"case sensitive", "GROQ,gemini", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseAIProviderOrder(tc.value)
			if tc.want == nil {
				if err == nil {
					t.Fatalf("value %q: want error, got %v", tc.value, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("value %q: unexpected error: %v", tc.value, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("value %q: got %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

func TestParseDuration(t *testing.T) {
	t.Setenv("TEST_DUR", "")
	if d, err := parseDuration("TEST_DUR", 30*time.Second, time.Second); err != nil || d != 30*time.Second {
		t.Fatalf("empty must return default: %v %v", d, err)
	}
	t.Setenv("TEST_DUR", "45")
	if d, err := parseDuration("TEST_DUR", 0, time.Second); err != nil || d != 45*time.Second {
		t.Fatalf("seconds unit: %v %v", d, err)
	}
	if d, err := parseDuration("TEST_DUR", 0, time.Millisecond); err != nil || d != 45*time.Millisecond {
		t.Fatalf("milliseconds unit: %v %v", d, err)
	}
	t.Setenv("TEST_DUR", "abc")
	if _, err := parseDuration("TEST_DUR", 0, time.Second); err == nil {
		t.Fatal("garbage must error")
	}
	// Переполнение int64 в наносекундах раньше обёртывалось в положительную
	// длительность мимо всех проверок <= 0.
	t.Setenv("TEST_DUR", "99999999999999999999")
	if _, err := parseDuration("TEST_DUR", 0, time.Second); err == nil {
		t.Fatal("overflow must error")
	}
	t.Setenv("TEST_DUR", "99999999999") // влезает в Atoi, но не в наносекунды
	if _, err := parseDuration("TEST_DUR", 0, time.Millisecond); err != nil {
		t.Fatalf("large ms must pass: %v", err)
	}
	t.Setenv("TEST_DUR", "99999999999")
	if _, err := parseDuration("TEST_DUR", 0, time.Second); err == nil {
		t.Fatal("large seconds must error (int64 ns overflow)")
	}
}

func TestLoadDefaults(t *testing.T) {
	clearEnv(t)
	t.Setenv("BOT_TOKEN", "123:abc")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Token != "123:abc" {
		t.Errorf("token: %q", cfg.Token)
	}
	if cfg.CaptchaStageInterval != 2*time.Minute || cfg.MaxAttempts != 3 ||
		cfg.NewcomerDays != 7 || cfg.SilentAnnounceDays != 30 ||
		cfg.CaptchaDelay != 3*time.Second || cfg.DailyStatsUTCHour != 6 {
		t.Errorf("defaults mismatch: %+v", cfg)
	}
	if cfg.DBPath != "bot.db" {
		t.Errorf("db path: %q", cfg.DBPath)
	}
	if cfg.AllowedChats != nil || cfg.OwnerIDs != nil {
		t.Errorf("chat maps must default to nil: %+v", cfg)
	}
	if !reflect.DeepEqual(cfg.AIProviderOrder, DefaultAIProviders) {
		t.Errorf("provider order default: got %v, want %v", cfg.AIProviderOrder, DefaultAIProviders)
	}
	if cfg.MessageCaptureEnabled {
		t.Error("message capture must default to off")
	}
	if cfg.ArchiveHotDays != 7 {
		t.Errorf("archive hot days default: %d, want 7", cfg.ArchiveHotDays)
	}
	if cfg.ArchiveDir != "" {
		t.Errorf("archive dir default must be empty (derived from DB path): %q", cfg.ArchiveDir)
	}
	if !cfg.ReplyWatchEnabled {
		t.Error("reply watch must default to on")
	}
	if cfg.ReplyWatchChatID != 0 {
		t.Errorf("reply watch chat id default must be 0 (owner DM): %d", cfg.ReplyWatchChatID)
	}
}

func TestReplyWatchConfigParsing(t *testing.T) {
	clearEnv(t)
	t.Setenv("BOT_TOKEN", "123:abc")
	t.Setenv("REPLY_WATCH_ENABLED", "0")
	t.Setenv("REPLY_WATCH_CHAT_ID", "-100123")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.ReplyWatchEnabled {
		t.Error("REPLY_WATCH_ENABLED=0 must disable the watch")
	}
	if cfg.ReplyWatchChatID != -100123 {
		t.Errorf("reply watch chat id = %d, want -100123", cfg.ReplyWatchChatID)
	}
}

func TestLoadValidation(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{"missing token", "BOT_TOKEN", ""},
		{"zero stage minutes", "CAPTCHA_STAGE_MINUTES", "0"},
		{"negative stage minutes", "CAPTCHA_STAGE_MINUTES", "-1"},
		{"zero attempts", "MAX_ATTEMPTS", "0"},
		{"zero newcomer days", "NEWCOMER_DAYS", "0"},
		{"negative silent days", "SILENT_ANNOUNCE_DAYS", "-1"},
		{"negative delay", "CAPTCHA_DELAY_MS", "-100"},
		{"hour out of range", "DAILY_STATS_UTC_HOUR", "24"},
		{"bad log level", "LOG_LEVEL", "loud"},
		{"unknown AI provider", "AI_PROVIDER_ORDER", "groq,cerebras"},
		{"duplicate AI provider", "AI_PROVIDER_ORDER", "groq,gemini,groq"},
		{"negative archive hot days", "MESSAGE_ARCHIVE_HOT_DAYS", "-1"},
		{"bad archive hot days", "MESSAGE_ARCHIVE_HOT_DAYS", "many"},
		{"bad capture flag", "MESSAGE_CAPTURE_ENABLED", "maybe"},
		{"bad reply watch flag", "REPLY_WATCH_ENABLED", "maybe"},
		{"bad reply watch chat id", "REPLY_WATCH_CHAT_ID", "chat"},
		{"zero reply watch chat id", "REPLY_WATCH_CHAT_ID", "0"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("BOT_TOKEN", "123:abc")
			t.Setenv(tc.key, tc.value)
			if _, err := Load(); err == nil {
				t.Fatalf("%s=%q must fail Load", tc.key, tc.value)
			}
		})
	}
}

func TestParseBool(t *testing.T) {
	tests := []struct {
		name  string
		value string
		def   bool
		want  bool
	}{
		{"empty default false", "", false, false},
		{"empty default true", "", true, true},
		{"one", "1", false, true},
		{"true", "true", false, true},
		{"TRUE upper", "TRUE", false, true},
		{"yes", "yes", false, true},
		{"on", "on", false, true},
		{"zero", "0", true, false},
		{"false", "false", true, false},
		{"no", "no", true, false},
		{"off", "off", true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TEST_PARSE_BOOL", tc.value)
			got, err := parseBool("TEST_PARSE_BOOL", tc.def)
			if err != nil {
				t.Fatalf("parseBool(%q): %v", tc.value, err)
			}
			if got != tc.want {
				t.Fatalf("parseBool(%q, %v) = %v, want %v", tc.value, tc.def, got, tc.want)
			}
		})
	}

	// Опечатка — ошибка запуска, а не молчаливый дефолт.
	t.Run("invalid is error", func(t *testing.T) {
		t.Setenv("TEST_PARSE_BOOL", "maybe")
		if _, err := parseBool("TEST_PARSE_BOOL", false); err == nil {
			t.Fatal("expected error for invalid bool")
		}
	})
}
