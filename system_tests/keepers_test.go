package arbtest

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/gorilla/websocket"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/offchainlabs/nitro/arbnode"
	"github.com/offchainlabs/nitro/execution/gethexec"
	"github.com/offchainlabs/nitro/solgen/go/bridgegen"
)

// Exercises the actual Nitro RPC registration and ArbOS state transition, not a
// mocked EVM. The contract stores nonempty calldata and returns storage slot 0.
func TestKeepersSequentialSimulationAndInsertion(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	builder := NewNodeBuilder(ctx).DefaultConfig(t, false)
	builder.WithArbOSVersion(61)
	builder.execConfig.Keepers.Enabled = true
	builder.l2StackConfig.WSHost = "127.0.0.1"
	builder.l2StackConfig.WSPort = 0
	builder.l2StackConfig.WSModules = append(builder.l2StackConfig.WSModules, "eth", "keepers", "flashsimv2")
	cleanup := builder.Build(t)
	defer cleanup()
	owner := builder.L2Info.GetAddress("Owner")
	initCode := common.FromHex("0x601e600c600039601e6000f3361560125760003560005560aa60006000a15b60005460005260206000f3")
	deployment := builder.L2Info.PrepareTxTo("Owner", nil, 2_000_000, common.Big0, initCode)
	Require(t, builder.L2.Client.SendTransaction(ctx, deployment))
	receipt, err := builder.L2.EnsureTxSucceeded(deployment)
	Require(t, err)
	address := receipt.ContractAddress
	client := builder.L2.Client.Client()
	call := func(data string) map[string]any {
		return map[string]any{"from": owner, "to": address, "gas": "0x1e8480", "data": data}
	}
	value := fmt.Sprintf("0x%064x", 42)
	var outputs []gethexec.KeepersResult
	Require(t, client.CallContext(ctx, &outputs, "keepers_simulateAtLatestState", []any{call(value), call("0x")}))
	if len(outputs) != 2 || outputs[0].Status != "success" || outputs[1].Status != "success" || new(big.Int).SetBytes(outputs[1].ReturnData).Uint64() != 42 {
		t.Fatalf("bundle did not share state: %+v", outputs)
	}
	if len(outputs[0].Logs) != 1 || len(outputs[1].Logs) != 0 {
		t.Fatalf("candidate logs are not isolated: %+v", outputs)
	}
	stored, err := builder.L2.Client.StorageAt(ctx, address, common.Hash{}, nil)
	Require(t, err)
	if new(big.Int).SetBytes(stored).Sign() != 0 {
		t.Fatal("simulation mutated canonical storage")
	}
	Require(t, client.CallContext(ctx, &outputs, "keepers_simulateAtLatestState", []any{call("0x")}))
	if new(big.Int).SetBytes(outputs[0].ReturnData).Sign() != 0 {
		t.Fatal("state leaked across requests")
	}

	// Concurrent overlays must not leak writes to each other or canonical state.
	var wg sync.WaitGroup
	for n := 1; n <= 8; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			var result []gethexec.KeepersResult
			err := client.CallContext(ctx, &result, "keepers_simulateAtLatestState", []any{call(fmt.Sprintf("0x%064x", n)), call("0x")})
			if err != nil || len(result) != 2 || new(big.Int).SetBytes(result[1].ReturnData).Uint64() != uint64(n) {
				t.Errorf("concurrent bundle %d: %+v, %v", n, result, err)
			}
		}(n)
	}
	wg.Wait()

	// The integrated feed observer makes the already executed head available.
	builder.L2.ExecNode.KeepersFeedStatus("test", true, 0)
	conn, _, err := websocket.DefaultDialer.Dial(builder.L2.Stack.WSEndpoint(), nil)
	Require(t, err)
	defer conn.Close()
	Require(t, conn.SetReadDeadline(time.Now().Add(10*time.Second)))
	Require(t, conn.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "flashsimv2_subscribeTxInclusionsV1", "params": []any{}}))
	var subscription struct {
		Result string          `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	Require(t, conn.ReadJSON(&subscription))
	if subscription.Result == "" {
		t.Fatalf("subscription failed: %s", subscription.Error)
	}

	logsConn, _, err := websocket.DefaultDialer.Dial(builder.L2.Stack.WSEndpoint(), nil)
	Require(t, err)
	defer logsConn.Close()
	Require(t, logsConn.SetReadDeadline(time.Now().Add(10*time.Second)))
	Require(t, logsConn.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "flashsimv2_subscribeFilteredLogsV1", "params": []any{map[string]any{"topic0s": []common.Hash{common.HexToHash("0xaa")}, "addresses": []common.Address{address}}}}))
	var logsSubscription struct {
		Result string
		Error  json.RawMessage
	}
	Require(t, logsConn.ReadJSON(&logsSubscription))
	if logsSubscription.Result == "" {
		t.Fatalf("logs subscription failed: %s", logsSubscription.Error)
	}

	// An actual write lets insertion replay prove that it observes the prefix,
	// rather than silently using post-block state.
	tx := builder.L2Info.PrepareTxTo("Owner", &address, 2_000_000, common.Big0, common.FromHex(value))
	Require(t, builder.L2.Client.SendTransaction(ctx, tx))
	written, err := builder.L2.EnsureTxSucceeded(tx)
	Require(t, err)
	var event struct {
		Method string `json:"method"`
		Params struct {
			Result struct {
				PayloadID   string        `json:"payloadId"`
				Index       uint64        `json:"index"`
				BlockNumber uint64        `json:"blockNumber"`
				TxHashes    []common.Hash `json:"txHashes"`
			} `json:"result"`
		} `json:"params"`
	}
	Require(t, conn.ReadJSON(&event))
	if event.Method != "flashsimv2_txInclusionsV1" || event.Params.Result.Index != 0 {
		t.Fatalf("wrong stream envelope: %+v", event)
	}
	var logEvent struct {
		Method string
		Params struct {
			Result struct {
				Type         string
				PayloadID    string
				Transactions []struct {
					TxHash common.Hash
					Logs   []gethexec.KeepersLog
				}
			}
		}
	}
	Require(t, logsConn.ReadJSON(&logEvent))
	if logEvent.Method != "flashsimv2_filteredLogsV1" || logEvent.Params.Result.Type != "update" || len(logEvent.Params.Result.Transactions) != 1 || logEvent.Params.Result.Transactions[0].TxHash != tx.Hash() {
		t.Fatalf("wrong filtered log event: %+v", logEvent)
	}
	tuple := event.Params.Result
	var tupleResult gethexec.KeepersSimulateResponse
	Require(t, client.CallContext(ctx, &tupleResult, "flashsimv2_simulateV1", tuple.PayloadID, tuple.Index, tuple.BlockNumber, []any{call("0x")}))
	if tupleResult.Results[0].Status != "success" {
		t.Fatalf("tuple simulation failed: %+v", tupleResult)
	}
	Require(t, conn.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "flashsimv2_unsubscribeTxInclusionsV1", "params": []any{subscription.Result}}))
	var unsub struct {
		Result bool `json:"result"`
	}
	Require(t, conn.ReadJSON(&unsub))
	if !unsub.Result {
		t.Fatal("unsubscribe failed")
	}
	block, err := builder.L2.Client.BlockByHash(ctx, written.BlockHash)
	Require(t, err)
	for _, test := range []struct {
		index uint64
		value uint64
	}{{0, 0}, {uint64(written.TransactionIndex), 0}, {uint64(len(block.Transactions())), 42}} {
		var output gethexec.KeepersResult
		Require(t, client.CallContext(ctx, &output, "eth_simulateTransactionAt", written.BlockHash, test.index, call("0x")))
		if output.Status != "success" || new(big.Int).SetBytes(output.ReturnData).Uint64() != test.value {
			t.Fatalf("index %d: %+v, want %d", test.index, output, test.value)
		}
	}
	var pending gethexec.KeepersResult
	Require(t, client.CallContext(ctx, &pending, "eth_simulateTransactionAt", "pending", len(block.Transactions()), call("0x")))
	if pending.Status != "success" || new(big.Int).SetBytes(pending.ReturnData).Uint64() != 42 {
		t.Fatalf("pending replay: %+v", pending)
	}
	var output gethexec.KeepersResult
	err = client.CallContext(ctx, &output, "eth_simulateTransactionAt", written.BlockHash, len(block.Transactions())+1, call("0x"))
	if rpcErr, ok := err.(rpc.Error); !ok || rpcErr.ErrorCode() != -32602 {
		t.Fatalf("bad index returned %v", err)
	}

	raw, err := tx.MarshalBinary()
	Require(t, err)
	Require(t, client.CallContext(ctx, &outputs, "keepers_simulateAtLatestState", []any{map[string]any{"rawTx": hexutil.Encode(raw)}, call("0x")}))
	if len(outputs) != 2 || outputs[0].Status != "success" || outputs[1].Status != "success" {
		t.Fatalf("raw simulation failed: %+v", outputs)
	}
	Require(t, client.CallContext(ctx, &outputs, "keepers_simulateAtLatestState", []any{
		map[string]any{"from": owner, "data": "0x60006000fd", "gas": "0x1e8480"},
		map[string]any{"from": owner, "data": "0xfe", "gas": "0x1e8480"}, call("0x"),
	}))
	if outputs[0].Status != "revert" || outputs[1].Status != "halt" || outputs[2].Status != "success" {
		t.Fatalf("wrong bundle failure behavior: %+v", outputs)
	}
	var removed any
	if err = client.CallContext(ctx, &removed, "flashsimv2_simulateAtLatestFlashblockState", []any{}); err == nil {
		t.Fatal("removed method exposed")
	}
}

// The native block contains StartBlock, retryable submission, and auto-redeem
// transactions. Replaying all of them must recover the recipient's real balance.
func TestKeepersRetryablePrefix(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	builder := NewNodeBuilder(ctx).DefaultConfig(t, true)
	builder.WithArbOSVersion(61)
	builder.execConfig.Keepers.Enabled = true
	builder.nodeConfig.BlockValidator.Enable = false
	builder.nodeConfig.Staker.Enable = false
	cleanup := builder.Build(t)
	defer cleanup()
	builder.L2Info.GenerateAccount("User2")
	builder.L2Info.GenerateAccount("Beneficiary")
	inbox, err := bridgegen.NewInbox(builder.L1Info.GetAddress("Inbox"), builder.L1.Client)
	Require(t, err)
	delayedBridge, err := arbnode.NewDelayedBridge(builder.L1.Client, builder.L1Info.GetAddress("Bridge"), 0)
	Require(t, err)
	lookup := getLookupL2Tx(t, ctx, delayedBridge)
	destination := builder.L2Info.GetAddress("User2")
	beneficiary := builder.L2Info.GetAddress("Beneficiary")
	opts := builder.L1Info.GetDefaultTransactOpts("Faucet", ctx)
	opts.Value = new(big.Int).Exp(big.NewInt(10), big.NewInt(20), nil)
	amount := big.NewInt(123456)
	tx, err := inbox.CreateRetryableTicket(&opts, destination, amount, big.NewInt(1e16), beneficiary, beneficiary, big.NewInt(1_000_000), big.NewInt(1e9), nil)
	Require(t, err)
	l1Receipt, err := builder.L1.EnsureTxSucceeded(tx)
	Require(t, err)
	waitForL1DelayBlocks(t, builder)
	submission, err := builder.L2.EnsureTxSucceeded(lookup(l1Receipt))
	Require(t, err)
	block, err := builder.L2.Client.BlockByHash(ctx, submission.BlockHash)
	Require(t, err)
	actual, err := builder.L2.Client.BalanceAt(ctx, destination, block.Number())
	Require(t, err)
	if actual.Cmp(amount) != 0 {
		t.Fatalf("retryable did not auto redeem: %s", actual)
	}
	input := map[string]any{"from": builder.L2Info.GetAddress("Owner"), "gas": "0x1e8480", "data": "0x73" + hexutil.Encode(destination[:])[2:] + "3160005260206000f3"}
	for _, index := range []int{0, len(block.Transactions())} {
		var result gethexec.KeepersResult
		Require(t, builder.L2.Client.Client().CallContext(ctx, &result, "eth_simulateTransactionAt", block.Hash(), index, input))
		want := new(big.Int)
		if index > 0 {
			want.Set(actual)
		}
		if result.Status != "success" || new(big.Int).SetBytes(result.ReturnData).Cmp(want) != 0 {
			t.Fatalf("retryable prefix %d: %+v, want %s", index, result, want)
		}
	}
}
