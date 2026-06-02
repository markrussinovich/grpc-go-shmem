//go:build linux || windows

/*
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
	"context"
	"encoding/binary"
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/status"
)

// TestShmUnboundedClientStreamSenderBackpressure is a regression test
// for the demo-agent-reported FC-VIOLATION bug:
//
//	"Fair (window=65535), client client-streaming with response_size=0,
//	 payload >= 64KB, server doesn't drain -> server replies
//	 FC-VIOLATION + cancels stream; client gets EOF."
//
// Pre-fix root cause: receiver's maybeAdjustAdditive granted unbounded
// pre-credit (capped only by maxWindowSize = 2 GiB). With a slow/non-
// draining app, delta and pendingData both grew until delta hit the
// HTTP/2 31-bit cap. Past that point onData's `pendingData+n > limit+delta`
// check tripped, the server cancelled the stream, and the client's
// in-flight sends surfaced as EOF.
//
// Fix: maybeAdjustAdditive now caps total buffered bytes at
// `limit + n` (one LPM in flight + one window of slack). When the app
// is not draining, pre-credit is refused, sender backpressures
// correctly (HTTP/2 standard semantics).
//
// Pass: client's Write blocks gracefully (writeCtx deadline fires
// after the first few admitted LPMs).
// Fail: server returns FC-VIOLATION -> client gets EOF / stream-done
// BEFORE writeCtx times out; OR Write loop completes all sends.
func TestShmUnboundedClientStreamSenderBackpressure(t *testing.T) {
	const window = 65535
	const payloadBody = 65536 // > window by 1 byte; LPM total = 65541

	ConfigureShmFlowControlForBench(window)
	ConfigureShmMaxFrameSizeForBench(16384)
	defer ResetShmFlowControlForBench()

	testCtx, testCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer testCancel()

	segName := fmt.Sprintf("test-unbounded-send-%d", time.Now().UnixNano())
	defer RemoveSegment(segName)

	const ringSize = 4 << 20
	serverSeg, err := CreateSegment(segName, ringSize, ringSize)
	if err != nil {
		t.Fatalf("create segment: %v", err)
	}
	serverSeg.H.SetServerReady(true)
	defer serverSeg.Close()

	clientSeg, err := OpenSegment(segName)
	if err != nil {
		t.Fatalf("open segment: %v", err)
	}
	clientSeg.H.SetClientReady(true)
	defer clientSeg.Close()

	srvTransport, err := NewShmServerTransport(serverSeg, testAddr{"shm", "server"}, testAddr{"shm", "client"})
	if err != nil {
		t.Fatalf("server transport: %v", err)
	}
	defer srvTransport.Close(nil)

	cliTransport, err := NewShmClientTransport(clientSeg, testAddr{"shm", "client"}, testAddr{"shm", "server"})
	if err != nil {
		t.Fatalf("client transport: %v", err)
	}
	// NOTE: no defer cliTransport.Close — the test explicitly closes
	// after the write loop is observed parked, to release the sender
	// from its FC-stalled deferred entry.

	// Server: DO NOT drain. Simulates a stuck app that accepted the
	// stream but is not yet ready to Recv. Cancel on test exit so the
	// client's deferred close() doesn't hang on s.done.
	handlerStarted := make(chan struct{}, 1)
	go srvTransport.HandleStreams(testCtx, func(s *ServerStream) {
		select {
		case handlerStarted <- struct{}{}:
		default:
		}
		<-testCtx.Done()
		_ = s.WriteStatus(status.New(codes.Canceled, "test cleanup"))
	})

	ctx, cancel := context.WithCancel(testCtx)
	defer cancel()
	cs, err := cliTransport.NewStream(ctx, &CallHdr{Method: "/test/UnboundedSend"}, nil)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}

	// Force send quota down to the FC window so the bug-relevant
	// constraint is the H2 stream window, not the 2 GiB default
	// fallback when initialWindowSize is unset.
	cliTransport.connSendQuota.Store(int64(window))
	cs.sendQuota.Store(int64(window))

	select {
	case <-handlerStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("server handler did not start within 3s")
	}

	body := make([]byte, payloadBody)
	hdr := make([]byte, 5)
	binary.BigEndian.PutUint32(hdr[1:5], uint32(payloadBody))

	type writeOutcome struct {
		count int
		err   error
	}
	out := make(chan writeOutcome, 1)

	go func() {
		count := 0
		for {
			data := mem.BufferSlice{mem.Copy(body, mem.DefaultBufferPool())}
			err := cs.Write(hdr, data, &WriteOptions{Last: false})
			if err != nil {
				out <- writeOutcome{count, err}
				return
			}
			count++
		}
	}()

	// Give the sender 1.5 s to either backpressure-park or flood
	// past the FC limit. Pre-fix this would either complete tens of
	// thousands of sends (initial small payload) or trip
	// FC-VIOLATION (larger payload, slower drain). Post-fix the
	// sender admits LPM 1, then parks on LPM 2 awaiting WU that
	// never comes — exactly the H2-correct behaviour.
	time.Sleep(1500 * time.Millisecond)
	// Tear down the client transport to release the parked sender.
	// The transport's close() path drains the deferred map and
	// signals ErrConnClosing to any parked writer (see
	// shmFrameWriter.close drain loop).
	cliTransport.Close(nil)

	select {
	case o := <-out:
		t.Logf("Write loop returned: count=%d err=%v", o.count, o.err)
		// The fix path: sender admits LPM 1 (initial pre-credit), parks
		// on LPM 2 awaiting WU. cliTransport.Close above releases the
		// parked sender with ErrConnClosing ("transport is closing"),
		// which is the expected outcome.
		if o.err == nil {
			t.Fatalf("BUG: Write loop returned with no error after %d sends; sender did not backpressure", o.count)
		}
		// FC-VIOLATION / EOF / FLOW_CONTROL_ERROR are the pre-fix bug
		// signatures (server cancelled the stream because maybeAdjustAdditive
		// saturated at maxWindowSize). "transport is closing" is the
		// expected post-fix wake-up reason.
		if isFCBugSignature(o.err) {
			t.Fatalf("BUG REPRODUCED: sender did not backpressure; got server-FC-driven error after %d sends: %v", o.count, o.err)
		}
		// Sender parked correctly. We expect exactly 1 admitted send
		// before the first backpressure-driven park: LPM 1 admits
		// (pre-credit grants enough delta to absorb it), LPM 2 onwards
		// refused (pendingData full, no app drain).
		if o.count > 3 {
			t.Errorf("Backpressure cap too loose: sender completed %d sends before parking; expected <= 3 (LPM 1 admits, LPM 2+ should park)", o.count)
		}
		t.Logf("PASS: sender backpressured correctly (count=%d, err=%v)", o.count, o.err)
	case <-time.After(8 * time.Second):
		t.Fatal("test timed out waiting for write loop outcome")
	}
}

// isFCBugSignature returns true for errors that indicate the pre-fix
// bug: receiver-side FC-VIOLATION which the server surfaces as stream
// cancel / FLOW_CONTROL_ERROR / EOF at the client. It does NOT match
// the expected post-fix wake-up reason "transport is closing", which
// is how the parked sender is released in this test.
func isFCBugSignature(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	// Bug signatures (pre-fix).
	for _, needle := range []string{
		"FC-VIOLATION",
		"exceeding the limit",
		"FLOW_CONTROL_ERROR",
		"EOF",
	} {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}
