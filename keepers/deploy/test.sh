#!/usr/bin/env bash
set -euo pipefail
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$ROOT"
TEST_DOCKERFILE=$(mktemp)
trap 'rm -f "$TEST_DOCKERFILE"' EXIT
cat Dockerfile keepers/deploy/Dockerfile.tests > "$TEST_DOCKERFILE"
# Keep the build cache, avoiding export of the very large development image.
docker build --file "$TEST_DOCKERFILE" --target keepers-tests --output type=cacheonly .
