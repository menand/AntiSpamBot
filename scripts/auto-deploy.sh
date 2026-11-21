#!/bin/bash
# Auto-deploy: pull the CI-built image from GHCR and restart when it changed.
# Сборка происходит на GitHub Actions (ci.yml, job image) ПОСЛЕ зелёных
# тестов — ВДС только забирает готовый образ, локальных билдов больше нет.
# Этот установленный скрипт только выбирает выпуск. Управляющий код и
# Compose берутся из ревизии уже опубликованного образа.
#
# Smoke + карантин: после деплоя проверяем, что контейнер жив (Running,
# RestartCount стабилен, heartbeat свежий). При жёстком провале возвращаем
# предыдущий комплект и помещаем ID образа в карантин (QUARANTINE). Петли нет: пока упавший образ
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
RELEASE_STATE="/var/lib/antispam/releases"
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

container_field() {
    docker inspect --format "$1" menand-antispam 2>/dev/null || true
}

image_ref() {
    local ref
    while IFS= read -r ref; do
        if [[ $ref =~ ^ghcr[.]io/menand/antispambot@sha256:[0-9a-f]{64}$ ]]; then
            printf '%s' "$ref"
            return
        fi
    done < <(docker image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "$1" 2>/dev/null)
    return 1
}

check_revision() {
    [[ $1 =~ ^[0-9a-f]{40}$ ]] && git merge-base --is-ancestor "$1" origin/main
}

# Paths to .env and the project directory are always those of the original
# checkout. The override pins the image and, on an existing install, /data.
run_bundle() {
    local bundle="$1" override="$1/pinned.yml"
    if [ "$first_install" -eq 1 ]; then
        override="$bundle/launch.yml"
        if [ "$resume_first" -eq 1 ] && { [ -f "$bundle/instance" ] || [ -n "$(container_field '{{.Image}}')" ]; }; then
            remember_first_instance "$bundle"
            return
        fi
    fi
    docker compose --project-name "$project" --project-directory "$REPO_DIR" \
        --env-file "$ENV_FILE" -f "$bundle/docker-compose.yml" -f "$override" \
        up -d --no-build --no-deps --pull never bot || return 1
    if [ "$first_install" -eq 1 ]; then
        remember_first_instance "$bundle" || return 1
    fi
}

remember_first_instance() {
    local bundle="$1" observed marker mounted actual_project
    observed=$(container_field '{{.Id}} {{.State.StartedAt}}')
    marker=$(container_field '{{with index .Config.Labels "io.menand.antispam.first-release"}}{{.}}{{end}}')
    mounted=$(container_field '{{range .Mounts}}{{if and (eq .Destination "/data") (eq .Type "volume")}}{{.Name}}{{end}}{{end}}')
    actual_project=$(container_field '{{with index .Config.Labels "com.docker.compose.project"}}{{.}}{{end}}')
    if [[ ! $observed =~ ^[a-zA-Z0-9]+\ [0-9]{4}- ]] || [ "$marker" != "${bundle##*/}" ] ||
       [ "$mounted" != "$volume" ] || [ "$actual_project" != "$project" ] || ! running_image_matches "$after"; then
        SMOKE_FAIL="не совпали метка, проект, образ или том первой установки"
        return 1
    fi
    if [ -f "$bundle/instance" ] && [ "$(cat "$bundle/instance")" != "$observed" ]; then
        SMOKE_FAIL="экземпляр первой установки изменился"
        return 1
    fi
    first_instance="$observed"
    printf '%s\n' "$first_instance" > "$bundle/instance.tmp" && mv -f "$bundle/instance.tmp" "$bundle/instance"
}

snapshot_bundle() {
    local dir="$1" revision="$2" ref="$3" volume="$4"
    git show "$revision:docker-compose.yml" > "$dir/docker-compose.yml" || return 1
    printf '%s\n' "$ref" > "$dir/image"
    printf '%s\n' "$revision" > "$dir/revision"
    printf '%s\n' "$project" > "$dir/project"
    printf '%s\n' "$volume" > "$dir/volume"
    {
        printf 'services:\n  bot:\n    image: "%s"\n' "$ref"
        if [ -n "$volume" ]; then
            printf '    volumes:\n      - bot-data:/data\nvolumes:\n  bot-data:\n    external: true\n    name: "%s"\n' "$volume"
        fi
    } > "$dir/pinned.yml"
}

write_bundle_pointer() {
    local dir="$1" name="$2" pointer
    pointer=$(mktemp "$RELEASE_STATE/$name.XXXXXX") || return 1
    printf '%s\n' "${dir##*/}" > "$pointer" || return 1
    mv -f "$pointer" "$RELEASE_STATE/$name"
}

save_confirmed_bundle() {
    local dir="$1" old_ref rotate=1
    if [ "$dir" = "${candidate:-}" ] && [ -n "$previous" ]; then
        if ! old_ref=$(cat "$previous/image") ||
           { [ "$old_ref" != "$candidate_ref" ] && ! write_bundle_pointer "$previous" previous; }; then
            echo "!!! release cleanup postponed: could not record previous release"
            rotate=0
        fi
    fi
    write_bundle_pointer "$dir" current || return 1
    if [ "$first_install" -eq 1 ]; then
        rm -f "$RELEASE_STATE/pending" || echo "!!! release cleanup: could not clear pending pointer"
    fi
    if [ "$dir" = "${candidate:-}" ] && [ "$rotate" -eq 1 ]; then
        rotate_release_bundles "$dir" || echo "!!! release cleanup postponed"
    fi
}

is_release_bundle() {
    [[ ${1##*/} =~ ^bundle[.][A-Za-z0-9]+$ ]] && [ -d "$1" ] && [ ! -L "$1" ]
}

rotate_release_bundles() {
    local current="$1" pointer selected bundle kept ref other other_ref containers images shared
    local retained=("$current")
    [ -n "$project" ] && [ -n "$volume" ] || return 1
    for pointer in previous pending; do
        [ -f "$RELEASE_STATE/$pointer" ] || continue
        selected=$(cat "$RELEASE_STATE/$pointer") || return 1
        [[ $selected =~ ^bundle[.][A-Za-z0-9]+$ ]] && is_release_bundle "$RELEASE_STATE/$selected" || return 1
        retained+=("$RELEASE_STATE/$selected")
    done
    images=$(docker image ls --digests --format '{{.Repository}}@{{.Digest}}' "${IMAGE%:*}") || return 1
    for bundle in "$RELEASE_STATE"/bundle.*; do
        is_release_bundle "$bundle" || continue
        for kept in "${retained[@]}"; do
            [ "$bundle" != "$kept" ] || continue 2
        done
        [ "$(cat "$bundle/project" 2>/dev/null)" = "$project" ] &&
            [ "$(cat "$bundle/volume" 2>/dev/null)" = "$volume" ] || continue
        ref=$(cat "$bundle/image" 2>/dev/null) || continue
        [[ $ref =~ ^ghcr[.]io/menand/antispambot@sha256:[0-9a-f]{64}$ ]] || continue

        shared=0
        for other in "$RELEASE_STATE"/bundle.*; do
            [ "$other" != "$bundle" ] && is_release_bundle "$other" || continue
            [ -f "$other/image" ] || continue
            read -r other_ref < "$other/image" || return 1
            if [ "$other_ref" = "$ref" ]; then
                shared=1
                break
            fi
        done
        if [ "$shared" -eq 0 ] && { grep -qxF "$ref" <<< "$images" || docker image inspect "$ref" >/dev/null 2>&1; }; then
            if ! containers=$(docker ps -aq --filter "ancestor=$ref"); then
                echo "!!! release cleanup: could not check containers for $ref"
                continue
            fi
            [ -z "$containers" ] || continue
            if ! docker image rm "$ref"; then
                echo "!!! release cleanup: could not remove $ref"
                continue
            fi
        fi
        rm -rf -- "$bundle" || echo "!!! release cleanup: could not remove ${bundle##*/}"
    done
}

first_install=0
resume_first=0
first_instance=""
rejected_first=0
if [ -f "$RELEASE_STATE/pending" ] && [ ! -f "$RELEASE_STATE/current" ]; then
    selected=$(cat "$RELEASE_STATE/pending")
    [[ $selected =~ ^bundle[.][A-Za-z0-9]+$ ]] || exit 1
    candidate="$RELEASE_STATE/$selected"
    candidate_ref=$(cat "$candidate/image")
    revision=$(cat "$candidate/revision")
    project=$(cat "$candidate/project")
    volume=$(cat "$candidate/volume")
    [[ $candidate_ref =~ ^ghcr[.]io/menand/antispambot@sha256:[0-9a-f]{64}$ && $revision =~ ^[0-9a-f]{40}$ &&
       $project =~ ^[a-z0-9][a-z0-9_-]*$ && $volume = "${project}_bot-data" ]] || exit 1
    after=$(docker image inspect --format '{{.Id}}' "$candidate_ref" 2>/dev/null || true)
    if ! is_quarantined "$candidate_ref" && ! is_quarantined "$after"; then
        [ -n "$after" ] && [ -f "$candidate/launch.yml" ] && [ -f "$candidate/docker-compose.yml" ] || exit 1
        before=""
        previous=""
        first_install=1
        resume_first=1
        # shellcheck source=/dev/null
        source "$candidate/deploy-release.sh"
        exit
    fi
    rejected_first=1
fi

# Fetch updates the remote-tracking branch only. An interrupted first install
# is checked against its saved digest before considering a newer tag.
if ! git fetch --quiet --no-tags origin +refs/heads/main:refs/remotes/origin/main; then
    echo "!!! $(date -Is) git fetch failed; deploy skipped"
    notify_once "fetch" "⚠️ Проверка обновления отложена: Git недоступен. Контейнер не изменялся. Повторная попытка — автоматически через минуту." || true
    exit 1
fi

before=$(container_field '{{.Image}}')
before_revision=$(container_field '{{with index .Config.Labels "org.opencontainers.image.revision"}}{{.}}{{end}}')
if [ "$rejected_first" -eq 1 ]; then
    before=""
fi
before_ref=""
if [ -n "$before" ]; then
    before_ref=$(image_ref "$before" || true)
    if [ -z "$before_ref" ]; then
        # containerd может хранить config ID отдельно от локального индекса.
        manifest=$(container_field '{{with index . "ImageManifestDescriptor"}}{{index . "digest"}}{{end}}')
        if [[ $manifest =~ ^sha256:[0-9a-f]{64}$ ]]; then
            before_ref="${IMAGE%:*}@$manifest"
        fi
    fi
fi
if ! pull_out=$(docker pull "$IMAGE" 2>&1); then
    echo "!!! $(date -Is) docker pull failed: ${pull_out:0:300}"
    notify_once "pull" "⚠️ Обновление отложено: не удалось скачать образ. Контейнер не изменялся. Повторная попытка — автоматически через минуту; предупреждение не чаще раза в сутки." || true
    exit 1
fi
candidate_ref=$(image_ref "$IMAGE") || { echo "!!! immutable image reference unavailable"; exit 1; }
after=$(docker image inspect --format '{{.Id}}' "$candidate_ref" 2>/dev/null || true)
[ -n "$after" ] || exit 1

previous=""
if [ -f "$RELEASE_STATE/current" ]; then
    selected=$(cat "$RELEASE_STATE/current")
    [[ $selected =~ ^bundle[.][A-Za-z0-9]+$ ]] || exit 1
    previous="$RELEASE_STATE/$selected"
    [ -f "$previous/docker-compose.yml" ] && [ -f "$previous/pinned.yml" ] || exit 1
    if [ -f "$RELEASE_STATE/pending" ] && [ "$(cat "$RELEASE_STATE/pending")" = "$selected" ]; then
        rm -f "$RELEASE_STATE/pending"
    fi
fi
if running_image_matches "$after" && { [ -z "$previous" ] || [ "$(cat "$previous/image")" = "$candidate_ref" ]; }; then
    exit 0
fi
if is_quarantined "$after" || is_quarantined "$candidate_ref"; then
    exit 0
fi

revision=$(image_revision "$candidate_ref")
if ! check_revision "$revision"; then
    echo "!!! candidate revision is missing from origin/main; deploy skipped"
    exit 1
fi
if [ -n "$previous" ]; then
    project=$(cat "$previous/project")
    volume=$(cat "$previous/volume")
    before_ref=$(cat "$previous/image")
    before=$(docker image inspect --format '{{.Id}}' "$before_ref" 2>/dev/null || true)
    [ -n "$before" ] || { echo "!!! previous image unavailable; deploy skipped"; exit 1; }
elif [ -n "$before" ]; then
    project=$(container_field '{{with index .Config.Labels "com.docker.compose.project"}}{{.}}{{end}}')
    volume=$(container_field '{{range .Mounts}}{{if and (eq .Destination "/data") (eq .Type "volume")}}{{.Name}}{{end}}{{end}}')
    if [ -z "$project" ] || [ -z "$volume" ] || [ -z "$before_ref" ] ||
       ! check_revision "$before_revision" ||
       ! docker image inspect "$before_ref" >/dev/null 2>&1; then
        echo "!!! running release cannot be saved for rollback; deploy skipped"
        exit 1
    fi
elif [ "$rejected_first" -eq 0 ]; then
    project=${COMPOSE_PROJECT_NAME:-$(parse_env COMPOSE_PROJECT_NAME || true)}
    if [ -z "$project" ]; then
        project=$(printf '%s' "${REPO_DIR##*/}" | tr '[:upper:]' '[:lower:]' | sed -E 's/[^a-z0-9_-]//g;s/^[^a-z0-9]+//')
    fi
    volume="${project}_bot-data"
fi
[[ $project =~ ^[a-z0-9][a-z0-9_-]*$ && ( -z "$volume" || $volume =~ ^[A-Za-z0-9][A-Za-z0-9_.-]*$ ) ]] || exit 1

mkdir -p "$RELEASE_STATE"
if [ -z "$previous" ] && [ -n "$before" ]; then
    previous=$(mktemp -d "$RELEASE_STATE/bundle.XXXXXX")
    snapshot_bundle "$previous" "$before_revision" "$before_ref" "$volume"
    save_confirmed_bundle "$previous"
fi
candidate=$(mktemp -d "$RELEASE_STATE/bundle.XXXXXX")
snapshot_bundle "$candidate" "$revision" "$candidate_ref" "$volume"
for file in deploy-release.sh deploy-notify.sh; do
    git show "$revision:scripts/$file" > "$candidate/$file" || exit 1
done
if [ -z "$previous" ]; then
    first_install=1
    # The first launch creates its volume. Rollback uses the external pin.
    {
        printf 'services:\n  bot:\n    image: "%s"\n' "$candidate_ref"
        printf '    labels:\n      io.menand.antispam.first-release: "%s"\n' "${candidate##*/}"
        printf 'volumes:\n  bot-data:\n    name: "%s"\n' "$volume"
    } > "$candidate/launch.yml"
    write_bundle_pointer "$candidate" pending
fi
# shellcheck source=/dev/null
source "$candidate/deploy-release.sh"
