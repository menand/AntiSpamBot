package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/menand/AntiSpamBot/internal/bot"
	"github.com/menand/AntiSpamBot/internal/config"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	cfg, err := config.Load()
	if err != nil {
		log.Error("load config", "err", err)
		os.Exit(1)
	}

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
