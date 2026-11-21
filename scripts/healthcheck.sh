#!/bin/sh
set -eu

heartbeat="${ANTISPAM_READY_FILE:?}"
test -f "$heartbeat"
stat=$(cat /proc/1/stat)
set -- ${stat##*) }
test "$#" -ge 20
boot=$(cat /proc/sys/kernel/random/boot_id)
test "$(cat "$heartbeat")" = "$boot:${20}"
test -n "$(find "$heartbeat" -mmin -5 -print -quit 2>/dev/null)"
