package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/joho/godotenv"
	"gopkg.in/natefinch/lumberjack.v2"

	"github.com/menand/AntiSpamBot/internal/bot"
	"github.com/menand/AntiSpamBot/internal/config"
)

// version проставляется на этапе сборки через -ldflags "-X main.version=...".
// "dev" означает обычный `go run` / сборку без тега.
var version = "dev"

func main() {
	// Автоподхват .env для локальной разработки. В Docker compose окружение
	// приходит из env_file/environment, а когда .env не существует, этот
	// вызов — молчаливый no-op.
	_ = godotenv.Load()

	cfg, err := config.Load()
	if err != nil {
		slog.Error("load config", "err", err)
		os.Exit(1)
	}

	writers := []io.Writer{os.Stdout}
	if cfg.LogFile != "" {
		writers = append(writers, &lumberjack.Logger{
			Filename:   cfg.LogFile,
			MaxSize:    10, // MB на файл до ротации
			MaxBackups: 3,
			MaxAge:     30, // дней
			Compress:   false,
		})
	}
	log := slog.New(slog.NewJSONHandler(io.MultiWriter(writers...),
		&slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(log)

	b, err := bot.New(cfg, log, version)
	if err != nil {
		log.Error("init bot", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Второй сигнал — принудительный выход: зависший bh.Stop()/db.Close()
	// иначе держал бы процесс вечно (вне Docker его некому добить по таймауту).
	// Единственный в процессе голый go: тело не может запаниковать (только
	// чтения канала и os.Exit), а b.goSafe недоступен — Bot ещё не создан.
	force := make(chan os.Signal, 1)
	signal.Notify(force, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-force // первый сигнал расходится и в NotifyContext
		<-force
		os.Exit(130)
	}()

	log.Info("starting bot", "username", b.Username())
	if err := b.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("bot run", "err", err)
		os.Exit(1)
	}
	log.Info("bot stopped")
}
