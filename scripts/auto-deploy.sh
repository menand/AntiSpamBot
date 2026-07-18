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
# Safe to run on a cron tick — file lock, silent when there's nothing to do.
# Usage (cron example, every 5 minutes):
#   */5 * * * * /root/AntiSpamBot/scripts/auto-deploy.sh >> /var/log/antispam-deploy.log 2>&1

set -euo pipefail

# Cron's PATH is minimal; make sure docker + git are findable.
export PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

# Resolve the repo root as the parent of this script, regardless of cwd.
REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_DIR"

# Prevent overlapping runs.
exec 9>"/tmp/antispam-deploy.lock"
if ! flock -n 9; then
    exit 0
fi

IMAGE="ghcr.io/menand/antispambot:latest"

# Тихо подтягиваем репо (скрипт/compose); --tags — чтобы git describe на
# сервере совпадал с релизными тегами.
git fetch --quiet --tags origin main
if [ "$(git rev-parse HEAD)" != "$(git rev-parse origin/main)" ]; then
    git merge --ff-only --quiet origin/main
fi

before=$(docker inspect --format '{{.Image}}' menand-antispam 2>/dev/null || true)
docker compose pull -q bot
after=$(docker image inspect --format '{{.Id}}' "$IMAGE" 2>/dev/null || true)

# Нечего деплоить (или CI ещё не дособрал образ) — молчим, лог не растёт.
if [ -z "$after" ] || [ "$before" = "$after" ]; then
    exit 0
fi

echo "=== $(date -Is) deploying image ${after:7:12} ==="
docker compose up -d --no-build
# Вытесненные старые образы (~35MB каждый) — чистим, чтобы не копились.
docker image prune -f >/dev/null 2>&1 || true
echo "=== $(date -Is) deploy done ==="
