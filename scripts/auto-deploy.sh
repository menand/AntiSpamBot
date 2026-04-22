#!/bin/bash
# Auto-deploy: if origin/main has new commits, pull and rebuild the container.
# Safe to run on a cron tick — uses a file lock so a slow build doesn't overlap
# with the next tick. Silent when there's nothing to do.
#
# Usage (cron example, every 5 minutes):
#   */5 * * * * /root/AntiSpamBot/scripts/auto-deploy.sh >> /var/log/antispam-deploy.log 2>&1

set -euo pipefail

# Cron's PATH is minimal; make sure docker + git are findable.
export PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

# Resolve the repo root as the parent of this script, regardless of cwd.
REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_DIR"

# Prevent overlapping runs if a previous build is still going.
exec 9>"/tmp/antispam-deploy.lock"
if ! flock -n 9; then
    exit 0
fi

git fetch --quiet origin main

LOCAL=$(git rev-parse HEAD)
REMOTE=$(git rev-parse origin/main)

if [ "$LOCAL" = "$REMOTE" ]; then
    # Nothing new — stay silent so the log file doesn't grow.
    exit 0
fi

echo "=== $(date -Is) deploying ${LOCAL:0:7} -> ${REMOTE:0:7} ==="
git pull --ff-only origin main
docker compose up -d --build
echo "=== $(date -Is) deploy done ==="
