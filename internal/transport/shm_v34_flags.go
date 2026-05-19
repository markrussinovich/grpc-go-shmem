/*
 *
 * Copyright 2026 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 */

package transport

import (
	"os"
	"sync/atomic"
)

// SHM v3.4 feature flags. See shm-rfc/design-loopywriter-v3.4-final.md.
//
// Phase 1a: env-var-gated "no WU" path. Both peers must agree (via env var
// today; via segment header version bump in a future iteration). When
// enabled:
//   - sender skips acquireSendQuota (treats quota as unlimited)
//   - sender does NOT emit WINDOW_UPDATE frames
//   - receiver ignores incoming WINDOW_UPDATE frames (NOP)
//   - flow control is implicit via ring backpressure
//   - per-stream backpressure is via recvHardCap with RST_STREAM (P1b)
//
// In P1a the recvHardCap mechanism is not yet implemented; the ring's
// natural backpressure is the only limit. A slow consumer will eventually
// fill the ring → sender blocks. This is the simplest possible no-WU
// model, suitable for the first bench run.
var shmNoWUEnabled atomic.Bool

func init() {
	if v := os.Getenv("SHM_NO_WU"); v == "1" || v == "true" {
		shmNoWUEnabled.Store(true)
	}
}

// shmNoWU returns true when the v3.4 no-WU flow control model is active.
// Both peers must agree (set the env var on both sides). When set, the
// sender skips quota tracking and the receiver ignores WINDOW_UPDATE.
func shmNoWU() bool {
	return shmNoWUEnabled.Load()
}

// Counters for the bench banner / metrics.
var (
	shmWUFramesIgnored atomic.Uint64 // receiver-side: WU frames received and dropped
	shmWUFramesElided  atomic.Uint64 // sender-side: WU frames that would have been emitted but were not
	shmQuotaSkips      atomic.Uint64 // sender-side: acquireSendQuota calls that returned immediately
)
