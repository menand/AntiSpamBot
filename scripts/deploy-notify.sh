#!/bin/bash
# Helpers sourced by auto-deploy.sh. Read metadata from images, never from HEAD:
# the checkout can already contain a release whose image is still building.

image_label() {
    [ -n "$1" ] || return 0
    docker image inspect --format "{{with index .Config.Labels \"$2\"}}{{.}}{{end}}" "$1" 2>/dev/null || true
}

image_version() {
    local version
    version=$(image_label "$1" org.opencontainers.image.version)
    if [[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9]+-g[0-9a-f]+)?$ ]]; then
        printf '%s' "$version"
    else
        printf '%s' 'неизвестна'
    fi
}

image_revision() {
    local revision
    revision=$(image_label "$1" org.opencontainers.image.revision)
    if [[ "$revision" =~ ^[0-9a-f]{40}$ ]]; then
        printf '%s' "$revision"
    fi
    return 0
}

# Читаем версию из файловой системы именно установленного образа. Отдельный
# одноразовый контейнер не получает сеть и volume с данными бота; --pull=never
# исключает незаметную загрузку другого образа. Таймаут не задерживает деплой.
alpine_version() {
    local version
    [ -n "$1" ] || { printf '%s' 'неизвестна'; return 0; }
    version=$(timeout 10s docker run --rm --pull=never --network=none --read-only \
        --tmpfs /data --entrypoint /bin/cat "$1" /etc/alpine-release 2>/dev/null || true)
    if [[ "$version" =~ ^[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}$ ]]; then
        printf '%s' "$version"
    else
        printf '%s' 'неизвестна'
    fi
}

alpine_details() {
    local old_image="$1" new_image="$2" old_digest new_digest old_version new_version
    old_digest=$(image_label "$old_image" base.digest.alpine)
    new_digest=$(image_label "$new_image" base.digest.alpine)
    [ -n "$new_digest" ] || return 0
    if [ -n "$old_digest" ] && [ "$old_digest" = "$new_digest" ]; then
        return 0
    fi
    old_version=$(alpine_version "$old_image")
    new_version=$(alpine_version "$new_image")
    printf 'Alpine: %s → %s\n' "$old_version" "$new_version"
    if [ "$new_version" = неизвестна ]; then
        printf '%s\n' 'Выпуски Alpine: https://alpinelinux.org/releases/'
    else
        printf 'История выпуска Alpine (все пакеты): https://git.alpinelinux.org/aports/log/?h=v%s\n' "$new_version"
    fi
    if [ "$old_version" = "$new_version" ] && [ "$new_version" != неизвестна ]; then
        printf '%s\n' 'Номер выпуска совпадает; содержимое образа может отличаться.'
    fi
}

# Bound whole UTF-8 lines by bytes, without slicing characters. This is also
# below Telegram's UTF-16 limit, including the rest of the notification.
bounded_notes() {
    LC_ALL=C awk '
        { sub(/\r$/, "") }
        /^- / { $0 = "• " substr($0, 3) }
        NF {
            if (count >= 4 || bytes + length($0) > 1800) { omitted = 1; next }
            print; count++; bytes += length($0) + 1
        }
        END { if (omitted) print "… Полный список — /whatsnew." }
    '
}

deployment_changes() {
    local old_image="$1" new_image="$2" old_version="$3" new_version="$4"
    local old_revision="$5" new_revision="$6" notes="" bases="" old_base new_base kind
    if [ -n "$old_revision" ] && [ "$old_revision" = "$new_revision" ]; then
        for kind in golang alpine; do
            old_base=$(image_label "$old_image" "base.digest.$kind")
            new_base=$(image_label "$new_image" "base.digest.$kind")
            if [ -n "$old_base" ] && [ -n "$new_base" ] && [ "$old_base" != "$new_base" ]; then
                [ -z "$bases" ] || bases="$bases, "
                bases="$bases$kind"
            fi
        done
        if [ -n "$bases" ]; then
            printf 'Что изменилось: обновлены базовые образы (%s). Код той же версии.\n' "$bases"
        else
            printf '%s\n' 'Образ пересобран; ревизия кода не изменилась. Причина пересборки не указана.'
        fi
        return 0
    fi
    if [ -n "$new_revision" ]; then
        if [ "$new_version" != неизвестна ] && [ "${old_version%%-*}" != "${new_version%%-*}" ]; then
            notes=$(git show "${new_revision}:CHANGELOG.md" 2>/dev/null | awk -v version="${new_version%%-*}" '
                /^## / { active = ($2 == version); next }
                active && /^- / { print }
            ' | bounded_notes) || notes=""
            if [ -n "$notes" ]; then
                printf 'Новое в %s:\n%s\n' "${new_version%%-*}" "$notes"
                return 0
            fi
        fi
        if [ -n "$old_revision" ]; then
            notes=$(git log --format='• %s' --max-count=4 "$old_revision..$new_revision" -- 2>/dev/null | bounded_notes) || notes=""
        else
            notes=$(git log -1 --format='• %s' "$new_revision" -- 2>/dev/null | bounded_notes) || notes=""
        fi
        if [ -n "$notes" ]; then
            printf 'Изменения кода:\n%s\n' "$notes"
            return 0
        fi
    fi
    printf '%s\n' 'Описание изменений недоступно: в образе нет метаданных или соответствующей ревизии в локальном Git.'
}

deployment_notice() {
    local old_version="$1" new_version="$2" image="$3" changes="$4" api_ok="$5" alpine_info="${6:-}"
    if [ "$api_ok" -eq 1 ]; then
        printf '%s\n' '✅ Обновление бота завершено'
    else
        printf '%s\n' '⚠️ Образ бота установлен; Telegram API недоступен'
    fi
    printf 'Версия: %s → %s\n\n%s\n\n' "$old_version" "$new_version" "$changes"
    if [ -n "$alpine_info" ]; then
        printf '%s\n\n' "$alpine_info"
    fi
    printf '%s\n' 'Проверка запуска: нужный образ работает, heartbeat свежий, перезапусков нет.'
    if [ "$api_ok" -eq 1 ]; then
        printf '%s\n' 'Telegram API: отвечает.'
    else
        printf '%s\n' 'Telegram API: getMe не ответил. Нужна проверка связи/токена; контейнер оставлен работать.'
    fi
    printf 'Образ: %s\n' "${image:7:12}"
}
