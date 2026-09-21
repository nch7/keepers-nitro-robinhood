#!/usr/bin/env bash
set -euo pipefail
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$ROOT"
git submodule update --init --recursive
if [[ -n $(git status --porcelain --untracked-files=no) ]]; then
  echo 'Refusing a deployment build with uncommitted source/submodule changes.' >&2
  exit 1
fi
REVISION=$(git rev-parse HEAD)
IMAGE=${KEEPERS_IMAGE:-keepers-nitro-robinhood:$REVISION}
docker build --target nitro-node-slim --build-arg "version=keepers-robinhood-$REVISION" --build-arg "datetime=$(date -u +%FT%TZ)" --build-arg modified=false -t "$IMAGE" .
printf '%s\n' "$IMAGE"
