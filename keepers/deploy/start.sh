#!/usr/bin/env bash
set -euo pipefail
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
: "${KEEPERS_IMAGE:?Set KEEPERS_IMAGE to a tested, revision-tagged image}"
: "${KEEPERS_DATA_DIR:?Set KEEPERS_DATA_DIR to an existing writable host datadir}"
: "${KEEPERS_NODE_CONFIG:?Set KEEPERS_NODE_CONFIG to a private server-local Nitro JSON config}"
[[ -d "$KEEPERS_DATA_DIR" && -f "$KEEPERS_NODE_CONFIG" ]]
cd "$ROOT"
shasum -a 256 -c keepers/config/robinhood/SHA256SUMS
# Credentials are supplied through the mounted JSON, never command-line arguments.
ARGS=(--conf.file=/run/keepers/node.json
 --chain.id=4663
 --chain.info-files=/home/user/config/chain-info.json
 --node.feed.input.url=wss://feed.mainnet.chain.robinhood.com
 --node.feed.input.enable-compression=true
 --node.feed.input.application-timeout=20s
 --execution.forwarding-target=null
 --execution.keepers.enabled=true
 --http.addr=0.0.0.0 --http.port=8547 --http.api=net,web3,eth,keepers,flashsimv2
 --ws.addr=0.0.0.0 --ws.port=8548 --ws.api=net,web3,eth,keepers,flashsimv2)
if [[ -n ${KEEPERS_INIT_SNAPSHOT:-} ]]; then
 ARGS+=("--init.url=$KEEPERS_INIT_SNAPSHOT")
else
 ARGS+=(--init.genesis-json-file=/home/user/config/genesis.json)
fi
exec docker run --rm --name keepers-nitro-robinhood --stop-timeout 300 \
 --user "$(id -u):$(id -g)" \
 -v "$KEEPERS_DATA_DIR:/home/user/.arbitrum" \
 -v "$ROOT/keepers/config/robinhood:/home/user/config:ro" \
 -v "$KEEPERS_NODE_CONFIG:/run/keepers/node.json:ro" \
 -p "${KEEPERS_BIND_ADDRESS:-127.0.0.1}:8547:8547" \
 -p "${KEEPERS_BIND_ADDRESS:-127.0.0.1}:8548:8548" \
 "$KEEPERS_IMAGE" "${ARGS[@]}"
