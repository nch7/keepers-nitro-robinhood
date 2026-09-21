#!/usr/bin/env bash
set -euo pipefail
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$ROOT"
read -r EXPECTED < keepers/config/robinhood/genesis-block-hash.txt
RESULT=$(go run ./cmd/genesis-generator --genesis-json-file=keepers/config/robinhood/genesis.json)
if [[ "$RESULT" != *"BlockHash: $EXPECTED,"* ]]; then
  printf 'Genesis mismatch: %s\n' "$RESULT" >&2
  exit 1
fi
printf 'Robinhood genesis verified: %s\n' "$EXPECTED"
