# Keepers Robinhood RPC contract

The client is Nitro, with Robinhood chain ID 4663. Enable the service with
`--execution.keepers.enabled=true` and expose `eth,keepers,flashsimv2` on the
selected HTTP/WS transports. The service is disabled by default upstream-style;
the supplied Robinhood launcher enables it.

## Simulations

- `eth_simulateTransactionAt(blockId, transactionIndex, transaction)` replays the
  block prefix `[0, transactionIndex)` from parent state. The inclusive upper
  bound is the number of transactions, including ArbOS internal transactions.
  Canonical block hashes/numbers/tags and the prepared `pending` snapshot are
  supported. If no prepared snapshot exists, `pending` resolves to local latest.
- `keepers_simulateAtLatestState(sims)` executes a sequential bundle on one
  private state overlay. Later items see earlier items' effects. Separate
  requests never share mutable state.
- `flashsimv2_simulateV1(payloadId, index, blockNumber, sims)` additionally requires
  the current ready tuple and checks readiness, head identity, and internal
  revision again after execution.

Each item is an unsigned Ethereum request or `{ "rawTx": "0x..." }`. Raw and
unsigned fields cannot be mixed. Supported signed user envelopes are legacy,
access-list, dynamic-fee, and EIP-7702; native internal transactions are only
replayed from real blocks. Blob candidates and disabling ArbOS L1 charging are
not supported. Chain-specific OP deposit envelopes are not Robinhood inputs.

Results contain `status` (`success`, `revert`, `halt`), hex `gasUsed`, `logs`, hex
`returnData`, and optional `error`. Reverts retain their return payload. Bundle
validation relaxes nonce, sender-code, and base-fee checks; insertion candidates
retain those checks. Prefix replay always uses native validation. Block-wide gas
pool exhaustion is disabled for candidates without changing the block context.
Gas defaults follow the OP implementation: insertion uses remaining block gas
bounded by the RPC gas cap; bundle missing-gas items divide unassigned block gas
once explicit gas is accounted for.

Execution uses Geth's native message transition and Nitro's installed ArbOS
processing hooks. Candidate execution uses eth-call mode for isolated Stylus
execution. No candidate state is committed to the chain database.

Historical replay defaults to at most 128 predecessor blocks of reconstruction,
subject to Nitro's state scheme and available history. This is a reconstruction
limit, not a promise that 128 blocks of state always exist. Missing historical
state returns an explicit error. Genesis insertion is rejected because it has no
replayable parent. There is no removed latest-flashblock compatibility alias.

## Snapshots and subscriptions

Standard Robinhood feed messages produce complete blocks. Each locally executed
live block has one snapshot, with `index: 0`, actual L2 `blockNumber`, an opaque
8-byte hex `payloadId`, and the actual block hash as `flashblockHash`. IDs are
allocated from durably reserved monotonic ranges; gaps are expected after restart.
Keep `keepers-payload-counter` with the datadir and never roll it back independently.
A snapshot is not an Ethereum finality claim.

`generation` retains `(blockNumber << 32) | index`. It is not a unique reorg or
restart identifier; clients must use the entire tuple.

Direct WS methods and notifications retain the OP wire contract:

| Subscribe | Notification | Unsubscribe |
| --- | --- | --- |
| `flashsimv2_subscribeFilteredLogsV1({topic0s, addresses?})` | `flashsimv2_filteredLogsV1` | `flashsimv2_unsubscribeFilteredLogsV1(id)` |
| `flashsimv2_subscribeTxInclusionsV1()` | `flashsimv2_txInclusionsV1` | `flashsimv2_unsubscribeTxInclusionsV1(id)` |

Filtered logs require nonempty `topic0s`, match any supplied topic0 and optional
address, and group matching logs in transaction order. Empty matching updates
are omitted. Inclusion events contain every block transaction hash in order.
Filtered-log streams emit OP-shaped `reverted` metadata before replacements.
Inclusion streams retain OP's event shape without a separate reversal event.

Subscriptions close their WS connection on continuity loss or queue overflow;
clients must reconnect and rebuild from current chain state. This also closes
other subscriptions sharing that connection. Unlike the current OP implementation,
lagged events are not silently skipped. There is no historical catch-up stream.

| Code | Meaning |
| --- | --- |
| -39100 | stale payload |
| -39101 | stale index (reserved parity code; snapshots use index zero) |
| -39102 | not ready / ahead of latest |
| -39103 | no longer latest after simulation |
| -39104 | stale block |
| -39105 | invalid filter |
| -32602 | invalid request |
| -32603 | internal, unavailable state, or request timeout |

## Readiness and limits

The supplied deployment is a combined Nitro consensus/execution process. An
optional in-process feed observer supplies health; split execution RPC deployments
intentionally never advertise ready snapshots without such an observer. They can
still serve canonical simulations. No new execution protocol is introduced.

Feed loss, application idle, stale head, and execution reorg invalidate readiness.
Latest-state requests fall back to the local head. Defaults are four concurrent
simulations, 256 items per bundle, five seconds per request including queue wait,
1024 queued events per subscription, and a twenty-second maximum head/feed age.
Only metadata enters stream queues; state overlays are retained by the current
snapshot and active requests.

Metrics under `keepers/` cover execution latency, pending/fallback source, result
status, snapshot publication latency/age/invalidation, feed age, execution lag,
and subscription overflow. Native feed metrics cover reconnects.
