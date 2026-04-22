<p align="center">
  <img src="docs/logo.svg" alt="AntiSpamBot logo" width="180"/>
</p>

<h1 align="center">AntiSpamBot</h1>

<p align="center">
  <i>Подозрительно косится на каждого нового участника.</i>
</p>

<p align="center">
  <img src="https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white" alt="Go 1.26"/>
  <img src="https://img.shields.io/github/actions/workflow/status/menand/AntiSpamBot/ci.yml?branch=main&label=CI" alt="CI status"/>
  <img src="https://img.shields.io/badge/docker-alpine%203.22-2496ED?logo=docker&logoColor=white" alt="Alpine 3.22"/>
  <img src="https://img.shields.io/badge/telegram-bot%20api%207.x+-26A5E4?logo=telegram&logoColor=white" alt="Telegram Bot API 7.x+"/>
  <img src="https://img.shields.io/github/last-commit/menand/AntiSpamBot" alt="Last commit"/>
</p>

Telegram анти-спам бот на Go. Новых участников встречает капчей (**цветные кружки** или **случайные эмодзи** — на выбор админа), а все настройки — через DM-меню у бота, **без** команд в чате.

Работает в Docker, разворачивается на VDS одной командой, переживает рестарты (SQLite), ~12 МБ RAM в idle.

---

## Содержание

- [Как выглядит](#как-выглядит)
- [Фичи](#фичи)
- [Быстрый старт](#быстрый-старт)
  - [1. Создать бота у BotFather](#1-создать-бота-у-botfather)
  - [2. Локальный запуск](#2-локальный-запуск)
  - [3. Деплой на VDS через Docker](#3-деплой-на-vds-через-docker)
  - [4. Авто-обновления из git](#4-авто-обновления-из-git)
- [Добавление бота в чат](#добавление-бота-в-чат)
- [Команды бота](#команды-бота)
- [Настройки чата через меню](#настройки-чата-через-меню)
- [Конфигурация (env)](#конфигурация-env)
- [Статистика и приватность](#статистика-и-приватность)
- [Структура проекта](#структура-проекта)
- [Разработка](#разработка)
- [Операционные вопросы](#операционные-вопросы)
- [Траблшутинг](#траблшутинг)

---

## Как выглядит

**При входе нового участника** — один из двух режимов капчи (настраивается per chat).

**Режим «Кружки» (по умолчанию):**

> **Привет, @vasya!**
> Для защиты от спама выбери **красный кружок** за 30 секунд.
> [🟢] [🟡] [🔴] [🔵] [🟣] [🟠]

**Режим «Эмодзи»:**

> **Привет, @vasya!**
> Для защиты от спама выбери **бабочку** за 30 секунд.
> [🍕] [🦋] [🌈] [⚽] [🦊] [🐢]

Правильно → ограничения снимаются, сообщение удаляется, бот пишет «🎉 Добро пожаловать» (отключаемо).
Неправильно или таймаут → кик. 3 провала подряд за 24ч → перманентный бан.

**Сервисные сообщения** «X вошёл в чат» / «бот исключил X» бот удаляет сам — чат остаётся чистым.

**Молчаливые возвращенцы:**

> 🎊 Сенсация! **Vasya** молчал(а) **2 года** и вот наконец-то написал(а)!

Срабатывает, когда пользователь заговорил после долгой паузы. Порог — `SILENT_ANNOUNCE_DAYS` (default 30).

**Ежедневная сводка в чат** (опционально, включается в настройках):

```
🌅 Сводка за сутки

👋 Новых участников: 3
• Прошли капчу: 2
• Кикнуты: 1
• Забанены: 0

💬 Сообщений: 145
• Новички: 12 (8%)
• Старички: 133 (92%)

🔝 Топ писателей:
1. Alice — 58 сообщений
2. @bob — 32 сообщения
3. Charlie — 19 сообщений

🚫 Топ провалов капчи:
1. id9999 — 1 раз
```

---

## Фичи

- 🧩 **2 режима капчи** — кружки или эмодзи (одна из 6 категорий: звери/летуны/морские/природа/еда/вещи), переключаются per chat.
- ⚙️ **Per-chat настройки через DM-меню** — попытки до бана, таймаут, режим капчи, приветствие, ежедневная сводка + её время (МСК). Команд в группе нет.
- 📊 **Статистика**: заходы/капча/сообщения, топы писателей и провалов, «новички vs старички», детектор молчуна-возвращенца, ежедневный дайджест в чат.
- 💾 **SQLite** — один файл, переживает рестарты, автоматически мигрирует данные при превращении basic-group в supergroup.
- 🔐 **Role model**: владелец бота (`OWNER_IDS`) vs админ чата (проверяется через `getChatMember`) vs обычный. Каждый видит и может управлять только своими чатами.
- ⏳ **Задержка 2 сек** перед капчей — чтобы Telegram-клиент только что вошедшего успел отрисовать сообщение. Любые сообщения от новичка в эти 2 сек удаляются.
- 🔧 **Инфра**: JSON-логи, уровни `debug|info|warn|error`, graceful shutdown, auto-deploy из git по cron, non-root Docker, авто-уведомления владельцу 🟢/🔴 при старте/остановке.

---

## Быстрый старт

### 1. Создать бота у BotFather

1. В Telegram напиши [@BotFather](https://t.me/BotFather) → `/newbot`, придумай имя и username.
2. Сохрани **BOT_TOKEN** вида `123456789:ABC-def...`.
3. **Выключи Privacy Mode** (иначе бот не видит сообщения):
   `/mybots → <your_bot> → Bot Settings → Group Privacy → Turn off`.

### 2. Локальный запуск

Нужен Go 1.26+.

```bash
git clone https://github.com/menand/AntiSpamBot.git
cd AntiSpamBot
cp .env.example .env
# впиши BOT_TOKEN в .env
go mod tidy
go run ./cmd/bot
```

Бот подхватит `.env` автоматически (godotenv). SQLite-файл создастся как `./bot.db`.

### 3. Деплой на VDS через Docker

На сервере нужен только Docker:

```bash
# если Docker ещё не установлен:
curl -fsSL https://get.docker.com | sh

git clone https://github.com/menand/AntiSpamBot.git
cd AntiSpamBot
cp .env.example .env
nano .env   # впиши BOT_TOKEN (+ при желании OWNER_IDS)
docker compose up -d --build
```

SQLite-файл живёт в Docker volume `bot-data`, переживает `docker compose down/up --build`.

Обновить:
```bash
git pull && docker compose up -d --build
```

Автостарт при ребуте VDS — `restart: unless-stopped` в `docker-compose.yml`.

### 4. Авто-обновления из git

В репо есть скрипт [`scripts/auto-deploy.sh`](scripts/auto-deploy.sh): проверяет `origin/main`, при новом коммите делает `git pull` + пересборку. Разовая настройка:

```bash
# crontab -e:
*/5 * * * * /root/AntiSpamBot/scripts/auto-deploy.sh >> /var/log/antispam-deploy.log 2>&1
```

Исполняемый бит уже в git. Молчит если нечего делать, пишет в лог когда реально деплоит. `flock` защищает от наложений.

Владельцы бота (`OWNER_IDS`) при каждом обновлении получают в ЛС 🔴 → 🟢.

---

## Добавление бота в чат

1. Группа → «Управление» → «Администраторы» → «Добавить администратора» → выбери своего бота.
2. **Обязательные права:**
   - ✅ **Блокировать пользователей** (для кика и бана)
   - ✅ **Удалять сообщения** (для чистки капчи и сервисных сообщений)
3. Остальные права — по желанию.

> ⚠️ Без статуса админа Telegram **не шлёт** `chat_member` события и бот не увидит входящих. Это не баг, это API.

Если используешь `ALLOWED_CHATS` — ID чата покажет [@userinfobot](https://t.me/userinfobot): перешли ему любое сообщение из группы.

---

## Команды бота

Команды видны только в «/»-меню **в личке** (в группе меню пустое). Серверная область действия каждой:

| Команда | Где | Кому | Что делает |
|---|---|---|---|
| `/start`, `/help` | в ЛС | всем | главное меню с inline-кнопками, показывает твой Telegram ID |
| `/chats` | в ЛС | владельцам бота и админам чатов | список чатов, которыми управляешь, с настройками и статистикой каждого |
| `/info` | в ЛС | только владельцам | uptime и время запуска бота |
| `/logs` | в ЛС | только владельцам | бот присылает свой лог-файл документом |

**В группе ни одна команда не работает** — это задумка. Все настройки — через DM-меню «⚙️ Настройки» в разделе `/chats` → выбираешь чат.

---

## Настройки чата через меню

Основная UX-фича. В ЛС: `/start` → **📊 Мои чаты** → выбираешь чат → **⚙️ Настройки**:

```
⚙️ Настройки: "My Group"

🧩 Капча: Эмодзи
🔄 Попыток до бана: 3
⏱ Секунд на ответ: 30
🎉 Приветствие: ✅
📊 Ежедневная сводка в чат: ✅ в 09:00 МСК

[🟢 Кружки]  [• 🦋 Эмодзи •]
[2х] [• 3х •] [5х] [10х]
[15с] [• 30с •] [45с] [60с]
[🎉 Приветствие ✅]  [📊 Сводка ✅]
[00] [04] [• 08 •] [12] [16] [20]
[⬅️ К статистике]
```

Значение в подписи под текстом, текущий выбор на кнопке отмечен `• ... •`. Клик мгновенно сохраняет и перерисовывает меню.

Доступ:
- **Владельцы бота** (`OWNER_IDS`) видят все чаты, управляют всеми.
- **Админы чата** (без OWNER_IDS) видят только свои чаты.
- **Обычные участники** открывают «Мои чаты» — видят пустой список.

Каждая настройка per-chat и независима — в одном чате можно держать эмодзи + 5 попыток, в другом кружки + 3 попытки.

---

## Конфигурация (env)

Большинство настроек — per-chat через меню (см. выше). Env-переменные задают только глобальные дефолты и инфраструктуру.

| Переменная | По умолчанию | Описание |
|---|---|---|
| `BOT_TOKEN` | **обязат.** | Токен от @BotFather |
| `OWNER_IDS` | — | Telegram user_id владельцев бота через запятую. Получают 🟢/🔴, видят все чаты в меню. |
| `ALLOWED_CHATS` | — (= все) | Chat ID через запятую. Если задан — бот игнорирует другие чаты. |
| `CAPTCHA_TIMEOUT_SECONDS` | `30` | Дефолт секунд на капчу (перекрывается per-chat) |
| `MAX_ATTEMPTS` | `3` | Дефолт порога бана (перекрывается per-chat) |
| `CAPTCHA_DELAY_MS` | `2000` | Задержка перед отправкой капчи, чтобы клиент новичка успел отрисовать чат. Сообщения от новичка в окне удаляются. |
| `NEWCOMER_DAYS` | `7` | Дни после прохождения капчи, в течение которых пользователь считается «новичком» в статистике |
| `SILENT_ANNOUNCE_DAYS` | `30` | Порог анонса «молчаливого возвращенца». `0` — выкл. |
| `DAILY_STATS_UTC_HOUR` | `6` (= 09:00 МСК) | Глобальный час UTC для ежедневной сводки. Перекрывается per-chat. |
| `LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |
| `LOG_FILE` | — (в Docker: `/data/bot.log`) | Если задан — логи дублируются в файл с ротацией (10 МБ × 3 бэкапа, 30 дней). Нужен для `/logs`. |
| `DB_PATH` | `bot.db` (в Docker: `/data/bot.db`) | Путь к SQLite-файлу |

---

## Статистика и приватность

### Что собирается
- События: `join` / `pass` / `kick` / `ban` (per-chat, per-user, по времени).
- Сообщения: кто, когда, в каком чате, сколько (без содержимого).
- Агрегаты по дням: сообщения новичков vs старичков.
- Кэш display-имён (first_name, last_name, username) — для мемов в `/stats`.

### Что НЕ собирается
- **Содержимое сообщений** — никогда.
- **Ничего за пределами чатов, где работает бот.**

### Приватность
- Каждый чат изолирован: админ чата А не видит чат Б.
- Per-user статистика хранится — если пользователи чата не ожидают, предупреди.
- Очистить вручную в SQLite:
  ```bash
  docker compose exec bot sqlite3 /data/bot.db <<SQL
  DELETE FROM events WHERE at < strftime('%s', 'now', '-1 year');
  DELETE FROM user_message_counts WHERE day < date('now', '-1 year');
  -- полный сброс per-user:
  DELETE FROM user_activity; DELETE FROM user_message_counts; DELETE FROM user_info;
  SQL
  ```

---

## Структура проекта

```
cmd/bot/                 entrypoint (main)
internal/config/         env-переменные и их валидация
internal/captcha/        генерация challenge (кружки + эмодзи-категории) + in-memory store
internal/storage/        SQLite: schema, миграции, CRUD (chats, members, events, stats, settings)
  └─ migrate.go          handler для basic-group → supergroup миграции данных
internal/bot/            telego-клиент, update handlers, DM-меню, ежедневный digest
  ├─ menu.go             главное меню и settings submenu
  ├─ handlers.go         chat_member, callbacks, captcha lifecycle
  ├─ daily.go            loop для ежедневных сводок
  ├─ access.go           helpers: userChats, canManageChat, effective*-resolvers
  └─ logs.go, info.go    команды /logs, /info
scripts/auto-deploy.sh   cron-скрипт для автодеплоя из git
Dockerfile               multi-stage: golang:1.26-alpine → alpine:3.22
docker-compose.yml       служба bot, volume bot-data для SQLite и логов
.env.example             шаблон настроек
CLAUDE.md                заметки по архитектуре для AI-ассистентов
```

---

## Разработка

```bash
make build         # go build
make run           # go run
make test          # go test -race ./...
make vet           # go vet ./...
make docker-up     # docker compose up -d --build
make docker-logs   # docker compose logs -f
make docker-down   # docker compose down
```

Главные зависимости:
- [`github.com/mymmrac/telego`](https://github.com/mymmrac/telego) — Bot API 7.x+ клиент
- [`modernc.org/sqlite`](https://modernc.org/sqlite) — pure-Go SQLite (no CGO)
- [`gopkg.in/natefinch/lumberjack.v2`](https://github.com/natefinch/lumberjack) — ротация лог-файла
- [`github.com/joho/godotenv`](https://github.com/joho/godotenv) — .env для локального запуска

CI (GitHub Actions): `vet` + `test -race` + `build` на каждый push/PR. Dependabot автоматически создаёт PR с обновлениями зависимостей, patch-уровень авто-мерджится если CI зелёный.

---

## Операционные вопросы

**Логи.** stdout в JSON (`slog`). Docker compose ротирует: 3 файла × 10 МБ. Плюс дублируется в `/data/bot.log` (владелец может достать через `/logs`).

**Обновления.** `git pull && docker compose up -d --build` или auto-deploy по cron. Данные в volume сохраняются.

**Бэкапы.**
```bash
docker compose exec bot sh -c 'cat /data/bot.db' > bot-$(date +%F).db
# или через sqlite3 backup (онлайн, без паузы):
docker run --rm -v antispambot_bot-data:/data alpine:3.22 sh -c \
  'apk add -q sqlite && sqlite3 /data/bot.db ".backup /data/bot.db.bak"'
```

**Миграции схемы.** Автоматически при старте — в `internal/storage/db.go` список `ALTER TABLE ADD COLUMN`, ошибки «duplicate column» свапаются. Для новых таблиц — дописать в `schema.sql` с `CREATE TABLE IF NOT EXISTS`.

**Ресурсы.** В idle: ~12 МБ RAM, 0.3% CPU (одно ядро), 32.7 МБ образ. SQLite-файл растёт ~1 МБ на 10К сообщений.

---

## Траблшутинг

**Бот молчит на вход новых.**
- Бот админ? С правами «Банить» + «Удалять»?
- Privacy Mode выключен у BotFather?
- `docker compose logs --tail=50` — если тихо, это вопрос прав.

**`/chats` в ЛС выдаёт пустой список для админа чата.**
- Бот должен быть в чате и хоть раз видеть его (по сообщению или входу). Если бот только что добавлен — подожди пока кто-нибудь напишет, или зайди новый участник.

**После рестарта — капчи теряются?**
- Нет. При старте бот читает `pending_captchas` и поднимает таймеры с корректным оставшимся временем. Если срок истёк во время простоя — мгновенный кик.

**`database is locked` в логах.**
- Редкость. Бот использует WAL + `busy_timeout=5s` + один writer. Если систематически — значит чат очень активный, стоит переехать на Postgres.

**`BOT_TOKEN is not set`.**
- Нет `.env` или в нём пустой `BOT_TOKEN=`. `cp .env.example .env` + впиши токен.

**`CHAT_ADMIN_REQUIRED` ошибка.**
- Кто-то снял админку у бота. Восстанови + выдай права заново.

**Пользователь прошёл капчу, но не может писать.**
- `release()` упал (сеть или права). Сними ограничения вручную в настройках чата. В `/logs` ищи строку `release` с `err`.

---

<details>
<summary>📸 Аватарка для бота (инструкция)</summary>

В [`docs/logo.svg`](docs/logo.svg) лежит иконка бота (робот с капчей-нимбом и значком «no spam»). Telegram принимает PNG/JPG:

```bash
# ImageMagick:
convert -background none -resize 512x512 docs/logo.svg docs/logo.png
# или rsvg-convert (librsvg):
rsvg-convert -w 512 -h 512 docs/logo.svg -o docs/logo.png
# или онлайн-конвертер
```

Далее у @BotFather: `/setuserpic → выбираешь бота → отправляешь docs/logo.png`. Telegram обрежет в круг.

Если хочется другой стиль — SVG правится вручную, или сгенерируй растр через ChatGPT/DALL-E промптом:

> Cute cartoon robot mascot for a Telegram anti-spam bot. Square format, 512x512, Telegram-style rounded corners. Blue gradient background. Friendly but slightly suspicious-looking robot face with squinty cyan eyes and a small smirk. Antenna on top with a red indicator light emitting small yellow sparkles. Around the head: a halo of six colored dots (red, green, yellow, purple, orange, blue). A small tilted red "no entry" prohibition badge in the upper right corner. Modern flat design with subtle shadows, thick outlines, vector style.

</details>

---

## Лицензия

В репозитории нет файла `LICENSE` — по умолчанию это означает «all rights reserved». Если хочешь разрешить переиспользование — добавь, например, MIT:
```bash
curl -sL https://opensource.org/license/mit > LICENSE
```
