//go:build linux

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

// Per-data-segment per-direction eventfd wake primitive.
//
// ARCHITECTURE
//
// Each data segment has TWO Linux eventfd(2) instances, one per
// direction:
//
//   evfd_c2s: client -> server wakes. Producer writes (atomic counter
//             += 1). Server reads via *os.File (netpoll, gopark).
//
//   evfd_s2c: server -> client wakes. Symmetric.
//
// Each side's waker holds:
//   myReadFile *os.File  -- this side's recv eventfd, wrapped for
//                            netpoll-integrated Read
//   myReadRawFd int       -- same fd as raw int, for the LOCAL
//                            re-wake path (cascade on spurious wake;
//                            see "Fan-out handling" below)
//   peerReadFd int        -- peer's recv eventfd, raw int; this is
//                            where Wake writes (kernel routes the
//                            +1 to peer's counter)
//
// FD economics per cross-process connection:
//   - 2 eventfds total per data segment (1 per direction)
//   - Each process holds 2 FDs (one for read, one for write)
//   - Compared to SOCK_STREAM socketpair (1 FD per side): we pay
//     +1 FD per side. Compared to per-address eventfd (up to 6 lazy):
//     we save 2-4 FDs at worst case.
//   - The ~14 us per-RPC overhead that SOCK_STREAM cost compared to
//     eventfd is GONE: eventfd's kernel path is the same lightweight
//     u64-counter operation we had in the per-address design.
//
// FAN-OUT HANDLING
//
// Multiple goroutines on the SAME side may park on the SAME direction
// eventfd at once. Concretely, on the server side, BOTH may be parked:
//   - H2 reader waiting on ring A.dataSeq (client writes data)
//   - H2 writer waiting on ring B.spaceSeq (client reads, freeing
//     space on ring B; only happens under back-pressure)
//
// In gRPC's transport model, stream-level parallelism is sequentialised
// through a single H2-reader goroutine and a single H2-writer
// goroutine per connection, so the realistic parker count per side
// per direction is bounded at TWO. Multiple connections (channels)
// each have their own segment and their own eventfds, so cross-
// connection parkers don't share a wake fd.
//
// Two distinct wake-stealing failure modes happen at N=2:
//
//   Mode A: "wrong parker"
//     Peer sends one Wake. Counter goes 0->1, edge fires. Kernel
//     wakes one of the two parked Gs. If it's the wrong one (the G
//     whose condition the Wake was NOT intended for), the right G
//     stays parked.
//
//   Mode B: "right parker, but stole extras"
//     Peer signals BOTH conditions in quick succession (typical under
//     bidirectional back-pressure: signalData immediately followed
//     by signalSpace). Counter goes 0->1 (edge), 1->2 (NO edge --
//     edge-triggered netpoll only fires on the rising 0->non-zero
//     transition). Kernel wakes one G. That G's Read drains counter
//     to 0 in one syscall. If its own condition was met, it returns
//     without notifying the other parker -- the other parker is now
//     stuck forever (no more edges, ring.go uses timeout=0 in the
//     hot path).
//
// Three layers of mitigation handle both modes:
//
//   Layer 1 (producer-side, always on): `if hdr.DataWaiters() > 0 {
//     signal }` retransmission guarantees the producer keeps
//     re-signalling as long as waiters are registered. Helps
//     ping-pong traffic self-heal between bursts.
//
//   Layer 2 (Wait returns drained count): Mode B fix. Wait returns
//     n = drained counter value. WaitForChange immediately writes
//     n-1 wakes back to OUR OWN counter via RewakeLocal so the next
//     edge fires for the second parker. Edge-trigger requirement
//     (counter goes 0 -> non-zero) is satisfied because Read just
//     drained to 0.
//
//   Layer 3 (cascade on wrong-parker, gated by parker count):
//     Mode A fix. If our condition is unmet after Wait, RewakeLocal
//     to hand off to the other parker -- but ONLY if `parkers > 1`
//     (at least one other goroutine is currently inside
//     WaitForChange on this side). Without this gate, a solo parker
//     would write to its own counter and the caller's outer-retry
//     loop would self-spin reading the same write back. parkers is
//     an atomic int32 on shmDataSegWaker, incremented on
//     WaitForChange entry and decremented on exit.
//
// At 64 B / huge-ring bench loads, only the H2 reader parks; n is
// always 1, no extras to redistribute, no cascade fires. Bench wall
// time is unaffected.

package transport

import (
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

// shmDataSegWakeEnabled gates the per-data-segment eventfd-per-
// direction primitive. Off by default; bench harness sets
// SHM_DATASEG_WAKE=1 to opt in. Cross-process child processes
// (GRPC_CROSS_PROCESS_CHILD set) opt out for safety until Phase 2
// wires SCM_RIGHTS.
var shmDataSegWakeEnabled = os.Getenv("SHM_DATASEG_WAKE") == "1" &&
	os.Getenv("GRPC_CROSS_PROCESS_CHILD") == ""

// Diagnostic counters.
var (
	shmDataSegWakeCallsTotal    uint64
	shmDataSegWakeSyscalls      uint64
	shmDataSegWaitCallsTotal    uint64
	shmDataSegWaitSyscalls      uint64
	shmDataSegWaitReturnNil     uint64
	shmDataSegWaitReturnTimeout uint64
	shmDataSegWaitReturnClosed  uint64
	shmDataSegWaitReturnEOF     uint64
	shmDataSegWaitReturnOther   uint64
	// Fan-out cascade counters.
	shmDataSegRewakeLocal  uint64 // local re-wakes for cascade
	shmDataSegFanOutBailout uint64 // wrong-parker but no other parker present
)

// maxSameSideParkers is the production bound on how many goroutines
// may concurrently park on one side of a data-segment direction
// eventfd. In gRPC's transport, this is 2: the H2-reader goroutine
// (waiting on rx.dataSeq) and the H2-writer goroutine (waiting on
// tx.spaceSeq or tx.contigSeq under back-pressure). Stream-level
// parallelism does not multiply parkers because all streams are
// sequentialised through these two goroutines. Multiple connections
// (channels) each have their own segment and eventfds.
//
// Used to bound the Wait-counter redistribution: when Wait returns
// n > 1, we write min(n-1, maxSameSideParkers-1) wakes back to our
// own counter so the other parker observes its edge. Capping at 1
// (= maxSameSideParkers-1) prevents an O(n^2) feedback storm where
// a coalesced wake burst self-amplifies through repeated reads of
// our own writes.
const maxSameSideParkers = 2

// shmDataSegWaker holds one side's view of a pair of per-direction
// eventfds. See file-header comment for design rationale.
type shmDataSegWaker struct {
	// myReadFile is this side's recv eventfd, wrapped for netpoll
	// Read. Peer wakes us by writing to it (kernel atomic_add to
	// the counter); we drain via *os.File.Read (gopark in user
	// space until the counter is non-zero).
	myReadFile *os.File
	// myReadRawFd is the raw fd of myReadFile, used by RewakeLocal
	// to bypass *os.File's poll mutex on the cascade write path.
	myReadRawFd int
	// peerReadFd is the raw fd of the PEER's recv eventfd. Wake
	// writes here to wake the peer.
	peerReadFd int
	// closed marks the waker as torn down. After Close, Wake is a
	// no-op and Wait returns ErrRingClosed.
	closed atomic.Uint32
	// closeOnce guards the file close path.
	closeOnce sync.Once
	// deadlineSet tracks whether myReadFile currently has a non-zero
	// Read deadline so the hot ring loop (which uses timeout=0) can
	// skip the SetReadDeadline syscall when redundant. See Wait().
	deadlineSet atomic.Uint32
	// parkers counts the number of goroutines currently inside
	// WaitForChange on this side (between increment-on-entry and
	// decrement-on-exit). Used to skip the wrong-parker cascade
	// RewakeLocal when this G is the only parker, preventing
	// self-spin in caller's outer-retry loop.
	parkers atomic.Int32
}

// newShmDataSegWakerPair creates TWO eventfds (one per direction)
// and returns two wakers, each holding the appropriate read/write
// assignment so writes from side A land in side B's counter and
// vice versa.
//
// Counter mode (default, NOT EFD_SEMAPHORE): each read drains the
// entire accumulated counter to zero. This is the right choice for
// our use case where typical traffic has 1-2 parkers per direction
// (one reader on dataSeq, one writer on spaceSeq under back-pressure).
// Counter mode coalesces bursts of producer wakes into a single read
// syscall, which keeps the per-RPC syscall cost low.
//
// EFD_SEMAPHORE was evaluated but rejected: it forces one read per
// wake, which combined with the cascade triples the syscall budget
// (measured +12% latency at 64 B). We instead handle the realistic
// N=2 fan-out case (reader + writer parked simultaneously, peer
// signals both in quick succession) by reading the drained counter
// value in Wait and having WaitForChange redistribute the n-1 extras
// via RewakeLocal so the second parker observes its edge fire. See
// WaitForChange for the cascade logic and the file-header comment
// for the production parker model.
//
// Returns (nil, nil, err) if any eventfd syscall fails (caller
// should fall back to the per-address eventfd registry / futex).
func newShmDataSegWakerPair() (*shmDataSegWaker, *shmDataSegWaker, error) {
	const flags = unix.EFD_NONBLOCK | unix.EFD_CLOEXEC
	efd1, err := unix.Eventfd(0, flags)
	if err != nil {
		return nil, nil, err
	}
	efd2, err := unix.Eventfd(0, flags)
	if err != nil {
		unix.Close(efd1)
		return nil, nil, err
	}
	f1 := os.NewFile(uintptr(efd1), "shm-dataseg-efd-1")
	f2 := os.NewFile(uintptr(efd2), "shm-dataseg-efd-2")
	if f1 == nil || f2 == nil {
		unix.Close(efd1)
		unix.Close(efd2)
		return nil, nil, errors.New("os.NewFile failed for eventfd")
	}
	// Side A reads efd1 (B writes here), writes to efd2 (B reads it).
	// Side B is the reverse.
	a := &shmDataSegWaker{myReadFile: f1, myReadRawFd: efd1, peerReadFd: efd2}
	b := &shmDataSegWaker{myReadFile: f2, myReadRawFd: efd2, peerReadFd: efd1}
	return a, b, nil
}

// Wake writes 1 to the peer's eventfd counter via a direct unix.Write
// on peerReadFd. The kernel atomic-adds 1 to the counter and wakes
// any reader parked on the peer's *os.File via netpoll.
//
// Critical: bypasses *os.File / netpoll on the send path. netpoll
// would absorb EAGAIN and park the writer, which deadlocks producers
// that can't also be readers.
func (w *shmDataSegWaker) Wake() {
	atomic.AddUint64(&shmDataSegWakeCallsTotal, 1)
	if w.closed.Load() != 0 {
		return
	}
	// eventfd write is a uint64 little-endian. The "1" increments
	// the counter; we don't care about absolute values, just that
	// the counter is non-zero so a reader wakes.
	var b = [8]byte{1, 0, 0, 0, 0, 0, 0, 0}
	atomic.AddUint64(&shmDataSegWakeSyscalls, 1)
	// Ignore errors: EBADF if peer closed, EAGAIN at u64-1 counter
	// saturation (effectively impossible).
	_, _ = unix.Write(w.peerReadFd, b[:])
}

// RewakeLocal writes 1 to OUR OWN eventfd counter, waking any other
// goroutine on this side that is parked on myReadFile. This is the
// fan-out cascade primitive: when a spurious wake reaches the wrong
// waiter, the wrong waiter re-issues a local wake so the correct
// waiter can pick it up.
//
// Only called by WaitForChange when at least one other parker is
// present (gated by parkers > 1) to avoid self-spin in the caller's
// outer-retry loop.
//
// Unlike Wake, the byte goes to OUR counter (not peer's). Peer's
// reader is unaffected.
func (w *shmDataSegWaker) RewakeLocal() {
	if w.closed.Load() != 0 {
		return
	}
	var b = [8]byte{1, 0, 0, 0, 0, 0, 0, 0}
	atomic.AddUint64(&shmDataSegRewakeLocal, 1)
	_, _ = unix.Write(w.myReadRawFd, b[:])
}

// Wait blocks until our counter becomes non-zero, the waker is
// Close()d, or timeout elapses. Returns (n, nil) on a real wake
// where n is the drained counter value (number of Wake / RewakeLocal
// writes that accumulated since the last Read), (0, ErrRingClosed)
// on close, (0, ErrFutexTimeout) on timeout.
//
// Counter-mode eventfd Read drains the full accumulated counter to
// zero. Callers that want fan-out to multiple same-side parkers
// must redistribute n-1 wakes via RewakeLocal -- WaitForChange does
// this for us.
//
// timeout == 0 means block indefinitely. SetReadDeadline is elided
// when the current deadline state matches the request (saves ~1 us
// per call on the hot path of ring waits, which use timeout=0).
func (w *shmDataSegWaker) Wait(timeout time.Duration) (uint64, error) {
	atomic.AddUint64(&shmDataSegWaitCallsTotal, 1)
	if w.closed.Load() != 0 {
		atomic.AddUint64(&shmDataSegWaitReturnClosed, 1)
		return 0, ErrRingClosed
	}

	if timeout > 0 {
		if err := w.myReadFile.SetReadDeadline(time.Now().Add(timeout)); err != nil {
			atomic.AddUint64(&shmDataSegWaitReturnClosed, 1)
			return 0, ErrRingClosed
		}
		w.deadlineSet.Store(1)
	} else if w.deadlineSet.Load() != 0 {
		if err := w.myReadFile.SetReadDeadline(time.Time{}); err != nil {
			atomic.AddUint64(&shmDataSegWaitReturnClosed, 1)
			return 0, ErrRingClosed
		}
		w.deadlineSet.Store(0)
	}

	// eventfd Read consumes the entire counter into 8 bytes
	// (little-endian uint64). The value is the number of wakes that
	// accumulated; callers use it to redistribute extras.
	var b [8]byte
	atomic.AddUint64(&shmDataSegWaitSyscalls, 1)
	_, err := w.myReadFile.Read(b[:])
	if err == nil {
		atomic.AddUint64(&shmDataSegWaitReturnNil, 1)
		n := uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24 |
			uint64(b[4])<<32 | uint64(b[5])<<40 | uint64(b[6])<<48 | uint64(b[7])<<56
		return n, nil
	}
	if os.IsTimeout(err) {
		atomic.AddUint64(&shmDataSegWaitReturnTimeout, 1)
		return 0, ErrFutexTimeout
	}
	if w.closed.Load() != 0 {
		atomic.AddUint64(&shmDataSegWaitReturnClosed, 1)
		return 0, ErrRingClosed
	}
	if errors.Is(err, io.EOF) {
		// Eventfd never returns EOF (it's not a stream). If we see
		// this it's a programming error or kernel bug -- count it
		// distinctly and surface as ring-closed to drop out cleanly.
		atomic.AddUint64(&shmDataSegWaitReturnEOF, 1)
		return 0, ErrRingClosed
	}
	atomic.AddUint64(&shmDataSegWaitReturnOther, 1)
	return 0, err
}

// WaitForChange parks on the eventfd until the uint32 at addr changes
// away from val (the condition the caller wants), the waker is
// Close()d, or timeout elapses.
//
// Fan-out correctness in counter mode:
//
// Production has up to 2 parkers per side on the same direction's
// eventfd (H2 reader on dataSeq + H2 writer on spaceSeq under
// back-pressure). Peer Wake = +1 to our counter. Our Read drains
// the entire accumulated counter, which can swallow wakes meant
// for OTHER same-side parkers when several peer signals coalesce
// faster than we can react.
//
// Two cascade primitives handle the two fan-out cases:
//
//  1. "Right parker, but stole extras" (the realistic N=2 case):
//     Wait returns n = drained count. If n > 1, we know there were
//     wakes intended for other parkers. We write ONE wake back to
//     OUR OWN counter via RewakeLocal so the next edge fires for
//     them. Edge-triggered netpoll requires the counter to
//     transition from 0 to non-zero, which is satisfied because we
//     just drained to 0 in Wait. Capped at 1 because production has
//     at most 2 parkers per side (uncapped would create O(n^2)
//     feedback).
//
//  2. "Wrong parker" (kernel woke the wrong G of two parked):
//     After Wait + redistribute, we check OUR condition. If unmet
//     AND parkers > 1, RewakeLocal once to hand the wake off to
//     the other parker. The parker-count gate prevents a solo
//     parker from writing-to-self-and-reading-back in a self-spin.
//
// Always returns after a single Wait + condition check (no internal
// retry loop). The caller's outer-retry loop (e.g., ring.go's
// waitForData) handles re-parking when our own condition is still
// unmet. This is correct because: (a) a wake we received with our
// condition unmet has been handed off via RewakeLocal, (b) the next
// producer signal will wake us via a fresh edge fire.
//
// Returns:
//   - nil: addr changed (caller's condition met)
//   - ErrRingClosed / ErrFutexTimeout / other errors as in Wait()
//   - nil with addr unchanged: wrong-parker case; caller's outer
//     loop should re-check ring state and re-park if still unmet
func (w *shmDataSegWaker) WaitForChange(addr *uint32, val uint32, timeout time.Duration) error {
	if w == nil {
		return errors.New("nil waker")
	}
	w.parkers.Add(1)
	defer w.parkers.Add(-1)

	n, err := w.Wait(timeout)
	if err != nil {
		return err
	}
	// Drained counter > 1: extras were intended for other same-
	// side parker. Redistribute one wake back to OUR own counter
	// so its edge fires (counter went 0 after our Read; first
	// write fires the edge). Capped at 1 because production has
	// at most 2 parkers per side; an O(n) loop would create an
	// O(n^2) feedback storm.
	if n > 1 {
		w.RewakeLocal()
	}
	if atomic.LoadUint32(addr) != val {
		// Caller's condition met; happy path.
		return nil
	}
	// Wrong parker: our condition is unmet, so the wake was meant
	// for another same-side parker. Cascade ONLY if another parker
	// is currently waiting -- otherwise we'd just write to our own
	// counter and the caller's outer-retry loop would self-spin
	// reading our own write. parkers > 1 means at least one other
	// goroutine is inside WaitForChange on this side.
	if w.parkers.Load() > 1 {
		w.RewakeLocal()
	} else {
		atomic.AddUint64(&shmDataSegFanOutBailout, 1)
	}
	return nil
}

// Close releases this side's read-end eventfd FD. Peer-read FD is
// owned by the PEER's waker and closed by their Close. Idempotent.
//
// Closing one process's view of an eventfd does NOT propagate "EOF"
// to the peer's view (unlike SOCK_STREAM socketpair). The kernel
// eventfd object stays alive as long as any process holds a
// reference. Peer's reads continue to work; peer's writes to its
// peerReadFd (which is OUR closed fd) return EBADF, swallowed by
// Wake.
func (w *shmDataSegWaker) Close() {
	w.closeOnce.Do(func() {
		w.closed.Store(1)
		_ = w.myReadFile.Close()
	})
}

// shmDataSegWakerStash is the same-process rendezvous keyed by
// segment path. Cross-process (Phase 2) will replace this with
// SCM_RIGHTS.
var shmDataSegWakerStash = struct {
	mu sync.Mutex
	m  map[string]*shmDataSegWaker
}{m: make(map[string]*shmDataSegWaker)}

func stashShmDataSegWakerForOpener(segmentPath string, peer *shmDataSegWaker) {
	if peer == nil {
		return
	}
	shmDataSegWakerStash.mu.Lock()
	if old := shmDataSegWakerStash.m[segmentPath]; old != nil {
		old.Close()
	}
	shmDataSegWakerStash.m[segmentPath] = peer
	shmDataSegWakerStash.mu.Unlock()
}

func claimShmDataSegWakerForOpener(segmentPath string) *shmDataSegWaker {
	shmDataSegWakerStash.mu.Lock()
	w := shmDataSegWakerStash.m[segmentPath]
	delete(shmDataSegWakerStash.m, segmentPath)
	shmDataSegWakerStash.mu.Unlock()
	return w
}

func dropShmDataSegWakerStash(segmentPath string) {
	if w := claimShmDataSegWakerForOpener(segmentPath); w != nil {
		w.Close()
	}
}

// ShmDataSegWakeCounters is a snapshot of the diagnostic counters.
type ShmDataSegWakeCounters struct {
	WakeCallsTotal    uint64
	WakeSyscalls      uint64
	WaitCallsTotal    uint64
	WaitSyscalls      uint64
	WaitReturnNil     uint64
	WaitReturnTimeout uint64
	WaitReturnClosed  uint64
	WaitReturnEOF     uint64
	WaitReturnOther   uint64
	RewakeLocal       uint64
	FanOutBailout     uint64
}

func (a ShmDataSegWakeCounters) Sub(b ShmDataSegWakeCounters) ShmDataSegWakeCounters {
	return ShmDataSegWakeCounters{
		WakeCallsTotal:    a.WakeCallsTotal - b.WakeCallsTotal,
		WakeSyscalls:      a.WakeSyscalls - b.WakeSyscalls,
		WaitCallsTotal:    a.WaitCallsTotal - b.WaitCallsTotal,
		WaitSyscalls:      a.WaitSyscalls - b.WaitSyscalls,
		WaitReturnNil:     a.WaitReturnNil - b.WaitReturnNil,
		WaitReturnTimeout: a.WaitReturnTimeout - b.WaitReturnTimeout,
		WaitReturnClosed:  a.WaitReturnClosed - b.WaitReturnClosed,
		WaitReturnEOF:     a.WaitReturnEOF - b.WaitReturnEOF,
		WaitReturnOther:   a.WaitReturnOther - b.WaitReturnOther,
		RewakeLocal:       a.RewakeLocal - b.RewakeLocal,
		FanOutBailout:     a.FanOutBailout - b.FanOutBailout,
	}
}

func LoadShmDataSegWakeCounters() ShmDataSegWakeCounters {
	return ShmDataSegWakeCounters{
		WakeCallsTotal:    atomic.LoadUint64(&shmDataSegWakeCallsTotal),
		WakeSyscalls:      atomic.LoadUint64(&shmDataSegWakeSyscalls),
		WaitCallsTotal:    atomic.LoadUint64(&shmDataSegWaitCallsTotal),
		WaitSyscalls:      atomic.LoadUint64(&shmDataSegWaitSyscalls),
		WaitReturnNil:     atomic.LoadUint64(&shmDataSegWaitReturnNil),
		WaitReturnTimeout: atomic.LoadUint64(&shmDataSegWaitReturnTimeout),
		WaitReturnClosed:  atomic.LoadUint64(&shmDataSegWaitReturnClosed),
		WaitReturnEOF:     atomic.LoadUint64(&shmDataSegWaitReturnEOF),
		WaitReturnOther:   atomic.LoadUint64(&shmDataSegWaitReturnOther),
		RewakeLocal:       atomic.LoadUint64(&shmDataSegRewakeLocal),
		FanOutBailout:     atomic.LoadUint64(&shmDataSegFanOutBailout),
	}
}
