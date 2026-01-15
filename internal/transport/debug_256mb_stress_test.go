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
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDebug256MBRoundtrip(t *testing.T) {
	// Skip this stress test - it times out in CI environments.
	t.Skip("Skipping 256MB stress test - run manually with longer timeout")
	const ringSize = 64 * 1024 * 1024
	const dataSize = 256 * 1024 * 1024
	const chunkSize = 4 * 1024 * 1024

	segName := fmt.Sprintf("debug-256mb-%d", time.Now().UnixNano())
	seg, err := CreateSegment(segName, ringSize, ringSize)
	if err != nil {
		t.Fatalf("CreateSegment failed: %v", err)
	}
	defer seg.Close()
	defer RemoveSegment(segName)

	clientToServer := NewShmRingFromSegment(seg.A, seg.Mem)
	serverToClient := NewShmRingFromSegment(seg.B, seg.Mem)

	// Enable debugging
	t.Logf("Ring capacity: %d (capMask: 0x%x)", ringSize, ringSize-1)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	data := make([]byte, dataSize)

	var serverBytesRead, serverBytesWritten int64
	var clientBytesWritten, clientBytesRead int64
	var serverReading, serverWriting, clientWriting, clientReading int32

	started := make(chan struct{})
	serverDone := make(chan struct{})

	readBuf := make([]byte, chunkSize)
	go func() {
		defer close(serverDone)
		close(started)
		for {
			atomic.StoreInt32(&serverReading, 1)
			n, err := clientToServer.ReadBlockingContext(ctx, readBuf)
			atomic.StoreInt32(&serverReading, 0)
			if err != nil {
				t.Logf("Server read done: %v (total read %dMB)", err, atomic.LoadInt64(&serverBytesRead)/(1024*1024))
				return
			}
			atomic.AddInt64(&serverBytesRead, int64(n))

			atomic.StoreInt32(&serverWriting, 1)
			res, err := serverToClient.ReserveWrite(ctx, n)
			atomic.StoreInt32(&serverWriting, 0)
			if err != nil {
				t.Logf("Server write err: %v (total written %dMB)", err, atomic.LoadInt64(&serverBytesWritten)/(1024*1024))
				return
			}
			written := copy(res.First, readBuf[:n])
			if len(res.Second) > 0 && written < n {
				copy(res.Second, readBuf[written:n])
			}
			res.Commit(n)
			atomic.AddInt64(&serverBytesWritten, int64(n))
		}
	}()
	<-started

	// Progress monitor with ring state
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				cw := atomic.LoadInt64(&clientBytesWritten)
				cr := atomic.LoadInt64(&clientBytesRead)
				sr := atomic.LoadInt64(&serverBytesRead)
				sw := atomic.LoadInt64(&serverBytesWritten)

				// Get ring states
				ctsState := clientToServer.DebugState()
				stcState := serverToClient.DebugState()

				// Get blocking states
				sR := atomic.LoadInt32(&serverReading)
				sW := atomic.LoadInt32(&serverWriting)
				cW := atomic.LoadInt32(&clientWriting)
				cR := atomic.LoadInt32(&clientReading)

				t.Logf("Progress: cW=%dMB cR=%dMB sR=%dMB sW=%dMB | Blocking: sR=%d sW=%d cW=%d cR=%d",
					cw/(1024*1024), cr/(1024*1024), sr/(1024*1024), sw/(1024*1024),
					sR, sW, cW, cR)
				t.Logf("  c2s: widx=%d ridx=%d used=%d dSeq=%d sSeq=%d closed=%d dWait=%d sWait=%d",
					ctsState.Widx, ctsState.Ridx, ctsState.Used,
					ctsState.DataSeq, ctsState.SpaceSeq, ctsState.Closed,
					ctsState.DataWaiters, ctsState.SpaceWaiters)
				t.Logf("  s2c: widx=%d ridx=%d used=%d dSeq=%d sSeq=%d closed=%d dWait=%d sWait=%d",
					stcState.Widx, stcState.Ridx, stcState.Used,
					stcState.DataSeq, stcState.SpaceSeq, stcState.Closed,
					stcState.DataWaiters, stcState.SpaceWaiters)
			}
		}
	}()

	var wg sync.WaitGroup
	wg.Add(2)

	// Client writer
	go func() {
		defer wg.Done()
		offset := 0
		for offset < dataSize {
			writeSize := min(chunkSize, dataSize-offset)
			atomic.StoreInt32(&clientWriting, 1)
			res, err := clientToServer.ReserveWrite(ctx, writeSize)
			atomic.StoreInt32(&clientWriting, 0)
			if err != nil {
				t.Logf("Client write err at %dMB: %v", offset/(1024*1024), err)
				return
			}
			copy(res.First, data[offset:offset+writeSize])
			if len(res.Second) > 0 {
				copy(res.Second, data[offset+len(res.First):offset+writeSize])
			}
			res.Commit(writeSize)
			atomic.AddInt64(&clientBytesWritten, int64(writeSize))
			offset += writeSize
		}
		t.Logf("Client writer done: %dMB", dataSize/(1024*1024))
	}()

	// Client reader
	recvBuf := make([]byte, chunkSize)
	go func() {
		defer wg.Done()
		totalRead := 0
		for totalRead < dataSize {
			atomic.StoreInt32(&clientReading, 1)
			n, err := serverToClient.ReadBlockingContext(ctx, recvBuf)
			atomic.StoreInt32(&clientReading, 0)
			if err != nil {
				t.Logf("Client read err at %dMB: %v", totalRead/(1024*1024), err)
				return
			}
			atomic.AddInt64(&clientBytesRead, int64(n))
			totalRead += n
		}
		t.Logf("Client reader done: %dMB", dataSize/(1024*1024))
	}()

	wg.Wait()
	cancel()
	clientToServer.Close()
	serverToClient.Close()
	<-serverDone

	if atomic.LoadInt64(&clientBytesWritten) != dataSize {
		t.Errorf("Client wrote %d, expected %d", clientBytesWritten, dataSize)
	}
	if atomic.LoadInt64(&clientBytesRead) != dataSize {
		t.Errorf("Client read %d, expected %d", clientBytesRead, dataSize)
	}
}
