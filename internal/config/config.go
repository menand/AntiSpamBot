package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	Token          string
	CaptchaTimeout time.Duration
	MaxAttempts    int
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

	maxAttempts, err := parseInt("MAX_ATTEMPTS", 3)
	if err != nil {
		return nil, err
	}

	return &Config{
		Token:          token,
		CaptchaTimeout: timeout,
		MaxAttempts:    maxAttempts,
	}, nil
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
