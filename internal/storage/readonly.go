package storage

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"

	_ "modernc.org/sqlite"
)

// OpenReadOnly открывает БД строго на чтение — для инструментов вроде msgtool,
// которые дёргаются на живой проде параллельно с ботом. Не выполняет миграций
// (никакой записи) и не трогает схему. WAL даёт конкурентное чтение с работающим
// писателем. Если файл не существует — ошибка (читать нечего).
//
// DSN строится через url.URL, а не sprintf: путь с «?» или «#» иначе ломал бы
// разбор URI и молча снимал mode=ro. OmitHost обязателен: без него url.String
// добавил бы «//» перед первым сегментом относительного пути («file://bot.db»),
// и SQLite разобрал бы «bot.db» как authority — «invalid uri authority», дефолт
// msgtool -db bot.db перестал бы открываться. query_only — вторая страховка,
// если mode=ro когда-нибудь не применился.
func OpenReadOnly(ctx context.Context, path string) (*DB, error) {
	u := url.URL{Scheme: "file", Path: path, OmitHost: true}
	q := url.Values{}
	q.Set("mode", "ro")
	// Именно Add: Set дважды по одному ключу оставляет только последнюю
	// прагму — query_only молча терялся бы.
	q.Add("_pragma", "query_only(1)")
	q.Add("_pragma", "busy_timeout(5000)")
	u.RawQuery = q.Encode()

	raw, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("open sqlite read-only: %w", err)
	}
	// Ограничение одним соединением — инструмент однопоточный, пул не нужен.
	raw.SetMaxOpenConns(1)
	if err := raw.PingContext(ctx); err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("ping sqlite read-only: %w", err)
	}
	return &DB{sql: raw}, nil
}
