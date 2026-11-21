// msgtool — редкий аналитический доступ к сохранённому следу сообщений.
//
// Читает «горячее» окно из SQLite (последние дни, быстрый индекс/подзапрос) и
// стримингово сканирует дневные zstd-архивы за нужный период. Печатает
// матчи в JSONL на stdout — для разбора ЛЛМ (сбор статистики, поиск).
//
// Запуск на проде (рядом с работающим ботом, read-only):
//
//	docker exec menand-antispam msgtool \
//	  -db /data/bot.db -archive /data/archive \
//	  -chat -1003832795188 -word ананас -since 2026-10-01 -until 2026-10-31
//
// Флаги (все опциональны, дефолты — всё без фильтров):
//
//	-chat N     — только этот chat_id
//	-user N     — только этот user_id (автор)
//	-word S     — подстрока в тексте или цитате
//	-since D    — дата начала (YYYY-MM-DD), включительно
//	-until D    — дата конца (YYYY-MM-DD), включительно
//	-limit N    — максимум строк (0 = без лимита)
//	-db PATH    — путь к SQLite (дефолт bot.db)
//	-archive P  — каталог архива (дефолт <dir(db)>/archive)
//	-verify-archive — проверить все дневные файлы без вывода записей
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/menand/AntiSpamBot/internal/archive"
	"github.com/menand/AntiSpamBot/internal/storage"
)

// wireEntry — строка вывода msgtool. Те же поля, что в архиве, плюс источник
// (hot|archive) и отрендеренное время — чтобы ЛЛМ не приходилось самой
// конвертировать unix.
type wireEntry struct {
	Source      string `json:"src"`
	Chat        int64  `json:"c"`
	User        int64  `json:"u"`
	TGMsgID     int64  `json:"mid"`
	Time        string `json:"ts"`
	Kind        string `json:"k"`
	Text        string `json:"t,omitempty"`
	HasMedia    bool   `json:"media,omitempty"`
	ReplyMsgID  *int64 `json:"rmid,omitempty"`
	ReplyUID    *int64 `json:"ruid,omitempty"`
	ReplyAuthor string `json:"ra,omitempty"`
	ReplyText   string `json:"rt,omitempty"`
}

func main() {
	// Дефолты берём из окружения контейнера (DB_PATH/ARCHIVE_DIR задаёт
	// docker-compose): тогда `docker exec menand-antispam msgtool …` работает
	// без флагов, а локальный запуск падает на привычный "bot.db" рядом с cwd.
	dbDefault := os.Getenv("DB_PATH")
	if dbDefault == "" {
		dbDefault = "bot.db"
	}
	var (
		dbPath   = flag.String("db", dbDefault, "путь к SQLite-файлу (дефолт $DB_PATH)")
		archiveP = flag.String("archive", os.Getenv("ARCHIVE_DIR"), "каталог архива (дефолт $ARCHIVE_DIR или <dir(db)>/archive)")
		chatID   = flag.Int64("chat", 0, "фильтр по chat_id")
		userID   = flag.Int64("user", 0, "фильтр по user_id")
		word     = flag.String("word", "", "подстрока в тексте/цитате")
		sinceS   = flag.String("since", "", "дата начала YYYY-MM-DD (вкл)")
		untilS   = flag.String("until", "", "дата конца YYYY-MM-DD (вкл)")
		limit    = flag.Int("limit", 0, "максимум строк (0 = без лимита)")
		verify   = flag.Bool("verify-archive", false, "проверить все файлы архива без вывода записей")
	)
	flag.Parse()
	archDir := *archiveP
	if archDir == "" {
		archDir = filepath.Join(filepath.Dir(*dbPath), "archive")
	}
	if *verify {
		if err := archive.VerifyDirectory(archDir); err != nil {
			fatal("archive verification", err)
		}
		return
	}

	since, err := parseDay(*sinceS)
	if err != nil {
		fatal("since", err)
	}
	until, err := parseDay(*untilS)
	if err != nil {
		fatal("until", err)
	}
	// until включаем как конец суток: пользователь думает «по 31 октября
	// включительно», а не «до 31 октября 00:00».
	if !until.IsZero() {
		until = until.AddDate(0, 0, 1)
	}

	ctx := context.Background()
	out := bufio.NewWriter(os.Stdout)
	// Обычный выход (return из main) обязан сбросить буфер: без defer весь
	// JSONL на stdout терялся бы молча — fatal успевал допечатать только через
	// flushOut, а штатный путь (без -limit) вовсе не печатал. fatal дополнительно
	// дёргает flushOut перед os.Exit (defer пропускается при exit).
	defer out.Flush() //nolint:errcheck
	flushOut = func() { _ = out.Flush() }

	enc := json.NewEncoder(out)
	emit := func(e wireEntry) {
		if err := enc.Encode(e); err != nil {
			fatal("encode", err)
		}
	}

	filter := storage.MessageFilter{ChatID: *chatID, UserID: *userID, Word: *word, From: since, Until: until, Limit: *limit}
	history, err := readHotHistory(ctx, *dbPath, filter)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warn: hot window skipped: %v\n", err)
	}
	defer history.close()
	history.visit(archDir, filter, func(err error) {
		fmt.Fprintf(os.Stderr, "warn: %v\n", err)
	}, emit)
}

func fromEntry(e archive.Entry, source string) wireEntry {
	return wireEntry{
		Source: source, Chat: e.Chat, User: e.User, TGMsgID: e.TGMsgID,
		Time: time.Unix(e.TS, 0).UTC().Format(time.RFC3339), Kind: e.Kind, Text: e.Text,
		HasMedia: e.HasMedia, ReplyMsgID: e.ReplyMsgID, ReplyUID: e.ReplyUID,
		ReplyAuthor: e.ReplyAuthor, ReplyText: e.ReplyText,
	}
}

// entryMatches — общая фильтрация для archive/hot записей. replyText может быть
// пустым (цитаты нет).
func entryMatches(chat, user int64, text, replyText string, fChat, fUser int64, fWord string) bool {
	if fChat != 0 && chat != fChat {
		return false
	}
	if fUser != 0 && user != fUser {
		return false
	}
	if fWord != "" {
		if !strings.Contains(text, fWord) && !strings.Contains(replyText, fWord) {
			return false
		}
	}
	return true
}

// parseDay толкует YYYY-MM-DD в полночь МСК; пустая строка = нулевое время.
func parseDay(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.ParseInLocation("2006-01-02", s, storage.StatsLocation)
	if err != nil {
		return time.Time{}, fmt.Errorf("ожидалась дата YYYY-MM-DD, получено %q", s)
	}
	return t, nil
}

// dayInRange — попадает ли день ('YYYY-MM-DD' как имя файла) в [since, until).
func dayInRange(day string, since, until time.Time) bool {
	if _, err := time.Parse("2006-01-02", day); err != nil {
		return false
	}
	if !since.IsZero() && day < storage.DayOf(since) {
		return false
	}
	if !until.IsZero() && day >= storage.DayOf(until) {
		return false
	}
	return true
}

// flushOut сбрасывает stdout-буфер; подставляется в main, чтобы fatal успел
// допечатать накопленное ДО os.Exit (defer пропускается при выходе через exit).
var flushOut = func() {}

func fatal(what string, err error) {
	fmt.Fprintf(os.Stderr, "error: %s: %v\n", what, err)
	flushOut()
	os.Exit(1)
}
