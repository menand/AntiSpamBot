#!/bin/bash
set -euo pipefail

# CI держит очередь продвижения, пока этот скрипт сверяет исходники и базы
# кандидата и назначает production-теги его неизменяемому digest.
[[ ${GITHUB_REF:-} == refs/heads/main ]] || exit 0
case ${GITHUB_EVENT_NAME:-} in
    push) ;;
    workflow_dispatch) [[ ${REBUILD_IMAGE:-} == true ]] || exit 0 ;;
    *) exit 0 ;;
esac

sha=${GITHUB_SHA:?missing commit}
digest=${CANDIDATE_DIGEST:?missing digest}
version=${CANDIDATE_VERSION:?missing version}
[[ $sha =~ ^[0-9a-f]{40}$ && $digest =~ ^sha256:[0-9a-f]{64}$ &&
   $version =~ ^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$ && $version != latest && $version != build-* ]]
[[ $(git rev-parse HEAD) == "$sha" ]]

bases=$(bash scripts/base-digests.sh)
golang=$(awk -F= '$1=="golang"{print $2}' <<< "$bases")
alpine=$(awk -F= '$1=="alpine"{print $2}' <<< "$bases")
if [[ $golang != "${CANDIDATE_GOLANG:?missing golang digest}" ||
      $alpine != "${CANDIDATE_ALPINE:?missing alpine digest}" ]]; then
    echo "Base images moved while candidate was building; promotion skipped."
    exit 0
fi

# Сверяем удалённую ветку непосредственно перед сменой тегов.
main=$(git ls-remote --exit-code origin refs/heads/main)
if [[ ${main%%$'\t'*} != "$sha" ]]; then
    echo "Candidate is no longer current main; promotion skipped."
    exit 0
fi
image=ghcr.io/menand/antispambot
docker buildx imagetools create --prefer-index=false \
    --tag "$image:$version" --tag "$image:latest" "$image@$digest"
