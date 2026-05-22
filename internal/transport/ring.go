//go:build linux || windows

/*
 *
 * Copyright 2025 gRPC authors.
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

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"runtime"
	"sync/atomic"
	"time"
	"unsafe"
	_ "unsafe" // for go:linkname
)

// runtime_procyield yields the processor, executing a PAUSE instruction on x86.
// This is more efficient than runtime.Gosched() for short spin waits as it:
// - Hints to the CPU that we're in a spin loop
// - Reduces power consumption
// - Allows hyperthreaded sibling to make progress
// cycles is typically 1 for a single PAUSE.
//
//go:linkname runtime_procyield runtime.procyield
//revive:disable:var-naming This name must match the Go runtime internal function
func runtime_procyield(cycles uint32)

// Spin-wait constants are defined in platform-specific files:
//   shm_spin_linux.go  — low values (Linux futex costs ~1-2µs)
//   shm_spin_windows.go — high values (Windows WaitOnAddress/cgocall costs ~40µs)

var shmDebugEnabled = os.Getenv("GRPC_SHM_DEBUG") != ""

func shmDebugf(format string, args ...any) {
	if !shmDebugEnabled {
		return
	}
	log.Printf(format, args...)
}

// ErrRingClosed indicates that the ring has been closed for writing
var ErrRingClosed = errors.New("ring closed")

// RingState represents a snapshot of ring buffer state for debugging and diagnostics
type RingState struct {
	Capacity     uint64 // Total ring capacity
	Widx         uint64 // Current write index (monotonic)
	Ridx         uint64 // Current read index (monotonic)
	Used         uint64 // Bytes currently in ring (Widx - Ridx)
	DataSeq      uint32 // Data availability sequence number
	SpaceSeq     uint32 // Space availability sequence number
	ContigSeq    uint32 // Contiguity sequence number (readSeq equivalent)
	Closed       uint32 // Ring closed flag (0 = open, 1 = closed)
	DataWaiters  uint32 // Number of readers waiting for data
	SpaceWaiters uint32 // Number of writers waiting for space
}

// ShmRing represents a single-producer single-consumer (SPSC) ring buffer
// operating over shared memory with event-driven blocking.
//
// This implementation provides high-performance cross-process communication
// with zero-copy operations and minimal kernel calls through futex-based
// synchronization.
type ShmRing struct {
	capMask  uint64  // capacity-1 for fast masking (capacity must be power of 2)
	capacity uint64  // actual data area capacity in bytes
	hdrOff   uintptr // base address of RingHeader in mmapped bytes
	dataOff  uintptr // base address of data area
	mem      []byte  // the mmapped region (no copying)
	closed   uint32  // atomic flag: 1 if this ring has been closed locally

	// pendingReadIdx tracks how far we've read (but not committed) in the ring.
	// This is process-local (not in shared memory) and allows the reader to
	// continue reading new frames while holding references to uncommitted buffers.
	// The shared readIdx only advances when buffers are freed.
	// Access via atomic operations (single reader in SPSC design).
	pendingReadIdx uint64

	// dataSegWaker is the per-data-segment per-direction eventfd
	// waker (2 eventfds per segment, 1 read fd held per side per
	// direction), propagated from the owning Segment at RegisterRing
	// time. Used by waitFor*/signal* when non-nil; otherwise falls
	// through to the per-address eventfd registry / Windows events /
	// Linux futex path. Always nil for control segments and when
	// the eventfd waker is disabled.
	dataSegWaker *shmDataSegWaker

	// Adaptive spin state for minimizing latency on fast paths.
	// These are process-local and help tune spin duration based on workload.
	dataSpinCutoff  uint32 // Current spin iterations for waiting on data
	spaceSpinCutoff uint32 // Current spin iterations for waiting on space

	// Pre-allocated commit context for read operations (reads are single-threaded).
	readCommit ReadCommit

	// events holds Windows named event handles for cross-mapping synchronization.
	// On Linux, this is nil and futex is used directly.
	events *RingEvents

	// batchDepth tracks nested batch-write operations. When > 0, Commit
	// suppresses DataSequence increments and reader wakeups. The final
	// EndBatch call performs a single increment + signal, amortizing the
	// cost of multiple frame writes.
	batchDepth uint32

	// h2Enc / h2Dec are the per-ring HPACK encoder/decoder state used by
	// the HTTP/2 wire codec for data-plane traffic. The encoder is
	// single-threaded (writer goroutine via inlineMu); the decoder is
	// single-threaded (reader goroutine in processIncomingData). Lazily
	// initialised by h2Encoder() / h2Decoder() on first use; control
	// rings (which never carry data-plane gRPC frames) never allocate
	// these.
	h2Enc *hpackEncoderHolder
	h2Dec *hpackDecoderHolder

	// ===== Position-aware speculative ZC protection =====
	//
	// Problem: SpeculativeReservedBytes is a count, not a position; it
	// cannot tell the writer WHERE the held bytes are. If a non-ZC frame
	// commits AFTER a ZC frame, header.ReadIdx advances past the ZC
	// region's tail, and the writer's available-space formula allows
	// wrapping onto still-held bytes.
	//
	// Solution (ported from grpc-dotnet-shm): don't advance the SHARED
	// header.ReadIdx while a ZC frame is in flight. Reader keeps its
	// progress in zcDeferredTarget; on ZC release the deferred value is
	// published to header.ReadIdx in one shot. Cross-process writer
	// reads header.ReadIdx normally and is correct without knowing
	// anything about ZC.
	//
	// Invariants enforced by callers:
	//   - At most ONE ZC in flight per ring (gated by IsSpeculativeZCEligible).
	//   - BeginZcReservation/EndZcReservation are paired exactly once per ZC.
	//   - The reader-side processIncomingData loop is single-threaded;
	//     ReadCommit.Commit is only called from that thread or from
	//     EndZcReservation (consumer side).
	//
	// Accessed via atomic.LoadUint32 / atomic.StoreUint32 with explicit
	// fencing rather than the sync/atomic.Bool wrapper to make the
	// release/acquire ordering explicit at every site.
	zcActive         uint32 // 1 while a speculative-ZC anchor is held
	zcDeferredTarget uint64 // furthest absolute idx wanting commit while zcActive
	// zcInFlight tracks the number of ZC buffers held by the consumer
	// for the current chain. Decremented by zcReleasePool.Put on each
	// buffer Free. When it reaches 0 AND the codec has closed the chain
	// (chainOpen=false), EndZcReservation fires.
	zcInFlight int64
	// chainOpen tracks codec-side chain state: 1 between the codec
	// emitting the first chunk of a multi-frame message and the final
	// (!MORE) chunk. zcReleasePool.Put gates EndZcReservation on
	// (zcInFlight == 0 && chainOpen == 0) so a partial chain can
	// never publish ReadIdx mid-message.
	chainOpen uint32
	// chainCopyMode tracks the codec-side per-message decision: the
	// codec rejected chain ZC for the in-progress multi-frame message
	// and will copy all remaining chunks. Cleared on the message's
	// final (!MORE) chunk so the next message starts fresh.
	chainCopyMode uint32
}

// ReadCommit holds the state needed to commit a read operation.
// This is embedded in ShmRing to avoid allocation on every ReadSlices call.
type ReadCommit struct {
	ring          *ShmRing
	commitReadIdx uint64
	maxBytes      int
}

// Commit advances the shared read index to free space for the writer.
// consumed must not exceed maxBytes.
// Safe to call after the ring is closed - silently does nothing.
//
// While a speculative-ZC anchor is held (BeginZcReservation called and not
// yet matched by EndZcReservation), Commit defers the shared-memory write
// and instead bumps the local zcDeferredTarget. EndZcReservation publishes
// the accumulated target in one shot. This ensures the cross-process
// writer never sees a header.ReadIdx that points inside a still-held ZC
// region.
//
// CRITICAL: Use ADDITIVE accumulation here (target += consumed), not the
// absolute (baseCommitReadIdx + consumed) formula used in the non-deferred
// branch. While zcActive is true, header.ReadIdx is FROZEN at the ZC
// frame's start, so every subsequent ReadSlices captures the STALE
// header.ReadIdx as its commitReadIdx. A naive (staleBase + perFrameBytes)
// formula yields a value at most ~one frame past baseZc — far behind the
// actual cumulative consumed position once several frames have been
// parsed. The reader thread is single-threaded so additive accumulation
// is race-free on the local field.
func (rc *ReadCommit) Commit(consumed int) {
	if consumed < 0 || consumed > rc.maxBytes {
		return // Invalid consumption, ignore
	}

	// Check if the ring has been closed before accessing shared memory.
	// This is critical because Commit may be called from a buffer's Free()
	// callback after the transport has been closed and the shared memory
	// segment unmapped. Accessing shared memory after unmap causes SIGSEGV.
	// The atomic load is cheap (~1ns) and avoids the crash.
	if rc.ring == nil || atomic.LoadUint32(&rc.ring.closed) != 0 {
		return
	}

	// ZC-active path: defer the shared-memory write; advance
	// zcDeferredTarget by the bytes the caller is committing. Reader is
	// single-threaded so the additive accumulation is race-free on the
	// local field. EndZcReservation reads this and publishes in one shot.
	if atomic.LoadUint32(&rc.ring.zcActive) != 0 {
		atomic.AddUint64(&rc.ring.zcDeferredTarget, uint64(consumed))
		return
	}

	hdr := rc.ring.header()

	newRI := rc.commitReadIdx + uint64(consumed)
	// Advance shared read index (release-publish) - frees space for writer.
	// CAS loop ensures ReadIdx only moves forward — a concurrent
	// EndZcReservation publish must not be regressed by a deferred
	// Commit that captured an earlier baseCommitReadIdx.
	for {
		current := hdr.ReadIndex()
		if newRI <= current {
			// Already past this point — nothing to do.
			break
		}
		if hdr.CompareAndSwapReadIndex(current, newRI) {
			break
		}
	}

	// Contiguity: only bump and signal when a writer is actually waiting.
	// Skipping the atomic increment when no waiters exist saves ~1-2% overhead.
	if consumed > 0 && hdr.ContigWaiters() > 0 {
		hdr.IncrementContigSequence()
		rc.ring.signalContig(&hdr.contigSeq)
	}
	// Space: always wake waiters if any are waiting.
	// This is conservative but avoids potential races in the waiter registration.
	if consumed > 0 && hdr.SpaceWaiters() > 0 {
		hdr.IncrementSpaceSequence()
		rc.ring.signalSpace(&hdr.spaceSeq)
	}
}

// ===== Speculative ZC deferred-publish protocol (ported from grpc-dotnet-shm) =====
//
// Single source of truth: ZC safety is achieved on the READER side by
// deferring header.ReadIdx advancement while a ZC frame is in flight. The
// writer's plain `used = writeIdx - readIdx` formula is automatically
// correct — no shared-memory ZC field, no protocol change.
//
// Invariants enforced by callers:
//   - At most ONE ZC in flight per ring (gated by IsSpeculativeZCEligible).
//   - BeginZcReservation/EndZcReservation are paired exactly once per ZC.
//   - The reader-side processIncomingData loop is single-threaded;
//     ReadCommit.Commit is only called from that thread or from
//     EndZcReservation (consumer side).

// BeginZcReservation begins a speculative-zero-copy reservation. Subsequent
// ReadCommit.Commit calls on this ring will be deferred (do not touch the
// shared header.ReadIdx) until EndZcReservation is called.
//
// baseIdx is the absolute read index at which the ZC frame starts. The ZC
// frame's own Commit (called right after BeginZcReservation) bumps the
// deferred target by the frame size.
//
// Pair every call with EndZcReservation via the buffer's Free() callback.
//
// Ordering: write the target FIRST, then set zcActive. The atomic store
// on zcActive provides a release barrier so any reader-thread Commit that
// observes zcActive=true (acquire load) will see the initialised target,
// never a leftover stale value from the previous ZC cycle.
func (r *ShmRing) BeginZcReservation(baseIdx uint64) {
	atomic.StoreUint64(&r.zcDeferredTarget, baseIdx)
	atomic.StoreUint32(&r.zcActive, 1)
}

// BeginSingleFrameZcCommit is the single-frame fast path: fuses
// BeginZcReservation + the frame's own deferred Commit bump into one
// atomic sequence. The reader thread is single-threaded so no other
// Commit can race between Begin and the frame's own deferred bump; we
// therefore set zcDeferredTarget directly to its post-frame value
// instead of doing the standard
//
//	(write base) → (read base) → (write base+totalBytes)
//
// triple-step. Saves 1 Load and 1 Store per single-frame ZC compared to
// the two-call sequence.
//
// Only safe when:
//   - This is a SINGLE-frame ZC anchor (no chain follows). Multi-frame
//     chains must use the separate BeginZcReservation + per-frame Commit
//     sequence so that intervening non-chain frames committed during the
//     chain hold are also captured into zcDeferredTarget.
//   - At-most-one-ZC FIFO invariant holds (caller already verified
//     IsSpeculativeZCEligible).
func (r *ShmRing) BeginSingleFrameZcCommit(baseIdx uint64, totalBytes int) {
	atomic.StoreUint64(&r.zcDeferredTarget, baseIdx+uint64(totalBytes))
	atomic.StoreUint32(&r.zcActive, 1)
}

// EndZcReservation ends the in-flight ZC reservation: publishes the
// deferred read index to the shared header.ReadIdx, releasing all bytes
// consumed during the ZC hold (the ZC frame itself plus any non-ZC
// frames that were committed-deferred while ZC was active).
//
// CRITICAL ordering: PUBLISH header.ReadIdx FIRST, then clear zcActive.
// The reverse order has a window where a concurrent reader-thread Commit
// observes zcActive=false but header.ReadIdx is still at the ZC start
// (we haven't published yet); the reader then takes the immediate-
// publish branch with a STALE commitReadIdx (= ZC start) and CASes
// header.ReadIdx to a value INSIDE the still-held ZC region. The cross-
// process writer then sees those bytes as free, wraps onto them, and
// corrupts the in-flight payload.
//
// After clearing zcActive, a small race window remains where the reader
// bumped zcDeferredTarget with zcActive=true still observed but our
// publish happened with the older snapshot. We catch that by re-reading
// and re-publishing. Once zcActive is false, no further bumps can occur
// (reader's Commit goes to the CAS path), so a single refresh suffices.
func (r *ShmRing) EndZcReservation() {
	if atomic.LoadUint32(&r.closed) != 0 {
		// Ring closed; do not touch shared memory which may be unmapped.
		atomic.StoreUint32(&r.zcActive, 0)
		return
	}
	hdr := r.header()
	target := atomic.LoadUint64(&r.zcDeferredTarget)

	// Phase 1: publish target while zcActive is still true. Any concurrent
	// reader-thread Commit still sees zcActive=true and stays on the
	// deferred path, which only bumps zcDeferredTarget — never touches
	// header.ReadIdx. So our CAS here is uncontended on the reader-thread
	// side; only cross-process EndZcReservation-on-the-other-side could
	// compete (impossible: ZC is per-direction).
	r.publishTarget(hdr, target)

	// Phase 2: drop the active flag. From this point the reader thread
	// will route future Commit calls through the CAS path. A frame whose
	// commitReadIdx was captured before our publish has newReadIdx <=
	// target and is a no-op (CAS sees current >= newReadIdx).
	atomic.StoreUint32(&r.zcActive, 0)

	// Phase 3: catch the small window where the reader bumped
	// zcDeferredTarget after our LoadUint64(target) above but before
	// StoreUint32(zcActive=0). Such a bump would have been routed to the
	// deferred path (reader saw zcActive=true) and is therefore NOT
	// reflected in header.ReadIdx yet. Publish it now.
	refreshed := atomic.LoadUint64(&r.zcDeferredTarget)
	if refreshed > target {
		r.publishTarget(hdr, refreshed)
	}

	// Wake the writer if it was waiting on space.
	if hdr.ContigWaiters() > 0 {
		hdr.IncrementContigSequence()
		r.signalContig(&hdr.contigSeq)
	}
	if hdr.SpaceWaiters() > 0 {
		hdr.IncrementSpaceSequence()
		r.signalSpace(&hdr.spaceSeq)
	}
}

// publishTarget CAS-loops header.ReadIdx forward to target, never
// regressing it.
func (r *ShmRing) publishTarget(hdr *RingHeader, target uint64) {
	for {
		current := hdr.ReadIndex()
		if target <= current {
			return
		}
		if hdr.CompareAndSwapReadIndex(current, target) {
			return
		}
	}
}

// IsSpeculativeZCEligible is the centralised speculative-ZC eligibility
// check, applied identically by every reader.
// Single source of truth for the heuristic so the two code paths cannot
// drift.
//
// payloadLength is the byte length of the candidate frame payload
// (excluding wire headers). contiguous is true iff the payload reservation
// is contiguous (no ring wrap).
//
// The whole point of SHM is to avoid the per-byte memcpy that UDS / TCP
// pay on every RPC. ZC is the mechanism that delivers on that promise,
// so we want ZC to fire for as many payload sizes as physically safe.
// The eligibility rules below were chosen accordingly:
//
//   - At-most-one-ZC in flight per ring (zcActive). The deferred-
//     publish protocol assumes a single producer of bumps to
//     zcDeferredTarget; multiple concurrent ZC frames would require
//     multi-producer ordering not yet implemented. For pipelined
//     streaming this means the second message of a back-to-back burst
//     falls back to the single-mem.Copy fast path, which is correct
//     and inexpensive.
//
//   - Payload must be contiguous (no ring wrap). The H2 codec's
//     copy-fallback handles wrap-aware reassembly.
//
//   - Ring must be at least 1 MiB. Below that a single ZC hold would
//     freeze a measurable fraction of the ring (e.g., 64 KiB out of
//     256 KiB = 25 %) and stall the writer.
//
//   - Minimum payload of 4 KiB. Below that the ZC bookkeeping cost
//     (atomic bumps + Buffer wrap + EndZcReservation publish) reliably
//     loses to a direct mem.Copy. 4 KiB is one page; copying a single
//     page is sub-µs on modern HW.
//
//   - Back-pressure self-disable: when the ring is already > 75 % full
//     (used*4 > cap*3), holding any extra bytes via ZC risks stalling
//     the writer; let the copy path drain into heap instead.
//
// Compared to the previous heuristic (64 KiB minimum), the 4 KiB floor
// lets ZC fire on the medium-message sizes (4 KiB – 16 KiB) where the
// SHM advantage over UDS / TCP is most visible: those sizes still cost
// the kernel two per-byte copies on UDS but zero on SHM with ZC.
func (r *ShmRing) IsSpeculativeZCEligible(payloadLength int, contiguous bool) bool {
	if !contiguous {
		return false
	}
	const minRingForZC = uint64(1) << 20 // 1 MiB
	if r.capacity < minRingForZC {
		return false
	}
	// 4 KiB minimum payload — below this a direct mem.Copy beats the
	// per-ZC bookkeeping cost.
	if payloadLength < 4*1024 {
		return false
	}
	if atomic.LoadUint32(&r.zcActive) != 0 {
		return false
	}
	// Back-pressure auto-degrade.
	hdr := r.header()
	used := hdr.WriteIndex() - hdr.ReadIndex()
	return used*4 <= r.capacity*3
}

// ===== Multi-frame chain ZC =====
//
// Reserved infrastructure for zero-copy reads of logical messages
// that span multiple ring frames. Currently NOT REACHABLE under the
// H2 wire format because the H2 codec's lpmAccumulator reassembles
// multi-DATA-frame LPMs into a heap buffer before surfacing the
// MESSAGE to the upper layer; only single-frame messages traverse
// the deferred-publish ZC path. The runtime hooks (chainOpen /
// IsZcChainActive / IsChainOpen / ChainZcBudget) remain in place
// for two reasons: (1) EndZcReservation's chainOpen check is
// load-bearing for the anchor-close ordering, and (2) future work
// may extend the H2 codec to surface chunk-by-chunk DATA frames as
// distinct MESSAGE frames carrying MessageFlagMORE, which would
// re-activate this path.

// ChainZcBudget is the maximum totalMsg (5-byte LPM header + body)
// that may enter chain-ZC mode. cap/2 ensures the writer always has
// at least cap/2 of headroom for the next message.
func (r *ShmRing) ChainZcBudget() int64 {
	return int64(r.capacity / 2)
}

// IsZcChainActive reports whether a speculative-ZC anchor is held
// (zcActive==1). Used by the codec to decide whether a continuation
// chunk should ride the existing anchor or open a new one.
func (r *ShmRing) IsZcChainActive() bool {
	return atomic.LoadUint32(&r.zcActive) != 0
}

// IsChainOpen reports whether the codec is mid-multi-frame-message
// (between the first MORE chunk and the final !MORE chunk).
func (r *ShmRing) IsChainOpen() bool {
	return atomic.LoadUint32(&r.chainOpen) != 0
}

// OpenZcChain marks the codec as inside a multi-frame ZC chain.
// Called when emitting the first chunk of a chain that opted into ZC.
// Must be paired with CloseZcChain on the message's final chunk.
func (r *ShmRing) OpenZcChain() {
	atomic.StoreUint32(&r.chainOpen, 1)
}

// CloseZcChain marks the codec as finished with a multi-frame ZC
// chain. The consumer's eventual Buffer.Free of the chain's buffers
// then triggers EndZcReservation when zcInFlight reaches 0.
//
// Race fixup: if the LAST in-flight ZC chunk was already freed BEFORE
// this call (zcInFlight==0 at close time), ReleaseChainZcBuffer would
// have observed IsChainOpen()==true and skipped EndZcReservation.
// Symmetrically, if the chain's final chunk falls back to the copy
// path (e.g., the payload straddles a wrap boundary so the chain ZC
// fast-path's len(pSecond)==0 guard rejects it), no AddChainZcInFlight
// is performed for that chunk, so zcInFlight has already been driven
// to 0 by the previous chunk's buf.Free. In either case nothing else
// will fire EndZcReservation, header.ReadIdx stays frozen at the
// chain start, and the writer eventually fills the ring and
// deadlocks. We must fire it here.
//
// Idempotency: EndZcReservation is gated by atomic.StoreUint32 of
// zcActive=0. If a concurrent ReleaseChainZcBuffer races us and fires
// first, our LoadUint32(zcActive) returns 0 and we skip — no double
// publish. If we win the race, ReleaseChainZcBuffer's later check
// observes zcInFlight==0 && !IsChainOpen() but zcActive==0 means
// EndZcReservation's CAS-on-header.ReadIdx is a forward-only no-op.
func (r *ShmRing) CloseZcChain() {
	atomic.StoreUint32(&r.chainOpen, 0)
	// Ordering: chainOpen must be cleared BEFORE we check zcInFlight.
	// If a concurrent ReleaseChainZcBuffer reads zcInFlight==0 it then
	// loads chainOpen — by the time it does, our store above is
	// visible (atomic store has release semantics) so it observes
	// chainOpen==0 and fires EndZcReservation itself.
	if atomic.LoadInt64(&r.zcInFlight) == 0 && atomic.LoadUint32(&r.zcActive) != 0 {
		r.EndZcReservation()
	}
}

// SetChainCopyMode marks the codec as committed to copy-mode for the
// in-progress multi-frame message. Cleared on the message's final
// chunk so the next message starts fresh.
func (r *ShmRing) SetChainCopyMode(v bool) {
	if v {
		atomic.StoreUint32(&r.chainCopyMode, 1)
	} else {
		atomic.StoreUint32(&r.chainCopyMode, 0)
	}
}

// ChainCopyMode reports whether the codec is in copy-mode for the
// in-progress multi-frame message.
func (r *ShmRing) ChainCopyMode() bool {
	return atomic.LoadUint32(&r.chainCopyMode) != 0
}

// AddChainZcInFlight increments the count of ZC buffers held by the
// consumer for the active chain. Called by the codec for each chunk
// emitted as a ring-backed buffer.
func (r *ShmRing) AddChainZcInFlight() {
	atomic.AddInt64(&r.zcInFlight, 1)
}

// ReleaseChainZcBuffer decrements the in-flight count and, when the
// count reaches 0 AND the chain is closed, fires EndZcReservation.
// Called by zcReleasePool.Put on each Buffer.Free.
func (r *ShmRing) ReleaseChainZcBuffer() {
	if atomic.AddInt64(&r.zcInFlight, -1) == 0 && !r.IsChainOpen() {
		r.EndZcReservation()
	}
}

// SMF (Shared Memory Framing) helpers are defined in frame.go. This file uses
// ReserveFrameHeader to reserve 16-byte headers which may straddle wrap boundaries.

// NewShmRingFromSegment creates a ShmRing from a segment's ring view.
// This provides the high-level blocking API over the low-level ring view.
func NewShmRingFromSegment(ringView *ringView, mem []byte) *ShmRing {
	capacity := ringView.Capacity()
	// Enforce power-of-two capacity invariant for masked indexing
	if capacity == 0 || !IsPowerOfTwo(capacity) {
		panic("ShmRing capacity must be a power of two")
	}

	// Assert mmapped length vs offsets
	end := ringView.offset + RingHeaderSize + capacity
	if int(end) > len(mem) {
		panic("segment too small for ring")
	}

	r := &ShmRing{
		capMask:         capacity - 1, // For modulo operations: pos = idx & capMask
		hdrOff:          uintptr(ringView.offset),
		dataOff:         uintptr(ringView.offset + RingHeaderSize),
		mem:             mem,
		capacity:        capacity, // Store actual capacity separately
		dataSpinCutoff:  loadShmSpinDefault(),
		spaceSpinCutoff: loadShmSpinDefault(),
	}
	// Initialize read commit context with back-pointer (reads are single-threaded)
	r.readCommit.ring = r
	// Initialize pendingReadIdx from current shared readIdx
	atomic.StoreUint64(&r.pendingReadIdx, r.header().ReadIndex())
	shmDebugf("[DEBUG] NewShmRingFromSegment: ring=%p, hdrOff=%d, mem[0]=%p, hdr=%p, pendingReadIdx=%d", r, r.hdrOff, &mem[0], r.header(), atomic.LoadUint64(&r.pendingReadIdx))
	return r
}

// SetEvents sets the Windows event handles for cross-mapping synchronization.
// On Linux, this is a no-op since futex works natively across mappings.
func (r *ShmRing) SetEvents(events *RingEvents) {
	r.events = events
}

// SetDataSegWaker attaches the per-data-segment per-direction
// eventfd waker (one read fd on this side) to this ring. Called by
// Segment.RegisterRing, which propagates the segment-level waker to
// all rings registered against it. Passing nil disables the per-
// data-segment fast path (callers fall through to the per-address
// eventfd registry / events / futex).
func (r *ShmRing) SetDataSegWaker(w *shmDataSegWaker) {
	r.dataSegWaker = w
}

// header returns a pointer to the RingHeader in shared memory
func (r *ShmRing) header() *RingHeader {
	return (*RingHeader)(unsafe.Pointer(uintptr(unsafe.Pointer(&r.mem[0])) + r.hdrOff))
}

// isRingClosed returns true if the ring's local closed flag is set.
// Cheap atomic load used by deferred-Free callbacks to avoid use-after-
// unmap when the segment is torn down between buffer creation and Free.
func isRingClosed(r *ShmRing) bool {
	return atomic.LoadUint32(&r.closed) != 0
}

// dataPtr returns a pointer to the data area in shared memory
func (r *ShmRing) dataPtr() unsafe.Pointer {
	return unsafe.Pointer(uintptr(unsafe.Pointer(&r.mem[0])) + r.dataOff)
}

// Capacity returns the ring capacity
func (r *ShmRing) Capacity() uint64 {
	return r.capacity
}

// DebugState returns a snapshot of the current ring state for debugging and diagnostics.
// All values are read atomically for consistent state observation.
// Returns a zero state if the ring is closed locally.
func (r *ShmRing) DebugState() RingState {
	// Check local closed flag first to avoid accessing unmapped memory
	if atomic.LoadUint32(&r.closed) != 0 {
		return RingState{
			Capacity: r.capacity,
			Closed:   1,
		}
	}

	hdr := r.header()
	shmDebugf("DebugState: ring=%p, hdr=%p, &dataSeq=%p, &spaceSeq=%p", r, hdr, &hdr.dataSeq, &hdr.spaceSeq)

	// Read all state atomically for consistent snapshot
	widx := hdr.WriteIndex()
	ridx := hdr.ReadIndex()
	dataSeq := hdr.DataSequence()
	spaceSeq := hdr.SpaceSequence()
	contigSeq := hdr.ContigSequence()
	closed := uint32(0)
	if hdr.Closed() {
		closed = 1
	}

	return RingState{
		Capacity:     r.capacity,
		Widx:         widx,
		Ridx:         ridx,
		Used:         widx - ridx,
		DataSeq:      dataSeq,
		SpaceSeq:     spaceSeq,
		ContigSeq:    contigSeq,
		Closed:       closed,
		DataWaiters:  hdr.DataWaiters(),
		SpaceWaiters: hdr.SpaceWaiters(),
	}
}

// waitForData waits until data is available.
// On Windows, uses named events. On Linux, uses futex.
func (r *ShmRing) waitForData(addr *uint32, val uint32, timeout time.Duration) error {
	if r.dataSegWaker != nil {
		return r.dataSegWaker.WaitForChange(addr, val, timeout)
	}
	if r.events != nil {
		return r.events.WaitData(addr, val, timeout)
	}
	if timeout > 0 {
		return futexWaitTimeout(addr, val, timeout.Nanoseconds())
	}
	return futexWait(addr, val)
}

// waitForSpace waits until space is available.
// On Windows, uses named events. On Linux, uses futex.
func (r *ShmRing) waitForSpace(addr *uint32, val uint32, timeout time.Duration) error {
	if r.dataSegWaker != nil {
		return r.dataSegWaker.WaitForChange(addr, val, timeout)
	}
	if r.events != nil {
		return r.events.WaitSpace(addr, val, timeout)
	}
	if timeout > 0 {
		return futexWaitTimeout(addr, val, timeout.Nanoseconds())
	}
	return futexWait(addr, val)
}

// waitForContig waits until contiguous space improves.
// On Windows, uses named events. On Linux, uses futex.
func (r *ShmRing) waitForContig(addr *uint32, val uint32, timeout time.Duration) error {
	if r.dataSegWaker != nil {
		return r.dataSegWaker.WaitForChange(addr, val, timeout)
	}
	if r.events != nil {
		return r.events.WaitContig(addr, val, timeout)
	}
	if timeout > 0 {
		return futexWaitTimeout(addr, val, timeout.Nanoseconds())
	}
	return futexWait(addr, val)
}

// signalData signals that new data is available.
// On Windows, signals the named event. On Linux, uses futex wake.
func (r *ShmRing) signalData(addr *uint32) {
	if r.dataSegWaker != nil {
		r.dataSegWaker.Wake()
		// Also issue a futex_wake on the same address so peers using a
		// different wake primitive (raw-ring tests that bypass
		// RegisterRing; cross-process peers whose SCM_RIGHTS handoff
		// failed and converged on futex via OpenerWakeReady=false)
		// still observe the signal. Safe: futex_wake on an address
		// with no waiters is a cheap kernel hash lookup (~150 ns) --
		// negligible against the ~100 us per-RPC cost. Use the on-
		// Linux primitive directly so the Windows events path is not
		// double-fired.
		futexWake(addr, 1)
		return
	}
	if r.events != nil {
		r.events.SignalData()
	} else {
		futexWake(addr, 1)
	}
}

// signalSpace signals that space is available.
// On Windows, signals the named event. On Linux, uses futex wake.
func (r *ShmRing) signalSpace(addr *uint32) {
	if r.dataSegWaker != nil {
		r.dataSegWaker.Wake()
		// See signalData for the futex_wake-after-dataSegWaker rationale.
		futexWake(addr, 1)
		return
	}
	if r.events != nil {
		r.events.SignalSpace()
	} else {
		futexWake(addr, 1)
	}
}

// signalContig signals that contiguous space improved.
// On Windows, signals the named event. On Linux, uses futex wake.
func (r *ShmRing) signalContig(addr *uint32) {
	if r.dataSegWaker != nil {
		r.dataSegWaker.Wake()
		// See signalData for the futex_wake-after-dataSegWaker rationale.
		futexWake(addr, 1)
		return
	}
	if r.events != nil {
		r.events.SignalContig()
	} else {
		futexWake(addr, 1)
	}
}

// WriteBlocking writes data to the ring buffer using an event-driven producer algorithm.
// Blocks until space is available or the ring is closed.
//
// This implements the high-performance SPSC algorithm as specified:
// - Uses write/read indices for actual data tracking
// - Uses dataSeq/spaceSeq for futex-based event notification
// - Performs zero-copy data transfer
// - Handles spurious wakes correctly
func (r *ShmRing) WriteBlocking(data []byte) error {
	if len(data) == 0 {
		return nil // No-op for empty data
	}

	// Check if data fits in ring capacity
	if uint64(len(data)) > r.capacity {
		return errors.New("data larger than ring capacity")
	}

	hdr := r.header()

	// Producer side: write data and signal consumer
	for {
		// Check for closure first
		if hdr.Closed() {
			return ErrRingClosed
		}

		// Load current indices to check available space
		writeIdx := hdr.WriteIndex()
		readIdx := hdr.ReadIndex()

		// Calculate available space, deducting speculative reserved bytes
		// so the writer cannot overwrite ring memory still held by zero-copy
		// read buffers.
		usedBefore := writeIdx - readIdx
		available := r.capacity - usedBefore
		specReserved := hdr.SpeculativeReserved()
		if specReserved > 0 {
			sr := uint64(specReserved)
			if sr > available {
				sr = available
			}
			available -= sr
		}

		if uint64(len(data)) <= available {
			// Space available - perform the write
			writePos := writeIdx & r.capMask

			// Handle ring wrap-around
			if writePos+uint64(len(data)) <= r.capacity {
				// Simple case: no wrap
				destPtr := unsafe.Pointer(uintptr(r.dataPtr()) + uintptr(writePos))
				copy(unsafe.Slice((*byte)(destPtr), len(data)), data)
			} else {
				// Wrap case: split the write
				firstChunk := r.capacity - writePos
				firstChunkI := int(firstChunk)

				// Write first chunk at end of buffer
				destPtr1 := unsafe.Pointer(uintptr(r.dataPtr()) + uintptr(writePos))
				copy(unsafe.Slice((*byte)(destPtr1), firstChunkI), data[:firstChunkI])

				// Write second chunk at beginning of buffer
				destPtr2 := r.dataPtr()
				copy(unsafe.Slice((*byte)(destPtr2), len(data)-firstChunkI), data[firstChunkI:])
			}

			// Advance write index.
			// Memory ordering rationale:
			// 1) Bytes are copied into the ring first (normal stores)
			// 2) Check emptiness right before publishing (avoids lost wake race)
			// 3) Publish the new write index with an atomic store (acts as a release)
			// 4) Wake only if we actually transitioned empty -> non-empty

			hdr.SetWriteIndex(writeIdx + uint64(len(data))) // release-publish

			// Readers may block waiting for N bytes, not just “ring is empty”, so wake
			// after any successful write commit.
			if len(data) > 0 {
				hdr.IncrementDataSequence()
				// Only wake if readers are actually blocked on futex (not spinning)
				if hdr.DataWaiters() > 0 {
					r.signalData(&hdr.dataSeq)
				}
			}

			return nil
		}

		// Insufficient space. Distinguish strictly full vs need-more-space.
		// Re-check under the same loop to avoid missed wake.
		writeIdx = hdr.WriteIndex()
		readIdx = hdr.ReadIndex()
		available = r.effectiveSpace(writeIdx, readIdx)
		if available == 0 {
			// Full: spin-wait then wait on spaceSeq (full→not-full)
			// Phase 1: Spin-wait before falling back to futex
			spinLimit := atomic.LoadUint32(&r.spaceSpinCutoff)
			spaceAvailable := false
			for spin := uint32(0); spin < spinLimit; spin++ {
				writeIdx = hdr.WriteIndex()
				readIdx = hdr.ReadIndex()
				if r.effectiveSpace(writeIdx, readIdx) >= uint64(len(data)) {
					// Space available! Update adaptive cutoff
					if spin > 0 {
						target := min(loadShmSpinMax(), spin*2)
						newCutoff := (7*spinLimit + target) / 8
						atomic.StoreUint32(&r.spaceSpinCutoff, max(loadShmSpinMin(), newCutoff))
					}
					spaceAvailable = true
					break
				}
				if hdr.Closed() {
					return ErrRingClosed
				}
				runtime_procyield(1)
			}
			if spaceAvailable {
				continue
			}
			// Phase 2: Spin failed, reduce cutoff and fall back to futex
			newCutoff := (7*spinLimit + loadShmSpinMin()) / 8
			atomic.StoreUint32(&r.spaceSpinCutoff, max(loadShmSpinMin(), newCutoff))

			hdr.IncSpaceWaiters()
			exp := hdr.SpaceSequence()
			// Re-check condition to avoid missed wake
			writeIdx = hdr.WriteIndex()
			readIdx = hdr.ReadIndex()
			if r.effectiveSpace(writeIdx, readIdx) >= uint64(len(data)) {
				hdr.DecSpaceWaiters()
				continue
			}
			_ = r.waitForSpace(&hdr.spaceSeq, exp, 0)
			hdr.DecSpaceWaiters()
			// Re-check closure after wake to avoid infinite loop
			if hdr.Closed() {
				return ErrRingClosed
			}
			continue
		}
		// Not full but not enough: spin-wait then wait on contigSeq.
		// Phase 1: Spin-wait before falling back to futex
		spinLimit := atomic.LoadUint32(&r.spaceSpinCutoff)
		spaceAvailable := false
		for spin := uint32(0); spin < spinLimit; spin++ {
			writeIdx = hdr.WriteIndex()
			readIdx = hdr.ReadIndex()
			if r.effectiveSpace(writeIdx, readIdx) >= uint64(len(data)) {
				if spin > 0 {
					target := min(loadShmSpinMax(), spin*2)
					newCutoff := (7*spinLimit + target) / 8
					atomic.StoreUint32(&r.spaceSpinCutoff, max(loadShmSpinMin(), newCutoff))
				}
				spaceAvailable = true
				break
			}
			if hdr.Closed() {
				return ErrRingClosed
			}
			runtime_procyield(1)
		}
		if spaceAvailable {
			continue
		}
		// Phase 2: Spin failed, fall back to futex
		newCutoff := (7*spinLimit + loadShmSpinMin()) / 8
		atomic.StoreUint32(&r.spaceSpinCutoff, max(loadShmSpinMin(), newCutoff))

		hdr.IncContigWaiters()
		exp := hdr.ContigSequence()
		// Re-check prior to waiting
		writeIdx = hdr.WriteIndex()
		readIdx = hdr.ReadIndex()
		if r.effectiveSpace(writeIdx, readIdx) >= uint64(len(data)) {
			hdr.DecContigWaiters()
			continue
		}
		_ = r.waitForContig(&hdr.contigSeq, exp, 0)
		hdr.DecContigWaiters()
		// Re-check closure after wake to avoid infinite loop
		if hdr.Closed() {
			return ErrRingClosed
		}
	}
}

// ReadBlocking reads data from the ring buffer using an event-driven consumer algorithm.
// Blocks until data is available or the ring is closed.
//
// This implements the high-performance SPSC algorithm as specified:
// - Uses write/read indices for actual data tracking
// - Uses dataSeq/spaceSeq for futex-based event notification
// - Performs zero-copy data transfer
// - Handles spurious wakes correctly
func (r *ShmRing) ReadBlocking(buf []byte) (int, error) {
	if len(buf) == 0 {
		return 0, nil // No-op for empty buffer
	}

	hdr := r.header()

	// Consumer side: read data and signal producer
	for {
		// Check for closure first
		if hdr.Closed() {
			// Check if data is still available even when closed
			writeIdx := hdr.WriteIndex()
			readIdx := hdr.ReadIndex()
			if writeIdx == readIdx {
				return 0, io.EOF
			}
			// Fall through to read remaining data
		}

		// Load current indices to check available data
		writeIdx := hdr.WriteIndex()
		readIdx := hdr.ReadIndex()

		// Calculate available data using indices
		availableBefore := writeIdx - readIdx

		if availableBefore > 0 {
			// Data available - perform the read
			readPos := readIdx & r.capMask

			// Determine how much to read (up to buffer size and available data)
			toRead := uint64(len(buf))
			if toRead > availableBefore {
				toRead = availableBefore
			}

			var bytesRead int

			// Handle ring wrap-around
			if readPos+toRead <= r.capacity {
				// Simple case: no wrap
				srcPtr := unsafe.Pointer(uintptr(r.dataPtr()) + uintptr(readPos))
				toReadI := int(toRead)
				bytesRead = copy(buf, unsafe.Slice((*byte)(srcPtr), toReadI))
			} else {
				// Wrap case: split the read
				firstChunk := r.capacity - readPos
				firstChunkI := int(firstChunk)

				// Read first chunk from end of buffer
				srcPtr1 := unsafe.Pointer(uintptr(r.dataPtr()) + uintptr(readPos))
				bytesRead = copy(buf, unsafe.Slice((*byte)(srcPtr1), firstChunkI))

				// Read second chunk from beginning of buffer
				srcPtr2 := r.dataPtr()
				secondI := int(toRead - firstChunk)
				bytesRead += copy(buf[bytesRead:], unsafe.Slice((*byte)(srcPtr2), secondI))
			}

			// Advance read index.
			// Memory ordering rationale:
			// 1) Reader copies out bytes first
			// 2) Publish the new read index with an atomic store (acts as a release)
			// 3) Bump contigSeq always (contiguity improved), and spaceSeq only on full→not-full.
			prevUsed := availableBefore
			hdr.SetReadIndex(readIdx + uint64(bytesRead)) // release-publish

			if bytesRead > 0 && hdr.ContigWaiters() > 0 {
				// Contiguity: only bump when a writer is waiting for space.
				hdr.IncrementContigSequence()
				r.signalContig(&hdr.contigSeq)
			}

			// Space became available only if we were full before this read
			if prevUsed == r.capacity {
				hdr.IncrementSpaceSequence()
				if hdr.SpaceWaiters() > 0 {
					r.signalSpace(&hdr.spaceSeq)
				}
			}

			return bytesRead, nil
		}

		// No data available - check closure and wait for producer
		if !hdr.Closed() {
			// Phase 1: Spin-wait for a short duration before falling back to futex.
			// This dramatically reduces latency in ping-pong patterns where data
			// arrives quickly, avoiding the ~10µs futex wake overhead.
			spinLimit := atomic.LoadUint32(&r.dataSpinCutoff)
			for spin := uint32(0); spin < spinLimit; spin++ {
				// Check if data arrived during spin
				if hdr.WriteIndex()-hdr.ReadIndex() > 0 {
					// Data arrived! Update adaptive cutoff (success = spin faster next time)
					if spin > 0 {
						// Exponential moving average: new = (7*old + target) / 8
						target := min(loadShmSpinMax(), spin*2)
						newCutoff := (7*spinLimit + target) / 8
						atomic.StoreUint32(&r.dataSpinCutoff, max(loadShmSpinMin(), newCutoff))
					}
					break // Exit spin loop; outer loop will read data
				}
				// Check closure during spin — break to let the outer
				// loop's drain logic decide whether data should be read.
				if hdr.Closed() {
					break
				}
				// PAUSE instruction - yields to hyperthread, saves power
				runtime_procyield(1)
			}

			// Phase 2: Spin didn't succeed, fall back to futex
			// Reduce spin cutoff (timeout = spin less next time)
			newCutoff := (7*spinLimit + loadShmSpinMin()) / 8
			atomic.StoreUint32(&r.dataSpinCutoff, max(loadShmSpinMin(), newCutoff))

			hdr.IncDataWaiters()
			dataSeq := hdr.DataSequence()
			// Re-check data availability and closed state before sleeping
			writeIdx := hdr.WriteIndex()
			readIdx := hdr.ReadIndex()
			if writeIdx-readIdx > 0 {
				hdr.DecDataWaiters()
				continue
			}
			// Re-check closed flag to avoid missing a close that happened
			// after our initial check but before we entered futexWait.
			// If closed, re-enter the main loop which checks data-then-close.
			if hdr.Closed() {
				hdr.DecDataWaiters()
				continue
			}
			if err := r.waitForData(&hdr.dataSeq, dataSeq, 0); err != nil {
				// Spurious wake or other wake reasons - just continue the loop
				_ = err // silence staticcheck SA9003
			}
			// Check if ring is still valid before decrementing - segment may
			// have been unmapped while we were blocked on futexWait
			if atomic.LoadUint32(&r.closed) == 0 {
				hdr.DecDataWaiters()
			}
		} else {
			// Closed and no data - return EOF
			return 0, io.EOF
		}
	}
}

// Close closes the ring for writing. Readers can still read remaining data.
func (r *ShmRing) Close() error {
	// Make Close idempotent. This is important because multiple higher-level
	// objects may try to close the same ring during shutdown, and a late Close
	// must not touch shared memory if the segment has already been unmapped.
	if !atomic.CompareAndSwapUint32(&r.closed, 0, 1) {
		return nil
	}

	hdr := r.header()
	hdr.SetClosed(true)

	// Wake up any waiting readers and writers; bump sequences to release waiters.
	hdr.IncrementDataSequence()
	hdr.IncrementSpaceSequence()
	hdr.IncrementContigSequence()
	r.signalData(&hdr.dataSeq)
	r.signalSpace(&hdr.spaceSeq)
	r.signalContig(&hdr.contigSeq)

	return nil
}

// Available returns the number of bytes available for writing
func (r *ShmRing) Available() uint64 {
	if atomic.LoadUint32(&r.closed) != 0 {
		return 0
	}
	return r.effectiveAvailable()
}

// HasPendingData reports whether the ring has data that the reader has not
// yet picked up. Cheap: two atomic loads, no syscall. Intended for hot-path
// gating decisions (e.g. "should the reader yield after delivering a MESSAGE
// to the application or keep draining?").
//
// This is a best-effort scheduling hint, not a synchronisation primitive:
// the answer can be stale on either side (a freshly-committed write may not
// yet be visible; a freshly-advanced read may still appear pending). Stale
// `false` causes one extra cooperative yield; stale `true` causes one extra
// loop iteration that will then either find the frame or fall into the real
// wait-with-sequence-snapshot path in ReadSlices. Both outcomes are
// performance-only and cannot cause a missed wake or a deadlock.
func (r *ShmRing) HasPendingData() bool {
	if atomic.LoadUint32(&r.closed) != 0 {
		return false
	}
	hdr := r.header()
	return hdr.WriteIndex() > atomic.LoadUint64(&r.pendingReadIdx)
}

// effectiveAvailable returns the bytes available for writing, deducting
// speculativeReserved so the writer cannot overwrite ring memory still
// referenced by zero-copy reader buffers.
func (r *ShmRing) effectiveAvailable() uint64 {
	hdr := r.header()
	writeIdx := hdr.WriteIndex()
	readIdx := hdr.ReadIndex()
	used := writeIdx - readIdx
	raw := r.capacity - used

	specReserved := hdr.SpeculativeReserved()
	if specReserved <= 0 {
		return raw
	}
	sr := uint64(specReserved)
	if sr > raw {
		sr = raw
	}
	return raw - sr
}

// effectiveSpace returns the writable space given current indices,
// deducting bytes speculatively reserved by zero-copy readers.
func (r *ShmRing) effectiveSpace(writeIdx, readIdx uint64) uint64 {
	raw := r.capacity - (writeIdx - readIdx)
	hdr := r.header()
	specReserved := hdr.SpeculativeReserved()
	if specReserved <= 0 {
		return raw
	}
	sr := uint64(specReserved)
	if sr > raw {
		sr = raw
	}
	return raw - sr
}

// ContiguousWriteSpace returns the number of contiguous bytes available for
// writing from the current write position to the end of the ring buffer
// (before wrap-around). This is useful for zero-copy writes that require
// a single contiguous slice.
func (r *ShmRing) ContiguousWriteSpace() uint64 {
	if atomic.LoadUint32(&r.closed) != 0 {
		return 0
	}
	hdr := r.header()
	writeIdx := hdr.WriteIndex()
	readIdx := hdr.ReadIndex()
	used := writeIdx - readIdx
	available := r.capacity - used

	// Deduct speculative reserved bytes (zero-copy reads still in use).
	specReserved := hdr.SpeculativeReserved()
	if specReserved > 0 {
		sr := uint64(specReserved)
		if sr > available {
			sr = available
		}
		available -= sr
	}

	writePos := writeIdx & r.capMask
	toEnd := r.capacity - writePos
	if toEnd < available {
		return toEnd
	}
	return available
}

// Used returns the number of bytes currently used in the ring
func (r *ShmRing) Used() uint64 {
	if atomic.LoadUint32(&r.closed) != 0 {
		return 0
	}
	return r.header().Used()
}

// IsClosed returns true if the ring is closed for writing
func (r *ShmRing) IsClosed() bool {
	if atomic.LoadUint32(&r.closed) != 0 {
		return true
	}
	return r.header().Closed()
}

// IsEmpty returns true if the ring contains no data
func (r *ShmRing) IsEmpty() bool {
	if atomic.LoadUint32(&r.closed) != 0 {
		return true
	}
	return r.header().Used() == 0
}

// IsFull returns true if the ring is completely full
func (r *ShmRing) IsFull() bool {
	if atomic.LoadUint32(&r.closed) != 0 {
		return false
	}
	return r.header().Available() == 0
}

// WriteBlockingContext writes data to the ring buffer with context deadline support.
// Blocks until space is available, the ring is closed, or context deadline exceeded.
// Returns context.DeadlineExceeded if the context deadline is exceeded.
func (r *ShmRing) WriteBlockingContext(ctx context.Context, data []byte) error {
	if len(data) == 0 {
		return nil // No-op for empty data
	}

	// Check if data fits in ring capacity
	if uint64(len(data)) > r.capacity {
		return errors.New("data larger than ring capacity")
	}

	hdr := r.header()

	// Producer side: write data and signal consumer
	for {
		// Check for closure first
		if hdr.Closed() {
			return ErrRingClosed
		}

		// Check context cancellation/deadline
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Load current indices to check available space
		writeIdx := hdr.WriteIndex()
		readIdx := hdr.ReadIndex()

		// Calculate available space using indices, deducting bytes
		// speculatively reserved by zero-copy readers.
		usedBefore := writeIdx - readIdx
		available := r.capacity - usedBefore
		specReserved := hdr.SpeculativeReserved()
		if specReserved > 0 {
			sr := uint64(specReserved)
			if sr > available {
				sr = available
			}
			available -= sr
		}

		if uint64(len(data)) <= available {
			// Space available - perform the write (same as original WriteBlocking)
			writePos := writeIdx & r.capMask

			// Handle ring wrap-around
			if writePos+uint64(len(data)) <= r.capacity {
				// Simple case: no wrap
				destPtr := unsafe.Pointer(uintptr(r.dataPtr()) + uintptr(writePos))
				copy(unsafe.Slice((*byte)(destPtr), len(data)), data)
			} else {
				// Wrap case: split the write
				firstChunk := r.capacity - writePos
				firstChunkI := int(firstChunk)

				// Write first chunk at end of buffer
				destPtr1 := unsafe.Pointer(uintptr(r.dataPtr()) + uintptr(writePos))
				copy(unsafe.Slice((*byte)(destPtr1), firstChunkI), data[:firstChunkI])

				// Write second chunk at beginning of buffer
				destPtr2 := r.dataPtr()
				copy(unsafe.Slice((*byte)(destPtr2), len(data)-firstChunkI), data[firstChunkI:])
			}

			hdr.SetWriteIndex(writeIdx + uint64(len(data))) // release-publish

			// Readers may block waiting for N bytes, not just “ring is empty”, so wake
			// after any successful write commit.
			if len(data) > 0 {
				hdr.IncrementDataSequence()
				// Only wake if readers are actually blocked on futex (not spinning)
				if hdr.DataWaiters() > 0 {
					r.signalData(&hdr.dataSeq)
				}
			}

			return nil
		}

		// Need to wait for space (distinguish full vs partial)

		// Calculate timeout from context deadline
		var timeoutNs int64
		if deadline, hasDeadline := ctx.Deadline(); hasDeadline {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return context.DeadlineExceeded
			}
			timeoutNs = remaining.Nanoseconds()
		}

		// Wait for space with timeout
		var err error
		if timeoutNs > 0 {
			// Re-check and choose wait primitive
			writeIdx = hdr.WriteIndex()
			readIdx = hdr.ReadIndex()
			available = r.effectiveSpace(writeIdx, readIdx)
			if uint64(len(data)) <= available {
				continue
			}
			if available == 0 {
				hdr.IncSpaceWaiters()
				exp := hdr.SpaceSequence()
				// Re-check
				writeIdx = hdr.WriteIndex()
				readIdx = hdr.ReadIndex()
				if r.effectiveSpace(writeIdx, readIdx) >= uint64(len(data)) {
					hdr.DecSpaceWaiters()
					continue
				}
				err = r.waitForSpace(&hdr.spaceSeq, exp, time.Duration(timeoutNs))
				hdr.DecSpaceWaiters()
			} else {
				hdr.IncContigWaiters()
				exp := hdr.ContigSequence()
				// Re-check
				writeIdx = hdr.WriteIndex()
				readIdx = hdr.ReadIndex()
				if r.effectiveSpace(writeIdx, readIdx) >= uint64(len(data)) {
					hdr.DecContigWaiters()
					continue
				}
				err = r.waitForContig(&hdr.contigSeq, exp, time.Duration(timeoutNs))
				hdr.DecContigWaiters()
			}
		} else {
			// No timeout: same logic with infinite waits
			writeIdx = hdr.WriteIndex()
			readIdx = hdr.ReadIndex()
			available = r.effectiveSpace(writeIdx, readIdx)
			if uint64(len(data)) <= available {
				continue
			}
			if available == 0 {
				hdr.IncSpaceWaiters()
				exp := hdr.SpaceSequence()
				// Re-check
				writeIdx = hdr.WriteIndex()
				readIdx = hdr.ReadIndex()
				if r.effectiveSpace(writeIdx, readIdx) >= uint64(len(data)) {
					hdr.DecSpaceWaiters()
					continue
				}
				err = r.waitForSpace(&hdr.spaceSeq, exp, 0)
				hdr.DecSpaceWaiters()
			} else {
				hdr.IncContigWaiters()
				exp := hdr.ContigSequence()
				// Re-check
				writeIdx = hdr.WriteIndex()
				readIdx = hdr.ReadIndex()
				if r.effectiveSpace(writeIdx, readIdx) >= uint64(len(data)) {
					hdr.DecContigWaiters()
					continue
				}
				err = r.waitForContig(&hdr.contigSeq, exp, 0)
				hdr.DecContigWaiters()
			}
		}

		if err != nil {
			// Check if it's a timeout error
			if errors.Is(err, ErrFutexTimeout) {
				return context.DeadlineExceeded
			}
			return err
		}
	}
}

// ReadBlockingContext reads data from the ring buffer with context deadline support.
// Blocks until data is available, the ring is closed, or context deadline exceeded.
// Returns context.DeadlineExceeded if the context deadline is exceeded.
func (r *ShmRing) ReadBlockingContext(ctx context.Context, buf []byte) (int, error) {
	if len(buf) == 0 {
		return 0, nil // No-op for empty buffer
	}

	hdr := r.header()

	// Consumer side: read data and signal producer
	for {
		// Check context cancellation/deadline
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		default:
		}

		// Load current indices to check available data
		writeIdx := hdr.WriteIndex()
		readIdx := hdr.ReadIndex()

		usedBefore := writeIdx - readIdx

		if usedBefore > 0 {
			// Data available - perform the read
			toRead := usedBefore
			if toRead > uint64(len(buf)) {
				toRead = uint64(len(buf))
			}

			readPos := readIdx & r.capMask

			var bytesRead int

			// Handle ring wrap-around
			if readPos+toRead <= r.capacity {
				// Simple case: no wrap
				srcPtr := unsafe.Pointer(uintptr(r.dataPtr()) + uintptr(readPos))
				toReadI := int(toRead)
				bytesRead = copy(buf, unsafe.Slice((*byte)(srcPtr), toReadI))
			} else {
				// Wrap case: split the read
				firstChunk := r.capacity - readPos
				firstChunkI := int(firstChunk)

				// Read first chunk from end of buffer
				srcPtr1 := unsafe.Pointer(uintptr(r.dataPtr()) + uintptr(readPos))
				bytesRead = copy(buf, unsafe.Slice((*byte)(srcPtr1), firstChunkI))

				// Read second chunk from beginning of buffer
				srcPtr2 := r.dataPtr()
				secondChunkI := int(toRead - firstChunk)
				bytesRead += copy(buf[bytesRead:], unsafe.Slice((*byte)(srcPtr2), secondChunkI))
			}

			// Advance read index.
			// Memory ordering rationale:
			// 1) Reader copies out bytes first
			// 2) Publish the new read index with an atomic store (acts as a release)
			// 3) Bump contigSeq if waiters, wake spaceSeq waiters if any.
			hdr.SetReadIndex(readIdx + uint64(bytesRead)) // release-publish

			if bytesRead > 0 && hdr.ContigWaiters() > 0 {
				// Contiguity: only bump when a writer is waiting for space.
				hdr.IncrementContigSequence()
				r.signalContig(&hdr.contigSeq)
			}

			// Space: wake waiters if any are waiting.
			if bytesRead > 0 && hdr.SpaceWaiters() > 0 {
				hdr.IncrementSpaceSequence()
				newSeq := hdr.SpaceSequence()
				shmDebugf("READBLOCKING_SPACE_WAKE: freed %d bytes, new spaceSeq=%d, waking waiters",
					bytesRead, newSeq)
				r.signalSpace(&hdr.spaceSeq)
			}

			return bytesRead, nil
		}

		// Check if ring is closed and no data available
		if hdr.Closed() {
			return 0, io.EOF
		}

		// Need to wait for data
		hdr.IncDataWaiters()
		dataSeq := hdr.DataSequence()
		shmDebugf("READBLOCKING_DATA_WAIT: empty ring, dataWaiters=%d, dataSeq=%d, widx=%d, ridx=%d",
			hdr.DataWaiters(), dataSeq, hdr.WriteIndex(), hdr.ReadIndex())

		// Re-check data availability before sleeping
		writeIdx = hdr.WriteIndex()
		readIdx = hdr.ReadIndex()
		if writeIdx-readIdx > 0 {
			hdr.DecDataWaiters()
			continue
		}

		// Re-check closed flag after incrementing waiters to avoid missing
		// a close that happened between our initial check and now.
		// If closed, re-enter the main loop which checks data-then-close.
		if hdr.Closed() {
			hdr.DecDataWaiters()
			continue
		}

		// Calculate timeout from context deadline
		var timeoutNs int64
		if deadline, hasDeadline := ctx.Deadline(); hasDeadline {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				// Check if ring is still valid before decrementing
				if atomic.LoadUint32(&r.closed) == 0 {
					hdr.DecDataWaiters()
				}
				return 0, context.DeadlineExceeded
			}
			timeoutNs = remaining.Nanoseconds()
		}

		// Wait for data with timeout
		var err error
		if timeoutNs > 0 {
			err = r.waitForData(&hdr.dataSeq, dataSeq, time.Duration(timeoutNs))
		} else {
			err = r.waitForData(&hdr.dataSeq, dataSeq, 0)
		}
		// Check if ring is still valid before decrementing - segment may
		// have been unmapped while we were blocked on futexWait
		if atomic.LoadUint32(&r.closed) == 0 {
			hdr.DecDataWaiters()
		}

		if err != nil {
			// Check if it's a timeout error
			if errors.Is(err, ErrFutexTimeout) {
				return 0, context.DeadlineExceeded
			}
			return 0, err
		}
	}
}

// DiagnoseDuelingBuffers checks if both rings in a duplex connection are full,
// indicating a potential deadlock scenario. Returns diagnostic information.
func DiagnoseDuelingBuffers(clientToServer, serverToClient *ShmRing) (bool, string) {
	csState := clientToServer.DebugState()
	scState := serverToClient.DebugState()

	// Check if both rings are full or nearly full
	csUsedPercent := float64(csState.Used) / float64(csState.Capacity) * 100
	scUsedPercent := float64(scState.Used) / float64(scState.Capacity) * 100

	isDueling := csUsedPercent >= 95.0 && scUsedPercent >= 95.0

	diagnostic := ""
	if isDueling {
		diagnostic = "DUELING FULL BUFFERS DETECTED:\n"
	} else {
		diagnostic = "Ring Buffer State:\n"
	}

	diagnostic += fmt.Sprintf("Client→Server: Used=%d/%d (%.1f%%) Widx=%d Ridx=%d DataSeq=%d SpaceSeq=%d Closed=%d\n",
		csState.Used, csState.Capacity, csUsedPercent,
		csState.Widx, csState.Ridx, csState.DataSeq, csState.SpaceSeq, csState.Closed)

	diagnostic += fmt.Sprintf("Server→Client: Used=%d/%d (%.1f%%) Widx=%d Ridx=%d DataSeq=%d SpaceSeq=%d Closed=%d\n",
		scState.Used, scState.Capacity, scUsedPercent,
		scState.Widx, scState.Ridx, scState.DataSeq, scState.SpaceSeq, scState.Closed)

	if isDueling {
		diagnostic += "This indicates both sides are blocked: client can't write (server→client full), server can't echo (client→server full).\n"
		diagnostic += "Solution: Use concurrent read/write instead of sequential operations."
	}

	return isDueling, diagnostic
}

// WriteReservation represents a reservation for writing data to the ring.
// The caller must fill exactly the reserved bytes and call Commit with the actual bytes written.
type WriteReservation struct {
	First    []byte   // First contiguous slice (from write position to end of buffer or requested size)
	Second   []byte   // Second contiguous slice (from start of buffer) - may be empty if First has enough space
	ring     *ShmRing // Ring buffer reference for commit
	writeIdx uint64   // Write index at reservation time
	maxBytes int      // Maximum bytes that can be committed
}

// Commit commits the written bytes and advances the write index.
// written must not exceed maxBytes (the reservation size).
// If the ring is in batch mode (BeginBatch), the DataSequence increment
// and reader signal are deferred until EndBatch.
func (wr *WriteReservation) Commit(written int) error {
	if written < 0 || written > wr.maxBytes {
		return fmt.Errorf("invalid written count %d, expected 0-%d", written, wr.maxBytes)
	}

	hdr := wr.ring.header()

	// Publish new write index.
	hdr.SetWriteIndex(wr.writeIdx + uint64(written)) // release-publish

	// In batch mode, skip the signal — EndBatch will do it once.
	if written > 0 && atomic.LoadUint32(&wr.ring.batchDepth) == 0 {
		hdr.IncrementDataSequence()
		newSeq := hdr.DataSequence()
		waiters := hdr.DataWaiters()
		shmDebugf("COMMIT_DATA_WAKE: written=%d, newSeq=%d, dataWaiters=%d", written, newSeq, waiters)
		// Only wake if there are waiters - avoids unnecessary syscalls
		if waiters > 0 {
			shmDebugf("COMMIT_DATA_WAKE: waking 1 waiter")
			wr.ring.signalData(&hdr.dataSeq)
		}
	}

	return nil
}

// BeginBatch starts a batch write session. While in batch mode, Commit calls
// will advance the write index but suppress the DataSequence increment and
// reader signal. This amortizes the signaling cost when writing multiple
// frames in quick succession.
func (r *ShmRing) BeginBatch() {
	atomic.AddUint32(&r.batchDepth, 1)
}

// EndBatch ends a batch write session and signals the reader if any data
// was written during the batch. Must be paired with BeginBatch.
func (r *ShmRing) EndBatch() {
	if atomic.AddUint32(&r.batchDepth, ^uint32(0)) == 0 { // decrement
		hdr := r.header()
		hdr.IncrementDataSequence()
		if hdr.DataWaiters() > 0 {
			r.signalData(&hdr.dataSeq)
		}
	}
}

// ReserveWrite blocks until at least n bytes of contiguous space is available, then returns
// the writable slice(s) and a commit function. This enables zero-copy writes directly into
// the ring buffer memory. Headers may span across wrap boundaries via First+Second slices.
func (r *ShmRing) ReserveWrite(ctx context.Context, n int) (WriteReservation, error) {
	if n <= 0 {
		return WriteReservation{}, errors.New("reservation size must be positive")
	}

	if uint64(n) > r.capacity {
		return WriteReservation{}, errors.New("reservation larger than ring capacity")
	}

	// Check local closed flag first - this is safe even if memory is unmapped
	if atomic.LoadUint32(&r.closed) != 0 {
		return WriteReservation{}, ErrRingClosed
	}

	hdr := r.header()

	for {
		// Check context cancellation first
		select {
		case <-ctx.Done():
			return WriteReservation{}, ctx.Err()
		default:
		}

		// Check local closed flag - this is safe even if memory is unmapped
		if atomic.LoadUint32(&r.closed) != 0 {
			return WriteReservation{}, ErrRingClosed
		}

		// Check for closure in shared memory
		if hdr.Closed() {
			return WriteReservation{}, ErrRingClosed
		}

		// Load current indices to check available space
		writeIdx := hdr.WriteIndex()
		readIdx := hdr.ReadIndex()

		// Calculate available space, deducting speculative reserved bytes.
		usedBefore := writeIdx - readIdx
		available := r.capacity - usedBefore
		specReserved := hdr.SpeculativeReserved()
		if specReserved > 0 {
			sr := uint64(specReserved)
			if sr > available {
				sr = available
			}
			available -= sr
		}

		if uint64(n) <= available {
			// Space available - create reservation
			writePos := writeIdx & r.capMask

			var first, second []byte

			// Handle ring wrap-around - headers may straddle wrap
			if writePos+uint64(n) <= r.capacity {
				// Simple case: no wrap needed
				firstPtr := unsafe.Pointer(uintptr(r.dataPtr()) + uintptr(writePos))
				first = unsafe.Slice((*byte)(firstPtr), n)
			} else {
				// Wrap case: split across end and beginning (header can straddle)
				firstLen := r.capacity - writePos
				firstPtr := unsafe.Pointer(uintptr(r.dataPtr()) + uintptr(writePos))
				firstLenI := int(firstLen)
				first = unsafe.Slice((*byte)(firstPtr), firstLenI)

				secondLen := uint64(n) - firstLen
				secondPtr := r.dataPtr()
				secondLenI := int(secondLen)
				second = unsafe.Slice((*byte)(secondPtr), secondLenI)
			}

			// Return reservation with embedded commit state (no heap allocation)
			return WriteReservation{
				First:    first,
				Second:   second,
				ring:     r,
				writeIdx: writeIdx,
				maxBytes: n,
			}, nil
		}

		// Insufficient space - spin briefly before falling back to futex.
		spinCutoff := atomic.LoadUint32(&r.spaceSpinCutoff)
		spinSuccess := false
		for i := uint32(0); i < spinCutoff; i++ {
			if atomic.LoadUint32(&r.closed) != 0 {
				return WriteReservation{}, ErrRingClosed
			}
			runtime_procyield(1) // PAUSE instruction to reduce power/contention
			writeIdx = hdr.WriteIndex()
			readIdx = hdr.ReadIndex()
			avail := r.capacity - (writeIdx - readIdx)
			sr := hdr.SpeculativeReserved()
			if sr > 0 {
				sru := uint64(sr)
				if sru > avail {
					sru = avail
				}
				avail -= sru
			}
			if avail >= uint64(n) {
				spinSuccess = true
				// Adapt spin cutoff upward
				newCutoff := (7*spinCutoff + loadShmSpinMax()) / 8
				if newCutoff > loadShmSpinMax() {
					newCutoff = loadShmSpinMax()
				}
				atomic.StoreUint32(&r.spaceSpinCutoff, newCutoff)
				break
			}
		}
		if spinSuccess {
			continue // Loop back to reserve space
		}
		// Spin timed out - adapt cutoff downward
		newCutoff := (7*spinCutoff + loadShmSpinMin()) / 8
		if newCutoff < loadShmSpinMin() {
			newCutoff = loadShmSpinMin()
		}
		atomic.StoreUint32(&r.spaceSpinCutoff, newCutoff)

		if dl, ok := ctx.Deadline(); ok {
			shmDebugf("ReserveWrite: waiting with timeout=%s", time.Until(dl))
		} else {
			shmDebugf("ReserveWrite: waiting WITHOUT timeout")
		}

		// Spin failed - fall back to futex, choosing wait type based on fullness
		writeIdx = hdr.WriteIndex()
		readIdx = hdr.ReadIndex()
		free := r.effectiveSpace(writeIdx, readIdx)

		// Deadlock guard: if we're inside an open BeginBatch session
		// and about to park waiting for space, we must first flush
		// the deferred data signal. Otherwise the reader -- which is
		// the only party that can free space for us -- may be parked
		// on a DataSequence value the batch has not yet
		// incremented, and our committed bytes are invisible to it
		// as "new data". Net effect: ring is full from our side,
		// reader is asleep with stale seq, neither makes progress.
		//
		// Triggered when a chunked writer (writeFrameH2DataChunked,
		// emitH2DataFromCursor) batches a logical MESSAGE whose
		// total bytes exceed effective ring capacity -- the same
		// pattern that caused BenchmarkGRPCShmLargeUnary/size=64MB
		// to hang on a 64-MiB ring.
		//
		// The flushed increment may pair with the eventual EndBatch
		// to fire two signals instead of one; the reader's loop
		// tolerates that by re-checking writeIdx after each wake.
		if atomic.LoadUint32(&r.batchDepth) > 0 {
			hdr.IncrementDataSequence()
			if hdr.DataWaiters() > 0 {
				r.signalData(&hdr.dataSeq)
			}
		}

		if free == 0 {
			hdr.IncSpaceWaiters()
			exp := hdr.SpaceSequence()
			shmDebugf("RESERVE_WRITE_SPACE_WAIT: ring FULL, spaceWaiters=%d, exp=%d, widx=%d, ridx=%d",
				hdr.SpaceWaiters(), exp, writeIdx, readIdx)
			// Re-check
			writeIdx = hdr.WriteIndex()
			readIdx = hdr.ReadIndex()
			if r.effectiveSpace(writeIdx, readIdx) >= uint64(n) {
				hdr.DecSpaceWaiters()
				continue
			}
			var err error
			if deadline, has := ctx.Deadline(); has {
				rem := time.Until(deadline)
				if rem <= 0 {
					hdr.DecSpaceWaiters()
					return WriteReservation{}, context.DeadlineExceeded
				}
				shmDebugf("FUTEX_ENTER: exp=%d, rem=%v", exp, rem)
				err = r.waitForSpace(&hdr.spaceSeq, exp, rem)
				shmDebugf("FUTEX_EXIT: exp=%d, err=%v, newSeq=%d", exp, err, hdr.SpaceSequence())
			} else {
				err = r.waitForSpace(&hdr.spaceSeq, exp, 0)
			}
			hdr.DecSpaceWaiters()
			if err != nil {
				if errors.Is(err, ErrFutexTimeout) {
					return WriteReservation{}, context.DeadlineExceeded
				}
				if ctx.Err() != nil {
					return WriteReservation{}, ctx.Err()
				}
				// else spurious wake: continue loop
			}
			// Re-check closure after wake to avoid infinite loop
			if hdr.Closed() {
				return WriteReservation{}, ErrRingClosed
			}
			continue
		}
		// Not full: contiguity-improving reads help
		hdr.IncContigWaiters()
		exp := hdr.ContigSequence()
		// Re-check
		writeIdx = hdr.WriteIndex()
		readIdx = hdr.ReadIndex()
		if r.effectiveSpace(writeIdx, readIdx) >= uint64(n) {
			hdr.DecContigWaiters()
			continue
		}
		var err error
		if deadline, has := ctx.Deadline(); has {
			rem := time.Until(deadline)
			if rem <= 0 {
				hdr.DecContigWaiters()
				return WriteReservation{}, context.DeadlineExceeded
			}
			err = r.waitForContig(&hdr.contigSeq, exp, rem)
		} else {
			err = r.waitForContig(&hdr.contigSeq, exp, 0)
		}
		hdr.DecContigWaiters()
		if err != nil {
			if errors.Is(err, ErrFutexTimeout) {
				return WriteReservation{}, context.DeadlineExceeded
			}
			if ctx.Err() != nil {
				return WriteReservation{}, ctx.Err()
			}
			// else spurious wake: continue loop
		}
		// Re-check closure after wake to avoid infinite loop
		if hdr.Closed() {
			return WriteReservation{}, ErrRingClosed
		}
	}
}

// ReadSlices blocks until at least n bytes are available to read; returns slices spanning wrap.
// This enables proper reconstruction of headers that may straddle wrap boundaries.
// The caller must call commit.Commit() with the number of bytes consumed.
func (r *ShmRing) ReadSlices(ctx context.Context, n int) (first, second []byte, commit *ReadCommit, err error) {
	if n <= 0 {
		return nil, nil, nil, errors.New("read size must be positive")
	}

	// Check local closed flag first - this is safe even if memory is unmapped.
	// However, in single-process tests, producer and consumer share the same ShmRing
	// instance, so r.closed gets set when producer closes but memory is still valid.
	// We need to check if there's data to drain before returning EOF.
	//
	// For the cross-process case where memory may actually be unmapped, the caller
	// should avoid calling ReadSlices after closing their side of the connection.
	// The decoupled tests use separate ShmRing instances per process.

	hdr := r.header()

	for {
		// Check context cancellation first
		select {
		case <-ctx.Done():
			return nil, nil, nil, ctx.Err()
		default:
		}

		// Check local closed flag - this is safe even if memory is unmapped.
		// BUT: we must still allow draining data if the ring was closed after writes.
		// Only return EOF immediately if we detect memory is actually unsafe to access.
		// In single-process tests, producer and consumer share the same ShmRing instance,
		// so r.closed gets set when producer closes, but memory is still valid.
		localClosed := atomic.LoadUint32(&r.closed) != 0

		// Check closed state in shared memory - but always allow reading remaining data first.
		headerClosed := hdr.Closed()

		// Use pendingReadIdx for availability (allows read-ahead while buffers are held)
		pendingIdx := atomic.LoadUint64(&r.pendingReadIdx)

		// If closed (either locally or in header), check if there's still data to drain
		if localClosed || headerClosed {
			// Check if data is still available even when closed
			writeIdx := hdr.WriteIndex()
			availableBefore := writeIdx - pendingIdx
			if availableBefore == 0 {
				return nil, nil, nil, io.EOF
			}
			// Fall through to read remaining data if available
		}

		// Load current write index to check available data
		writeIdx := hdr.WriteIndex()
		// Also get shared readIdx for the commit function (to free space for writer)
		sharedReadIdx := hdr.ReadIndex()

		// Calculate available data using pendingReadIdx (not shared readIdx)
		availableBefore := writeIdx - pendingIdx

		if availableBefore >= uint64(n) {
			// Data available - create slices using pendingIdx (local read position)
			readPos := pendingIdx & r.capMask

			var firstSlice, secondSlice []byte

			// Handle ring wrap-around - allow headers to straddle wrap
			if readPos+uint64(n) <= r.capacity {
				// Simple case: no wrap needed
				srcPtr := unsafe.Pointer(uintptr(r.dataPtr()) + uintptr(readPos))
				firstSlice = unsafe.Slice((*byte)(srcPtr), n)
			} else {
				// Wrap case: split across end and beginning
				firstLen := r.capacity - readPos
				firstPtr := unsafe.Pointer(uintptr(r.dataPtr()) + uintptr(readPos))
				firstLenI := int(firstLen)
				firstSlice = unsafe.Slice((*byte)(firstPtr), firstLenI)

				secondLen := uint64(n) - firstLen
				secondPtr := r.dataPtr()
				secondLenI := int(secondLen)
				secondSlice = unsafe.Slice((*byte)(secondPtr), secondLenI)
			}

			// Advance pendingReadIdx now - this allows us to read ahead
			// while the application holds the buffer
			atomic.StoreUint64(&r.pendingReadIdx, pendingIdx+uint64(n))

			// Set up pre-allocated commit context (no closure allocation)
			r.readCommit.commitReadIdx = sharedReadIdx
			r.readCommit.maxBytes = n

			return firstSlice, secondSlice, &r.readCommit, nil
		}

		// No data available - spin briefly before falling back to futex.
		// This avoids syscall overhead in the common case where data arrives quickly.
		spinCutoff := atomic.LoadUint32(&r.dataSpinCutoff)
		spinSuccess := false
		for i := uint32(0); i < spinCutoff; i++ {
			// Mixed spin strategy: PAUSE for first phase, then Gosched
			// to let writer goroutine run (critical for same-process
			// unary ping-pong where reader and writer compete for CPU).
			if i > 0 && i%100 == 0 {
				// Check closed before AND after Gosched — the segment may
				// be unmapped during Gosched, making hdr access unsafe.
				// Break out of spin and let the outer loop's drain logic
				// decide whether remaining data should be read.
				if atomic.LoadUint32(&r.closed) != 0 {
					break
				}
				runtime.Gosched()
				if atomic.LoadUint32(&r.closed) != 0 {
					break
				}
			} else {
				runtime_procyield(1)
			}
			writeIdx = hdr.WriteIndex()
			pendingIdx = atomic.LoadUint64(&r.pendingReadIdx)
			if writeIdx-pendingIdx >= uint64(n) {
				spinSuccess = true
				// Adapt spin cutoff upward (exponential moving average)
				newCutoff := (7*spinCutoff + loadShmSpinMax()) / 8
				if newCutoff > loadShmSpinMax() {
					newCutoff = loadShmSpinMax()
				}
				atomic.StoreUint32(&r.dataSpinCutoff, newCutoff)
				break
			}
		}
		if spinSuccess {
			continue // Loop back to return data
		}
		// Spin timed out - adapt cutoff downward
		newCutoff := (7*spinCutoff + loadShmSpinMin()) / 8
		if newCutoff < loadShmSpinMin() {
			newCutoff = loadShmSpinMin()
		}
		atomic.StoreUint32(&r.dataSpinCutoff, newCutoff)

		// Spin failed, fall back to futex - check context first
		select {
		case <-ctx.Done():
			return nil, nil, nil, ctx.Err()
		default:
		}

		if dl, ok := ctx.Deadline(); ok {
			shmDebugf("ReadSlices: waiting with timeout=%s", time.Until(dl))
		} else {
			shmDebugf("ReadSlices: waiting WITHOUT timeout")
		}

		// Check local closed flag before accessing header
		// If closed, re-check for data - producer may have written before closing
		localClosed = atomic.LoadUint32(&r.closed) != 0
		headerClosed = hdr.Closed()

		if localClosed || headerClosed {
			// Re-check if data appeared (race with producer)
			writeIdx = hdr.WriteIndex()
			pendingIdx = atomic.LoadUint64(&r.pendingReadIdx)
			available := writeIdx - pendingIdx
			if available == 0 {
				return nil, nil, nil, io.EOF
			}
			// If data exists but not enough to satisfy the request, and the ring
			// is closed (no more data will arrive), return EOF rather than looping.
			if available < uint64(n) {
				return nil, nil, nil, io.EOF
			}
			// Enough data appeared, loop back to read it
			continue
		}

		// Snapshot the producer's wake-up sequence, then re-check indices before
		// sleeping to avoid a lost-wake race where the producer commits data after
		// our availability check but before we enter futexWait.
		hdr.IncDataWaiters()
		dataSeq := hdr.DataSequence()
		writeIdx = hdr.WriteIndex()
		pendingIdx = atomic.LoadUint64(&r.pendingReadIdx)
		if writeIdx-pendingIdx >= uint64(n) {
			// Data became available; loop back to return slices.
			hdr.DecDataWaiters()
			continue
		}

		// Re-check closed flag after incrementing waiters and before futexWait
		// to avoid missing a close that happened between our initial check and now
		localClosed = atomic.LoadUint32(&r.closed) != 0
		headerClosed = hdr.Closed()
		if localClosed || headerClosed {
			hdr.DecDataWaiters()
			// Re-check if data appeared
			writeIdx = hdr.WriteIndex()
			pendingIdx = atomic.LoadUint64(&r.pendingReadIdx)
			if writeIdx-pendingIdx >= uint64(n) {
				continue
			}
			return nil, nil, nil, io.EOF
		}

		shmDebugf("[DEBUG] Ring read: no data available, dataSeq=%d, waiting on futex...", dataSeq)

		// If ctx has a deadline, wait with timeout; otherwise, infinite wait.
		var err error
		if deadline, has := ctx.Deadline(); has {
			rem := time.Until(deadline)
			if rem <= 0 {
				// Check if ring is still valid before decrementing
				if atomic.LoadUint32(&r.closed) == 0 {
					hdr.DecDataWaiters()
				}
				return nil, nil, nil, context.DeadlineExceeded
			}
			shmDebugf("[DEBUG] Ring read: calling waitForData with timeout=%v", rem)
			err = r.waitForData(&hdr.dataSeq, dataSeq, rem)
		} else {
			shmDebugf("[DEBUG] Ring read: calling waitForData (no timeout)")
			err = r.waitForData(&hdr.dataSeq, dataSeq, 0)
		}
		// Check if ring is still valid before decrementing - the segment may have
		// been unmapped while we were blocked on futexWait
		if atomic.LoadUint32(&r.closed) == 0 {
			hdr.DecDataWaiters()
		}
		shmDebugf("[DEBUG] Ring read: wait returned, err=%v", err)

		if err != nil {
			// Translate futex timeout to context timeout; keep going on spurious wake.
			if errors.Is(err, ErrFutexTimeout) {
				return nil, nil, nil, context.DeadlineExceeded
			}
			// Other errors: fall through and reloop (spurious wake/etc.)
		}
	}
}

// WriteAll writes all bytes to the ring buffer, blocking as needed.
// This is a convenience method that handles multiple reservations if needed.
// Supports chunking when message > available space.
func (r *ShmRing) WriteAll(ctx context.Context, p []byte) error {
	if len(p) == 0 {
		return nil
	}

	remaining := p
	for len(remaining) > 0 {
		// Reserve space for as much as possible (up to remaining length)
		toWrite := len(remaining)
		if uint64(toWrite) > r.capacity {
			toWrite = int(r.capacity)
		}

		reservation, err := r.ReserveWrite(ctx, toWrite)
		if err != nil {
			return err
		}

		// Copy data into the reservation (zero-copy into ring memory)
		written := 0
		if len(reservation.First) > 0 {
			n := copy(reservation.First, remaining[written:])
			written += n
		}
		if len(reservation.Second) > 0 && written < toWrite {
			n := copy(reservation.Second, remaining[written:])
			written += n
		}

		// Commit the written bytes
		if err := reservation.Commit(written); err != nil {
			return err
		}

		remaining = remaining[written:]
	}

	return nil
}

// ReadExact reads exactly n bytes into dst, blocking as needed.
// If len(dst) >= n, it uses dst as the buffer (alloc-free).
// Otherwise, it allocates a new slice. Handles header reconstruction across wraps.
func (r *ShmRing) ReadExact(ctx context.Context, n int, dst []byte) ([]byte, error) {
	if n <= 0 {
		return nil, errors.New("read size must be positive")
	}

	// Use dst if it's large enough, otherwise allocate
	var result []byte
	if len(dst) >= n {
		result = dst[:n]
	} else {
		result = make([]byte, n)
	}

	totalRead := 0
	for totalRead < n {
		remaining := n - totalRead

		// Read slices for the remaining bytes
		first, second, commit, err := r.ReadSlices(ctx, remaining)
		if err != nil {
			return nil, err
		}

		// Copy from the slices to our result buffer (handles wrap reconstruction)
		copied := 0
		if len(first) > 0 {
			copyLen := len(first)
			if copyLen > remaining {
				copyLen = remaining
			}
			copy(result[totalRead:], first[:copyLen])
			copied += copyLen
		}
		if len(second) > 0 && copied < remaining {
			copyLen := len(second)
			if copyLen > remaining-copied {
				copyLen = remaining - copied
			}
			copy(result[totalRead+copied:], second[:copyLen])
			copied += copyLen
		}

		// Commit the read
		commit.Commit(copied)
		totalRead += copied
	}

	return result, nil
}

// ReserveFrameHeader reserves a 16-byte frame header region.
// NOTE: Header may straddle wrap boundaries. Use returned First/Second slices accordingly.
// The reader supports straddled headers via ReadSlices for proper reconstruction.
//
// Memory ordering follows the SPSC invariant:
//
//	writer: memcpy header -> atomic.Store(w,new) [release] -> AddUint32(dataSeq) -> futex_wake
//	reader: atomic.Load(w) [acquire] -> copy -> atomic.Store(r,new) [release] -> AddUint32(spaceSeq) -> futex_wake
func (r *ShmRing) ReserveFrameHeader(ctx context.Context) (WriteReservation, error) {
	return r.ReserveWrite(ctx, frameHeaderSize)
}
