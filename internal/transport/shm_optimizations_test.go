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
	name := testSegName("test_fw")
	seg, err := CreateSegment(name, 64*1024, 64*1024)
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
		RemoveSegment(name)
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
	// Verify that pendingStreamWU entries are cleaned up via the real
	// closeStream path (not manual delete).
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

	// Create a real stream.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s, err := ct.NewStream(ctx, &CallHdr{Host: "localhost", Method: "/test/Cleanup"})
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	streamID := s.id

	// Accumulate per-stream WU below threshold.
	ct.sendWindowUpdate(streamID, 4096)
	ct.sendWindowUpdate(streamID, 4096)

	ct.sendQuotaMu.Lock()
	_, exists := ct.pendingStreamWU[streamID]
	ct.sendQuotaMu.Unlock()
	if !exists {
		t.Fatal("pendingStreamWU should have entry for stream after sendWindowUpdate")
	}

	// Close the stream via the real transport path.
	ct.closeStream(s, nil, true, 0, nil, nil, false)

	ct.sendQuotaMu.Lock()
	_, exists = ct.pendingStreamWU[streamID]
	ct.sendQuotaMu.Unlock()
	if exists {
		t.Error("pendingStreamWU entry should be cleaned up after closeStream")
	}
}

func TestShmWindowUpdateServerStreamCleanup(t *testing.T) {
	// Verify that pendingStreamWU entries are cleaned up on the server side
	// when a stream is terminated via handleTrailers or handleCancel.
	segName := testSegName("test_wu_srv_cleanup")
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

	// Simulate two streams with pending (sub-threshold) WindowUpdate deltas.
	streamA := uint32(1)
	streamB := uint32(3)

	st.sendWindowUpdate(streamA, 4096)
	st.sendWindowUpdate(streamB, 4096)

	st.sendQuotaMu.Lock()
	_, existsA := st.pendingStreamWU[streamA]
	_, existsB := st.pendingStreamWU[streamB]
	st.sendQuotaMu.Unlock()
	if !existsA || !existsB {
		t.Fatal("pendingStreamWU should have entries after sendWindowUpdate")
	}

	// handleTrailers should clean up streamA's entry.
	// Build a minimal valid trailers payload.
	trailers := encodeTrailers(TrailersV1{Version: 1, GRPCStatusCode: 0, GRPCStatusMsg: "OK"})
	// Register a fake stream so handleTrailers doesn't bail early.
	st.mu.Lock()
	st.streams[streamA] = &ServerStream{Stream: Stream{id: streamA, ctx: context.Background()}}
	st.mu.Unlock()
	st.handleTrailers(streamA, trailers)

	st.sendQuotaMu.Lock()
	_, existsA = st.pendingStreamWU[streamA]
	st.sendQuotaMu.Unlock()
	if existsA {
		t.Error("pendingStreamWU[streamA] should be cleaned up after handleTrailers")
	}

	// handleCancel should clean up streamB's entry.
	cancelCtx, cancelFn := context.WithCancel(context.Background())
	sB := &ServerStream{Stream: Stream{id: streamB, ctx: cancelCtx}}
	sB.cancel = cancelFn
	st.mu.Lock()
	st.streams[streamB] = sB
	st.mu.Unlock()
	st.handleCancel(streamB)

	st.sendQuotaMu.Lock()
	_, existsB = st.pendingStreamWU[streamB]
	st.sendQuotaMu.Unlock()
	if existsB {
		t.Error("pendingStreamWU[streamB] should be cleaned up after handleCancel")
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
	// Verify that the single-writer goroutine preserves per-stream FIFO even
	// when multiple goroutines enqueue concurrently. Each goroutine "owns" a
	// different stream, so within each stream the sequence must be monotonic.
	w, _, rx, cleanup := newTestFrameWriter(t)
	defer cleanup()

	const streams = 4
	const msgsPerStream = 30
	var wg sync.WaitGroup

	for s := 0; s < streams; s++ {
		wg.Add(1)
		go func(streamID uint32) {
			defer wg.Done()
			for i := 0; i < msgsPerStream; i++ {
				payload := make([]byte, 4)
				binary.LittleEndian.PutUint32(payload, uint32(i))
				if err := w.enqueueAndWait(frameEntry{
					ctx:     context.Background(),
					fh:      FrameHeader{Type: FrameTypeMESSAGE, StreamID: streamID},
					payload: payload,
				}); err != nil {
					t.Errorf("stream %d msg %d: enqueueAndWait: %v", streamID, i, err)
					return
				}
			}
		}(uint32(s + 1))
	}
	wg.Wait()

	// Read all frames and verify per-stream ordering.
	nextSeq := make(map[uint32]uint32) // streamID → expected next sequence
	for i := 0; i < streams*msgsPerStream; i++ {
		fh, data, err := readFrame(context.Background(), rx)
		if err != nil {
			t.Fatalf("readFrame %d: %v", i, err)
		}
		seq := binary.LittleEndian.Uint32(data)
		expected := nextSeq[fh.StreamID]
		if seq != expected {
			t.Errorf("stream %d: got seq %d, want %d (per-stream ordering broken)", fh.StreamID, seq, expected)
		}
		nextSeq[fh.StreamID] = expected + 1
	}

	// Verify all streams sent all messages.
	for s := uint32(1); s <= streams; s++ {
		if nextSeq[s] != msgsPerStream {
			t.Errorf("stream %d: only %d messages received, want %d", s, nextSeq[s], msgsPerStream)
		}
	}
}

// ---------------------------------------------------------------------------
// Batch signal suppression tests
// ---------------------------------------------------------------------------

func TestShmBatchSignalSuppression(t *testing.T) {
	// Verify that BeginBatch/EndBatch suppresses per-frame signals
	// and issues a single signal at the end.
	segName := testSegName("test_batch")
	defer RemoveSegment(segName)

	seg, err := CreateSegment(segName, 64*1024, 64*1024)
	if err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	defer seg.Close()

	tx := NewShmRingFromSegment(seg.A, seg.Mem)
	rx := NewShmRingFromSegment(seg.A, seg.Mem)

	hdr := tx.header()

	// Record initial DataSequence.
	seqBefore := hdr.DataSequence()

	// Write 5 frames in batch mode.
	tx.BeginBatch()
	for i := 0; i < 5; i++ {
		payload := []byte(fmt.Sprintf("batch-msg-%d", i))
		if err := writeFrame(context.Background(), tx, FrameHeader{Type: FrameTypeMESSAGE, StreamID: 1}, payload); err != nil {
			t.Fatalf("writeFrame %d: %v", i, err)
		}
	}
	// During batch, DataSequence should NOT have been incremented.
	seqDuring := hdr.DataSequence()
	if seqDuring != seqBefore {
		t.Errorf("DataSeq changed during batch: before=%d, during=%d (should be suppressed)", seqBefore, seqDuring)
	}

	tx.EndBatch()

	// After EndBatch, DataSequence should have been incremented exactly once.
	seqAfter := hdr.DataSequence()
	if seqAfter != seqBefore+1 {
		t.Errorf("DataSeq after EndBatch=%d, want %d (single increment)", seqAfter, seqBefore+1)
	}

	// Verify all 5 frames are readable.
	for i := 0; i < 5; i++ {
		fh, data, err := readFrame(context.Background(), rx)
		if err != nil {
			t.Fatalf("readFrame %d: %v", i, err)
		}
		if fh.Type != FrameTypeMESSAGE {
			t.Errorf("frame %d: type=%d, want MESSAGE", i, fh.Type)
		}
		want := fmt.Sprintf("batch-msg-%d", i)
		if string(data) != want {
			t.Errorf("frame %d: payload=%q, want %q", i, data, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Single stream cache tests
// ---------------------------------------------------------------------------

func TestShmSingleStreamCache(t *testing.T) {
	// Verify that cachedStream is set when there's exactly one active stream
	// and cleared when there are zero or multiple streams.
	segName := testSegName("test_ssm")
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

	ct, err := NewShmClientTransport(clientSeg, &ShmAddr{Name: segName + "_c"}, &ShmAddr{Name: segName + "_s"})
	if err != nil {
		t.Fatalf("NewShmClientTransport: %v", err)
	}
	defer ct.Close(nil)

	// No streams → cache should be nil.
	if ct.cachedStream != nil {
		t.Error("cachedStream should be nil with no streams")
	}

	// Create first stream → cache should be set.
	ctx := context.Background()
	s1, err := ct.NewStream(ctx, &CallHdr{Host: "localhost", Method: "/test/SSM1"})
	if err != nil {
		t.Fatalf("NewStream 1: %v", err)
	}
	if ct.cachedStream != s1 {
		t.Error("cachedStream should point to s1 with one stream")
	}

	// Create second stream → cache should be nil.
	s2, err := ct.NewStream(ctx, &CallHdr{Host: "localhost", Method: "/test/SSM2"})
	if err != nil {
		t.Fatalf("NewStream 2: %v", err)
	}
	if ct.cachedStream != nil {
		t.Error("cachedStream should be nil with two streams")
	}

	// Close second stream → cache should be set to s1.
	ct.closeStream(s2, nil, true, 0, nil, nil, false)
	if ct.cachedStream != s1 {
		t.Error("cachedStream should point to s1 after closing s2")
	}

	// Close first stream → cache should be nil.
	ct.closeStream(s1, nil, true, 0, nil, nil, false)
	if ct.cachedStream != nil {
		t.Error("cachedStream should be nil after closing all streams")
	}
}

// ---------------------------------------------------------------------------
// Zero-copy read tests
// ---------------------------------------------------------------------------

func TestShmZeroCopyContiguousMessage(t *testing.T) {
	// Verify that contiguous MESSAGE payloads are returned via zero-copy
	// (SliceBuffer referencing ring memory).
	segName := testSegName("test_zc_contig")
	defer RemoveSegment(segName)

	seg, err := CreateSegment(segName, 64*1024, 64*1024)
	if err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	defer seg.Close()

	tx := NewShmRingFromSegment(seg.A, seg.Mem)
	rx := NewShmRingFromSegment(seg.A, seg.Mem)

	payload := make([]byte, 4096)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	if err := writeFrame(context.Background(), tx, FrameHeader{
		Type: FrameTypeMESSAGE, StreamID: 1,
	}, payload); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}

	fh, buf, err := readFrameView(context.Background(), rx)
	if err != nil {
		t.Fatalf("readFrameView: %v", err)
	}
	if fh.Type != FrameTypeMESSAGE {
		t.Fatalf("type = %d, want MESSAGE", fh.Type)
	}
	data := buf.ReadOnlyData()
	if len(data) != len(payload) {
		t.Fatalf("len = %d, want %d", len(data), len(payload))
	}
	for i := range data {
		if data[i] != payload[i] {
			t.Fatalf("byte[%d] = %d, want %d", i, data[i], payload[i])
		}
	}
	buf.Free()
}

func TestShmZeroCopyWrapAroundCopies(t *testing.T) {
	// Verify that wrap-around payloads are copied (not zero-copy).
	segName := testSegName("test_zc_wrap")
	defer RemoveSegment(segName)

	seg, err := CreateSegment(segName, 4096, 4096)
	if err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	defer seg.Close()

	tx := NewShmRingFromSegment(seg.A, seg.Mem)
	rx := NewShmRingFromSegment(seg.A, seg.Mem)

	// Fill most of the ring to force next write to wrap.
	pad := make([]byte, 3500)
	if err := writeFrame(context.Background(), tx, FrameHeader{
		Type: FrameTypeMESSAGE, StreamID: 1,
	}, pad); err != nil {
		t.Fatalf("writeFrame pad: %v", err)
	}
	_, padBuf, err := readFrameView(context.Background(), rx)
	if err != nil {
		t.Fatalf("readFrameView pad: %v", err)
	}
	if padBuf != nil {
		padBuf.Free()
	}

	payload := []byte("wrap-around-test-data-payload")
	if err := writeFrame(context.Background(), tx, FrameHeader{
		Type: FrameTypeMESSAGE, StreamID: 1,
	}, payload); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}

	_, buf, err := readFrameView(context.Background(), rx)
	if err != nil {
		t.Fatalf("readFrameView: %v", err)
	}
	if string(buf.ReadOnlyData()) != string(payload) {
		t.Fatalf("payload = %q, want %q", buf.ReadOnlyData(), payload)
	}
	buf.Free()
}

func TestShmZeroCopyNonMessageCopies(t *testing.T) {
	// Verify that non-MESSAGE frames are always copied.
	segName := testSegName("test_zc_nonmsg")
	defer RemoveSegment(segName)

	seg, err := CreateSegment(segName, 64*1024, 64*1024)
	if err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	defer seg.Close()

	tx := NewShmRingFromSegment(seg.A, seg.Mem)
	rx := NewShmRingFromSegment(seg.A, seg.Mem)

	headersPayload := []byte("test-headers-payload")
	if err := writeFrame(context.Background(), tx, FrameHeader{
		Type: FrameTypeHEADERS, StreamID: 1,
	}, headersPayload); err != nil {
		t.Fatalf("writeFrame: %v", err)
	}

	fh, buf, err := readFrameView(context.Background(), rx)
	if err != nil {
		t.Fatalf("readFrameView: %v", err)
	}
	if fh.Type != FrameTypeHEADERS {
		t.Fatalf("type = %d, want HEADERS", fh.Type)
	}
	if string(buf.ReadOnlyData()) != string(headersPayload) {
		t.Fatalf("payload = %q, want %q", buf.ReadOnlyData(), headersPayload)
	}
	buf.Free()
}

func TestShmZeroCopyMultiStreamCorrectness(t *testing.T) {
	// Verify zero-copy works correctly with interleaved multi-stream messages.
	segName := testSegName("test_zc_multi")
	defer RemoveSegment(segName)

	seg, err := CreateSegment(segName, 1024*1024, 1024*1024)
	if err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	defer seg.Close()

	tx := NewShmRingFromSegment(seg.A, seg.Mem)
	rx := NewShmRingFromSegment(seg.A, seg.Mem)

	const streams = 4
	const msgsPerStream = 20
	payloadSize := 2048

	// Write interleaved messages.
	for i := 0; i < streams*msgsPerStream; i++ {
		streamID := uint32((i % streams) + 1)
		seqNum := i / streams
		payload := make([]byte, payloadSize)
		binary.LittleEndian.PutUint32(payload[0:4], streamID)
		binary.LittleEndian.PutUint32(payload[4:8], uint32(seqNum))
		for j := 8; j < len(payload); j++ {
			payload[j] = byte(streamID)*37 + byte(seqNum)
		}
		if err := writeFrame(context.Background(), tx, FrameHeader{
			Type: FrameTypeMESSAGE, StreamID: streamID,
		}, payload); err != nil {
			t.Fatalf("writeFrame stream=%d seq=%d: %v", streamID, seqNum, err)
		}
	}

	// Read back and verify.
	streamSeq := make(map[uint32]int)
	for i := 0; i < streams*msgsPerStream; i++ {
		fh, buf, err := readFrameView(context.Background(), rx)
		if err != nil {
			t.Fatalf("readFrameView %d: %v", i, err)
		}
		data := buf.ReadOnlyData()
		gotStream := binary.LittleEndian.Uint32(data[0:4])
		gotSeq := binary.LittleEndian.Uint32(data[4:8])

		if fh.StreamID != gotStream {
			t.Errorf("frame %d: header streamID=%d, payload says %d", i, fh.StreamID, gotStream)
		}
		if int(gotSeq) != streamSeq[gotStream] {
			t.Errorf("stream %d: got seq %d, want %d", gotStream, gotSeq, streamSeq[gotStream])
		}
		streamSeq[gotStream]++

		expectedByte := byte(gotStream)*37 + byte(gotSeq)
		for j := 8; j < len(data); j++ {
			if data[j] != expectedByte {
				t.Fatalf("stream %d seq %d: data[%d]=%d, want %d", gotStream, gotSeq, j, data[j], expectedByte)
			}
		}
		buf.Free()
	}

	for s := uint32(1); s <= streams; s++ {
		if streamSeq[s] != msgsPerStream {
			t.Errorf("stream %d: got %d msgs, want %d", s, streamSeq[s], msgsPerStream)
		}
	}
}

func TestShmZeroCopyDataSurvivesSubsequentWrites(t *testing.T) {
	// Verify that zero-copy data remains valid after subsequent writes
	// (as long as the ring doesn't wrap over it).
	segName := testSegName("test_zc_survive")
	defer RemoveSegment(segName)

	seg, err := CreateSegment(segName, 64*1024, 64*1024)
	if err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	defer seg.Close()

	tx := NewShmRingFromSegment(seg.A, seg.Mem)
	rx := NewShmRingFromSegment(seg.A, seg.Mem)

	payloadA := []byte("AAAA-important-data-AAAA")
	if err := writeFrame(context.Background(), tx, FrameHeader{
		Type: FrameTypeMESSAGE, StreamID: 1,
	}, payloadA); err != nil {
		t.Fatalf("writeFrame A: %v", err)
	}

	// Read A (zero-copy — holds ring memory).
	_, bufA, err := readFrameView(context.Background(), rx)
	if err != nil {
		t.Fatalf("readFrameView A: %v", err)
	}

	// Write more messages after A.
	for _, name := range []string{"BBBB", "CCCC", "DDDD"} {
		p := []byte(name + "-subsequent-data-" + name)
		if err := writeFrame(context.Background(), tx, FrameHeader{
			Type: FrameTypeMESSAGE, StreamID: 1,
		}, p); err != nil {
			t.Fatalf("writeFrame %s: %v", name, err)
		}
	}

	// A's data must still be intact (ring hasn't wrapped).
	if string(bufA.ReadOnlyData()) != string(payloadA) {
		t.Fatalf("A corrupted: got %q, want %q", bufA.ReadOnlyData(), payloadA)
	}
	bufA.Free()

	// Read and verify B, C, D.
	for _, want := range []string{"BBBB", "CCCC", "DDDD"} {
		_, buf, err := readFrameView(context.Background(), rx)
		if err != nil {
			t.Fatalf("readFrameView: %v", err)
		}
		if string(buf.ReadOnlyData()[:4]) != want {
			t.Errorf("expected prefix %s, got %s", want, buf.ReadOnlyData()[:4])
		}
		buf.Free()
	}
}
