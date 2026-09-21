package gethexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/rpc"
)

type KeepersEthAPI struct{ service *KeepersService }
type KeepersAPI struct{ service *KeepersService }
type KeepersFlashAPI struct{ service *KeepersService }

func (s *KeepersService) acquire(ctx context.Context, count int) (context.Context, func(), error) {
	if count > s.config.MaxBundle {
		return nil, nil, keepersRPCError(-32602, "simulation bundle exceeds max-bundle")
	}
	ctx, cancel := context.WithTimeout(ctx, s.config.Timeout)
	select {
	case s.slots <- struct{}{}:
		start := time.Now()
		return ctx, func() { <-s.slots; cancel(); keepersLatency.Update(time.Since(start).Nanoseconds()) }, nil
	case <-ctx.Done():
		cancel()
		return nil, nil, keepersRPCError(-32603, ctx.Err().Error())
	}
}
func (api *KeepersAPI) SimulateAtLatestState(ctx context.Context, sims []KeepersInput) ([]KeepersResult, error) {
	s := api.service
	ctx, release, err := s.acquire(ctx, len(sims))
	if err != nil {
		return nil, err
	}
	defer release()
	snapshot, err := s.snapshot()
	if err == nil {
		keepersPendingRequests.Inc(1)
		return s.simulate(ctx, snapshot.block, snapshot.post, sims, true, 0, 0)
	}
	keepersFallbackRequests.Inc(1)
	// Pin the header/hash before opening state, rather than resolving latest twice.
	header := s.chain.CurrentBlock()
	block := s.chain.GetBlock(header.Hash(), header.Number.Uint64())
	if block == nil {
		return nil, keepersRPCError(-32603, "latest block unavailable")
	}
	db, free, err := s.backend.StateAtBlock(ctx, block, s.config.Reexec, nil, true, false)
	if err != nil {
		return nil, keepersRPCError(-32603, "latest state unavailable: "+err.Error())
	}
	defer free()
	return s.simulate(ctx, block, db.Copy(), sims, true, 0, 0)
}
func (api *KeepersFlashAPI) SimulateV1(ctx context.Context, payloadID string, index, blockNumber uint64, sims []KeepersInput) (*KeepersSimulateResponse, error) {
	s := api.service
	decodedID, err := hexutil.Decode(payloadID)
	if err != nil || len(decodedID) != 8 {
		return nil, keepersRPCError(-32602, "payloadId must be 8 bytes of hex data")
	}
	payloadID = hexutil.Encode(decodedID)
	start := time.Now()
	ctx, release, err := s.acquire(ctx, len(sims))
	if err != nil {
		return nil, err
	}
	defer release()
	snapshot, err := s.snapshot()
	if err != nil {
		return nil, err
	}
	if err = validateKeepersTuple(keepersTuple{payloadID, index, blockNumber}, snapshot.tuple); err != nil {
		return nil, err
	}
	results, err := s.simulate(ctx, snapshot.block, snapshot.post, sims, true, 0, 0)
	if err != nil {
		return nil, err
	}
	if !s.unchanged(snapshot) {
		return nil, keepersRPCError(-39103, "requested tuple is no longer latest")
	}
	return &KeepersSimulateResponse{keepersTuple: snapshot.tuple, Generation: blockNumber<<32 | index&0xffffffff, DurationMs: uint64(time.Since(start).Milliseconds()), Results: results}, nil
}
func (api *KeepersEthAPI) SimulateTransactionAt(ctx context.Context, blockID rpc.BlockNumberOrHash, index uint64, input KeepersInput) (*KeepersResult, error) {
	s := api.service
	ctx, release, err := s.acquire(ctx, 1)
	if err != nil {
		return nil, err
	}
	defer release()
	var block *types.Block
	var db *state.StateDB
	var snapshot *keepersSnapshot
	if number, ok := blockID.Number(); ok && number == rpc.PendingBlockNumber {
		snapshot, err = s.snapshot()
		if err == nil {
			block = snapshot.block
			db = snapshot.parent
		}
		if err != nil {
			blockID = rpc.BlockNumberOrHashWithNumber(rpc.LatestBlockNumber)
		}
	}
	if block == nil {
		block, err = s.backend.BlockByNumberOrHash(ctx, blockID)
		if err != nil || block == nil {
			return nil, keepersRPCError(-32602, "block not found")
		}
		if s.chain.GetCanonicalHash(block.NumberU64()) != block.Hash() {
			return nil, keepersRPCError(-32602, "block is not canonical")
		}
	}
	if index > uint64(len(block.Transactions())) {
		return nil, keepersRPCError(-32602, "transactionIndex exceeds block transaction count")
	}
	if block.NumberU64() == s.chain.Config().ArbitrumChainParams.GenesisBlockNum {
		return nil, keepersRPCError(-32602, "genesis has no replayable parent state")
	}
	if db == nil {
		parent := s.chain.GetBlock(block.ParentHash(), block.NumberU64()-1)
		if parent == nil {
			return nil, keepersRPCError(-32603, "historical parent block unavailable")
		}
		var free func()
		db, free, err = s.backend.StateAtBlock(ctx, parent, s.config.Reexec, nil, true, false)
		if err != nil {
			return nil, keepersRPCError(-32603, "historical state unavailable: "+err.Error())
		}
		defer free()
		db = db.Copy()
	}
	header := block.Header()
	blockCtx := core.NewEVMBlockContext(header, s.chain, &header.Coinbase)
	evm := s.backend.GetEVM(ctx, db, header, &vm.Config{}, &blockCtx)
	stop := context.AfterFunc(ctx, evm.Cancel)
	defer stop()
	if root := block.BeaconRoot(); root != nil {
		core.ProcessBeaconBlockRoot(*root, evm)
	}
	signer := types.MakeSigner(s.chain.Config(), block.Number(), block.Time(), blockCtx.ArbOSVersion)
	var gasUsed uint64
	for i, tx := range block.Transactions()[:index] {
		if err = ctx.Err(); err != nil {
			return nil, keepersRPCError(-32603, err.Error())
		}
		msg, decodeErr := core.TransactionToMessage(tx, signer, block.BaseFee(), core.NewMessageReplayContext())
		if decodeErr != nil {
			return nil, keepersRPCError(-32603, "prefix decoding failed: "+decodeErr.Error())
		}
		db.SetTxContext(tx.Hash(), i)
		result, execErr := core.ApplyMessage(evm, msg, new(core.GasPool).AddGas(tx.Gas()))
		if execErr != nil {
			return nil, keepersRPCError(-32603, "prefix execution failed: "+execErr.Error())
		}
		gasUsed = saturatingKeepersAdd(gasUsed, result.UsedGas)
		db.Finalise(s.chain.Config().IsEIP158(block.Number()))
	}
	results, err := s.simulate(ctx, block, db, []KeepersInput{input}, false, index, gasUsed)
	if err != nil {
		return nil, err
	}
	return &results[0], nil
}

func saturatingKeepersAdd(a, b uint64) uint64 {
	if math.MaxUint64-a < b {
		return math.MaxUint64
	}
	return a + b
}
func keepersDefaultGas(blockLimit, cap uint64, inputs []KeepersInput, lenient bool, used uint64) uint64 {
	if !lenient {
		remaining := uint64(0)
		if used < blockLimit {
			remaining = blockLimit - used
		}
		if cap > 0 && cap < remaining {
			remaining = cap
		}
		return remaining
	}
	var specified, missing uint64
	for _, in := range inputs {
		if in.Args.Gas == nil {
			missing++
		} else {
			specified = saturatingKeepersAdd(specified, uint64(*in.Args.Gas))
		}
	}
	if specified > blockLimit {
		blockLimit = specified
	}
	if missing == 0 {
		return 0
	}
	return (blockLimit - specified) / missing
}

func (s *KeepersService) simulate(ctx context.Context, block *types.Block, db *state.StateDB, inputs []KeepersInput, lenient bool, offset, used uint64) ([]KeepersResult, error) {
	results := make([]KeepersResult, 0, len(inputs))
	header := block.Header()
	defaultGas := keepersDefaultGas(header.GasLimit, s.backend.RPCGasCap(), inputs, lenient, used)
	blockCtx := core.NewEVMBlockContext(header, s.chain, &header.Coinbase)
	for i, input := range inputs {
		if err := ctx.Err(); err != nil {
			return nil, keepersRPCError(-32603, err.Error())
		}
		// Clone through JSON-owned input values before defaults mutate any fields.
		args := input.Args
		failure := func(err error) {
			metrics.GetOrRegisterCounter("keepers/simulate/status/halt", nil).Inc(1)
			results = append(results, KeepersResult{Status: "halt", Logs: []KeepersLog{}, ReturnData: hexutil.Bytes{}, Error: err.Error()})
		}
		if args.Data != nil && args.Input != nil && !bytes.Equal(*args.Data, *args.Input) {
			failure(errors.New("data and input differ"))
			continue
		}
		if args.BlobHashes != nil || args.Blobs != nil {
			failure(errors.New("blob transactions are not supported on Robinhood"))
			continue
		}
		if args.SkipL1Charging != nil && *args.SkipL1Charging {
			failure(errors.New("skipL1Charging is not supported by Keepers"))
			continue
		}
		sender := common.Address{}
		if args.From != nil {
			sender = *args.From
		}
		if args.Nonce == nil {
			nonce := hexutil.Uint64(db.GetNonce(sender))
			args.Nonce = &nonce
		}
		if args.Gas == nil {
			gas := hexutil.Uint64(defaultGas)
			args.Gas = &gas
		}
		if args.GasPrice == nil && args.MaxFeePerGas == nil {
			fee := new(big.Int)
			if header.BaseFee != nil {
				fee.Set(header.BaseFee)
			}
			args.MaxFeePerGas = (*hexutil.Big)(fee)
		}
		if err := args.CallDefaults(0, header.BaseFee, s.chain.Config().ChainID); err != nil {
			failure(err)
			continue
		}
		msg := args.ToMessage(header.BaseFee, 0, header, nil, core.NewMessageEthcallContext(), lenient)
		msg.SkipTransactionChecks = lenient
		msg.KeepersSkipBaseFeeCheck = lenient
		tx := args.ToTransaction(types.DynamicFeeTxType)
		// Preserve the signed bytes for exact ArbOS poster pricing. Unsigned
		// calls keep Tx nil and use Nitro's native message-based estimation.
		msg.Tx = input.Raw
		hash := tx.Hash()
		if input.Raw != nil {
			hash = input.Raw.Hash()
		}
		db.SetTxContext(hash, int(offset)+i)
		checkpoint := db.Snapshot()
		recentWasms := db.GetRecentWasms().Copy()
		evm := s.backend.GetEVM(ctx, db, header, &vm.Config{NoBaseFee: lenient}, &blockCtx)
		stop := context.AfterFunc(ctx, evm.Cancel)
		result, err := core.ApplyMessage(evm, msg, new(core.GasPool).AddGas(math.MaxUint64))
		stop()
		if ctx.Err() != nil {
			return nil, keepersRPCError(-32603, ctx.Err().Error())
		}
		if err != nil {
			db.RevertToSnapshot(checkpoint)
			db.RestoreRecentWasms(recentWasms)
			failure(err)
			continue
		}
		out := KeepersResult{Status: "success", GasUsed: hexutil.Uint64(result.UsedGas), Logs: []KeepersLog{}, ReturnData: hexutil.Bytes(result.ReturnData)}
		if result.Err != nil {
			out.Status = "halt"
			out.Error = result.Err.Error()
			out.ReturnData = hexutil.Bytes{}
			if errors.Is(result.Err, vm.ErrExecutionReverted) {
				out.Status = "revert"
				out.ReturnData = result.ReturnData
				if reason, e := abi.UnpackRevert(result.ReturnData); e == nil {
					out.Error = reason
				}
			}
		} else {
			// Only return this transaction's logs, even if identical unsigned requests
			// yield the same synthetic transaction hash in a bundle.
			for _, entry := range db.GetLogs(hash, block.NumberU64(), common.Hash{}, block.Time()) {
				if uint64(entry.TxIndex) == offset+uint64(i) {
					copied := *entry
					out.Logs = append(out.Logs, keepersLog(&copied, block.Time(), true))
				}
			}
		}
		db.Finalise(s.chain.Config().IsEIP158(block.Number()))
		metrics.GetOrRegisterCounter("keepers/simulate/status/"+out.Status, nil).Inc(1)
		results = append(results, out)
	}
	if err := db.Error(); err != nil {
		return nil, keepersRPCError(-32603, fmt.Sprintf("simulation state read failed: %v", err))
	}
	return results, nil
}
