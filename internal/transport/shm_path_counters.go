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
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package transport

import "sync/atomic"

// SHM write/read path counters for diagnosing ZC firing rates.
//
// These are process-global atomic counters bumped on the hot path
// (one atomic.AddUint64 per RPC). The cost is negligible — single
// L1 cache hit — and they let us prove from a benchmark whether
// a given size actually hits the zero-copy path or falls back to
// the materialise-then-chunk slow path.
//
// Reviewers asked "why isn't 1 MiB going through ZC?" — these
// counters answer that quantitatively. SHM_BENCH_CPU=1 already
// adds CPU metrics; SHM_BENCH_ZC=1 (any value works) makes the
// bench harness also dump these counters as benchmark metrics
// (zc-write-fires/op, zc-write-falls/op, zc-read-fires/op, ...).
var (
	// Write-path counters.
	//
	// shmZCWriteFire: writeProtoToRingH2 ran end-to-end — proto
	// was marshalled directly into the ring with no heap
	// intermediate. This is the true zero-copy send path.
	shmZCWriteFire uint64

	// shmZCWriteSkipBudget: writeProtoToRingH2 was attempted but
	// the message + H2 header exceeded ring/3 (the contiguous
	// budget) or exceeded the H2 16 MiB-1 single-frame ceiling.
	// Caller fell back to writeFrameBuffers.
	shmZCWriteSkipBudget uint64

	// shmZCWriteSkipMaxFrame: writeProtoToRingH2 was attempted but
	// exceeded the configurable shmMaxFrameSize (e.g., 16 KiB
	// under fair-default profile). Falls back to chunking.
	shmZCWriteSkipMaxFrame uint64

	// shmZCWriteSkipSpace: writeProtoToRingH2 was attempted but
	// the ring didn't have enough contiguous space at the
	// candidate write position (ring wrap or back-pressure).
	shmZCWriteSkipSpace uint64

	// shmZCWriteSkipQuota: writeProtoToRingH2 was attempted but
	// the stream / connection send window didn't have enough
	// quota for the whole message. Falls back to chunked write
	// under flow control.
	shmZCWriteSkipQuota uint64

	// shmZCWriteSkipInlineBusy: writeProtoToRingH2 was attempted
	// but the frameWriter's inlineMu TryLock failed (the writer
	// goroutine is currently draining its channel). Falls back to
	// the materialise-then-enqueue path.
	shmZCWriteSkipInlineBusy uint64

	// shmVectoredWriteFire: writeFrameH2Message ran — the vectored
	// fast path that writes hdr + data segments directly into one
	// ring reservation (no heap materialisation). This is the
	// fallback when writeProtoToRingH2 doesn't apply but the
	// message still fits in one H2 DATA frame.
	shmVectoredWriteFire uint64

	// shmChunkedWriteFire: writeFrameH2DataChunked ran — message
	// exceeded the per-frame limit and had to be split. Each
	// chunk pays one ring reservation + one copy from the
	// materialised heap buffer.
	shmChunkedWriteFire uint64

	// shmChunkedWriteVecFire: writeFrameH2DataChunkedVec ran —
	// vectored chunked path that emits H2 DATA frames straight from
	// (lpmHdr + mem.BufferSlice) without first materialising into a
	// single contiguous heap buffer. Saves one full producer-side
	// memcpy per LargeUnary / large MESSAGE relative to the legacy
	// shmChunkedWriteFire path.
	shmChunkedWriteVecFire uint64

	// Read-path counters (anchored at readFrameViewH2's three
	// fast paths — ZC, single-frame copy, slow accumulator).

	// shmZCReadFire: receiver returned a ring-backed mem.Buffer
	// (no copy — the application's proto.Unmarshal reads directly
	// from the ring). This is the true zero-copy receive path.
	shmZCReadFire uint64

	// shmCopyReadFire: receiver wrapped the payload in a pool
	// buffer via mem.Copy (one allocation + one memcpy).
	shmCopyReadFire uint64

	// shmAccReadFire: receiver routed through the multi-DATA-frame
	// lpmAccumulator (one allocation in the pool + one append-copy
	// per chunk). Used when the LPM spans multiple DATA frames or
	// when the candidate frame failed the ZC / single-copy guards.
	shmAccReadFire uint64

	// shmZCAnchorBudgetExceeded: BeginMultiAnchor returned nil because
	// the FIFO slot for the next sequence was still in use. The caller
	// falls back to the single-frame copy path. A high value vs
	// ZCReadFire signals the budget needs tuning (zcAnchorBudgetCount
	// in ring_zc_multi.go).
	// Note: defined as a package-level var in ring_zc_multi.go; this
	// field is the snapshot exported via ShmPathCounters.

	// Inline-write fast-path counters (anchored at
	// (*shmFrameWriter).tryInlineWrite). The inline path emits a
	// single-frame whole-message DATA frame directly from the sender
	// goroutine, bypassing the channel + writer-goroutine handoff.
	// Targets low-concurrency latency: when the writer goroutine
	// can't amortise its wake cost across a batch, the channel
	// handoff dominates the per-message wall time. At high
	// concurrency the existing channel path's batched drain is
	// strictly better, so the inline path bails immediately when
	// any pending work is observable.

	// shmInlineWriteFire: tryInlineWrite emitted the whole message
	// directly into the ring without going through the writer
	// goroutine. The sender returned immediately (no doneCh wait).
	shmInlineWriteFire uint64

	// shmInlineWriteBailLocked: TryLock(inlineMu) failed — the writer
	// goroutine is currently draining a batch. Forces channel path.
	// This is the only "Bail*" bucket that reflects real lock
	// contention; the other post-lock bails (Closed / StreamDone /
	// CtxDone) are bucketed separately so a dirty Locked count
	// doesn't shadow contention diagnosis.
	shmInlineWriteBailLocked uint64

	// shmInlineWriteBailClosed: TryLock succeeded but w.closed was
	// already set by close(). The inline path observes the closed
	// flag after the lock and bails to the channel path which
	// returns ErrConnClosing via trySend. Should be near-zero
	// outside shutdown windows.
	shmInlineWriteBailClosed uint64

	// shmInlineWriteBailStreamDone: TryLock succeeded but
	// streamPtr.getState() == streamDone — the stream was cancelled
	// or completed (e.g. server-side WriteStatus already ran)
	// between the caller's pre-checks and this point. Forces
	// channel path so processWholeMessage produces the canonical
	// errStreamDone result.
	shmInlineWriteBailStreamDone uint64

	// shmInlineWriteBailCtxDone: TryLock succeeded but ctx.Err()
	// became non-nil after the lock. The channel path also returns
	// ctx.Err() in this case; bucketing it separately keeps the
	// Locked counter pure.
	shmInlineWriteBailCtxDone uint64

	// shmInlineWriteBailQueued: w.ch had pending entries OR
	// w.deferred had blocked messages. Forces channel path so the
	// writer goroutine can preserve batch + FC-retry ordering.
	shmInlineWriteBailQueued uint64

	// shmInlineWriteBailQuota: stream / conn outbound flow-control
	// quota was insufficient for the whole message in one frame.
	// Forces channel path so the writer goroutine's chunking +
	// deferred-retry machinery can handle the partial credit.
	shmInlineWriteBailQuota uint64

	// shmInlineWriteBailFrameSize: payloadLen exceeded the negotiated
	// H2 max frame size — must chunk via the channel path.
	shmInlineWriteBailFrameSize uint64

	// shmInlineWriteBailZeroLen: zero-length payload (e.g. client
	// half-close). The channel path's specialised zero-length
	// MESSAGE handling stays canonical for this rare case.
	shmInlineWriteBailZeroLen uint64

	// shmInlinePiggybackDrain: count of frames a tryInlineWrite
	// success-path holder drained from the writer chan (w.ch) BEFORE
	// releasing inlineMu, amortising the writer-goroutine cycle for
	// those frames. Bounded at maxInlinePiggyback (8) entries per
	// inline success. High value at high concurrency = piggyback
	// working as designed; near-zero at low concurrency = chan was
	// empty (no work to amortise — which is also expected).
	// Counter shape: increments by 1 per drained entry, NOT by 1
	// per inline-write success.
	shmInlinePiggybackDrain uint64
)

// LoadShmPathCounters returns a snapshot of the SHM write/read path
// counters. The bench harness reads these to report per-op metrics
// like zc-write-fires/op.
//
// The struct intentionally mirrors the var names for one-to-one
// mapping in dumped benchmark output.
type ShmPathCounters struct {
	ZCWriteFire           uint64
	ZCWriteSkipBudget     uint64
	ZCWriteSkipMaxFrame   uint64
	ZCWriteSkipSpace      uint64
	ZCWriteSkipQuota      uint64
	ZCWriteSkipInlineBusy uint64
	VectoredWriteFire     uint64
	ChunkedWriteFire      uint64
	ChunkedWriteVecFire   uint64
	ZCReadFire             uint64
	CopyReadFire           uint64
	AccReadFire            uint64
	ZCAnchorBudgetExceeded uint64
	ZCFailPSecondNonzero   uint64
	ZCFailPFirstShort      uint64
	ZCFailAccInProgress    uint64
	ZCFailLpmMismatch      uint64
	ZCFailIneligible       uint64
	ZCEligNotContig        uint64
	ZCEligRingTooSmall     uint64
	ZCEligPayloadSmall     uint64
	ZCEligBackPressure     uint64

	InlineWriteFire              uint64
	InlineWriteBailLocked        uint64
	InlineWriteBailClosed        uint64
	InlineWriteBailStreamDone    uint64
	InlineWriteBailCtxDone       uint64
	InlineWriteBailQueued        uint64
	InlineWriteBailQuota         uint64
	InlineWriteBailFrameSize     uint64
	InlineWriteBailZeroLen       uint64

	InlinePiggybackDrain uint64
}

// LoadShmPathCounters returns a snapshot. Safe to call concurrently
// with the data plane.
func LoadShmPathCounters() ShmPathCounters {
	return ShmPathCounters{
		ZCWriteFire:           atomic.LoadUint64(&shmZCWriteFire),
		ZCWriteSkipBudget:     atomic.LoadUint64(&shmZCWriteSkipBudget),
		ZCWriteSkipMaxFrame:   atomic.LoadUint64(&shmZCWriteSkipMaxFrame),
		ZCWriteSkipSpace:      atomic.LoadUint64(&shmZCWriteSkipSpace),
		ZCWriteSkipQuota:      atomic.LoadUint64(&shmZCWriteSkipQuota),
		ZCWriteSkipInlineBusy: atomic.LoadUint64(&shmZCWriteSkipInlineBusy),
		VectoredWriteFire:     atomic.LoadUint64(&shmVectoredWriteFire),
		ChunkedWriteFire:      atomic.LoadUint64(&shmChunkedWriteFire),
		ChunkedWriteVecFire:   atomic.LoadUint64(&shmChunkedWriteVecFire),
		ZCReadFire:             atomic.LoadUint64(&shmZCReadFire),
		CopyReadFire:           atomic.LoadUint64(&shmCopyReadFire),
		AccReadFire:            atomic.LoadUint64(&shmAccReadFire),
		ZCAnchorBudgetExceeded: atomic.LoadUint64(&shmZCAnchorBudgetExceeded),
		ZCFailPSecondNonzero:   atomic.LoadUint64(&shmZCFailPSecondNonzero),
		ZCFailPFirstShort:      atomic.LoadUint64(&shmZCFailPFirstShort),
		ZCFailAccInProgress:    atomic.LoadUint64(&shmZCFailAccInProgress),
		ZCFailLpmMismatch:      atomic.LoadUint64(&shmZCFailLpmMismatch),
		ZCFailIneligible:       atomic.LoadUint64(&shmZCFailIneligible),
		ZCEligNotContig:        atomic.LoadUint64(&shmZCElig_NotContig),
		ZCEligRingTooSmall:     atomic.LoadUint64(&shmZCElig_RingTooSmall),
		ZCEligPayloadSmall:     atomic.LoadUint64(&shmZCElig_PayloadSmall),
		ZCEligBackPressure:     atomic.LoadUint64(&shmZCElig_BackPressure),

		InlineWriteFire:              atomic.LoadUint64(&shmInlineWriteFire),
		InlineWriteBailLocked:        atomic.LoadUint64(&shmInlineWriteBailLocked),
		InlineWriteBailClosed:        atomic.LoadUint64(&shmInlineWriteBailClosed),
		InlineWriteBailStreamDone:    atomic.LoadUint64(&shmInlineWriteBailStreamDone),
		InlineWriteBailCtxDone:       atomic.LoadUint64(&shmInlineWriteBailCtxDone),
		InlineWriteBailQueued:        atomic.LoadUint64(&shmInlineWriteBailQueued),
		InlineWriteBailQuota:         atomic.LoadUint64(&shmInlineWriteBailQuota),
		InlineWriteBailFrameSize:     atomic.LoadUint64(&shmInlineWriteBailFrameSize),
		InlineWriteBailZeroLen:       atomic.LoadUint64(&shmInlineWriteBailZeroLen),

		InlinePiggybackDrain: atomic.LoadUint64(&shmInlinePiggybackDrain),
	}
}

// Sub returns the difference between two snapshots (after - before).
// Useful for reporting "per-iteration" rates in benchmarks.
func (a ShmPathCounters) Sub(before ShmPathCounters) ShmPathCounters {
	return ShmPathCounters{
		ZCWriteFire:           a.ZCWriteFire - before.ZCWriteFire,
		ZCWriteSkipBudget:     a.ZCWriteSkipBudget - before.ZCWriteSkipBudget,
		ZCWriteSkipMaxFrame:   a.ZCWriteSkipMaxFrame - before.ZCWriteSkipMaxFrame,
		ZCWriteSkipSpace:      a.ZCWriteSkipSpace - before.ZCWriteSkipSpace,
		ZCWriteSkipQuota:      a.ZCWriteSkipQuota - before.ZCWriteSkipQuota,
		ZCWriteSkipInlineBusy: a.ZCWriteSkipInlineBusy - before.ZCWriteSkipInlineBusy,
		VectoredWriteFire:     a.VectoredWriteFire - before.VectoredWriteFire,
		ChunkedWriteFire:      a.ChunkedWriteFire - before.ChunkedWriteFire,
		ChunkedWriteVecFire:   a.ChunkedWriteVecFire - before.ChunkedWriteVecFire,
		ZCReadFire:             a.ZCReadFire - before.ZCReadFire,
		CopyReadFire:           a.CopyReadFire - before.CopyReadFire,
		AccReadFire:            a.AccReadFire - before.AccReadFire,
		ZCAnchorBudgetExceeded: a.ZCAnchorBudgetExceeded - before.ZCAnchorBudgetExceeded,
		ZCFailPSecondNonzero:   a.ZCFailPSecondNonzero - before.ZCFailPSecondNonzero,
		ZCFailPFirstShort:      a.ZCFailPFirstShort - before.ZCFailPFirstShort,
		ZCFailAccInProgress:    a.ZCFailAccInProgress - before.ZCFailAccInProgress,
		ZCFailLpmMismatch:      a.ZCFailLpmMismatch - before.ZCFailLpmMismatch,
		ZCFailIneligible:       a.ZCFailIneligible - before.ZCFailIneligible,
		ZCEligNotContig:        a.ZCEligNotContig - before.ZCEligNotContig,
		ZCEligRingTooSmall:     a.ZCEligRingTooSmall - before.ZCEligRingTooSmall,
		ZCEligPayloadSmall:     a.ZCEligPayloadSmall - before.ZCEligPayloadSmall,
		ZCEligBackPressure:     a.ZCEligBackPressure - before.ZCEligBackPressure,

		InlineWriteFire:              a.InlineWriteFire - before.InlineWriteFire,
		InlineWriteBailLocked:        a.InlineWriteBailLocked - before.InlineWriteBailLocked,
		InlineWriteBailClosed:        a.InlineWriteBailClosed - before.InlineWriteBailClosed,
		InlineWriteBailStreamDone:    a.InlineWriteBailStreamDone - before.InlineWriteBailStreamDone,
		InlineWriteBailCtxDone:       a.InlineWriteBailCtxDone - before.InlineWriteBailCtxDone,
		InlineWriteBailQueued:        a.InlineWriteBailQueued - before.InlineWriteBailQueued,
		InlineWriteBailQuota:         a.InlineWriteBailQuota - before.InlineWriteBailQuota,
		InlineWriteBailFrameSize:     a.InlineWriteBailFrameSize - before.InlineWriteBailFrameSize,
		InlineWriteBailZeroLen:       a.InlineWriteBailZeroLen - before.InlineWriteBailZeroLen,

		InlinePiggybackDrain: a.InlinePiggybackDrain - before.InlinePiggybackDrain,
	}
}
