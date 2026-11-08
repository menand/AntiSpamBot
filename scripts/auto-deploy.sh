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
# RestartCount стабилен, heartbeat свежий, getMe отвечает). Жёсткий провал — возвращаем прежний
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

source "$REPO_DIR/scripts/deploy-notify.sh"

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

# В classic image store .Image контейнера и image inspect .Id — config ID.
# В containerd store .Id может быть digest OCI-индекса, а .Image остаётся
# config ID. Тогда сравниваем manifest контейнера с manifest нужной платформы
# скачанного образа. Исходный ID сохраняем для карантина и отката.
running_image_matches() {
    local expected="$1" current manifest platform target
    current=$(docker inspect --format '{{.Image}}' menand-antispam 2>/dev/null || true)
    if [ -n "$current" ] && [ "$current" = "$expected" ]; then
        return 0
    fi
    manifest=$(docker inspect --format '{{with index . "ImageManifestDescriptor"}}{{index . "digest"}}{{end}}' menand-antispam 2>/dev/null || true)
    [ -n "$manifest" ] || return 1
    platform=$(docker inspect --format '{{with index . "ImageManifestDescriptor"}}{{with index . "platform"}}{{index . "os"}}/{{index . "architecture"}}{{with index . "variant"}}/{{.}}{{end}}{{end}}{{end}}' menand-antispam 2>/dev/null || true)
    [ -n "$platform" ] || return 1
    target=$(docker image inspect --platform "$platform" --format '{{.Id}}' "$expected" 2>/dev/null || true)
    [ -n "$target" ] && [ "$manifest" = "$target" ]
}

# Проверяем и новый образ, и результат отката. Успех compose сам по себе
# ничего не говорит о жизнеспособности запущенного контейнера.
smoke_image() {
    local expected="$1" r1 r2
    SMOKE_FAIL=""
    sleep 10
    if [ "$(docker inspect --format '{{.State.Status}}' menand-antispam 2>/dev/null || true)" != running ]; then
        SMOKE_FAIL="контейнер не запущен"
        return 1
    fi
    r1=$(docker inspect --format '{{.RestartCount}}' menand-antispam 2>/dev/null || true)
    if [ "$r1" != 0 ]; then
        SMOKE_FAIL="контейнер уже перезапускался ($r1)"
        return 1
    fi
    sleep 25
    if [ "$(docker inspect --format '{{.State.Status}}' menand-antispam 2>/dev/null || true)" != running ]; then
        SMOKE_FAIL="контейнер остановился во время проверки"
        return 1
    fi
    r2=$(docker inspect --format '{{.RestartCount}}' menand-antispam 2>/dev/null || true)
    if [ "$r1" != "$r2" ]; then
        SMOKE_FAIL="контейнер перезапустился ($r1 → $r2)"
    elif ! docker exec menand-antispam /usr/local/bin/healthcheck >/dev/null 2>&1; then
        SMOKE_FAIL="heartbeat не прошёл проверку свежести"
    elif ! running_image_matches "$expected"; then
        SMOKE_FAIL="запущен другой образ"
    fi
    [ -z "$SMOKE_FAIL" ]
}

# DM владельцам из OWNER_IDS. Возвращает успех, если хотя бы одна отправка
# прошла (curl без ошибок) — notify_once держит маркер только при успехе.
# Токен не логируется и не попадает в argv: URL передаётся через конфиг на
# stdin (-K -), иначе он светился бы в ps на время каждого cron-тика.
notify_owners() {
    local token msg owner sent=0
    token="$(parse_env BOT_TOKEN)" || return 1
    [ -z "$token" ] && return 1
    msg="$1"
    for owner in $(parse_env OWNER_IDS | tr ',' ' '); do
        [ -z "$owner" ] && continue
        if printf 'url = "https://api.telegram.org/bot%s/sendMessage"\n' "$token" |
            curl -fs --max-time 10 -o /dev/null -K - \
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

# Подтягиваем ветку со скриптами/Compose без исторических тегов: несовпадение
# старого локального тега с origin не должно блокировать обновление ветки.
# Версию запущенного бота читаем из образа, а не через git describe на VDS.
# Расхождение репо (локальный хотфикс,
# правка compose руками) раньше убивало весь скрипт через set -e ДО всякой
# логики деплоя — и каждый следующий тик умирал там же молча. Теперь: раз в
# сутки владельцам уходит сигнал, тик завершается с ошибкой в лог.
if ! git fetch --quiet --no-tags origin main; then
    echo "!!! $(date -Is) git fetch failed; deploy skipped"
    notify_once "fetch" "⚠️ Проверка обновления отложена: не удалось получить скрипты и Compose из Git. Контейнер не изменялся. Повторная попытка — автоматически через минуту; предупреждение не чаще раза в сутки. Подробности в журнале автодеплоя." || true
    exit 1
fi
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
    notify_once "pull" "⚠️ Обновление отложено: не удалось скачать образ. Контейнер не изменялся. Повторная попытка — автоматически через минуту; предупреждение не чаще раза в сутки. Подробности в журнале автодеплоя." || true
    exit 1
fi
after=$(docker image inspect --format '{{.Id}}' "$IMAGE" 2>/dev/null || true)

# Нечего деплоить (или CI ещё не дособрал образ) — молчим, лог не растёт.
if [ -z "$after" ] || [ "$before" = "$after" ] || running_image_matches "$after"; then
    exit 0
fi

# Проваленный ранее образ (тот же ID) — не деплоим снова, петли нет.
if is_quarantined "$after"; then
    exit 0
fi

BEFORE_VERSION=$(image_version "$before")
AFTER_VERSION=$(image_version "$after")
BEFORE_REVISION=$(image_revision "$before")
AFTER_REVISION=$(image_revision "$after")
CHANGES=$(deployment_changes "$before" "$after" "$BEFORE_VERSION" "$AFTER_VERSION" "$BEFORE_REVISION" "$AFTER_REVISION")

echo "=== $(date -Is) deploying image $(short_id "$after") ==="
SMOKE_FAIL=""
if ! docker compose up -d --no-build --no-deps bot; then
    # Compose мог уже остановить старый контейнер до ошибки создания нового.
    # Поэтому ошибка команды требует той же попытки отката, что и smoke fail.
    SMOKE_FAIL="команда запуска Compose завершилась ошибкой"
else
    smoke_image "$after" || true
fi

if [ -n "$SMOKE_FAIL" ]; then
    # Жёсткий провал — откат на прежний образ + карантин. Prune НЕ делаем:
    # старый образ ещё нужен, а упавший остаётся в registry для диагностики.
    mkdir -p "$(dirname "$QUARANTINE")" || true
    if ! grep -qxF "$after" "$QUARANTINE" 2>/dev/null; then
        echo "$after" >> "$QUARANTINE" 2>/dev/null || true
    fi
    if [ -z "$before" ]; then
        echo "!!! $(date -Is) SMOKE FAIL ($SMOKE_FAIL); no previous image for rollback"
        notify_owners "🚨 Первый деплой провалился ($SMOKE_FAIL), предыдущего образа для отката нет. Нужно вмешательство" || true
        exit 1
    fi
    DEPLOY_FAIL="$SMOKE_FAIL"
    echo "!!! $(date -Is) SMOKE FAIL ($DEPLOY_FAIL); rolling back to $(short_id "$before")"
    ROLLBACK_OK=0
    if docker tag "$before" "$IMAGE" && docker compose up -d --no-build --no-deps bot; then
        if smoke_image "$before"; then
            ROLLBACK_OK=1
        fi
    else
        SMOKE_FAIL="команда отката Compose завершилась ошибкой"
    fi
    if [ "$ROLLBACK_OK" -eq 1 ]; then
        echo "!!! $(date -Is) rollback done; image $(short_id "$after") quarantined"
        notify_owners "⚠️ Обновление $BEFORE_VERSION → $AFTER_VERSION отменено.
Причина: $DEPLOY_FAIL.
Восстановлена версия: $BEFORE_VERSION; прежний образ работает, heartbeat свежий, перезапусков нет.
Отклонённый образ $(short_id "$after") в карантине и повторно не устанавливается." || true
    else
        echo "!!! $(date -Is) ROLLBACK FAILED ($SMOKE_FAIL) — manual intervention required!"
        notify_owners "🚨 Обновление до $AFTER_VERSION не прошло: $DEPLOY_FAIL.
Откат на $BEFORE_VERSION НЕ подтверждён: $SMOKE_FAIL.
Нужно вмешательство; работоспособность бота не подтверждена." || true
    fi
    exit 1
fi

# getMe — мягкий сигнал: сетевой сбой не откатывает проверенный контейнер.
TOKEN="$(parse_env BOT_TOKEN || true)"
GETME_OK=0
if [ -n "$TOKEN" ]; then
    if printf 'url = "https://api.telegram.org/bot%s/getMe"\n' "$TOKEN" |
        curl -fs --max-time 5 -K - | grep -q '"ok"[[:space:]]*:[[:space:]]*true'; then
        GETME_OK=1
    fi
fi
if [ "$GETME_OK" -eq 1 ]; then
    # Без --force и только прежний образ бота; чужие образы не затрагиваем.
    if [ -n "$before" ]; then
        docker image rm "$before" >/dev/null 2>&1 || true
    fi
    echo "=== $(date -Is) deploy done ($(short_id "$after")) ==="
else
    echo "!!! $(date -Is) getMe failed but container is running; keeping $(short_id "$after")"
fi
notify_owners "$(deployment_notice "$BEFORE_VERSION" "$AFTER_VERSION" "$after" "$CHANGES" "$GETME_OK")" || true
exit 0
