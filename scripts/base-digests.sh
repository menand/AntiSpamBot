#!/bin/bash
set -euo pipefail

# Digest именно запускаемого linux/amd64 manifest, а не общего OCI-индекса.
# Публикуемый бот и GitHub Actions image job зафиксированы на этой платформе.
platform_digest() {
    docker buildx imagetools inspect --raw "$1" | jq -er '
        [.manifests[]? | select(.platform.os == "linux" and .platform.architecture == "amd64") | .digest]
        | if length == 1 and (.[0] | test("^sha256:[0-9a-f]{64}$")) then .[0]
          else error("expected exactly one linux/amd64 image manifest") end
    '
}

golang=$(awk -F= '/^ARG GOLANG_IMAGE=/{print $2; exit}' Dockerfile)
alpine=$(awk -F= '/^ARG ALPINE_IMAGE=/{print $2; exit}' Dockerfile)
[ -n "$golang" ] && [ -n "$alpine" ]
golang_digest=$(platform_digest "$golang")
alpine_digest=$(platform_digest "$alpine")
printf 'golang=%s\nalpine=%s\ngolang_image=%s@%s\nalpine_image=%s@%s\n' \
    "$golang_digest" "$alpine_digest" "$golang" "$golang_digest" "$alpine" "$alpine_digest"
