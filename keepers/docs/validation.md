# Implementation validation — September 21, 2026

Verified locally on macOS ARM64 with Go 1.25 and the pinned native Stylus library:

- Complete `cmd/nitro` executable builds.
- Geth RPC suite passes, including direct Keepers subscription/unsubscribe and
  notification envelopes. Native signed-input conversion and opt-in base-fee
  relaxation tests pass.
- Keepers execution, system, and feed tests pass under the Go race detector.
- ArbOS 61 integration tests exercise sequential dependent bundles, concurrent
  request isolation, signed inputs, revert/halt results, canonical start/middle/end
  insertion, prepared pending replay, both direct WebSocket stream families, and
  a real L1 retryable submission/auto-redeem prefix.
- Unit tests cover persisted counter restart/corruption/overflow, tuple errors,
  revision invalidation on reorg, subscriber overflow, input rejection, gas
  defaults, filtered-log transaction ordering, and result JSON shape.
- Application-idle feed test reconnects despite continuing ping/pong traffic.
- Nitro's genesis generator reproduces Robinhood's public block 0 exactly:
  `0xaad15f3d702aaea00caf3e9bb56395efe9127bc3b31b24921abf1eee3409305c`.
- Public config/genesis checksums, script syntax, and Python syntax pass.

The integration tests use local test chains. Mainnet shadow sync, a sustained
reference-node comparison, configured historical replay range, production load
percentiles, and server rollout remain operational acceptance gates. No mainnet
performance numbers or production-readiness claim is inferred from unit tests.
The optional OP endpoint checker validates shared result shape; a deployed OP/Nitro
golden-fixture acceptance run still belongs to the shadow rollout.
