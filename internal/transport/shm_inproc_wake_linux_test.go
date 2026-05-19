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
	"errors"
	"sync"
	"testing"
	"time"
	"unsafe"
)

// TestShmInprocWaker_WakeUnblocksWait verifies the basic primitive:
// Wake on the producer side unblocks a goroutine parked in Wait on
// the consumer side, and Wait returns nil.
func TestShmInprocWaker_WakeUnblocksWait(t *testing.T) {
	w := newShmInprocWaker()
	if w == nil {
		t.Fatal("newShmInprocWaker returned nil")
	}
	defer w.Close()

	done := make(chan error, 1)
	go func() {
		done <- w.Wait(2 * time.Second)
	}()

	// Give the reader a moment to actually park on Read.
	time.Sleep(10 * time.Millisecond)
	w.Wake()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Wait returned %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not return after Wake")
	}
}

// TestShmInprocWaker_TimeoutReturnsErrFutexTimeout verifies that a
// Wait that nobody Wake()s returns ErrFutexTimeout once its deadline
// elapses, so the caller can map it onto the same control-flow path
// as the futex path.
func TestShmInprocWaker_TimeoutReturnsErrFutexTimeout(t *testing.T) {
	w := newShmInprocWaker()
	if w == nil {
		t.Fatal("newShmInprocWaker returned nil")
	}
	defer w.Close()

	start := time.Now()
	err := w.Wait(50 * time.Millisecond)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrFutexTimeout) {
		t.Fatalf("Wait returned %v, want ErrFutexTimeout", err)
	}
	if elapsed < 40*time.Millisecond {
		t.Fatalf("Wait returned after %v, expected ~50ms", elapsed)
	}
}

// TestShmInprocWaker_CloseUnblocksWait verifies that closing the
// waker while a reader is parked returns ErrRingClosed (or an error
// that the caller treats as ring-closed). The fast-path check at the
// top of Wait also returns ErrRingClosed for any subsequent call.
func TestShmInprocWaker_CloseUnblocksWait(t *testing.T) {
	w := newShmInprocWaker()
	if w == nil {
		t.Fatal("newShmInprocWaker returned nil")
	}

	done := make(chan error, 1)
	go func() {
		done <- w.Wait(2 * time.Second)
	}()

	time.Sleep(10 * time.Millisecond)
	w.Close()

	select {
	case err := <-done:
		// Either ErrRingClosed (fast path saw closed=1) or some
		// non-nil error from the broken-fd Read; both are acceptable
		// recovery signals for the caller.
		if err == nil {
			t.Fatal("Wait returned nil after Close, want non-nil error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not return after Close")
	}

	// Subsequent Wait sees the closed flag and returns immediately.
	if err := w.Wait(time.Second); !errors.Is(err, ErrRingClosed) {
		t.Fatalf("post-Close Wait returned %v, want ErrRingClosed", err)
	}
}

// TestShmInprocWaker_WakeCoalesces fires many Wake() calls back-to-
// back. The kernel buffer of a SOCK_DGRAM socketpair is small, so
// most writes return EAGAIN and are silently dropped. None of them
// must block or panic; a single Wait must return as long as at least
// one Wake landed.
func TestShmInprocWaker_WakeCoalesces(t *testing.T) {
	w := newShmInprocWaker()
	if w == nil {
		t.Fatal("newShmInprocWaker returned nil")
	}
	defer w.Close()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 10000; i++ {
			w.Wake()
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Wake calls blocked or never completed")
	}

	if err := w.Wait(time.Second); err != nil {
		t.Fatalf("Wait after Wake burst returned %v, want nil", err)
	}
}

// TestShmInprocWaker_WakeBeforeWaitReturnsImmediately verifies the
// "wake before park" ordering: a Wake() that lands before the
// consumer ever calls Wait() must NOT be lost. The 1-byte token
// sits in the kernel socket buffer and the next Wait() returns
// immediately with nil. This is the critical correctness property
// that lets ring.go's outer loop pattern (publish-then-signal on
// producer; load-then-park on consumer) work without explicit
// per-byte sequencing.
func TestShmInprocWaker_WakeBeforeWaitReturnsImmediately(t *testing.T) {
	w := newShmInprocWaker()
	if w == nil {
		t.Fatal("newShmInprocWaker returned nil")
	}
	defer w.Close()

	w.Wake()

	// Use a non-trivial timeout so a regression that DID lose the
	// wake would hang for at least 50 ms — easily distinguishable
	// from the few microseconds the netpoll Read takes when the
	// token is already buffered.
	start := time.Now()
	if err := w.Wait(2 * time.Second); err != nil {
		t.Fatalf("Wait after pre-park Wake returned %v, want nil", err)
	}
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Fatalf("Wait after pre-park Wake took %v, expected <50ms (wake may have been lost and only the spurious-return fallback woke us)", d)
	}
}

// TestShmInprocWaker_RegistryIdentity verifies that getInprocWaker
// returns the same instance for the same (segmentID, offset) key
// across calls, and different instances for different keys. This is
// what allows the producer's signalData and the consumer's
// waitForData to rendezvous on the same socketpair when they wrap
// distinct mmaps of the same backing file.
func TestShmInprocWaker_RegistryIdentity(t *testing.T) {
	segID := "test-seg-identity"
	defer dropInprocWakersForSegment(segID)

	// Two distinct backing arrays simulate two distinct mmaps of the
	// same shared-memory file. base differs; offset within base does
	// not.
	baseA := make([]byte, 128)
	baseB := make([]byte, 128)
	addrA := (*uint32)(unsafe.Pointer(&baseA[64]))
	addrB := (*uint32)(unsafe.Pointer(&baseB[64]))

	w1 := getInprocWaker(segID, unsafe.Pointer(&baseA[0]), addrA)
	w2 := getInprocWaker(segID, unsafe.Pointer(&baseB[0]), addrB)
	if w1 == nil || w2 == nil {
		t.Fatalf("getInprocWaker returned nil (w1=%v, w2=%v)", w1, w2)
	}
	if w1 != w2 {
		t.Fatal("getInprocWaker returned different wakers for same (segmentID, offset)")
	}

	addrC := (*uint32)(unsafe.Pointer(&baseA[96]))
	w3 := getInprocWaker(segID, unsafe.Pointer(&baseA[0]), addrC)
	if w3 == w1 {
		t.Fatal("getInprocWaker returned same waker for different offsets")
	}
}

// TestShmInprocWaker_DropClearsRegistry verifies that
// dropInprocWakersForSegment removes every entry tied to the given
// segmentID and Close()s them, releasing any parked readers.
func TestShmInprocWaker_DropClearsRegistry(t *testing.T) {
	segID := "test-seg-drop"
	base := make([]byte, 128)
	addr := (*uint32)(unsafe.Pointer(&base[0]))
	w := getInprocWaker(segID, unsafe.Pointer(&base[0]), addr)
	if w == nil {
		t.Fatal("getInprocWaker returned nil")
	}

	// Park a reader so Drop must wake it.
	done := make(chan error, 1)
	go func() {
		done <- w.Wait(2 * time.Second)
	}()
	time.Sleep(10 * time.Millisecond)

	dropInprocWakersForSegment(segID)

	select {
	case <-done:
		// Any return is acceptable; Drop's job is just to unblock.
	case <-time.After(2 * time.Second):
		t.Fatal("Drop did not unblock parked Wait")
	}

	// After Drop, a fresh lookup must allocate a new waker (the
	// previous one is closed and removed).
	w2 := getInprocWaker(segID, unsafe.Pointer(&base[0]), addr)
	if w2 == nil {
		t.Fatal("getInprocWaker after Drop returned nil")
	}
	if w2 == w {
		t.Fatal("getInprocWaker after Drop returned the closed waker")
	}
	dropInprocWakersForSegment(segID)
}

// TestShmInprocWaker_ConcurrentWakeWait stresses many concurrent
// wake/wait pairs against a single waker. Verifies no panic / hang /
// goroutine leak under contention.
func TestShmInprocWaker_ConcurrentWakeWait(t *testing.T) {
	w := newShmInprocWaker()
	if w == nil {
		t.Fatal("newShmInprocWaker returned nil")
	}
	defer w.Close()

	// Duration-bounded, not iteration-bounded: with eventfd a single
	// Wake increments the counter and one Wait read drains the whole
	// counter. Many Wakes coalesce into one Wait return — the
	// "iteration count" of Waits is therefore not predictable. The
	// real property under test is: no panic, no data race, no
	// deadlock when Wake and Wait are called concurrently. Both
	// goroutines just spin for a bounded duration.
	const duration = 500 * time.Millisecond
	deadline := time.Now().Add(duration)
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for time.Now().Before(deadline) {
			// Short timeout so a "wake already drained" case doesn't
			// stall this loop — it just times out and loops again.
			_ = w.Wait(10 * time.Millisecond)
		}
	}()

	go func() {
		defer wg.Done()
		for time.Now().Before(deadline) {
			w.Wake()
			// Pace at ~20 us so the consumer has a chance to be
			// parked rather than running through the eventfd
			// counter ahead of us.
			time.Sleep(20 * time.Microsecond)
		}
	}()

	doneCh := make(chan struct{})
	go func() { wg.Wait(); close(doneCh) }()

	select {
	case <-doneCh:
	case <-time.After(duration + 2*time.Second):
		t.Fatal("concurrent wake/wait did not finish")
	}
}
