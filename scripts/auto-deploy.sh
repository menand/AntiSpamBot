#!/bin/bash
# Auto-deploy: pull the CI-built image from GHCR and restart when it changed.
# Сборка происходит на GitHub Actions (ci.yml, job image) ПОСЛЕ зелёных
# тестов — ВДС только забирает готовый образ, локальных билдов больше нет.
# git-обновление здесь нужно лишь для самого скрипта и docker-compose.yml.
#
# Гонка «коммит запушен, образ ещё собирается» безопасна by design: pull
# приносит прежний latest, image ID не меняется, тик молчит; следующий тик
# подхватит готовый образ.
#
# Smoke + карантин: после деплоя проверяем, что контейнер жив (Running,
# RestartCount стабилен, getMe отвечает). Жёсткий провал — возвращаем прежний
# образ и кладём ID в карантин (QUARANTINE). Петли нет: пока упавший образ
# (тот же ID) в карантине, тик молчит; новый ID (реальный фикс) проходит.
# Карантин живёт ВНЕ репо и volume контейнера — переживает git-pull и рестарты.
#
# Safe to run on a cron tick — file lock, silent when there's nothing to do.
# Usage (cron, реальный тик — КАЖДУЮ МИНУТУ):
#   * * * * * /root/AntiSpamBot/scripts/auto-deploy.sh >> /var/log/antispam-deploy.log 2>&1

set -euo pipefail

# Cron's PATH is minimal; make sure docker + git are findable.
export PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

# Resolve the repo root as the parent of this script, regardless of cwd.
REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_DIR"

DEPLOY_LOG="/var/log/antispam-deploy.log"
QUARANTINE="/var/lib/antispam/bad-images"
IMAGE="ghcr.io/menand/antispambot:latest"
ENV_FILE="$REPO_DIR/.env"

# Деплой-лог растёт от минутных pull-тиков; при 50MB срезаем в ноль.
# fd крона открыт с O_APPEND, поэтому truncate не даёт NUL-хвоста.
if [ -f "$DEPLOY_LOG" ] && [ "$(stat -c %s "$DEPLOY_LOG" 2>/dev/null || echo 0)" -gt 52428800 ]; then
    : > "$DEPLOY_LOG"
fi

# Prevent overlapping runs.
exec 9>"/tmp/antispam-deploy.lock"
if ! flock -n 9; then
    exit 0
fi

# Значение ключа из compose-овского .env (KEY=value, `export ` и кавычки снимаем).
parse_env() {
    local key="$1" v
    v="$(grep -E "^(export[[:space:]]+)?${key}=" "$ENV_FILE" 2>/dev/null | head -1 | sed -E "s/^(export[[:space:]]+)?[A-Za-z0-9_]+=//")" || return 1
    [ -z "$v" ] && return 1
    v="${v#\"}" v="${v%\"}"
    v="${v#\'}" v="${v%\'}"
    printf '%s' "$v"
}

# ID образа в карантине?
is_quarantined() {
    [ -f "$QUARANTINE" ] && grep -qxF "$1" "$QUARANTINE" 2>/dev/null
}

short_id() { echo "${1:7:12}"; }

# DM владельцам из OWNER_IDS. Возвращает успех, если хотя бы одна отправка
# прошла (curl без ошибок) — notify_once держит маркер только при успехе.
# Токен не логируется.
notify_owners() {
    local token msg owner sent=0
    token="$(parse_env BOT_TOKEN)" || return 1
    [ -z "$token" ] && return 1
    msg="$1"
    for owner in $(parse_env OWNER_IDS | tr ',' ' '); do
        [ -z "$owner" ] && continue
        if curl -s --max-time 10 -o /dev/null \
            "https://api.telegram.org/bot${token}/sendMessage" \
            --data-urlencode "chat_id=${owner}" \
            --data-urlencode "text=${msg}"; then
            sent=1
        fi
    done
    [ "$sent" -eq 1 ]
}

# notify_once <kind> <msg> — то же, но не чаще раза в сутки на kind: минутный
# cron при застарелой проблеме (разошедшееся репо, вечный провал pull) иначе
# захламил бы ЛС владельцев. Маркер пишется ТОЛЬКО после удачной отправки —
# иначе один сетевой сбой подавил бы алерт на сутки.
NOTIFY_STATE="/var/lib/antispam/notify-state"
notify_once() {
    local kind="$1" msg="$2" today
    today="$(date +%F)"
    mkdir -p "$(dirname "$NOTIFY_STATE")" 2>/dev/null || true
    if [ -f "$NOTIFY_STATE" ] && grep -qxF "${kind}:${today}" "$NOTIFY_STATE" 2>/dev/null; then
        return 0
    fi
    if notify_owners "$msg"; then
        echo "${kind}:${today}" >> "$NOTIFY_STATE" 2>/dev/null || true
    fi
}

# Тихо подтягиваем репо (скрипт/compose); --tags — чтобы git describe на
# сервере совпадал с релизными тегами. Расхождение репо (локальный хотфикс,
# правка compose руками) раньше убивало весь скрипт через set -e ДО всякой
# логики деплоя — и каждый следующий тик умирал там же молча. Теперь: раз в
# сутки владельцам уходит сигнал, тик завершается с ошибкой в лог.
git fetch --quiet --tags origin main || true
if [ "$(git rev-parse HEAD)" != "$(git rev-parse origin/main)" ]; then
    if ! git merge --ff-only --quiet origin/main; then
        echo "!!! $(date -Is) git merge --ff-only failed (repo diverged); deploy skipped"
        notify_once "merge" "⚠️ Автодеплой стоит: репо на сервере разошлось с origin/main. Нужен ручной git stash/reset на ВДС." || true
        exit 1
    fi
fi

before=$(docker inspect --format '{{.Image}}' menand-antispam 2>/dev/null || true)
# pull молчит в логе при успехе, но провал теперь различим: заглушенный
# «|| true» превращал вечный сбой pull (токен, DNS, диск) в тихое «нечего
# деплоить» — бот не обновлялся бы вообще без единого сигнала.
pull_failed=0
pull_out="$(docker compose pull --quiet bot 2>&1)" || pull_failed=1
if [ "$pull_failed" -eq 1 ]; then
    echo "!!! $(date -Is) docker compose pull failed: ${pull_out:0:300}"
    notify_once "pull" "⚠️ Автодеплой: docker compose pull падает — ${pull_out:0:200}. Бот не обновляется." || true
fi
after=$(docker image inspect --format '{{.Id}}' "$IMAGE" 2>/dev/null || true)

# Нечего деплоить (или CI ещё не дособрал образ) — молчим, лог не растёт.
if [ -z "$after" ] || [ "$before" = "$after" ]; then
    exit 0
fi

# Проваленный ранее образ (тот же ID) — не деплоим снова, петли нет.
if is_quarantined "$after"; then
    exit 0
fi

echo "=== $(date -Is) deploying image $(short_id "$after") ==="
if ! docker compose up -d --no-build; then
    echo "!!! $(date -Is) deploy command failed; container stays on $(short_id "$before")"
    notify_owners "⚠️ Деплой провалился (up -d): бот остался на $(short_id "$before")" || true
    exit 0
fi

# --- Smoke ---
sleep 10
SMOKE_FAIL=""
if [ "$(docker inspect --format '{{.State.Status}}' menand-antispam 2>/dev/null || true)" != "running" ]; then
    SMOKE_FAIL="container not running"
else
    r1=$(docker inspect --format '{{.RestartCount}}' menand-antispam 2>/dev/null || echo 0)
    # 25 секунд, а не 5: образ, падающий на t≈20s (первый LLM-вызов, первая
    # миграция), раньше проскакивал мимо smoke — а следующий тик видел
    # неизменный image ID и молчал вечно.
    sleep 25
    if [ "$(docker inspect --format '{{.State.Status}}' menand-antispam 2>/dev/null || true)" != "running" ]; then
        SMOKE_FAIL="container died within smoke window"
    else
        r2=$(docker inspect --format '{{.RestartCount}}' menand-antispam 2>/dev/null || echo 0)
        if [ "$r1" != "$r2" ]; then
            SMOKE_FAIL="restart count grew ($r1 -> $r2)"
        fi
    fi
fi

# getMe — мягкий сигнал (валидность токена/сети); падение само по себе НЕ
# откатывает живой контейнер (сетевые блипы случаются).
TOKEN="$(parse_env BOT_TOKEN || true)"
GETME_OK=0
if [ -n "$TOKEN" ]; then
    if curl -s --max-time 5 "https://api.telegram.org/bot${TOKEN}/getMe" | grep -q '"ok":true'; then
        GETME_OK=1
    fi
fi

if [ -z "$SMOKE_FAIL" ] && [ "$GETME_OK" -eq 1 ]; then
    # Вытесненные старые образы (~35MB каждый) — чистим, чтобы не копились.
    docker image prune -f >/dev/null 2>&1 || true
    echo "=== $(date -Is) deploy done ($(short_id "$after")) ==="
    notify_owners "✅ Бот обновлён: image $(short_id "$after")" || true
    exit 0
fi

if [ -n "$SMOKE_FAIL" ]; then
    # Жёсткий провал — откат на прежний образ + карантин. Prune НЕ делаем:
    # старый образ ещё нужен, а упавший остаётся в registry для диагностики.
    mkdir -p "$(dirname "$QUARANTINE")" || true
    if ! grep -qxF "$after" "$QUARANTINE" 2>/dev/null; then
        echo "$after" >> "$QUARANTINE" 2>/dev/null || true
    fi
    echo "!!! $(date -Is) SMOKE FAIL ($SMOKE_FAIL); rolling back to $(short_id "$before")"
    if docker tag "$before" "$IMAGE" && docker compose up -d --no-build; then
        echo "!!! $(date -Is) rollback done; image $(short_id "$after") quarantined"
        notify_owners "⚠️ Откат: image $(short_id "$after") провалил smoke ($SMOKE_FAIL), вернулся на $(short_id "$before")" || true
    else
        echo "!!! $(date -Is) ROLLBACK FAILED — manual intervention required!"
        notify_owners "🚨 Откат на $(short_id "$before") НЕ удался! Нужно вмешательство" || true
    fi
    exit 1
fi

# Контейнер жив, но getMe молчит — предупреждаем, НЕ откатываем.
echo "!!! $(date -Is) getMe failed but container is running; keeping $(short_id "$after")"
notify_owners "⚠️ Бот обновлён ($(short_id "$after")), но getMe не отвечает — проверьте" || true
exit 0