package gethexec

import (
	"encoding/json"
	"errors"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common/hexutil"
)

func TestKeepersCounterNeverReusesAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "counter")
	first, err := newKeepersCounter(path)
	if err != nil {
		t.Fatal(err)
	}
	id, err := first.allocate()
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := newKeepersCounter(path)
	if err != nil {
		t.Fatal(err)
	}
	next, err := restarted.allocate()
	if err != nil {
		t.Fatal(err)
	}
	a, err := strconv.ParseUint(strings.TrimPrefix(id, "0x"), 16, 64)
	if err != nil {
		t.Fatal(err)
	}
	b, err := strconv.ParseUint(strings.TrimPrefix(next, "0x"), 16, 64)
	if err != nil {
		t.Fatal(err)
	}
	if b <= a || id == next {
		t.Fatalf("payload identity reused: %s -> %s", id, next)
	}
	if err = os.WriteFile(path, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = newKeepersCounter(path); err == nil {
		t.Fatal("corrupt counter accepted")
	}
	first.next = math.MaxUint64
	first.limit = math.MaxUint64
	if _, err = first.allocate(); err == nil {
		t.Fatal("counter wrapped")
	}
}
func TestKeepersTupleContract(t *testing.T) {
	latest := keepersTuple{"0x0000000000000001", 0, 10}
	for _, test := range []struct {
		name  string
		tuple keepersTuple
		code  int
	}{
		{"current", latest, 0},
		{"stale-block", keepersTuple{latest.PayloadID, 0, 9}, -39104},
		{"ahead-block", keepersTuple{latest.PayloadID, 0, 11}, -39102},
		{"replaced-payload", keepersTuple{"0x0000000000000002", 0, 10}, -39100},
		{"ahead-index", keepersTuple{latest.PayloadID, 1, 10}, -39102},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateKeepersTuple(test.tuple, latest)
			if test.code == 0 {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var rpcErr *keepersError
			if !errors.As(err, &rpcErr) || rpcErr.ErrorCode() != test.code {
				t.Fatalf("error = %v, want %d", err, test.code)
			}
		})
	}
}
func TestKeepersInputValidation(t *testing.T) {
	for _, invalid := range []string{`null`, `[]`, `{"rawTx":"0x"}`, `{"rawTx":"0x1234"}`, `{"rawTx":"0x","gas":"0x1"}`} {
		var input KeepersInput
		if json.Unmarshal([]byte(invalid), &input) == nil {
			t.Fatalf("accepted %s", invalid)
		}
	}
	var input KeepersInput
	if err := json.Unmarshal([]byte(`{"from":"0x0000000000000000000000000000000000000001","gas":"0x5208"}`), &input); err != nil {
		t.Fatal(err)
	}
	if input.Args.Gas == nil || uint64(*input.Args.Gas) != 21000 {
		t.Fatal("lost unsigned gas")
	}
}
func TestKeepersGasDefaults(t *testing.T) {
	gas := hexutil.Uint64(60)
	inputs := []KeepersInput{{}, {}}
	if got := keepersDefaultGas(100, 0, inputs, true, 0); got != 50 {
		t.Fatalf("bundle gas %d", got)
	}
	inputs[0].Args.Gas = &gas
	if got := keepersDefaultGas(100, 0, inputs, true, 0); got != 40 {
		t.Fatalf("mixed bundle gas %d", got)
	}
	if got := keepersDefaultGas(100, 20, inputs, false, 50); got != 20 {
		t.Fatalf("insertion gas cap %d", got)
	}
	if got := keepersDefaultGas(100, 0, inputs, false, 101); got != 0 {
		t.Fatalf("gas underflow %d", got)
	}
	if got := saturatingKeepersAdd(math.MaxUint64, 1); got != math.MaxUint64 {
		t.Fatal("gas overflow")
	}
}
func TestKeepersResultWireShape(t *testing.T) {
	result := KeepersResult{Status: "success", GasUsed: 21000, Logs: []KeepersLog{}, ReturnData: hexutil.Bytes{}}
	got, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile("../../keepers/fixtures/success.json")
	if err != nil {
		t.Fatal(err)
	}
	want := strings.TrimSpace(string(fixture))
	if string(got) != want {
		t.Fatalf("got %s, want %s", got, want)
	}
	response := KeepersSimulateResponse{keepersTuple: keepersTuple{"0x0000000000000001", 0, 10}, Generation: 10 << 32, Results: []KeepersResult{}}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"payloadId", "index", "blockNumber", "generation", "durationMs", "results"} {
		if _, ok := fields[name]; !ok {
			t.Fatalf("missing %s in %s", name, encoded)
		}
	}
}

func TestKeepersSnapshotInvalidationAndOverflow(t *testing.T) {
	sub := &keepersSubscriber{events: make(chan keepersEvent, 1), failed: make(chan struct{})}
	snap := &keepersSnapshot{revision: 1}
	s := &KeepersService{ready: true, revision: 1, latest: snap, subscribers: map[*keepersSubscriber]struct{}{sub: {}}}
	s.broadcastLocked(keepersEvent{snapshot: snap})
	s.broadcastLocked(keepersEvent{snapshot: snap})
	select {
	case <-sub.failed:
	default:
		t.Fatal("overflow did not close subscriber")
	}
	if len(s.subscribers) != 0 {
		t.Fatal("overflow subscriber retained")
	}
	s.reorg()
	if s.ready || s.latest != nil || s.previous != snap || s.revision == snap.revision {
		t.Fatal("reorg retained ready tuple")
	}
	// Replacements must invalidate an in-flight call even at the same height.
	if s.unchanged(snap) {
		t.Fatal("reorg accepted an old execution result")
	}
	sub = &keepersSubscriber{events: make(chan keepersEvent, 1), failed: make(chan struct{})}
	s.subscribers[sub] = struct{}{}
	s.latest, s.ready = snap, true
	s.invalidateLocked(true)
	select {
	case <-sub.failed:
	default:
		t.Fatal("discontinuity kept stream open")
	}
	if s.latest != nil || s.ready {
		t.Fatal("discontinuity retained snapshot")
	}
}

func TestKeepersFilteredLogsOrdering(t *testing.T) {
	address := common.HexToAddress("0x1234")
	topic := common.HexToHash("0xaa")
	txs := []*types.Transaction{
		types.NewTx(&types.LegacyTx{Nonce: 1, GasPrice: big.NewInt(7)}),
		types.NewTx(&types.LegacyTx{Nonce: 2, GasPrice: big.NewInt(8)}),
	}
	block := types.NewBlockWithHeader(&types.Header{Number: big.NewInt(10), Time: uint64(time.Now().Unix()), BaseFee: big.NewInt(3)}).WithBody(types.Body{Transactions: txs})
	snap := &keepersSnapshot{tuple: keepersTuple{"0x0000000000000001", 0, 10}, block: block, receipts: types.Receipts{
		{Logs: []*types.Log{{Address: address, Topics: []common.Hash{topic}, TxHash: txs[0].Hash(), Index: 0}}},
		{Logs: []*types.Log{{Address: address, Topics: []common.Hash{common.HexToHash("0xbb")}, Index: 1}, {Address: address, Topics: []common.Hash{topic}, TxHash: txs[1].Hash(), Index: 2}}},
	}}
	update := keepersFilteredEvent(snap, KeepersLogFilter{Topic0s: []common.Hash{topic}, Addresses: []common.Address{address}})
	if update == nil || len(update.Transactions) != 2 || len(update.Transactions[1].Logs) != 1 || update.Transactions[0].TxHash != txs[0].Hash() || update.Transactions[1].TxHash != txs[1].Hash() {
		t.Fatalf("wrong filtered ordering: %+v", update)
	}
	if keepersFilteredEvent(snap, KeepersLogFilter{Topic0s: []common.Hash{topic}, Addresses: []common.Address{common.Address{}}}) != nil {
		t.Fatal("address filter ignored")
	}
	inclusion := keepersInclusionEvent(snap)
	if len(inclusion.TxHashes) != 2 || inclusion.TxHashes[1] != txs[1].Hash() || inclusion.FlashblockHash != block.Hash() {
		t.Fatalf("wrong inclusion: %+v", inclusion)
	}
}
