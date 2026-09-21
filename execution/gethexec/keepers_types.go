package gethexec

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/ethereum/go-ethereum/arbitrum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/spf13/pflag"
)

// KeepersConfig only affects the local simulation and subscription service.
type KeepersConfig struct {
	Enabled     bool          `koanf:"enabled"`
	Concurrency int           `koanf:"concurrency"`
	MaxBundle   int           `koanf:"max-bundle"`
	QueueSize   int           `koanf:"queue-size"`
	Timeout     time.Duration `koanf:"timeout"`
	MaxHeadAge  time.Duration `koanf:"max-head-age"`
	Reexec      uint64        `koanf:"reexec"`
}

var DefaultKeepersConfig = KeepersConfig{Concurrency: 4, MaxBundle: 256, QueueSize: 1024, Timeout: 5 * time.Second, MaxHeadAge: 20 * time.Second, Reexec: 128}

func keepersConfigOptions(prefix string, f *pflag.FlagSet) {
	c := DefaultKeepersConfig
	f.Bool(prefix+".enabled", c.Enabled, "enable Keepers simulation and block snapshot APIs")
	f.Int(prefix+".concurrency", c.Concurrency, "maximum concurrent Keepers simulations")
	f.Int(prefix+".max-bundle", c.MaxBundle, "maximum transactions in a Keepers simulation bundle")
	f.Int(prefix+".queue-size", c.QueueSize, "events buffered per Keepers subscription")
	f.Duration(prefix+".timeout", c.Timeout, "Keepers request timeout including queue wait")
	f.Duration(prefix+".max-head-age", c.MaxHeadAge, "maximum age of a ready Keepers snapshot")
	f.Uint64(prefix+".reexec", c.Reexec, "maximum historical blocks to reconstruct for Keepers simulations")
}

// KeepersInput deliberately accepts ordinary user transactions only. Internal
// ArbOS transactions are replayed from blocks, never fabricated by RPC callers.
type KeepersInput struct {
	Args arbitrum.KeepersTransactionArgs
	Raw  *types.Transaction
}

func (in *KeepersInput) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if fields == nil {
		return fmt.Errorf("transaction must be an object")
	}
	if raw, ok := fields["rawTx"]; ok {
		if len(fields) != 1 {
			return fmt.Errorf("rawTx cannot be combined with request fields")
		}
		var encoded hexutil.Bytes
		if err := json.Unmarshal(raw, &encoded); err != nil {
			return err
		}
		tx := new(types.Transaction)
		if err := tx.UnmarshalBinary(encoded); err != nil {
			return fmt.Errorf("invalid signed transaction: %w", err)
		}
		if tx.Type() != types.LegacyTxType && tx.Type() != types.AccessListTxType && tx.Type() != types.DynamicFeeTxType && tx.Type() != types.SetCodeTxType {
			return fmt.Errorf("unsupported candidate transaction type %d", tx.Type())
		}
		args, err := arbitrum.KeepersArgsFromTransaction(tx)
		if err != nil {
			return err
		}
		in.Args, in.Raw = args, tx
		return nil
	}
	return json.Unmarshal(data, &in.Args)
}

type KeepersResult struct {
	Status     string         `json:"status"`
	GasUsed    hexutil.Uint64 `json:"gasUsed"`
	Logs       []KeepersLog   `json:"logs"`
	ReturnData hexutil.Bytes  `json:"returnData"`
	Error      string         `json:"error,omitempty"`
}
type keepersError struct {
	code    int
	message string
}

func (e *keepersError) Error() string                { return e.message }
func (e *keepersError) ErrorCode() int               { return e.code }
func keepersRPCError(code int, message string) error { return &keepersError{code, message} }

type keepersTuple struct {
	PayloadID   string `json:"payloadId"`
	Index       uint64 `json:"index"`
	BlockNumber uint64 `json:"blockNumber"`
}
type KeepersSimulateResponse struct {
	keepersTuple
	Generation uint64          `json:"generation"`
	DurationMs uint64          `json:"durationMs"`
	Results    []KeepersResult `json:"results"`
}
type KeepersLogFilter struct {
	Topic0s   []common.Hash    `json:"topic0s"`
	Addresses []common.Address `json:"addresses,omitempty"`
}
type keepersLogTransaction struct {
	TxHash            common.Hash  `json:"txHash"`
	PriorityFeePerGas *hexutil.Big `json:"priorityFeePerGas"`
	Logs              []KeepersLog `json:"logs"`
}
type keepersLogsUpdate struct {
	Type string `json:"type"`
	keepersTuple
	BlockTimestamp uint64                  `json:"blockTimestamp"`
	BaseFeePerGas  *hexutil.Big            `json:"baseFeePerGas"`
	Transactions   []keepersLogTransaction `json:"transactions"`
}
type keepersInclusions struct {
	keepersTuple
	BlockTimestamp uint64        `json:"blockTimestamp"`
	FlashblockHash common.Hash   `json:"flashblockHash"`
	TxHashes       []common.Hash `json:"txHashes"`
}
type keepersReverted struct {
	Type                string `json:"type"`
	PreviousPayloadID   string `json:"previousPayloadId"`
	PreviousIndex       uint64 `json:"previousIndex"`
	PreviousBlockNumber uint64 `json:"previousBlockNumber"`
	CurrentPayloadID    string `json:"currentPayloadId"`
	CurrentIndex        uint64 `json:"currentIndex"`
	CurrentBlockNumber  uint64 `json:"currentBlockNumber"`
}

func validateKeepersTuple(request, latest keepersTuple) error {
	if request.BlockNumber < latest.BlockNumber {
		return keepersRPCError(-39104, "requested blockNumber is stale")
	}
	if request.BlockNumber > latest.BlockNumber {
		return keepersRPCError(-39102, "requested blockNumber is not ready")
	}
	if request.PayloadID != latest.PayloadID {
		return keepersRPCError(-39100, "requested payloadId is stale")
	}
	if request.Index < latest.Index {
		return keepersRPCError(-39101, "requested index is stale")
	}
	if request.Index > latest.Index {
		return keepersRPCError(-39102, "requested index is not ready")
	}
	return nil
}

// KeepersLog matches Alloy's nullable block metadata for simulated logs.
type KeepersLog struct {
	Address          common.Address `json:"address"`
	Topics           []common.Hash  `json:"topics"`
	Data             hexutil.Bytes  `json:"data"`
	BlockHash        *common.Hash   `json:"blockHash"`
	BlockNumber      hexutil.Uint64 `json:"blockNumber"`
	BlockTimestamp   hexutil.Uint64 `json:"blockTimestamp"`
	TransactionHash  common.Hash    `json:"transactionHash"`
	TransactionIndex hexutil.Uint64 `json:"transactionIndex"`
	LogIndex         hexutil.Uint64 `json:"logIndex"`
	Removed          bool           `json:"removed"`
}

func keepersLog(entry *types.Log, timestamp uint64, simulated bool) KeepersLog {
	topics := entry.Topics
	if topics == nil {
		topics = []common.Hash{}
	}
	result := KeepersLog{Address: entry.Address, Topics: topics, Data: entry.Data, BlockHash: &entry.BlockHash, BlockNumber: hexutil.Uint64(entry.BlockNumber), BlockTimestamp: hexutil.Uint64(timestamp), TransactionHash: entry.TxHash, TransactionIndex: hexutil.Uint64(entry.TxIndex), LogIndex: hexutil.Uint64(entry.Index), Removed: entry.Removed}
	if simulated {
		result.BlockHash = nil
	}
	return result
}
