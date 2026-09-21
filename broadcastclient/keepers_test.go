package broadcastclient

import (
	"context"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/offchainlabs/nitro/broadcaster"
	"github.com/offchainlabs/nitro/util/signature"
	"github.com/offchainlabs/nitro/wsbroadcastserver"
)

func TestKeepersApplicationIdleReconnectsDespitePings(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	config := wsbroadcastserver.DefaultTestBroadcasterConfig
	config.Ping = 20 * time.Millisecond
	config.ClientTimeout = 5 * time.Second
	key, err := crypto.GenerateKey()
	Require(t, err)
	sequencer := crypto.PubkeyToAddress(key.PublicKey)
	failures := make(chan error, 10)
	server := broadcaster.NewBroadcaster(func() *wsbroadcastserver.BroadcasterConfig { return &config }, 8742, failures, signature.DataSignerFromPrivateKey(key))
	Require(t, server.Initialize())
	Require(t, server.Start(ctx))
	defer server.StopAndWait()
	settings := DefaultTestConfig
	settings.Timeout = time.Second
	settings.ApplicationTimeout = 100 * time.Millisecond
	client, err := newTestBroadcastClient(settings, server.ListenerAddr(), 8742, 0, nil, nil, failures, &sequencer, t)
	Require(t, err)
	client.Start(ctx)
	defer client.StopAndWait()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if client.GetRetryCount() > 0 {
				return
			}
		case err := <-failures:
			t.Fatal(err)
		case <-deadline.C:
			t.Fatal("ping/pong kept an idle feed connected")
		}
	}
}
