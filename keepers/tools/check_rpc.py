#!/usr/bin/env python3
"""Read-only Keepers contract checks, reference comparison, and latency sampling."""
import argparse
import concurrent.futures
import json
import statistics
import time
import urllib.request


def rpc(url, method, params):
    request = urllib.request.Request(url, data=json.dumps({"jsonrpc": "2.0", "id": 1, "method": method, "params": params}).encode(), headers={"Content-Type": "application/json", "User-Agent": "keepers-node-validation/1.0"})
    with urllib.request.urlopen(request, timeout=15) as response:
        body = json.load(response)
    if "error" in body:
        raise RuntimeError(f"{method}: {body['error']}")
    return body["result"]


def check_result(result):
    assert result["status"] in ("success", "revert", "halt"), result
    assert isinstance(result["logs"], list), result
    for key in ("gasUsed", "returnData"):
        assert isinstance(result[key], str) and result[key].startswith("0x"), result
    if result["status"] != "success":
        assert isinstance(result.get("error"), str), result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--rpc", required=True)
    parser.add_argument("--reference", help="Unmodified Robinhood Nitro RPC")
    parser.add_argument("--op-rpc", help="Optional OP endpoint for shared result-contract checks")
    parser.add_argument("--bundle", help="JSON array of representative simulation requests")
    parser.add_argument("--samples", type=int, default=100)
    parser.add_argument("--concurrency", type=int, default=4)
    args = parser.parse_args()
    assert args.samples > 0 and args.concurrency > 0
    assert int(rpc(args.rpc, "eth_chainId", []), 16) == 4663
    if args.bundle:
        with open(args.bundle) as file:
            bundle = json.load(file)
    else:
        bundle = [{"to": "0x0000000000000000000000000000000000000000", "gas": "0x186a0", "gasPrice": "0x0"}]
    assert isinstance(bundle, list)
    for endpoint in filter(None, [args.rpc, args.op_rpc]):
        results = rpc(endpoint, "keepers_simulateAtLatestState", [bundle])
        assert len(results) == len(bundle)
        for result in results:
            check_result(result)
    if args.reference:
        assert int(rpc(args.reference, "eth_chainId", []), 16) == 4663
        heads = [int(rpc(endpoint, "eth_blockNumber", []), 16) for endpoint in (args.rpc, args.reference)]
        # Compare a stable suffix; this is a sample, not a proof of full-chain agreement.
        for height in range(max(0, min(heads) - 12), max(0, min(heads) - 2)):
            blocks = [rpc(endpoint, "eth_getBlockByNumber", [hex(height), False]) for endpoint in (args.rpc, args.reference)]
            assert all(blocks), height
            for key in ("hash", "parentHash", "stateRoot", "receiptsRoot", "transactionsRoot", "transactions"):
                assert blocks[0][key] == blocks[1][key], (height, key)
            for tx_hash in blocks[0]["transactions"]:
                receipts = [rpc(endpoint, "eth_getTransactionReceipt", [tx_hash]) for endpoint in (args.rpc, args.reference)]
                for key in ("status", "gasUsed", "cumulativeGasUsed", "logs", "contractAddress"):
                    assert receipts[0].get(key) == receipts[1].get(key), (tx_hash, key)
    def sample(_):
        start = time.perf_counter()
        results = rpc(args.rpc, "keepers_simulateAtLatestState", [bundle])
        for result in results:
            check_result(result)
        return (time.perf_counter() - start) * 1000
    with concurrent.futures.ThreadPoolExecutor(max_workers=args.concurrency) as executor:
        durations = sorted(executor.map(sample, range(args.samples)))
    def percentile(p):
        return durations[min(len(durations) - 1, int((len(durations) - 1) * p))]
    print(json.dumps({"samples": len(durations), "concurrency": args.concurrency, "bundleItems": len(bundle), "p50Ms": statistics.median(durations), "p95Ms": percentile(.95), "p99Ms": percentile(.99), "maxMs": max(durations)}, indent=2))

if __name__ == "__main__":
    main()
