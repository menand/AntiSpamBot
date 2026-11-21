#!/bin/bash
# Worker sourced by the installed bootstrap from the revision of the image.
# Holds the host deploy lock while applying or rolling back the bundle.
source "$candidate/deploy-notify.sh"

# Both the candidate and rollback must pass the check for their own
# instance. Images from before this protocol can only prove a heartbeat
# written strictly after their Docker start time.
check_readiness() {
    docker exec "$1" sh -ec '
        if [ -z "${ANTISPAM_READY_FILE:-}" ]; then
            heartbeat="$(dirname "${DB_PATH:-/data/bot.db}")/.heartbeat"
            test "$(stat -c %Y "$heartbeat")" -gt "$1"
        fi
        exec /usr/local/bin/healthcheck
    ' healthcheck "$2" >/dev/null 2>&1
}

smoke_image() {
    local expected="$1" instance current started_at start_time elapsed
    local instance_format='{{.Id}} {{.State.StartedAt}}'
    SMOKE_FAIL=""
    sleep 10
    instance=$(container_field "$instance_format")
    if [ -n "${first_instance:-}" ] && [ "$instance" != "$first_instance" ]; then
        SMOKE_FAIL="экземпляр первой установки изменился до проверки"
        return 1
    fi
    started_at=${instance#* }
    if [[ ! $started_at =~ ^[0-9]{4}- ]] || ! start_time=$(date -d "$started_at" +%s); then
        SMOKE_FAIL="не удалось определить время запуска контейнера"
        return 1
    fi
    for ((elapsed=10; elapsed<=120; elapsed+=5)); do
        current=$(container_field "$instance_format")
        if [ "$instance" != "$current" ] ||
           [ "$(container_field '{{.State.Status}}')" != running ] ||
           [ "$(container_field '{{.RestartCount}}')" != 0 ]; then
            SMOKE_FAIL="контейнер остановился, был заменён или перезапустился"
            return 1
        fi
        if ! running_image_matches "$expected"; then
            SMOKE_FAIL="запущен другой образ"
            return 1
        fi
        if [ -n "$volume" ] && [ "$(container_field '{{range .Mounts}}{{if and (eq .Destination "/data") (eq .Type "volume")}}{{.Name}}{{end}}{{end}}')" != "$volume" ]; then
            SMOKE_FAIL="подключён другой том данных"
            return 1
        fi
        if [ "$elapsed" -ge 35 ] && check_readiness "${instance%% *}" "$start_time" &&
           [ "$instance" = "$(container_field "$instance_format")" ]; then
            return 0
        fi
        [ "$elapsed" -eq 120 ] || sleep 5
    done
    SMOKE_FAIL="текущий экземпляр не подтвердил готовность за 120 секунд"
    return 1
}

BEFORE_VERSION=$(image_version "$before")
AFTER_VERSION=$(image_version "$after")
BEFORE_REVISION=$(image_revision "$before")
AFTER_REVISION=$(image_revision "$after")
CHANGES=$(deployment_changes "$before" "$after" "$BEFORE_VERSION" "$AFTER_VERSION" "$BEFORE_REVISION" "$AFTER_REVISION")
ALPINE_INFO=$(alpine_details "$before" "$after")

SMOKE_FAIL=""
if [ "${resume_rollback:-0}" -eq 1 ]; then
    SMOKE_FAIL="предыдущая попытка отката не завершилась"
else
    echo "=== $(date -Is) deploying image $(short_id "$after") ==="
    if ! run_bundle "$candidate"; then
        # Compose мог уже остановить старый контейнер до ошибки создания нового.
        SMOKE_FAIL=${SMOKE_FAIL:-"команда запуска Compose завершилась ошибкой"}
    else
        smoke_image "$after" || true
    fi
fi

if [ -n "$SMOKE_FAIL" ]; then
    # Keep the recovery intent across interruptions before and during rollback.
    quarantine=1
    if [ -n "$previous" ] && [ "${resume_rollback:-0}" -eq 0 ] &&
       ! write_bundle_pointer "$candidate" rollback; then
        echo "!!! could not save rollback state; attempting rollback now"
        quarantine=0
    fi
    # Жёсткий провал — откат на прежний образ + карантин. Prune НЕ делаем:
    # старый образ ещё нужен, а упавший остаётся в registry для диагностики.
    mkdir -p "$(dirname "$QUARANTINE")" || true
    for rejected in "$after" "$candidate_ref"; do
        if [ "$quarantine" -eq 1 ] && [ -n "$rejected" ] && ! is_quarantined "$rejected"; then
            echo "$rejected" >> "$QUARANTINE" 2>/dev/null || true
        fi
    done
    if [ -z "$previous" ]; then
        echo "!!! $(date -Is) SMOKE FAIL ($SMOKE_FAIL); no previous image for rollback"
        notify_owners "🚨 Первый деплой провалился ($SMOKE_FAIL), предыдущего образа для отката нет. Нужно вмешательство" || true
        exit 1
    fi
    DEPLOY_FAIL="$SMOKE_FAIL"
    echo "!!! $(date -Is) SMOKE FAIL ($DEPLOY_FAIL); rolling back to $(short_id "$before")"
    ROLLBACK_OK=0
    rollback_recreate=""
    if [ "${resume_rollback:-0}" -eq 1 ] && running_image_matches "$before" &&
       [ "$(container_field '{{.RestartCount}}')" != 0 ]; then
        rollback_recreate=--force-recreate
    fi
    if run_bundle "$previous" ${rollback_recreate:+"$rollback_recreate"}; then
        if smoke_image "$before"; then
            ROLLBACK_OK=1
        fi
    else
        SMOKE_FAIL="команда отката Compose завершилась ошибкой"
    fi
    if [ "$ROLLBACK_OK" -eq 1 ]; then
        rm -f "$RELEASE_STATE/rollback"
        echo "!!! $(date -Is) rollback done; image $(short_id "$after") quarantined"
        notify_owners "⚠️ Обновление $BEFORE_VERSION → $AFTER_VERSION отменено.
Причина: $DEPLOY_FAIL.
Восстановлена версия: $BEFORE_VERSION; прежний образ работает, heartbeat текущего запуска подтверждён.
Отклонённый образ $(short_id "$after") в карантине и повторно не устанавливается." || true
    else
        echo "!!! $(date -Is) ROLLBACK FAILED ($SMOKE_FAIL) — retry pending"
        notify_once rollback "🚨 Обновление до $AFTER_VERSION не прошло: $DEPLOY_FAIL.
Откат на $BEFORE_VERSION НЕ подтверждён: $SMOKE_FAIL.
Бот пока не подтвердил запуск. Автодеплой повторит откат на следующем запуске." || true
    fi
    if [ "${resume_rollback:-0}" -eq 1 ] && [ "$ROLLBACK_OK" -eq 1 ]; then
        exit 0
    fi
    exit 1
fi

if [ -z "$volume" ]; then
    # Older installed bootstraps pass no volume on a first install.
    volume=$(container_field '{{range .Mounts}}{{if and (eq .Destination "/data") (eq .Type "volume")}}{{.Name}}{{end}}{{end}}')
    [[ $volume =~ ^[A-Za-z0-9][A-Za-z0-9_.-]*$ ]] || { echo "!!! first release has no data volume"; exit 1; }
    snapshot_bundle "$candidate" "$revision" "$candidate_ref" "$volume"
fi
save_confirmed_bundle "$candidate"

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
    echo "=== $(date -Is) deploy done ($(short_id "$after")) ==="
else
    echo "!!! $(date -Is) getMe failed but container is running; keeping $(short_id "$after")"
fi
notify_owners "$(deployment_notice "$BEFORE_VERSION" "$AFTER_VERSION" "$after" "$CHANGES" "$GETME_OK" "$ALPINE_INFO")" || true
exit 0
