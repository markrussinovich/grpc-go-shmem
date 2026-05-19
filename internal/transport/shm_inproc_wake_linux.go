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

package transport

import (
	"os"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// SHM same-process eventfd wake mechanism. ACTIVATED ONLY when the
// SHM_INPROC_WAKE=1 env var is set, AND only when both endpoints are
// in the same Go process (the operator's promise).
//
// Why it exists:
//
// The kernel futex on Linux costs ~15-30 us per wake/wait cycle because
// of `syscall.Syscall6`'s entersyscallblock / exitsyscall dance, mostly
// because exitsyscall triggers runtime.wakep to spread wake load
// across cores. UDS over the same Go runtime gets to ~3-5 us per wake
// because Go's netpoll integration uses pure user-space gopark /
// goready instead of a real kernel syscall.
//
// This implementation uses a Linux eventfd(2) as the wake channel for
// each (segmentID, address-offset) key. The single FD per waker is
// wrapped in *os.File, which Go's runtime automatically registers
// with epoll. Read through *os.File routes through internal/poll,
// which gopark's the goroutine in user space — the same mechanism
// net.UnixConn uses to make UDS fast. Write is done via direct
// unix.Write on the raw FD so the producer doesn't pay netpoll's
// per-call mutex / poll-desc cost (and so EAGAIN, if it ever
// happened, would not gopark the writer).
//
// Earlier revisions of this primitive used an AF_UNIX SOCK_DGRAM
// socketpair (2 FDs per waker, 4 FDs per connection). eventfd halves
// the FD count and the kernel object is a single u64 counter rather
// than a full socket + skb queue + waitqueue. Doug Fawley's gRFC
// review explicitly flagged Google-internal experience where epoll
// integration with 4 FDs per connection regressed small-payload
// performance, which is the exact tradeoff this primitive sits on top
// of, so cutting FD count was worth a careful experiment. The
// resulting 64 B ping-pong dropped from ~90 us (socketpair) to ~38 us
// (eventfd) on WSL2 EPYC 7763, and the new number beats UDS at every
// size.
//
// Coalescing semantics: each Wake increments the eventfd counter by
// 1. A consumer Read drains the entire counter in one call. If 5
// Wakes arrive between two Reads, the consumer sees a single Read
// return after the first Wake; it then re-checks ring state and
// observes all 5 commits at once. Tests that assume "one Wake = one
// Read return" must be written against duration bounds rather than
// iteration counts.
//
// This is the same-process, prove-out-architecture form of the
// "FD-backed wake channel" described in the upcoming gRFC v2 revision.
// Cross-process operation requires SCM_RIGHTS FD passing during the
// SHM handshake; that work is a follow-up.
//
// The shared-memory DataSeq / SpaceSeq sequence fields remain present
// and are still incremented on every commit, so a fallback futex path
// can in principle be selected at handshake time. The current
// implementation only routes through the FD path when SHM_INPROC_WAKE=1;
// otherwise it falls through to the existing futex code.

var shmInprocWakeEnabled = os.Getenv("SHM_INPROC_WAKE") == "1" &&
	os.Getenv("GRPC_CROSS_PROCESS_CHILD") == ""

// Diagnostic counters (bumped on hot path; reset via the
// ResetShmInprocWakeCountersForBench function). Used by the benchmark
// suite to answer "how many wakes per RPC" without strace overhead
// distorting the timing.
var (
	// shmInprocWakeCallsTotal counts every Wake() entry (including
	// no-ops when w.closed != 0). Divide by b.N to get wakes per RPC.
	shmInprocWakeCallsTotal uint64
	// shmInprocWakeSyscalls counts every unix.Write actually issued
	// in Wake() (i.e., excludes the closed-fast-path early return).
	// Divide by b.N for wake-syscalls per RPC.
	shmInprocWakeSyscalls uint64
	// shmInprocWaitCallsTotal counts every Wait() entry.
	shmInprocWaitCallsTotal uint64
	// shmInprocWaitSyscalls counts every recv.Read actually issued
	// in Wait() (i.e., we got to the kernel; matches what netpoll
	// would gopark on).
	shmInprocWaitSyscalls uint64
	// shmInprocWaitSyscallReturned counts Wait()s that came back
	// with nil from Read (i.e., a real Wake landed). Misses on this
	// minus shmInprocWaitSyscalls = timeouts + closed-fd returns.
	shmInprocWaitSyscallReturned uint64
)

// shmInprocWaker wraps a single eventfd. The producer increments the
// counter via a direct unix.Write of an 8-byte uint64 (1) onto fd;
// the consumer drains the counter via *os.File.Read through netpoll.
// Multiple producer-side Wakes coalesce into the counter and a single
// Read drains them all — see top-of-file comment for details.
//
// fd is wrapped in *os.File for netpoll-integrated Read. Wake calls
// `unix.Write(fd.Fd(), buf[:])` directly to bypass netpoll on the
// send path (netpoll would absorb EAGAIN and gopark the writer; not
// what we want, and for eventfd EAGAIN is essentially impossible
// anyway, but consistency with the socketpair version keeps the
// teardown path symmetric).
//
// EAGAIN on Wake is treated as "wake already pending, OK to skip".
type shmInprocWaker struct {
	// fd is the underlying eventfd file. Reads go through netpoll;
	// writes go through a raw unix.Write on fd.Fd().
	fd *os.File
	// rawFd caches fd.Fd() so Wake doesn't have to acquire the
	// internal/poll mutex on every call. Set once at construction
	// and never mutated, so racing with Close is fine — unix.Write
	// on a closed FD returns EBADF which we swallow.
	rawFd int
	// closed marks the waker as torn down. After close, Wait returns
	// immediately and Wake is a no-op.
	closed atomic.Uint32
	// closeOnce guards the file close paths.
	closeOnce sync.Once
}

// newShmInprocWaker creates an eventfd and returns the wrapper.
// Returns nil if the syscall fails (caller should fall back to futex).
func newShmInprocWaker() *shmInprocWaker {
	// eventfd2(initval=0, flags=NONBLOCK|CLOEXEC). NONBLOCK lets
	// Read return EAGAIN (which netpoll then converts to gopark) and
	// lets Wake's unix.Write be a fast no-wait path; CLOEXEC keeps
	// the FD out of any future exec.
	efd, err := unix.Eventfd(0, unix.EFD_NONBLOCK|unix.EFD_CLOEXEC)
	if err != nil {
		return nil
	}
	file := os.NewFile(uintptr(efd), "shm-wake-evfd")
	if file == nil {
		unix.Close(efd)
		return nil
	}
	return &shmInprocWaker{fd: file, rawFd: efd}
}

// Wake increments the eventfd counter by 1 via a direct unix.Write on
// the raw FD. The kernel adds the value to the counter and wakes any
// reader (our Wait via netpoll). If multiple Wakes accumulate before
// the consumer drains, the counter just sums them and a single read
// clears the whole batch — implicit coalescing.
//
// Critical: this MUST NOT go through *os.File / netpoll on the send
// side. netpoll wraps Write in internal/poll's runtime_pollWait dance
// which would burn extra CPU on the hot path and can gopark on
// EAGAIN. For eventfd EAGAIN only happens at u64-max-1 (effectively
// never), but the direct-syscall path is also a few hundred ns cheaper
// per call.
func (w *shmInprocWaker) Wake() {
	atomic.AddUint64(&shmInprocWakeCallsTotal, 1)
	if w.closed.Load() != 0 {
		return
	}
	// eventfd write is uint64 little-endian. The "1" increments the
	// internal counter; we don't care about absolute values.
	var b = [8]byte{1, 0, 0, 0, 0, 0, 0, 0}
	atomic.AddUint64(&shmInprocWakeSyscalls, 1)
	// We intentionally ignore all errors: EAGAIN (counter saturated)
	// means the consumer already has a huge pending wake, and any
	// other error (EBADF on race with Close) is also fine to swallow
	// since the consumer's next Wait will re-check w.closed.
	_, _ = unix.Write(w.rawFd, b[:])
}

// Wait blocks until Wake is called, the waker is Close()d, or timeout
// elapses. Returns nil on wake, ErrRingClosed if the waker was closed,
// ErrFutexTimeout on timeout (so callers can map both wake mechanisms
// onto the same control-flow path).
//
// timeout == 0 means block indefinitely (matching the futex path's
// timeout = 0 semantics). timeout > 0 sets a read deadline on the
// underlying FD; deadline expiry surfaces as ErrFutexTimeout.
//
// No goroutine is spawned per call: the cancellation channel is the
// FD itself, so a Close from another goroutine breaks the Read with
// EBADF / use-of-closed-file. This matters because waitForData /
// waitForSpace / waitForContig are called from the hot path of every
// stream and we cannot afford a goroutine launch per park.
func (w *shmInprocWaker) Wait(timeout time.Duration) error {
	atomic.AddUint64(&shmInprocWaitCallsTotal, 1)
	if w.closed.Load() != 0 {
		return ErrRingClosed
	}

	deadline := time.Time{}
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	if err := w.fd.SetReadDeadline(deadline); err != nil {
		// Most likely "use of closed file" — treat as ring closed.
		return ErrRingClosed
	}

	// eventfd Read consumes the entire counter into 8 bytes
	// (little-endian uint64). We don't care about the value; any
	// successful read means at least one Wake happened.
	var b [8]byte
	atomic.AddUint64(&shmInprocWaitSyscalls, 1)
	_, err := w.fd.Read(b[:])
	if err == nil {
		atomic.AddUint64(&shmInprocWaitSyscallReturned, 1)
		return nil
	}
	// Deadline-exceeded comes back as os.ErrDeadlineExceeded.
	if os.IsTimeout(err) {
		return ErrFutexTimeout
	}
	// Closed file: treat as ring closed for the caller's purposes.
	if w.closed.Load() != 0 {
		return ErrRingClosed
	}
	// Other errors: return as-is; the call site will loop and re-check
	// ring state, which is the same recovery used by the futex path.
	return err
}

// Close releases the eventfd. Idempotent.
func (w *shmInprocWaker) Close() {
	w.closeOnce.Do(func() {
		w.closed.Store(1)
		// Closing the *os.File wakes any parked reader with EBADF via
		// the Go runtime's netpoll teardown path, and also frees the
		// underlying eventfd FD. No separate raw close needed.
		_ = w.fd.Close()
	})
}

// shmInprocWakerRegistry maps (segmentID, byte-offset-within-mmap)
// to wakers. Both the producer's signalData and the consumer's
// waitForData look up the same key because both reference the same
// shared-memory uint32 at the same offset within the same backing
// file.
//
// Why not key by *uint32 address directly: two ShmRings that wrap the
// same /dev/shm file via separate mmap calls get DIFFERENT virtual
// addresses (Linux mmap does not guarantee a specific vaddr; each
// mmap gets its own). So the producer's &hdr.dataSeq and the
// consumer's &hdr.dataSeq are not equal pointers — they alias the
// same byte but at different vaddrs. The byte offset within the
// segment IS identical across mappings, so that's our stable key.
var shmInprocWakerRegistry = struct {
	mu     sync.Mutex
	wakers map[shmInprocKey]*shmInprocWaker
}{wakers: make(map[shmInprocKey]*shmInprocWaker)}

type shmInprocKey struct {
	segmentID string
	offset    uintptr // offset of the wake-address uint32 within the mmap
}

// getInprocWaker returns the waker for the given (segmentID, addr)
// pair, creating one on first call. base is the mmap base pointer
// (i.e., &r.mem[0]); addr is the wake-address uint32 within that
// mmap. Their difference gives the stable offset key.
//
// Returns nil if creating the underlying socketpair fails — caller
// should fall through to the futex path in that case.
func getInprocWaker(segmentID string, base unsafe.Pointer, addr *uint32) *shmInprocWaker {
	key := shmInprocKey{
		segmentID: segmentID,
		offset:    uintptr(unsafe.Pointer(addr)) - uintptr(base),
	}
	shmInprocWakerRegistry.mu.Lock()
	w, ok := shmInprocWakerRegistry.wakers[key]
	if !ok {
		w = newShmInprocWaker()
		if w == nil {
			shmInprocWakerRegistry.mu.Unlock()
			return nil
		}
		shmInprocWakerRegistry.wakers[key] = w
	}
	shmInprocWakerRegistry.mu.Unlock()
	return w
}

// dropInprocWakersForSegment removes every waker tied to the given
// segmentID and closes its socketpair. Called when a segment closes
// so the registry doesn't accumulate dead entries across tests /
// connections, and so any goroutines still parked on Recv return
// promptly.
//
// Safe to call multiple times; idempotent.
func dropInprocWakersForSegment(segmentID string) {
	if segmentID == "" {
		return
	}
	shmInprocWakerRegistry.mu.Lock()
	var toClose []*shmInprocWaker
	for k, w := range shmInprocWakerRegistry.wakers {
		if k.segmentID != segmentID {
			continue
		}
		toClose = append(toClose, w)
		delete(shmInprocWakerRegistry.wakers, k)
	}
	shmInprocWakerRegistry.mu.Unlock()
	for _, w := range toClose {
		w.Close()
	}
}

// ShmInprocWakeCounters is a snapshot of the diagnostic counters
// described next to the var block at the top of this file. Returned
// by LoadShmInprocWakeCounters for bench harness consumption.
type ShmInprocWakeCounters struct {
	WakeCallsTotal      uint64
	WakeSyscalls        uint64
	WaitCallsTotal      uint64
	WaitSyscalls        uint64
	WaitSyscallReturned uint64
}

// Sub returns the difference between two snapshots (after - before).
func (a ShmInprocWakeCounters) Sub(before ShmInprocWakeCounters) ShmInprocWakeCounters {
	return ShmInprocWakeCounters{
		WakeCallsTotal:      a.WakeCallsTotal - before.WakeCallsTotal,
		WakeSyscalls:        a.WakeSyscalls - before.WakeSyscalls,
		WaitCallsTotal:      a.WaitCallsTotal - before.WaitCallsTotal,
		WaitSyscalls:        a.WaitSyscalls - before.WaitSyscalls,
		WaitSyscallReturned: a.WaitSyscallReturned - before.WaitSyscallReturned,
	}
}

// LoadShmInprocWakeCounters returns a snapshot of the in-proc wake
// counters. Safe to call concurrently with the data plane.
func LoadShmInprocWakeCounters() ShmInprocWakeCounters {
	return ShmInprocWakeCounters{
		WakeCallsTotal:      atomic.LoadUint64(&shmInprocWakeCallsTotal),
		WakeSyscalls:        atomic.LoadUint64(&shmInprocWakeSyscalls),
		WaitCallsTotal:      atomic.LoadUint64(&shmInprocWaitCallsTotal),
		WaitSyscalls:        atomic.LoadUint64(&shmInprocWaitSyscalls),
		WaitSyscallReturned: atomic.LoadUint64(&shmInprocWaitSyscallReturned),
	}
}
