package gethexec

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/arbitrum"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/offchainlabs/nitro/arbutil"
)

var (
	keepersFeedToSnapshot      = metrics.NewRegisteredHistogram("keepers/feed/to_snapshot_duration_ns", nil, metrics.NewBoundedHistogramSample())
	keepersNotificationLatency = metrics.NewRegisteredHistogram("keepers/subscriptions/notify_duration_ns", nil, metrics.NewBoundedHistogramSample())
	keepersInvalidations       = metrics.NewRegisteredCounter("keepers/snapshot/invalidations", nil)
	keepersOverflows           = metrics.NewRegisteredCounter("keepers/subscriptions/overflow", nil)
	keepersPendingRequests     = metrics.NewRegisteredCounter("keepers/simulate/pending", nil)
	keepersFallbackRequests    = metrics.NewRegisteredCounter("keepers/simulate/fallback", nil)
	keepersHeadAge             = metrics.NewRegisteredGauge("keepers/snapshot/age_ms", nil)
	keepersFeedAge             = metrics.NewRegisteredGauge("keepers/feed/age_ms", nil)
	keepersExecutionLag        = metrics.NewRegisteredGauge("keepers/feed/execution_lag_blocks", nil)
	keepersLatency             = metrics.NewRegisteredHistogram("keepers/simulate/duration_ns", nil, metrics.NewBoundedHistogramSample())
	keepersPublishLatency      = metrics.NewRegisteredHistogram("keepers/snapshot/publish_duration_ns", nil, metrics.NewBoundedHistogramSample())
)

// Counter ranges are reserved with fsync before use. Crashes may skip IDs but
// cannot reuse them. The containing datadir must not be shared by two nodes.
type keepersCounter struct {
	path        string
	next, limit uint64
}

func newKeepersCounter(path string) (*keepersCounter, error) {
	c := &keepersCounter{path: path}
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if err == nil {
		if len(data) != 8 {
			return nil, errors.New("invalid Keepers payload counter")
		}
		c.next = binary.BigEndian.Uint64(data)
	}
	c.limit = c.next
	return c, nil
}
func (c *keepersCounter) allocate() (string, error) {
	if c.next == c.limit {
		if c.limit > math.MaxUint64-65536 {
			return "", errors.New("Keepers payload counter exhausted")
		}
		limit := c.limit + 65536
		if err := os.MkdirAll(filepath.Dir(c.path), 0700); err != nil {
			return "", err
		}
		f, err := os.CreateTemp(filepath.Dir(c.path), ".keepers-counter-*")
		if err != nil {
			return "", err
		}
		name := f.Name()
		defer os.Remove(name)
		data := make([]byte, 8)
		binary.BigEndian.PutUint64(data, limit)
		if _, err = f.Write(data); err != nil {
			f.Close()
			return "", err
		}
		if err = f.Sync(); err != nil {
			f.Close()
			return "", err
		}
		if err = f.Close(); err != nil {
			return "", err
		}
		if err = os.Rename(name, c.path); err != nil {
			return "", err
		}
		dir, err := os.Open(filepath.Dir(c.path))
		if err != nil {
			return "", err
		}
		err = dir.Sync()
		dir.Close()
		if err != nil {
			return "", err
		}
		c.limit = limit
	}
	c.next++
	return fmt.Sprintf("0x%016x", c.next), nil
}

type keepersSnapshot struct {
	tuple        keepersTuple
	revision     uint64
	block        *types.Block
	receipts     types.Receipts
	post, parent *state.StateDB
	received     time.Time
}
type keepersEvent struct {
	snapshot *keepersSnapshot
	reverted *keepersReverted
}
type keepersSubscriber struct {
	events chan keepersEvent
	failed chan struct{}
}
type keepersFeed struct {
	healthy  bool
	seen     time.Time
	sequence uint64
}

// KeepersService owns immutable snapshots; all execution uses private StateDB
// copies. Publishing never calls network writers or waits on subscribers.
type KeepersService struct {
	config      KeepersConfig
	backend     *arbitrum.APIBackend
	chain       *core.BlockChain
	engine      *ExecutionEngine
	mu          sync.Mutex
	counter     *keepersCounter
	latest      *keepersSnapshot
	previous    *keepersSnapshot
	revision    uint64
	ready       bool
	feeds       map[string]keepersFeed
	subscribers map[*keepersSubscriber]struct{}
	slots       chan struct{}
	cancel      context.CancelFunc
	done        chan struct{}
}

func newKeepersService(config KeepersConfig, backend *arbitrum.APIBackend, engine *ExecutionEngine, counterPath string) (*KeepersService, error) {
	if config.Concurrency < 1 || config.MaxBundle < 1 || config.QueueSize < 1 || config.Timeout <= 0 || config.MaxHeadAge <= 0 {
		return nil, errors.New("invalid Keepers service limits")
	}
	counter, err := newKeepersCounter(counterPath)
	if err != nil {
		return nil, err
	}
	return &KeepersService{config: config, backend: backend, chain: backend.BlockChain(), engine: engine, counter: counter, feeds: make(map[string]keepersFeed), subscribers: make(map[*keepersSubscriber]struct{}), slots: make(chan struct{}, config.Concurrency)}, nil
}
func (s *KeepersService) start(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	s.done = make(chan struct{})
	go func() {
		defer close(s.done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				s.mu.Lock()
				s.invalidateLocked(true)
				s.mu.Unlock()
				return
			case <-ticker.C:
				s.mu.Lock()
				s.refreshReadyLocked(time.Now())
				s.mu.Unlock()
			}
		}
	}()
}
func (s *KeepersService) stop() {
	if s.cancel != nil {
		s.cancel()
		<-s.done
	}
}
func (s *KeepersService) invalidateLocked(closeSubscribers bool) {
	if s.ready {
		keepersInvalidations.Inc(1)
	}
	s.ready = false
	s.revision++
	if closeSubscribers {
		s.latest = nil
	}
	if closeSubscribers {
		for sub := range s.subscribers {
			close(sub.failed)
			delete(s.subscribers, sub)
		}
	}
}
func (s *KeepersService) refreshReadyLocked(now time.Time) {
	healthy := false
	var newest time.Time
	var highest uint64
	for _, feed := range s.feeds {
		if feed.seen.After(newest) {
			newest = feed.seen
		}
		if feed.healthy && now.Sub(feed.seen) <= s.config.MaxHeadAge {
			healthy = true
			if feed.sequence > highest {
				highest = feed.sequence
			}
		}
	}
	if !newest.IsZero() {
		keepersFeedAge.Update(now.Sub(newest).Milliseconds())
	}
	fresh := false
	if s.latest != nil {
		age := now.Sub(time.Unix(int64(s.latest.block.Time()), 0))
		keepersHeadAge.Update(age.Milliseconds())
		fresh = age <= s.config.MaxHeadAge && age >= -time.Minute
		feedBlock := s.engine.MessageIndexToBlockNumber(arbutil.MessageIndex(highest))
		if feedBlock > s.latest.block.NumberU64() {
			keepersExecutionLag.Update(int64(feedBlock - s.latest.block.NumberU64()))
		} else {
			keepersExecutionLag.Update(0)
		}
	}
	if !healthy || !fresh {
		if s.ready {
			s.invalidateLocked(true)
		}
		return
	}
	s.ready = true
}

func (s *KeepersService) feedStatus(source string, healthy bool, sequence uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	feed := s.feeds[source]
	feed.healthy = healthy
	if healthy {
		feed.seen = time.Now()
		feed.sequence = sequence
	}
	s.feeds[source] = feed
	s.refreshReadyLocked(time.Now())
}

// Called with the execution engine's createBlocksMutex, before rewinding state.
func (s *KeepersService) reorg() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.previous = s.latest
	s.latest = nil
	s.invalidateLocked(false)
}

// Called only after the block and receipts have successfully entered the local chain.
func (s *KeepersService) publish(block *types.Block, receipts types.Receipts) {
	start := time.Now()
	defer func() { keepersPublishLatency.Update(time.Since(start).Nanoseconds()) }()
	if start.Sub(time.Unix(int64(block.Time()), 0)) > s.config.MaxHeadAge {
		s.mu.Lock()
		s.invalidateLocked(true)
		s.latest = nil
		s.mu.Unlock()
		return
	}
	post, err := s.chain.StateAt(block.Root())
	if err != nil {
		s.failSnapshot(err)
		return
	}
	var parent *state.StateDB
	if block.NumberU64() > s.chain.Config().ArbitrumChainParams.GenesisBlockNum {
		header := s.chain.GetHeader(block.ParentHash(), block.NumberU64()-1)
		if header == nil {
			s.failSnapshot(errors.New("snapshot parent missing"))
			return
		}
		parent, err = s.chain.StateAt(header.Root)
		if err != nil {
			s.failSnapshot(err)
			return
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id, err := s.counter.allocate()
	if err != nil {
		s.invalidateLocked(true)
		log.Error("Keepers snapshot allocation failed", "err", err)
		return
	}
	s.revision++
	snapshot := &keepersSnapshot{tuple: keepersTuple{PayloadID: id, BlockNumber: block.NumberU64()}, revision: s.revision, block: block, receipts: receipts, post: post, parent: parent, received: start}
	previous := s.previous
	s.previous = nil
	if previous == nil && s.latest != nil && block.ParentHash() != s.latest.block.Hash() {
		previous = s.latest
	}
	s.latest = snapshot
	s.refreshReadyLocked(time.Now())
	if !s.ready {
		return
	}
	// Stream queues retain metadata, not complete StateDB overlays.
	streamSnapshot := *snapshot
	streamSnapshot.post = nil
	streamSnapshot.parent = nil
	var newestFeed time.Time
	for _, feed := range s.feeds {
		if feed.healthy && feed.seen.After(newestFeed) {
			newestFeed = feed.seen
		}
	}
	if !newestFeed.IsZero() {
		keepersFeedToSnapshot.Update(time.Since(newestFeed).Nanoseconds())
	}
	event := keepersEvent{snapshot: &streamSnapshot}
	if previous != nil {
		event.reverted = &keepersReverted{Type: "reverted", PreviousPayloadID: previous.tuple.PayloadID, PreviousIndex: previous.tuple.Index, PreviousBlockNumber: previous.tuple.BlockNumber, CurrentPayloadID: id, CurrentBlockNumber: block.NumberU64()}
	}
	s.broadcastLocked(event)
}

// The execution publisher must never wait for a client to drain its queue.
func (s *KeepersService) broadcastLocked(event keepersEvent) {
	for sub := range s.subscribers {
		select {
		case sub.events <- event:
		default:
			keepersOverflows.Inc(1)
			close(sub.failed)
			delete(s.subscribers, sub)
		}
	}
}
func (s *KeepersService) failSnapshot(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invalidateLocked(true)
	s.latest = nil
	log.Warn("Keepers snapshot unavailable", "err", err)
}

func (s *KeepersService) snapshot() (*keepersSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshReadyLocked(time.Now())
	if !s.ready || s.latest == nil {
		return nil, keepersRPCError(-39102, "sequencer snapshot is not ready")
	}
	// Copies are made while holding the lock because StateDB.Copy itself is not
	// safe to call concurrently on the same StateDB.
	snapshot := *s.latest
	snapshot.post = s.latest.post.Copy()
	if snapshot.parent != nil {
		snapshot.parent = s.latest.parent.Copy()
	}
	return &snapshot, nil
}
func (s *KeepersService) unchanged(snapshot *keepersSnapshot) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshReadyLocked(time.Now())
	return s.ready && s.latest != nil && s.revision == snapshot.revision && s.latest.block.Hash() == snapshot.block.Hash()
}

// Execution and consensus run in the same process in the supplied deployment.
// This optional observer leaves the upstream execution wire protocol unchanged.
func (n *ExecutionNode) KeepersFeedStatus(source string, healthy bool, sequence uint64) {
	if n.Keepers != nil {
		n.Keepers.feedStatus(source, healthy, sequence)
	}
}
