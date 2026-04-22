package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Token              string
	CaptchaTimeout     time.Duration
	MaxAttempts        int
	LogLevel           slog.Level
	AllowedChats       map[int64]struct{} // nil = allow all
	DBPath             string
	NewcomerDays       int
	SilentAnnounceDays int // 0 = disabled
	OwnerIDs           map[int64]struct{} // Telegram user IDs with super-admin rights
	LogFile            string             // empty = stdout only; set = tee to file (for /logs command)
	CaptchaDelay       time.Duration      // delay between join and sending captcha
}

func Load() (*Config, error) {
	token := os.Getenv("BOT_TOKEN")
	if token == "" {
		return nil, errors.New("BOT_TOKEN is not set")
	}

	timeout, err := parseDurationSec("CAPTCHA_TIMEOUT_SECONDS", 30*time.Second)
	if err != nil {
		return nil, err
	}
	if timeout <= 0 {
		return nil, errors.New("CAPTCHA_TIMEOUT_SECONDS must be > 0")
	}

	maxAttempts, err := parseInt("MAX_ATTEMPTS", 3)
	if err != nil {
		return nil, err
	}
	if maxAttempts <= 0 {
		return nil, errors.New("MAX_ATTEMPTS must be > 0")
	}

	logLevel, err := parseLogLevel("LOG_LEVEL", slog.LevelInfo)
	if err != nil {
		return nil, err
	}

	allowedChats, err := parseChatIDs("ALLOWED_CHATS")
	if err != nil {
		return nil, err
	}

	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		dbPath = "bot.db"
	}

	newcomerDays, err := parseInt("NEWCOMER_DAYS", 7)
	if err != nil {
		return nil, err
	}
	if newcomerDays <= 0 {
		return nil, errors.New("NEWCOMER_DAYS must be > 0")
	}

	silentDays, err := parseInt("SILENT_ANNOUNCE_DAYS", 30)
	if err != nil {
		return nil, err
	}
	if silentDays < 0 {
		return nil, errors.New("SILENT_ANNOUNCE_DAYS must be >= 0 (0 disables announcements)")
	}

	ownerIDs, err := parseChatIDs("OWNER_IDS")
	if err != nil {
		return nil, err
	}

	captchaDelay, err := parseDurationMs("CAPTCHA_DELAY_MS", 2000*time.Millisecond)
	if err != nil {
		return nil, err
	}
	if captchaDelay < 0 {
		return nil, errors.New("CAPTCHA_DELAY_MS must be >= 0")
	}

	return &Config{
		Token:              token,
		CaptchaTimeout:     timeout,
		MaxAttempts:        maxAttempts,
		LogLevel:           logLevel,
		AllowedChats:       allowedChats,
		DBPath:             dbPath,
		NewcomerDays:       newcomerDays,
		SilentAnnounceDays: silentDays,
		OwnerIDs:           ownerIDs,
		LogFile:            os.Getenv("LOG_FILE"),
		CaptchaDelay:       captchaDelay,
	}, nil
}

func parseDurationMs(name string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	ms, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("invalid %s: %w", name, err)
	}
	return time.Duration(ms) * time.Millisecond, nil
}

func parseDurationSec(name string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	sec, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("invalid %s: %w", name, err)
	}
	return time.Duration(sec) * time.Second, nil
}

func parseInt(name string, def int) (int, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("invalid %s: %w", name, err)
	}
	return n, nil
}

func parseLogLevel(name string, def slog.Level) (slog.Level, error) {
	v := strings.ToLower(os.Getenv(name))
	if v == "" {
		return def, nil
	}
	switch v {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("invalid %s: %q (expected debug|info|warn|error)", name, v)
}

func parseChatIDs(name string) (map[int64]struct{}, error) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return nil, nil
	}
	out := make(map[int64]struct{})
	for _, raw := range strings.Split(v, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid %s entry %q: %w", name, raw, err)
		}
		out[id] = struct{}{}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}
