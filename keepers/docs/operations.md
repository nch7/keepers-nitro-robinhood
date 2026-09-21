# Robinhood operations

## Baseline and inputs

Pinned source: Nitro v3.11.4, commit
`7d5ac271b400f710f6267ad759c6afc3e12d7059`, recommended in Robinhood's September 14,
2026 notice. The older node guide still shows v3.11.2. A read-only public RPC check
on September 21 returned chain ID `0x1237` and client version
`nitro/v3.12.0-rc.2+19e94c6-20260901T094258Z/linux-arm64/go1.25.12`.
Use the recommended stable source baseline, and require shadow validation before
production cutover. Public endpoint software identity is not proof of consensus
compatibility.

Official sources:
- https://docs.robinhood.com/chain/run-a-full-node/
- https://docs.robinhood.com/chain/notices-and-upgrades/
- https://docs.robinhood.com/chain/connecting/

The public config and genesis are checked in with SHA-256 checksums. Mainnet uses
Ethereum execution RPC and beacon blob access. Feed compression is mandatory as
of September 17; the launcher uses Nitro's native RFC 7692 client.

## Build and deploy

1. Commit/push the Geth fork first, update Nitro's submodule pin, then commit/push
   Nitro. On the server, pull the reviewed revision and update recursive submodules.
2. Run `keepers/deploy/build.sh`. It rejects tracked dirty source and produces an
   image tagged by the exact Nitro commit, using the upstream Dockerfile. Its
   reported version retains `v3.11.4` with Keepers commit build metadata so the
   upstream minimum-version alerts continue to work.
3. Create a `keepers` service account and a writable local-NVMe datadir. Store a
   private copy of `node.example.json` in `/etc/keepers-nitro-robinhood/node.json`,
   fill Ethereum endpoints, and restrict access to the service account.
4. Set the revision-tagged image and paths in the supplied environment template.
   For initial snapshot restore, select a current official Robinhood full-node
   snapshot and set `KEEPERS_INIT_SNAPSHOT`; remove it after initialization.
5. Install the supplied systemd unit with the repository at
   `/opt/keepers-nitro-robinhood`, then start and monitor the service.

The actual v3.11.4 Dockerfile uses `/home/user/.arbitrum`. The launcher mounts that
path and runs with the service account's numeric UID/GID; it does not use the
conflicting `/home/nitro` example from the current node guide. RPC binds to host
loopback by default. Metrics are enabled and published on host loopback port 6070
(configurable with `KEEPERS_METRICS_PORT`). The launcher uses `forwarding-target=null`: this is a local
read/simulation node, not a transaction forwarding service.

No server is implicitly selected and no live deployment is performed by setup.

## Acceptance before consumer cutover

- Require chain ID 4663 and successful catch-up. Compare multiple block hashes,
  state roots, receipts, and logs against an unmodified Nitro reference.
- Run the fixture and system tests, including sequential calls and prefix replay.
- Exercise direct WS methods, reconnection, idle feed, overflow, and a test reorg.
- Measure feed-to-notification and simulation p50/p95/p99 under representative
  concurrent load. Inspect head age and execution lag, not latency alone.
- Record the actual historical replay range on the chosen state scheme/snapshot.
- Keep the prior image and config. Before any database-format change, preserve a
  compatible datadir backup; never start an older binary on an incompatible DB.
- Preserve the monotonic counter high-water mark when restoring a datadir. If
  restoring an older snapshot after publishing tuples, copy the current counter
  file to the restored datadir before startup, never the snapshot's older copy.

## Updating upstream

Merge a reviewed stable Nitro tag, preserve each upstream submodule revision, and
rebase the small Geth RPC/argument-conversion patches on its new pinned Geth SHA.
Re-run execution, RPC, race, and recovery tests before rollout. Do not update a
submodule to its moving default branch. Consensus/execution changes remain
upstream-controlled; Keepers service flags must not affect normal block results.

## Reference and latency checks

After syncing a shadow node, run:

```sh
python3 keepers/tools/check_rpc.py --rpc http://127.0.0.1:8547 \
  --reference https://YOUR_UNMODIFIED_NITRO_RPC \
  --bundle /etc/keepers/representative-bundle.json --samples 1000 --concurrency 4
```

An optional `--op-rpc` runs the same result-schema checks against the OP API.
This is a schema comparison, not an assertion that two different chains produce
identical execution results. Use chain-appropriate accounts/contracts in fixtures.
Metrics measure local feed-to-snapshot and notification latency separately from
HTTP round-trip latency. Preserve the raw workload, results, and head-lag metrics
with the rollout record.

`keepers/deploy/verify-genesis.sh` uses Nitro's genesis generator and the official
serialized chain config/initial L1 fee. The expected hash was checked against the
public Robinhood block 0 on September 21, 2026. An offline node started with
`no-l1-listener` synthesizes a different init message and is not a valid genesis
verification procedure. Production keeps L1 listening and genesis assertion
validation enabled.
