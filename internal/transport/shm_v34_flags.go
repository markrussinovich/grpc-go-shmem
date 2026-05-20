//go:build linux || windows

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
	"sync/atomic"
)

// SHM v3.4 feature flags. See shm-rfc/design-loopywriter-v3.4-final.md.
//
// Phase 1a: "no WINDOW_UPDATE" flow control. Default is ON in v3.4 —
// the SHM transport's ring backpressure subsumes the role of HTTP/2
// WINDOW_UPDATE frames. Both peers MUST be in the same mode (either
// both no-WU or both WU). When enabled:
//   - sender skips acquireSendQuota (treats quota as unlimited)
//   - sender does NOT emit WINDOW_UPDATE frames
//   - receiver ignores incoming WINDOW_UPDATE frames (NOP)
//   - flow control is implicit via ring backpressure
//   - per-stream backpressure is via recvHardCap with RST_STREAM (P1b)
//
// In P1a the recvHardCap mechanism is not yet implemented; the ring's
// natural backpressure is the only limit. A slow consumer will
// eventually fill the ring -> sender blocks. This is the simplest
// possible no-WU model.
//
// Tests / benchmarks that want to compare SHM with HTTP/2-style WU
// flow control can flip the mode off via ConfigureShmNoWindowUpdate.
var shmNoWUEnabled atomic.Bool

func init() {
	// Default ON: v3.4 baseline.
	shmNoWUEnabled.Store(true)
}

// shmNoWU returns true when the v3.4 no-WU flow control model is active.
// Both peers must be in the same state. When set, the sender skips
// quota tracking and the receiver ignores WINDOW_UPDATE.
func shmNoWU() bool {
	return shmNoWUEnabled.Load()
}

// ConfigureShmNoWindowUpdate enables or disables the v3.4 no-WINDOW_UPDATE
// flow control model. Both peers MUST be in the same state before any
// SHM dial / listen; mixing modes between peers is a protocol error
// (P1b will negotiate via the segment header version field).
//
// Callers MUST invoke this before constructing any SHM transport; the
// values are read on every send/recv but the per-peer decision is made
// at handshake-equivalent time.
//
// Notice: This API is EXPERIMENTAL and may be changed or removed in a
// later release.
func ConfigureShmNoWindowUpdate(enabled bool) {
	shmNoWUEnabled.Store(enabled)
}

// ResetShmNoWindowUpdateForBench restores the default no-WU state
// (ON). Tests and benchmarks should defer this so subsequent tests
// in the same `go test` invocation do not inherit the override.
//
// Notice: This API is EXPERIMENTAL and may be changed or removed in a
// later release.
func ResetShmNoWindowUpdateForBench() {
	shmNoWUEnabled.Store(true)
}

// Counters for the bench banner / metrics.
var (
	shmWUFramesIgnored atomic.Uint64 // receiver-side: WU frames received and dropped
	shmWUFramesElided  atomic.Uint64 // sender-side: WU frames that would have been emitted but were not
	shmQuotaSkips      atomic.Uint64 // sender-side: acquireSendQuota calls that returned immediately
)
