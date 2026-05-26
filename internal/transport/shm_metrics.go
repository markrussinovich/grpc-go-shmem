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

// HTTP/2-compatible flow control is the only profile on the SHM
// transport. Earlier drafts had a NoWU mode toggled via
// ConfigureShmNoWindowUpdate / a FlowControlMode enum / a CONNECT
// Flags bit; that mode has been removed in favor of a single unified
// path that achieves NoWU-equivalent behavior simply by configuring a
// large initial window (the SHM-tuned default `shmInitialWindowSize`
// of 32 MiB, which exceeds typical per-message sizes so WINDOW_UPDATE
// emission stays dormant in production). To exercise HTTP/2-strict
// flow control set `grpc.WithInitialWindowSize` / a smaller window
// via `DialOptions.InitialWindowSize`.
//
// Counters below are kept for bench/metrics visibility.
var (
	// shmWUFramesIgnored historically counted WU frames a NoWU receiver
	// dropped. With the unified path, receivers always honor WU, so this
	// counter is no longer incremented. Retained to keep the metric
	// surface stable for tooling that already reads it.
	shmWUFramesIgnored atomic.Uint64

	// shmWUFramesBackpressured counts WU emissions that the frame writer
	// was too busy to accept synchronously (channel full + inlineMu
	// busy). On such failure the sender restores the captured delta
	// to the appropriate atomic accumulator (transport.pendingConnWU
	// or Stream.pendingWU) and signals wuRetryWake so the writer
	// loop drains the restored value on its next tick. Non-zero
	// values indicate sustained writer saturation and are a useful
	// signal for tuning frameWriter contention.
	shmWUFramesBackpressured atomic.Uint64

	// shmConnPreCreditEmitted counts the bytes of connection-level
	// WINDOW_UPDATE issued via trInFlow.maybeAdjust on a parse-time
	// onMessageStart trigger. This separates "pre-credit emitted to
	// admit a large LPM" from ordinary drip-credit traffic counted
	// elsewhere. Useful both as an observability signal (large value
	// = small-window scenarios where pre-credit is doing real work)
	// and as a regression guard (if zero in a fair-default bench
	// matrix, the pre-credit pathway is not being exercised).
	shmConnPreCreditEmitted atomic.Uint64

	// shmStreamPreCreditEmitted counts the bytes of stream-level
	// WINDOW_UPDATE issued via inFlow.maybeAdjust on onMessageStart.
	// Stream-level pre-credit is the SHM analogue of stock grpc-go's
	// "app.Read(length) triggers maybeAdjust" path; it has always
	// fired on multi-frame LPMs, this counter just makes the metric
	// visible alongside the new conn-level counter.
	shmStreamPreCreditEmitted atomic.Uint64

	// shmConnWUCoalesced counts the number of times the frame writer
	// merged two or more adjacent connection-level WINDOW_UPDATE
	// frames into a single frame within one drain pass. Non-zero
	// values indicate the coalescer is paying its keep; expected to
	// rise sharply at high stream concurrency where many onMessageStart
	// callbacks emit conn pre-credit WUs back-to-back. Each unit is
	// "one flush" (which may have absorbed N input entries), not "N
	// frames saved" — see writeLoop for the absorb/flush semantics.
	shmConnWUCoalesced atomic.Uint64
)

