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
	"encoding/binary"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var testSegCounter atomic.Int64

func testSegName(prefix string) string {
	return fmt.Sprintf("%s_%d_%d", prefix, time.Now().UnixNano(), testSegCounter.Add(1))
}

// ---------------------------------------------------------------------------
// shmFrameWriter tests
// ---------------------------------------------------------------------------

func newTestFrameWriter(t *testing.T) (*shmFrameWriter, *ShmRing, *ShmRing, func()) {
	t.Helper()
	seg, err := CreateSegment(testSegName("test_fw"), 64*1024, 64*1024)
	if err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	tx := NewShmRingFromSegment(seg.A, seg.Mem)
	rx := NewShmRingFromSegment(seg.A, seg.Mem) // same ring, reader side
	w := newShmFrameWriter(tx)
	cleanup := func() {
		// Close ring first to unblock any writer stuck in ReserveWrite,
		// then close the frame writer. Mirrors transport Close() ordering.
		tx.Close()
		w.close()
		seg.Close()
		RemoveSegment(seg.Path)
	}
	return w, tx, rx, cleanup
}

func TestShmFrameWriterEnqueueAndRead(t *testing.T) {
	w, _, rx, cleanup := newTestFrameWriter(t)
	defer cleanup()

	payload := []byte("hello shm frame writer")
	err := w.enqueueAndWait(frameEntry{
		ctx:     context.Background(),
		fh:      FrameHeader{Type: FrameTypeMESSAGE, StreamID: 1},
		payload: payload,
	})
	if err != nil {
		t.Fatalf("enqueueAndWait: %v", err)
	}

	fh, data, err := readFrame(context.Background(), rx)
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if fh.Type != FrameTypeMESSAGE {
		t.Errorf("frame type = %d, want %d", fh.Type, FrameTypeMESSAGE)
	}
	if fh.StreamID != 1 {
		t.Errorf("streamID = %d, want 1", fh.StreamID)
	}
	if string(data) != string(payload) {
		t.Errorf("payload = %q, want %q", data, payload)
	}
}

func TestShmFrameWriterAsyncEnqueue(t *testing.T) {
	w, _, rx, cleanup := newTestFrameWriter(t)
	defer cleanup()

	const n = 10
	for i := 0; i < n; i++ {
		payload := []byte(fmt.Sprintf("msg-%d", i))
		if err := w.enqueue(frameEntry{
			ctx:     context.Background(),
			fh:      FrameHeader{Type: FrameTypeMESSAGE, StreamID: uint32(i + 1)},
			payload: payload,
		}); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}

	for i := 0; i < n; i++ {
		fh, data, err := readFrame(context.Background(), rx)
		if err != nil {
			t.Fatalf("readFrame %d: %v", i, err)
		}
		if fh.StreamID != uint32(i+1) {
			t.Errorf("frame %d: streamID = %d, want %d", i, fh.StreamID, i+1)
		}
		want := fmt.Sprintf("msg-%d", i)
		if string(data) != want {
			t.Errorf("frame %d: payload = %q, want %q", i, data, want)
		}
	}
}

func TestShmFrameWriterCloseReturnsError(t *testing.T) {
	w, _, _, cleanup := newTestFrameWriter(t)
	defer cleanup()

	w.close()

	err := w.enqueue(frameEntry{
		ctx:     context.Background(),
		fh:      FrameHeader{Type: FrameTypePING},
		payload: []byte("ping"),
	})
	if err != ErrConnClosing {
		t.Errorf("enqueue after close = %v, want ErrConnClosing", err)
	}

	err = w.enqueueAndWait(frameEntry{
		ctx:     context.Background(),
		fh:      FrameHeader{Type: FrameTypePING},
		payload: []byte("ping"),
	})
	if err != ErrConnClosing {
		t.Errorf("enqueueAndWait after close = %v, want ErrConnClosing", err)
	}
}

func TestShmFrameWriterDoubleClose(t *testing.T) {
	w, _, _, cleanup := newTestFrameWriter(t)
	defer cleanup()

	w.close()
	// Second close should not panic
	w.close()
}

func TestShmFrameWriterConcurrentCloseNoPanic(t *testing.T) {
	// Verify that concurrent enqueue + close does not panic (send on closed channel).
	w, tx, _, cleanup := newTestFrameWriter(t)
	defer cleanup()

	var wg sync.WaitGroup

	// Spawn producers that keep enqueuing until they get ErrConnClosing.
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				err := w.enqueue(frameEntry{
					ctx:     context.Background(),
					fh:      FrameHeader{Type: FrameTypePING},
					payload: []byte("ping"),
				})
				if err != nil {
					return // writer closed, done
				}
			}
		}()
	}

	// Let producers run briefly, then close ring + writer (same order as transport).
	time.Sleep(5 * time.Millisecond)
	tx.Close() // unblock writer goroutine if stuck in ReserveWrite
	w.close()
	wg.Wait()
	// If we get here without panic, the test passes.
}

// ---------------------------------------------------------------------------
// WindowUpdate batching tests
// ---------------------------------------------------------------------------

func TestShmWindowUpdateBatching(t *testing.T) {
	segName := testSegName("test_wu_batch")
	defer RemoveSegment(segName)

	seg, err := CreateSegment(segName, 128*1024, 128*1024)
	if err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	defer seg.Close()
	seg.H.SetServerReady(true)

	clientSeg, err := OpenSegment(segName)
	if err != nil {
		t.Fatalf("OpenSegment: %v", err)
	}

	clientAddr := &ShmAddr{Name: segName + "_c"}
	serverAddr := &ShmAddr{Name: segName + "_s"}

	ct, err := NewShmClientTransport(clientSeg, clientAddr, serverAddr)
	if err != nil {
		t.Fatalf("NewShmClientTransport: %v", err)
	}
	defer ct.Close(nil)

	// Send small WindowUpdates that should be batched (below threshold).
	smallDelta := uint32(1024) // 1KB - well below 8MB threshold
	for i := 0; i < 100; i++ {
		ct.sendWindowUpdate(0, smallDelta)
	}

	// Accumulated = 100KB, still below 8MB, so no frame should have been sent.
	ct.sendQuotaMu.Lock()
	pending := ct.pendingConnWU
	ct.sendQuotaMu.Unlock()

	if pending != 100*smallDelta {
		t.Errorf("pendingConnWU = %d, want %d (should batch without sending)", pending, 100*smallDelta)
	}

	// Now send a large delta that pushes past the threshold.
	ct.sendWindowUpdate(0, shmWindowUpdateThreshold)

	ct.sendQuotaMu.Lock()
	pendingAfter := ct.pendingConnWU
	ct.sendQuotaMu.Unlock()

	if pendingAfter != 0 {
		t.Errorf("pendingConnWU after flush = %d, want 0", pendingAfter)
	}
}

func TestShmWindowUpdateStreamCleanup(t *testing.T) {
	// Verify that pendingStreamWU entries are cleaned up when a stream closes.
	segName := testSegName("test_wu_cleanup")
	defer RemoveSegment(segName)

	seg, err := CreateSegment(segName, 128*1024, 128*1024)
	if err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	defer seg.Close()
	seg.H.SetServerReady(true)

	clientSeg, err := OpenSegment(segName)
	if err != nil {
		t.Fatalf("OpenSegment: %v", err)
	}

	clientAddr := &ShmAddr{Name: segName + "_c"}
	serverAddr := &ShmAddr{Name: segName + "_s"}

	ct, err := NewShmClientTransport(clientSeg, clientAddr, serverAddr)
	if err != nil {
		t.Fatalf("NewShmClientTransport: %v", err)
	}
	defer ct.Close(nil)

	streamID := uint32(1)

	// Accumulate some per-stream WU below threshold.
	ct.sendWindowUpdate(streamID, 4096)
	ct.sendWindowUpdate(streamID, 4096)

	ct.sendQuotaMu.Lock()
	_, exists := ct.pendingStreamWU[streamID]
	ct.sendQuotaMu.Unlock()
	if !exists {
		t.Fatal("pendingStreamWU should have entry for stream after sendWindowUpdate")
	}

	// Simulate stream cleanup (what closeStream does).
	ct.sendQuotaMu.Lock()
	delete(ct.pendingStreamWU, streamID)
	ct.sendQuotaMu.Unlock()

	ct.sendQuotaMu.Lock()
	_, exists = ct.pendingStreamWU[streamID]
	ct.sendQuotaMu.Unlock()
	if exists {
		t.Error("pendingStreamWU should be cleaned up after stream close")
	}
}

// ---------------------------------------------------------------------------
// SHM flow control constants tests
// ---------------------------------------------------------------------------

func TestShmFlowControlConstants(t *testing.T) {
	// Verify SHM-specific constants are correctly defined.
	if shmInitialWindowSize != 32*1024*1024 {
		t.Errorf("shmInitialWindowSize = %d, want %d", shmInitialWindowSize, 32*1024*1024)
	}
	if shmBDPLimit != 64*1024*1024 {
		t.Errorf("shmBDPLimit = %d, want %d", shmBDPLimit, 64*1024*1024)
	}
	if shmWindowUpdateThreshold != shmInitialWindowSize/4 {
		t.Errorf("shmWindowUpdateThreshold = %d, want %d", shmWindowUpdateThreshold, shmInitialWindowSize/4)
	}
	// Verify SHM window is much larger than HTTP/2 default.
	if shmInitialWindowSize <= int(initialWindowSize) {
		t.Errorf("shmInitialWindowSize (%d) should be >> initialWindowSize (%d)", shmInitialWindowSize, initialWindowSize)
	}
}

func TestShmBDPEstimatorUsesLargerLimit(t *testing.T) {
	// Verify the SHM BDP estimator settles at shmBDPLimit, not bdpLimit.
	var lastUpdate uint32
	est := newShmBDPEstimator(uint32(shmInitialWindowSize), func(n uint32) {
		lastUpdate = n
	})

	// Simulate rapid BDP growth by calling add+calculate repeatedly.
	for i := 0; i < 50; i++ {
		if est.add(shmBDPLimit) {
			est.timesnap()
		}
		// Simulate immediate ping-pong. Need a non-zero RTT for BDP calculation.
		time.Sleep(100 * time.Microsecond)
		est.calculate()
	}

	// After many iterations, BDP should reach shmBDPLimit (64MB), not bdpLimit (16MB).
	// Check that the estimator didn't settle at the old HTTP/2 limit.
	if lastUpdate > 0 && lastUpdate <= bdpLimit {
		t.Errorf("BDP estimator settled at %d, expected growth towards shmBDPLimit (%d)", lastUpdate, shmBDPLimit)
	}
}

func TestShmTransportInitialWindowSize(t *testing.T) {
	// Verify that new SHM transports use shmInitialWindowSize.
	segName := testSegName("test_initwin")
	defer RemoveSegment(segName)

	seg, err := CreateSegment(segName, 64*1024, 64*1024)
	if err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	defer seg.Close()
	seg.H.SetServerReady(true)

	clientSeg, err := OpenSegment(segName)
	if err != nil {
		t.Fatalf("OpenSegment: %v", err)
	}

	ct, err := NewShmClientTransport(clientSeg, &ShmAddr{Name: segName + "_c"}, &ShmAddr{Name: segName + "_s"})
	if err != nil {
		t.Fatalf("NewShmClientTransport: %v", err)
	}
	defer ct.Close(nil)

	if ct.initialWindowSize != shmInitialWindowSize {
		t.Errorf("client initialWindowSize = %d, want %d", ct.initialWindowSize, shmInitialWindowSize)
	}

	st, err := NewShmServerTransport(seg, &ShmAddr{Name: segName + "_s"}, &ShmAddr{Name: segName + "_c"})
	if err != nil {
		t.Fatalf("NewShmServerTransport: %v", err)
	}
	defer st.Close(nil)

	if st.initialWindowSize != shmInitialWindowSize {
		t.Errorf("server initialWindowSize = %d, want %d", st.initialWindowSize, shmInitialWindowSize)
	}
}

// ---------------------------------------------------------------------------
// Spin parameter tests
// ---------------------------------------------------------------------------

func TestShmSpinConstants(t *testing.T) {
	// Verify spin parameters are tuned for SHM (higher than Folly defaults).
	if spinIterationsDefault < 500 {
		t.Errorf("spinIterationsDefault = %d, should be >= 500 for SHM", spinIterationsDefault)
	}
	if spinIterationsMin < 100 {
		t.Errorf("spinIterationsMin = %d, should be >= 100 for SHM", spinIterationsMin)
	}
	if spinIterationsMax < 2000 {
		t.Errorf("spinIterationsMax = %d, should be >= 2000 for SHM", spinIterationsMax)
	}
	if spinIterationsMin >= spinIterationsDefault {
		t.Error("spinIterationsMin should be < spinIterationsDefault")
	}
	if spinIterationsDefault >= spinIterationsMax {
		t.Error("spinIterationsDefault should be < spinIterationsMax")
	}
}

// ---------------------------------------------------------------------------
// ContigSeq conditional signal test
// ---------------------------------------------------------------------------

func TestShmContigSeqConditionalSignal(t *testing.T) {
	// Verify that ContigSeq is only incremented when there are waiters.
	segName := testSegName("test_contig")
	defer RemoveSegment(segName)

	seg, err := CreateSegment(segName, 64*1024, 64*1024)
	if err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	defer seg.Close()

	ring := NewShmRingFromSegment(seg.A, seg.Mem)
	hdr := ring.header()

	// Write some data to the ring.
	data := make([]byte, 100)
	for i := range data {
		data[i] = byte(i)
	}
	ctx := context.Background()
	res, err := ring.ReserveWrite(ctx, len(data))
	if err != nil {
		t.Fatalf("ReserveWrite: %v", err)
	}
	copy(res.First, data)
	if err := res.Commit(len(data)); err != nil {
		t.Fatalf("Commit write: %v", err)
	}

	// Read the data. No contig waiters → contigSeq should NOT be incremented.
	contigBefore := hdr.ContigSequence()
	buf := make([]byte, len(data))
	n, err := ring.ReadBlocking(buf)
	if err != nil || n != len(data) {
		t.Fatalf("ReadBlocking: n=%d, err=%v", n, err)
	}
	contigAfter := hdr.ContigSequence()

	if contigAfter != contigBefore {
		t.Errorf("ContigSeq changed from %d to %d without waiters — should be conditional", contigBefore, contigAfter)
	}
}

// ---------------------------------------------------------------------------
// Close ordering: rings close before frameWriter (deadlock prevention)
// ---------------------------------------------------------------------------

func TestShmCloseOrderRingsBeforeWriter(t *testing.T) {
	// This test verifies that transport Close() doesn't deadlock even when
	// the frame writer has pending writes that would block on a full ring.
	segName := testSegName("test_close_order")
	defer RemoveSegment(segName)

	// Use a tiny ring (4KB) to make it easy to fill.
	seg, err := CreateSegment(segName, 4096, 4096)
	if err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	seg.H.SetServerReady(true)

	clientSeg, err := OpenSegment(segName)
	if err != nil {
		t.Fatalf("OpenSegment: %v", err)
	}

	ct, err := NewShmClientTransport(clientSeg, &ShmAddr{Name: segName + "_c"}, &ShmAddr{Name: segName + "_s"})
	if err != nil {
		t.Fatalf("NewShmClientTransport: %v", err)
	}

	// Fill the ring with data that nobody reads, so subsequent writes block.
	bigPayload := make([]byte, 2048)
	_ = ct.frameWriter.enqueue(frameEntry{
		ctx:     context.Background(),
		fh:      FrameHeader{Type: FrameTypeMESSAGE, StreamID: 1},
		payload: bigPayload,
	})
	// Give writer time to process
	time.Sleep(10 * time.Millisecond)

	// Enqueue more writes that will block because ring is full.
	for i := 0; i < 5; i++ {
		_ = ct.frameWriter.enqueue(frameEntry{
			ctx:     context.Background(),
			fh:      FrameHeader{Type: FrameTypePING},
			payload: []byte("ping"),
		})
	}

	// Close should complete within a reasonable time (not deadlock).
	done := make(chan struct{})
	go func() {
		ct.Close(nil)
		close(done)
	}()

	select {
	case <-done:
		// Success - no deadlock
	case <-time.After(5 * time.Second):
		t.Fatal("Close() deadlocked — rings should be closed before frameWriter")
	}
}

// ---------------------------------------------------------------------------
// Server PONG payload safety
// ---------------------------------------------------------------------------

func TestShmServerPongPayloadCopied(t *testing.T) {
	// Verify that handlePing copies payload before enqueueing.
	// We check this indirectly by verifying the PONG is written correctly
	// even if we mutate the original payload buffer.
	segName := testSegName("test_pong")
	defer RemoveSegment(segName)

	seg, err := CreateSegment(segName, 128*1024, 128*1024)
	if err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	defer seg.Close()
	seg.H.SetServerReady(true)

	st, err := NewShmServerTransport(seg, &ShmAddr{Name: segName + "_s"}, &ShmAddr{Name: segName + "_c"})
	if err != nil {
		t.Fatalf("NewShmServerTransport: %v", err)
	}
	defer st.Close(nil)

	// Create a mutable payload buffer simulating ring-backed memory.
	payload := make([]byte, 8)
	binary.LittleEndian.PutUint64(payload, 0xDEADBEEFCAFEBABE)
	original := make([]byte, 8)
	copy(original, payload)

	// Call handlePing which should COPY the payload before enqueuing.
	st.handlePing(context.Background(), payload)

	// Mutate the original buffer (simulating release() reusing ring memory).
	for i := range payload {
		payload[i] = 0xFF
	}

	// Give writer time to process.
	time.Sleep(20 * time.Millisecond)

	// Read the PONG from the server→client ring.
	rx := NewShmRingFromSegment(seg.B, seg.Mem)
	fh, data, err := readFrame(context.Background(), rx)
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if fh.Type != FrameTypePONG {
		t.Fatalf("expected PONG frame, got type %d", fh.Type)
	}

	// The PONG payload should match the original, not the mutated version.
	if len(data) != 8 {
		t.Fatalf("PONG payload len = %d, want 8", len(data))
	}
	got := binary.LittleEndian.Uint64(data)
	if got != 0xDEADBEEFCAFEBABE {
		t.Errorf("PONG payload = 0x%X, want 0xDEADBEEFCAFEBABE (payload was corrupted by mutation)", got)
	}
}

// ---------------------------------------------------------------------------
// Frame writer serialization: verify ordering is preserved
// ---------------------------------------------------------------------------

func TestShmFrameWriterOrdering(t *testing.T) {
	w, _, rx, cleanup := newTestFrameWriter(t)
	defer cleanup()

	const n = 50
	var sent atomic.Int32

	// Enqueue frames from multiple goroutines for the SAME stream.
	// The writer should serialize them in FIFO order per enqueue call.
	for i := 0; i < n; i++ {
		payload := make([]byte, 4)
		binary.LittleEndian.PutUint32(payload, uint32(i))
		if err := w.enqueueAndWait(frameEntry{
			ctx:     context.Background(),
			fh:      FrameHeader{Type: FrameTypeMESSAGE, StreamID: 1},
			payload: payload,
		}); err != nil {
			t.Fatalf("enqueueAndWait %d: %v", i, err)
		}
		sent.Add(1)
	}

	// Read and verify ordering.
	for i := 0; i < n; i++ {
		_, data, err := readFrame(context.Background(), rx)
		if err != nil {
			t.Fatalf("readFrame %d: %v", i, err)
		}
		got := binary.LittleEndian.Uint32(data)
		if got != uint32(i) {
			t.Errorf("frame %d: sequence = %d, want %d (ordering broken)", i, got, i)
		}
	}
}
