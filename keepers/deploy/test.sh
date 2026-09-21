#!/usr/bin/env bash
set -euo pipefail
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$ROOT"
# The upstream node-builder image supplies Solidity artifacts and native Stylus
# libraries. Build once; rebuilding uses Docker's dependency cache.
docker build --target node-builder -t keepers-nitro-robinhood-tests .
docker run --rm --entrypoint bash keepers-nitro-robinhood-tests -lc \
 'keepers/deploy/verify-genesis.sh && go test ./execution/gethexec -run Keepers -count=1 && go test ./system_tests -run TestKeepers -count=1 && go test ./broadcastclient -count=1 && cd go-ethereum && go test ./rpc -count=1 && go test ./arbitrum -run TestKeepersRawTransactionConversion -count=1 && go test ./core -run TestKeepersBaseFeeRelaxationIsOptIn -count=1'
