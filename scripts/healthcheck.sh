#!/bin/sh
set -eu

heartbeat="$(dirname "${DB_PATH:-/data/bot.db}")/.heartbeat"
test -f "$heartbeat"
test -n "$(find "$heartbeat" -mmin -5 -print -quit 2>/dev/null)"
