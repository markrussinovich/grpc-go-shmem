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
	"sync"

	"google.golang.org/grpc/mem"
)

// recvMsg represents the received msg from the transport. All transport
// protocol specific info has been removed.
type recvMsg struct {
	buffer mem.Buffer
	// nil: received some data
	// io.EOF: stream is completed. data is nil.
	// other non-nil error: transport failure. data is nil.
	err error
}

// recvBuffer is an unbounded channel of recvMsg structs.
//
// Note: recvBuffer differs from buffer.Unbounded only in the fact that it
// holds a channel of recvMsg structs instead of objects implementing "item"
// interface. recvBuffer is written to much more often and using strict recvMsg
// structs helps avoid allocation in "recvBuffer.put"
type recvBuffer struct {
	c       chan recvMsg
	mu      sync.Mutex
	backlog []recvMsg
	err     error
}

// init allows a recvBuffer to be initialized in-place, which is useful
// for resetting a buffer or for avoiding a heap allocation when the buffer
// is embedded in another struct.
func (b *recvBuffer) init() {
	b.c = make(chan recvMsg, 1)
}

func (b *recvBuffer) put(r recvMsg) {
	b.mu.Lock()
	if b.err != nil {
		// drop the buffer on the floor. Since b.err is not nil, any subsequent reads
		// will always return an error, making this buffer inaccessible.
		if r.buffer != nil {
			r.buffer.Free()
		}
		b.mu.Unlock()
		// An error had occurred earlier, don't accept more
		// data or errors.
		return
	}
	b.err = r.err
	if len(b.backlog) == 0 {
		select {
		case b.c <- r:
			b.mu.Unlock()
			return
		default:
		}
	}
	b.backlog = append(b.backlog, r)
	b.mu.Unlock()
}

func (b *recvBuffer) load() {
	b.mu.Lock()
	if len(b.backlog) > 0 {
		select {
		case b.c <- b.backlog[0]:
			b.backlog[0] = recvMsg{}
			b.backlog = b.backlog[1:]
		default:
		}
	}
	b.mu.Unlock()
}

// drainAndFree removes any queued messages and frees their buffers. It does
// not mutate the buffer error state; callers should invoke this only when the
// stream is ending and no further reads are expected.
func (b *recvBuffer) drainAndFree() {
	b.mu.Lock()
	backlog := b.backlog
	b.backlog = nil
	b.mu.Unlock()

	for _, m := range backlog {
		if m.buffer != nil {
			m.buffer.Free()
		}
	}

	for {
		select {
		case m := <-b.c:
			if m.buffer != nil {
				m.buffer.Free()
			}
		default:
			return
		}
	}
}

// get returns the channel that receives a recvMsg in the buffer.
//
// Upon receipt of a recvMsg, the caller should call load to send another
// recvMsg onto the channel if there is any.
func (b *recvBuffer) get() <-chan recvMsg {
	return b.c
}

// recvBufferReader implements io.Reader interface to read the data from
// recvBuffer.
type recvBufferReader struct {
	_ noCopy
	// closeStream, when non-nil, marks this reader as CLIENT-side. On context
	// cancellation it is invoked with ContextErr(ctx.Err()); the callback MUST
	// enqueue that error into recv as a recvMsg (mirroring the client stream's
	// Close), so that the ctx error is delayed until the recv buffer drains.
	// This preserves the ctx-cancel/trailer race fix inherited from grpc-go's
	// HTTP/2 transport. A nil closeStream marks a server-side reader, which
	// returns the ctx error immediately.
	//
	// The callback is injected (rather than storing a concrete client stream)
	// so that this receive machinery stays independent of the stream types and
	// carries no back-reference to them.
	closeStream func(error)
	ctx         context.Context
	ctxDone     <-chan struct{} // cache of ctx.Done() (for performance).
	recv        *recvBuffer
	last        mem.Buffer // Stores the remaining data in the previous calls.
	err         error
}

func (r *recvBufferReader) ReadMessageHeader(header []byte) (n int, err error) {
	if r.err != nil {
		return 0, r.err
	}
	if r.last != nil {
		n, r.last = mem.ReadUnsafe(header, r.last)
		return n, nil
	}
	if r.closeStream != nil {
		n, r.err = r.readMessageHeaderClient(header)
	} else {
		n, r.err = r.readMessageHeader(header)
	}
	return n, r.err
}

// Read reads the next n bytes from last. If last is drained, it tries to read
// additional data from recv. It blocks if there no additional data available in
// recv. If Read returns any non-nil error, it will continue to return that
// error.
func (r *recvBufferReader) Read(n int) (buf mem.Buffer, err error) {
	if r.err != nil {
		return nil, r.err
	}
	if r.last != nil {
		buf = r.last
		if r.last.Len() > n {
			buf, r.last = mem.SplitUnsafe(buf, n)
		} else {
			r.last = nil
		}
		return buf, nil
	}
	if r.closeStream != nil {
		buf, r.err = r.readClient(n)
	} else {
		buf, r.err = r.read(n)
	}
	return buf, r.err
}

func (r *recvBufferReader) readMessageHeader(header []byte) (n int, err error) {
	select {
	case <-r.ctxDone:
		return 0, ContextErr(r.ctx.Err())
	case m := <-r.recv.get():
		return r.readMessageHeaderAdditional(m, header)
	}
}

func (r *recvBufferReader) read(n int) (buf mem.Buffer, err error) {
	select {
	case <-r.ctxDone:
		return nil, ContextErr(r.ctx.Err())
	case m := <-r.recv.get():
		return r.readAdditional(m, n)
	}
}

func (r *recvBufferReader) readMessageHeaderClient(header []byte) (n int, err error) {
	// If the context is canceled, then closes the stream with nil metadata.
	// closeStream writes its error parameter to r.recv as a recvMsg.
	// r.readAdditional acts on that message and returns the necessary error.
	select {
	case <-r.ctxDone:
		// Note that this adds the ctx error to the end of recv buffer, and
		// reads from the head. This will delay the error until recv buffer is
		// empty, thus will delay ctx cancellation in Recv().
		//
		// It's done this way to fix a race between ctx cancel and trailer. The
		// race was, stream.Recv() may return ctx error if ctxDone wins the
		// race, but stream.Trailer() may return a non-nil md because the stream
		// was not marked as done when trailer is received. This closeStream
		// call will mark stream as done, thus fix the race.
		//
		// TODO: delaying ctx error seems like a unnecessary side effect. What
		// we really want is to mark the stream as done, and return ctx error
		// faster.
		r.closeStream(ContextErr(r.ctx.Err()))
		m := <-r.recv.get()
		return r.readMessageHeaderAdditional(m, header)
	case m := <-r.recv.get():
		return r.readMessageHeaderAdditional(m, header)
	}
}

func (r *recvBufferReader) readClient(n int) (buf mem.Buffer, err error) {
	// If the context is canceled, then closes the stream with nil metadata.
	// closeStream writes its error parameter to r.recv as a recvMsg.
	// r.readAdditional acts on that message and returns the necessary error.
	select {
	case <-r.ctxDone:
		// Note that this adds the ctx error to the end of recv buffer, and
		// reads from the head. This will delay the error until recv buffer is
		// empty, thus will delay ctx cancellation in Recv().
		//
		// It's done this way to fix a race between ctx cancel and trailer. The
		// race was, stream.Recv() may return ctx error if ctxDone wins the
		// race, but stream.Trailer() may return a non-nil md because the stream
		// was not marked as done when trailer is received. This closeStream
		// call will mark stream as done, thus fix the race.
		//
		// TODO: delaying ctx error seems like a unnecessary side effect. What
		// we really want is to mark the stream as done, and return ctx error
		// faster.
		r.closeStream(ContextErr(r.ctx.Err()))
		m := <-r.recv.get()
		return r.readAdditional(m, n)
	case m := <-r.recv.get():
		return r.readAdditional(m, n)
	}
}

func (r *recvBufferReader) readMessageHeaderAdditional(m recvMsg, header []byte) (n int, err error) {
	r.recv.load()
	if m.err != nil {
		if m.buffer != nil {
			m.buffer.Free()
		}
		return 0, m.err
	}

	n, r.last = mem.ReadUnsafe(header, m.buffer)

	return n, nil
}

func (r *recvBufferReader) readAdditional(m recvMsg, n int) (b mem.Buffer, err error) {
	r.recv.load()
	if m.err != nil {
		if m.buffer != nil {
			m.buffer.Free()
		}
		return nil, m.err
	}

	if m.buffer.Len() > n {
		m.buffer, r.last = mem.SplitUnsafe(m.buffer, n)
	}

	return m.buffer, nil
}

// transportReader reads all the data available for this stream from the
// transport and passes them into the decoder, which converts them into a gRPC
// message stream. The error is io.EOF when the stream is done or another
// non-nil error if the stream broke.
type transportReader struct {
	_ noCopy
	// The handler to control the window update procedure for both this
	// particular stream and the associated transport.
	windowHandler windowHandler
	er            error
	reader        recvBufferReader
}

// windowHandler controls the window update procedure for both a particular
// stream and its associated transport.
type windowHandler interface {
	updateWindow(int)
}

func (t *transportReader) ReadMessageHeader(header []byte) (int, error) {
	n, err := t.reader.ReadMessageHeader(header)
	if err != nil {
		t.er = err
		return 0, err
	}
	t.windowHandler.updateWindow(n)
	return n, nil
}

func (t *transportReader) Read(n int) (mem.Buffer, error) {
	buf, err := t.reader.Read(n)
	if err != nil {
		t.er = err
		return buf, err
	}
	t.windowHandler.updateWindow(buf.Len())
	return buf, nil
}
