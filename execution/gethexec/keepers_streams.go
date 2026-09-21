package gethexec

import (
	"context"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"
)

func (api *KeepersFlashAPI) FilteredLogsV1(ctx context.Context, filter KeepersLogFilter) (*rpc.Subscription, error) {
	if len(filter.Topic0s) == 0 {
		return nil, keepersRPCError(-39105, "topic0s must be non-empty")
	}
	return api.service.subscribe(ctx, &filter)
}
func (api *KeepersFlashAPI) TxInclusionsV1(ctx context.Context) (*rpc.Subscription, error) {
	return api.service.subscribe(ctx, nil)
}
func (s *KeepersService) subscribe(ctx context.Context, filter *KeepersLogFilter) (*rpc.Subscription, error) {
	notifier, ok := rpc.NotifierFromContext(ctx)
	if !ok {
		return nil, rpc.ErrNotificationsUnsupported
	}
	s.mu.Lock()
	s.refreshReadyLocked(time.Now())
	if !s.ready {
		s.mu.Unlock()
		return nil, keepersRPCError(-39102, "sequencer snapshot is not ready")
	}
	sub := &keepersSubscriber{events: make(chan keepersEvent, s.config.QueueSize), failed: make(chan struct{})}
	s.subscribers[sub] = struct{}{}
	rpcSub := notifier.CreateSubscription()
	s.mu.Unlock()
	finished := make(chan struct{})
	// Notify can be blocked by the peer; failure must still close the connection.
	go func() {
		select {
		case <-sub.failed:
			notifier.Close()
		case <-finished:
			select {
			case <-sub.failed:
				notifier.Close()
			default:
			}
		}
	}()
	go func() {
		defer close(finished)
		defer func() { s.mu.Lock(); delete(s.subscribers, sub); s.mu.Unlock() }()
		for {
			select {
			case <-rpcSub.Err():
				return
			case <-sub.failed:
				return
			case event := <-sub.events:
				if event.reverted != nil && filter != nil {
					if notifier.Notify(rpcSub.ID, event.reverted) != nil {
						return
					}
				}
				var payload any
				if filter == nil {
					payload = keepersInclusionEvent(event.snapshot)
				} else {
					update := keepersFilteredEvent(event.snapshot, *filter)
					if update == nil {
						continue
					}
					payload = update
				}
				if notifier.Notify(rpcSub.ID, payload) != nil {
					return
				}
				keepersNotificationLatency.Update(time.Since(event.snapshot.received).Nanoseconds())
			}
		}
	}()
	return rpcSub, nil
}
func keepersInclusionEvent(snapshot *keepersSnapshot) keepersInclusions {
	hashes := make([]common.Hash, 0, len(snapshot.block.Transactions()))
	for _, tx := range snapshot.block.Transactions() {
		hashes = append(hashes, tx.Hash())
	}
	return keepersInclusions{keepersTuple: snapshot.tuple, BlockTimestamp: snapshot.block.Time(), FlashblockHash: snapshot.block.Hash(), TxHashes: hashes}
}
func keepersFilteredEvent(snapshot *keepersSnapshot, filter KeepersLogFilter) *keepersLogsUpdate {
	topics := make(map[common.Hash]struct{}, len(filter.Topic0s))
	for _, topic := range filter.Topic0s {
		topics[topic] = struct{}{}
	}
	addresses := make(map[common.Address]struct{}, len(filter.Addresses))
	for _, address := range filter.Addresses {
		addresses[address] = struct{}{}
	}
	transactions := make([]keepersLogTransaction, 0)
	for i, tx := range snapshot.block.Transactions() {
		if i >= len(snapshot.receipts) {
			break
		}
		logs := make([]KeepersLog, 0)
		for _, entry := range snapshot.receipts[i].Logs {
			if len(entry.Topics) == 0 {
				continue
			}
			if _, ok := topics[entry.Topics[0]]; !ok {
				continue
			}
			if len(addresses) > 0 {
				if _, ok := addresses[entry.Address]; !ok {
					continue
				}
			}
			logs = append(logs, keepersLog(entry, snapshot.block.Time(), false))
		}
		if len(logs) == 0 {
			continue
		}
		tip := tx.GasTipCap()
		if tip == nil {
			tip = new(big.Int)
		}
		transactions = append(transactions, keepersLogTransaction{TxHash: tx.Hash(), PriorityFeePerGas: (*hexutil.Big)(tip), Logs: logs})
	}
	if len(transactions) == 0 {
		return nil
	}
	fee := snapshot.block.BaseFee()
	if fee == nil {
		fee = new(big.Int)
	}
	return &keepersLogsUpdate{Type: "update", keepersTuple: snapshot.tuple, BlockTimestamp: snapshot.block.Time(), BaseFeePerGas: (*hexutil.Big)(fee), Transactions: transactions}
}
