package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/joho/godotenv"

	"github.com/menand/AntiSpamBot/internal/bot"
	"github.com/menand/AntiSpamBot/internal/config"
)

func main() {
	// Auto-load .env for local development. In Docker compose the env comes
	// from env_file/environment, and this call is a silent no-op when .env
	// doesn't exist.
	_ = godotenv.Load()

	cfg, err := config.Load()
	if err != nil {
		slog.Error("load config", "err", err)
		os.Exit(1)
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(log)

	b, err := bot.New(cfg, log)
	if err != nil {
		log.Error("init bot", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Info("starting bot", "username", b.Username())
	if err := b.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("bot run", "err", err)
		os.Exit(1)
	}
	log.Info("bot stopped")
}
