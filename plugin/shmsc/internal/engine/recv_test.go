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

package engine

import (
	"context"
	"io"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/status"
)

func TestRecvBufferPutGetLoad(t *testing.T) {
	b := &recvBuffer{}
	b.init()
	b.put(recvMsg{buffer: mem.SliceBuffer([]byte("a"))})
	b.put(recvMsg{buffer: mem.SliceBuffer([]byte("b"))})

	m1 := <-b.get()
	if got := string(m1.buffer.ReadOnlyData()); got != "a" {
		t.Fatalf("first get = %q, want %q", got, "a")
	}
	b.load()
	m2 := <-b.get()
	if got := string(m2.buffer.ReadOnlyData()); got != "b" {
		t.Fatalf("second get = %q, want %q", got, "b")
	}
}

func TestRecvBufferDropsAfterError(t *testing.T) {
	b := &recvBuffer{}
	b.init()
	b.put(recvMsg{err: io.EOF})
	// Once an error is recorded, subsequent data is dropped (not enqueued).
	b.put(recvMsg{buffer: mem.SliceBuffer([]byte("dropped"))})

	m := <-b.get()
	if m.err != io.EOF {
		t.Fatalf("first msg err = %v, want io.EOF", m.err)
	}
	b.load()
	select {
	case extra := <-b.get():
		t.Fatalf("unexpected extra message after error: %+v", extra)
	default:
	}
}

func TestRecvBufferReaderServerRead(t *testing.T) {
	b := &recvBuffer{}
	b.init()
	b.put(recvMsg{buffer: mem.SliceBuffer([]byte("hello"))})
	b.put(recvMsg{err: io.EOF})

	r := &recvBufferReader{ctx: context.Background(), ctxDone: make(chan struct{}), recv: b}
	buf, err := r.Read(5)
	if err != nil {
		t.Fatalf("Read(5) err = %v", err)
	}
	if got := string(buf.ReadOnlyData()); got != "hello" {
		t.Fatalf("Read(5) = %q, want %q", got, "hello")
	}
	buf.Free()
	if _, err := r.Read(1); err != io.EOF {
		t.Fatalf("Read after data err = %v, want io.EOF", err)
	}
}

func TestRecvBufferReaderReadMessageHeader(t *testing.T) {
	b := &recvBuffer{}
	b.init()
	b.put(recvMsg{buffer: mem.SliceBuffer([]byte{0, 0, 0, 0, 5})})

	r := &recvBufferReader{ctx: context.Background(), ctxDone: make(chan struct{}), recv: b}
	hdr := make([]byte, 5)
	n, err := r.ReadMessageHeader(hdr)
	if err != nil || n != 5 {
		t.Fatalf("ReadMessageHeader = (%d, %v), want (5, nil)", n, err)
	}
}

// TestRecvBufferReaderClientCtxCancel verifies the client-side reader invokes
// the injected closeStream callback exactly once on context cancellation and
// surfaces the ctx error (delayed through the recv buffer), mirroring grpc-go's
// HTTP/2 ctx-cancel / trailer race fix.
func TestRecvBufferReaderClientCtxCancel(t *testing.T) {
	b := &recvBuffer{}
	b.init()
	ctx, cancel := context.WithCancel(context.Background())

	var closedWith error
	calls := 0
	r := &recvBufferReader{
		ctx:     ctx,
		ctxDone: ctx.Done(),
		recv:    b,
		closeStream: func(e error) {
			calls++
			closedWith = e
			// Mirror ClientStream.Close: enqueue the error into recv.
			b.put(recvMsg{err: e})
		},
	}
	cancel()

	_, err := r.Read(4)
	if calls != 1 {
		t.Fatalf("closeStream called %d times, want 1", calls)
	}
	if status.Code(err) != codes.Canceled {
		t.Fatalf("Read err code = %v, want Canceled", status.Code(err))
	}
	if status.Code(closedWith) != codes.Canceled {
		t.Fatalf("closeStream error code = %v, want Canceled", status.Code(closedWith))
	}
}

type fakeWindowHandler struct{ total int }

func (f *fakeWindowHandler) updateWindow(n int) { f.total += n }

func TestTransportReaderUpdateWindow(t *testing.T) {
	b := &recvBuffer{}
	b.init()
	b.put(recvMsg{buffer: mem.SliceBuffer([]byte("abcd"))})

	wh := &fakeWindowHandler{}
	tr := &transportReader{
		windowHandler: wh,
		reader:        recvBufferReader{ctx: context.Background(), ctxDone: make(chan struct{}), recv: b},
	}
	buf, err := tr.Read(4)
	if err != nil {
		t.Fatalf("Read err = %v", err)
	}
	if buf.Len() != 4 {
		t.Fatalf("Read len = %d, want 4", buf.Len())
	}
	buf.Free()
	if wh.total != 4 {
		t.Fatalf("updateWindow total = %d, want 4", wh.total)
	}
}
