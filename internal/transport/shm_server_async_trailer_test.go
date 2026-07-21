//go:build linux

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

// Unit tests for the server-side async trailer-sentinel design.
// These tests backstop the discardDeferredTrailer-vs-flushDeferredTrailer
// split that processProtoEntry / advanceDeferred / retryDeferredProto
// route their terminal calls through. The split exists because emitting
// OK-status TRAILERS after dropping (streamDone / ctx-cancel / ring-err)
// the preceding DATA puts the peer in a cardinality-violation state —
// the exact bug that the original synchronous server writeProto path
// was carved out to avoid before the trailer-sentinel design was added.
// A regression in the discard/flush dispatch would re-introduce that
// bug silently (no panic, no error — just a corrupt frame stream).

package transport

import (
	"context"
	"fmt"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
)

// trailerEntryForTest builds a TRAILERS frameEntry that emitTrailerEntry
// can encode end-to-end through writeFrame → writeFrameH2 → HPACK. The
// payload is in the custom TrailersV1 form (which decodeTrailers parses
// before the H2 re-encode in writeFrameH2).
func trailerEntryForTest(streamID uint32, code codes.Code, msg string,
	s *Stream, doneCh chan error) frameEntry {
	payload := encodeTrailers(TrailersV1{
		Version:        1,
		GRPCStatusCode: uint32(code),
		GRPCStatusMsg:  msg,
	})
	return frameEntry{
		ctx: context.Background(),
		fh: FrameHeader{
			Type:     FrameTypeTRAILERS,
			StreamID: streamID,
			Length:   uint32(len(payload)),
		},
		payload:   payload,
		streamPtr: s,
		doneCh:    doneCh,
	}
}

// TestShmAsyncTrailerDiscardOnDropTerminal verifies that when an async
// DATA-drain terminal observes a DROP outcome (streamDone, ctx-cancel,
// or ring-write-err), a TRAILERS sentinel parked behind that DATA is
// DISCARDED — i.e. errStreamDone surfaces to the writeStatus sender
// and the trailer is NEVER written to the ring. The wire-write
// suppression is the load-bearing property: a peer that observed
// missing DATA followed by TRAILERS-OK would surface a unary
// cardinality violation (gRPC requires exactly one response).
func TestShmAsyncTrailerDiscardOnDropTerminal(t *testing.T) {
	name := fmt.Sprintf("test-trailer-discard-%d", time.Now().UnixNano())
	defer RemoveSegment(name)
	seg, err := CreateSegment(name, 65536, 65536)
	if err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	defer seg.Close()

	tx := NewShmRingFromSegment(seg.A, seg.Mem)
	w := newShmFrameWriter(tx)
	defer w.close()

	// Snapshot the producer index. The discard path MUST leave it
	// unchanged — otherwise some frame leaked onto the wire.
	produceBefore := tx.header().WriteIndex()

	s := &Stream{}
	doneCh := make(chan error, 1)
	entry := trailerEntryForTest(1, codes.OK, "", s, doneCh)

	// Drive the writer's discard path directly. inlineMu is the
	// helper's documented invariant (same mutex every DATA-drain
	// terminal already holds when it dispatches to flush/discard);
	// acquiring it from the test keeps the call-site contract
	// honest and serialises against the writer goroutine.
	w.inlineMu.Lock()
	w.deferredTrailers[1] = entry
	w.discardDeferredTrailer(1, s)
	w.inlineMu.Unlock()

	select {
	case got := <-doneCh:
		if got != errStreamDone {
			t.Fatalf("doneCh: got %v, want errStreamDone", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("doneCh did not receive within 2s (writeStatus would hang)")
	}

	if !s.shmDataDropped.Load() {
		t.Fatalf("stream not tombstoned: shmDataDropped=false, want true")
	}
	if got := s.getState(); got == streamDone {
		t.Fatalf("stream state: got streamDone, want non-streamDone (writer tombstone must NOT set streamDone — that deadlocks closeStream)")
	}

	produceAfter := tx.header().WriteIndex()
	if produceAfter != produceBefore {
		t.Fatalf("ring producer advanced: before=%d after=%d "+
			"(discard path must NOT write trailer to wire — peer would "+
			"see TRAILERS without the preceding DATA, cardinality violation)",
			produceBefore, produceAfter)
	}

	w.inlineMu.Lock()
	_, stillParked := w.deferredTrailers[1]
	w.inlineMu.Unlock()
	if stillParked {
		t.Fatal("deferredTrailers still holds entry after discard (would leak across close)")
	}
}

// TestShmAsyncTrailerFlushOnSuccessTerminal is the inverse case: when
// the DATA-drain terminal observed SUCCESS, the parked TRAILERS must
// be emitted on the wire so the peer sees the complete unary /
// streaming-end frame sequence.
func TestShmAsyncTrailerFlushOnSuccessTerminal(t *testing.T) {
	name := fmt.Sprintf("test-trailer-flush-%d", time.Now().UnixNano())
	defer RemoveSegment(name)
	seg, err := CreateSegment(name, 65536, 65536)
	if err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	defer seg.Close()

	tx := NewShmRingFromSegment(seg.A, seg.Mem)
	w := newShmFrameWriter(tx)
	defer w.close()

	produceBefore := tx.header().WriteIndex()

	s := &Stream{}
	doneCh := make(chan error, 1)
	entry := trailerEntryForTest(2, codes.OK, "", s, doneCh)

	w.inlineMu.Lock()
	w.deferredTrailers[2] = entry
	w.flushDeferredTrailer(2)
	w.inlineMu.Unlock()

	select {
	case got := <-doneCh:
		if got != nil {
			t.Fatalf("doneCh: got %v, want nil (success → trailer on wire)", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("doneCh did not receive within 2s")
	}

	if got := s.getState(); got != streamDone {
		t.Fatalf("stream state: got %v, want streamDone", got)
	}

	produceAfter := tx.header().WriteIndex()
	if produceAfter == produceBefore {
		t.Fatalf("ring producer did not advance: before=%d after=%d "+
			"(flush path MUST write trailer to wire — peer would hang "+
			"forever waiting for end-of-stream)",
			produceBefore, produceAfter)
	}

	w.inlineMu.Lock()
	_, stillParked := w.deferredTrailers[2]
	w.inlineMu.Unlock()
	if stillParked {
		t.Fatal("deferredTrailers still holds entry after flush")
	}
}

// TestShmAsyncTrailerDiscardRespectsSibling verifies that
// discardDeferredTrailer leaves the trailer PARKED when the sibling
// DATA queue (deferred for whole-messages, deferredProto for proto
// chains) still has entries for the stream. Without this guard,
// firing discard on a partial drain would prematurely signal
// errStreamDone before the still-queued DATA had a chance to land
// on the ring (or be dropped, depending on its own terminal).
//
// The stream IS tombstoned to streamDone even on this early-return
// path so that a later sibling-success flushDeferredTrailer can see
// the tombstone and refuse to emit OK trailers (which would be a
// cardinality violation for the DATA that this discard call's
// terminal already dropped).
func TestShmAsyncTrailerDiscardRespectsSibling(t *testing.T) {
	name := fmt.Sprintf("test-trailer-sibling-%d", time.Now().UnixNano())
	defer RemoveSegment(name)
	seg, err := CreateSegment(name, 65536, 65536)
	if err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	defer seg.Close()

	tx := NewShmRingFromSegment(seg.A, seg.Mem)
	w := newShmFrameWriter(tx)
	defer w.close()

	s := &Stream{}
	doneCh := make(chan error, 1)
	entry := trailerEntryForTest(4, codes.OK, "", s, doneCh)

	w.inlineMu.Lock()
	// Sibling DATA still pending — discard must defer to the
	// sibling's eventual terminal for the doneCh signal.
	w.deferredProto[4] = []frameEntry{{}}
	w.deferredTrailers[4] = entry
	w.discardDeferredTrailer(4, s)
	_, stillParked := w.deferredTrailers[4]
	w.inlineMu.Unlock()

	if !stillParked {
		t.Fatal("trailer removed despite sibling deferredProto entry (should defer)")
	}

	// doneCh must NOT have been signalled — writeStatus stays blocked
	// until the sibling's terminal fires the correct flush/discard.
	select {
	case got := <-doneCh:
		t.Fatalf("doneCh signalled prematurely with %v (sibling DATA still pending)", got)
	case <-time.After(50 * time.Millisecond):
		// expected: still parked
	}

	// Stream MUST be tombstoned (shmDataDropped) even though the
	// trailer stayed parked — this is the TOCTOU close: if the
	// sibling's drain eventually succeeds and calls
	// flushDeferredTrailer, that flush MUST see shmDataDropped and
	// refuse to emit OK trailers (the current call's DATA was dropped
	// → peer cardinality violation if OK trailers were emitted later).
	// The tombstone MUST NOT be the streamDone state, which is
	// reserved for closeStream.
	if !s.shmDataDropped.Load() {
		t.Fatalf("stream not tombstoned: shmDataDropped=false, want true (TOCTOU tombstone)")
	}
	if got := s.getState(); got == streamDone {
		t.Fatalf("stream state: got streamDone, want non-streamDone (tombstone must not set streamDone)")
	}
}

// TestShmAsyncTrailerTOCTOUDropBeforeStatus verifies the TOCTOU
// close: when async DATA drops BEFORE writeStatus has enqueued the
// TRAILER, a later writeStatus call MUST NOT emit OK trailers on
// the wire (peer cardinality violation). The fix relies on
// discardDeferredTrailer tombstoning the stream via the sticky
// shmDataDropped flag unconditionally, so a subsequent
// processTrailerEntry call sees the tombstone and signals
// errStreamDone instead of emitting.
//
// This exercises the race the original trailer-sentinel design
// didn't cover: the trailer-sentinel only protects against
// "trailer enqueued WHILE DATA pending"; the TOCTOU here is
// "DATA drops, THEN trailer arrives".
func TestShmAsyncTrailerTOCTOUDropBeforeStatus(t *testing.T) {
	name := fmt.Sprintf("test-trailer-toctou-%d", time.Now().UnixNano())
	defer RemoveSegment(name)
	seg, err := CreateSegment(name, 65536, 65536)
	if err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	defer seg.Close()

	tx := NewShmRingFromSegment(seg.A, seg.Mem)
	w := newShmFrameWriter(tx)
	defer w.close()

	produceBefore := tx.header().WriteIndex()

	// Simulate: async DATA was just dropped at a terminal (ctx.Err /
	// ring-write-err / streamDone). The trailer has NOT been
	// enqueued yet by writeStatus. Call discardDeferredTrailer
	// with the stream, sid 5, no parked trailer.
	s := &Stream{}
	w.inlineMu.Lock()
	w.discardDeferredTrailer(5, s)
	w.inlineMu.Unlock()

	// Stream must be tombstoned (shmDataDropped) — the late
	// writeStatus arriving after us must be rejected by
	// processTrailerEntry.
	if !s.shmDataDropped.Load() {
		t.Fatalf("stream not tombstoned: shmDataDropped=false, want true after drop-before-status")
	}
	if got := s.getState(); got == streamDone {
		t.Fatalf("stream state: got streamDone, want non-streamDone (tombstone must not set streamDone)")
	}

	// Now simulate writeStatus arriving LATE: processTrailerEntry
	// is called with the trailer entry. It must see streamDone and
	// signal errStreamDone to the writeStatus sender WITHOUT
	// emitting on the wire.
	doneCh := make(chan error, 1)
	entry := trailerEntryForTest(5, codes.OK, "", s, doneCh)
	w.inlineMu.Lock()
	w.processTrailerEntry(entry)
	w.inlineMu.Unlock()

	select {
	case got := <-doneCh:
		if got != errStreamDone {
			t.Fatalf("doneCh: got %v, want errStreamDone", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("doneCh did not receive within 2s")
	}

	produceAfter := tx.header().WriteIndex()
	if produceAfter != produceBefore {
		t.Fatalf("ring producer advanced: before=%d after=%d "+
			"(late writeStatus must NOT emit OK trailers after a "+
			"prior DATA-drop tombstoned the stream — peer would see "+
			"cardinality violation)",
			produceBefore, produceAfter)
	}
}

// TestShmAsyncTrailerCloseDuringDefer verifies that close() drains a
// parked TRAILERS sentinel with ErrConnClosing — writeStatus would
// otherwise hang forever (the writer goroutine has exited, so neither
// the sibling DATA terminal nor a fresh flush/discard call can ever
// fire). This is the close-side analogue of the deferred-message
// drain at shm_frame_writer.go close().
func TestShmAsyncTrailerCloseDuringDefer(t *testing.T) {
	name := fmt.Sprintf("test-trailer-close-%d", time.Now().UnixNano())
	defer RemoveSegment(name)
	seg, err := CreateSegment(name, 65536, 65536)
	if err != nil {
		t.Fatalf("CreateSegment: %v", err)
	}
	defer seg.Close()

	tx := NewShmRingFromSegment(seg.A, seg.Mem)
	w := newShmFrameWriter(tx)

	s := &Stream{}
	doneCh := make(chan error, 1)
	entry := trailerEntryForTest(3, codes.OK, "", s, doneCh)

	// Park trailer with a sibling DATA placeholder so neither flush
	// nor discard fires before close drains the maps.
	w.inlineMu.Lock()
	w.deferredProto[3] = []frameEntry{{}}
	w.deferredTrailers[3] = entry
	w.inlineMu.Unlock()

	w.close()

	select {
	case got := <-doneCh:
		if got != ErrConnClosing {
			t.Fatalf("doneCh: got %v, want ErrConnClosing", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("doneCh did not receive within 2s (writeStatus would hang on transport close)")
	}
}
