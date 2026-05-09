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
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// Ring size fixed at 64MB — matches production default.
// Large payloads (>32MB) will fall back from ZC to copy automatically.
const benchRingSize = 64 * 1024 * 1024

// protoSizes returns 0B → 256MB payload body sizes.
func protoSizes() []int {
	return []int{
		0, 64, 256, 1024, 4096, 16384,
		64 * 1024, 256 * 1024,
		1024 * 1024, 4 * 1024 * 1024,
		16 * 1024 * 1024, 64 * 1024 * 1024,
		256 * 1024 * 1024,
	}
}

func makePayload(n int) *wrapperspb.BytesValue {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i & 0xFF)
	}
	return &wrapperspb.BytesValue{Value: b}
}

func sizeLabel(n int) string {
	switch {
	case n == 0:
		return "size=0B"
	case n >= 1024*1024:
		return fmt.Sprintf("size=%dMB", n/(1024*1024))
	case n >= 1024:
		return fmt.Sprintf("size=%dKB", n/1024)
	default:
		return fmt.Sprintf("size=%dB", n)
	}
}

// ---------------------------------------------------------------------------
// Write helpers — reuse production code paths
// ---------------------------------------------------------------------------
//
// Default flags=0 means "complete logical message; more messages may
// follow" — neither MessageFlagMORE (chunk-level "more chunks of THIS
// message") nor MessageFlagEndStream (H2-only "this is the last
// message I'll send; half-close after"). This exercises the new
// MORE-vs-EndStream split correctly on every iteration: H2 emits
// END_STREAM only when EndStream is set, not as a side-effect of the
// last DATA frame in a burst. Tests that want to exercise the
// EndStream path explicitly should use writeProto*ToRing directly
// with the desired flags rather than these bench-oriented helpers.

// writeZC calls writeProtoToRing (shared with transport.writeProto).
// Falls back to a marshal-then-write path if ZC can't fit contiguously.
func writeZC(ctx context.Context, tx *ShmRing, id uint32, msg proto.Message) error {
	ok, err := writeProtoToRing(ctx, tx, id, msg, -1, 0)
	if err != nil {
		return err
	}
	if !ok {
		return writeCopy(ctx, tx, id, msg)
	}
	return nil
}

// writeCopy marshals the message into a heap buffer (with a 5-byte gRPC
// LPM header) and writes it as a single MESSAGE frame. Used by the bench
// suite to compare the copy-then-write path against the ZC fast path.
func writeCopy(ctx context.Context, tx *ShmRing, id uint32, msg proto.Message) error {
	pSize := proto.Size(msg)
	buf := make([]byte, 5, 5+pSize)
	buf[0] = 0
	binary.BigEndian.PutUint32(buf[1:5], uint32(pSize))
	buf, err := proto.MarshalOptions{UseCachedSize: true}.MarshalAppend(buf, msg)
	if err != nil {
		return err
	}
	return writeFrame(ctx, tx, FrameHeader{
		Type:     FrameTypeMESSAGE,
		StreamID: id,
	}, buf)
}

// ---------------------------------------------------------------------------
// Read helpers — reuse production code paths
// ---------------------------------------------------------------------------
//
// The bench readers drive message-completion off the LPM 5-byte
// length prefix (consumed/total bytes) rather than the per-frame
// MessageFlagMORE bit. This decouples bench correctness from the
// codec's MORE-vs-EndStream signalling: a single H2 DATA carrying
// one complete LPM with no END_STREAM still surfaces as
// MessageFlagMORE-set in the new semantics (MORE=1 means "more
// frames follow on this stream"), but the message is logically
// complete when assembled bytes equal 5+payloadLen. Driving the
// loop off the LPM length is also closer to production
// MaterializeToBuffer semantics.

// readZC calls readFrameView (zero-copy SliceBuffer) + direct Unmarshal.
// For single-frame messages: proto.Unmarshal reads directly from ring (true ZC).
// For multi-chunk messages: falls back to readFrame (copy) because the caller
// must assemble chunks anyway, and readFrameView's mem.Buffer overhead +
// speculative hold provide no benefit over raw ReadExact.
func readZC(ctx context.Context, rx *ShmRing, msg proto.Message) error {
	// Read first frame via ZC path — might be single-frame (the win case).
	fh, buf, err := readFrameView(ctx, rx)
	if err != nil {
		return err
	}
	if fh.Type != FrameTypeMESSAGE || buf == nil {
		if buf != nil {
			buf.Free()
		}
		return fmt.Errorf("unexpected frame type %d", fh.Type)
	}

	data := buf.ReadOnlyData()
	if len(data) < 5 {
		buf.Free()
		return fmt.Errorf("short payload %d", len(data))
	}
	payloadLen := int(binary.BigEndian.Uint32(data[1:5]))
	actualPayload := len(data) - 5
	if payloadLen == actualPayload {
		// Single-frame complete message — unmarshal directly from ring.
		err = proto.Unmarshal(data[5:], msg)
		buf.Free()
		return err
	}
	if actualPayload > payloadLen {
		buf.Free()
		return fmt.Errorf("CORRUPTION: grpcLen=%d actual=%d fhLen=%d",
			payloadLen, actualPayload, fh.Length)
	}

	// Multi-chunk: pre-allocate from gRPC length prefix in first chunk.
	assembled := make([]byte, 0, 5+payloadLen)
	assembled = append(assembled, data...)
	buf.Free()

	// Read remaining chunks via readFrameView so the chain-ZC codec
	// sees them and can close the chain on the final !MORE chunk.
	// Using raw ReadSlices/ReadExact here would bypass the chain
	// state machine and freeze header.ReadIdx forever (the open ZC
	// anchor would never be released because the codec never observes
	// the final chunk).
	for len(assembled) < 5+payloadLen {
		fh2, buf2, err := readFrameView(ctx, rx)
		if err != nil {
			return err
		}
		if fh2.Type != FrameTypeMESSAGE {
			if buf2 != nil {
				buf2.Free()
			}
			return fmt.Errorf("unexpected frame type %d", fh2.Type)
		}
		if buf2 != nil {
			assembled = append(assembled, buf2.ReadOnlyData()...)
			buf2.Free()
		}
	}
	return proto.Unmarshal(assembled[5:], msg)
}

// readCopy calls readFrame (always copies) + Unmarshal. Handles chunks.
func readCopy(ctx context.Context, rx *ShmRing, msg proto.Message) error {
	fh, payload, err := readFrame(ctx, rx)
	if err != nil {
		return err
	}
	if fh.Type != FrameTypeMESSAGE {
		return fmt.Errorf("unexpected frame type %d", fh.Type)
	}
	if len(payload) < 5 {
		return fmt.Errorf("short payload %d", len(payload))
	}
	payloadLen := int(binary.BigEndian.Uint32(payload[1:5]))
	actualPayload := len(payload) - 5
	if payloadLen == actualPayload {
		// Single-frame complete message.
		return proto.Unmarshal(payload[5:], msg)
	}
	if actualPayload > payloadLen {
		return fmt.Errorf("CORRUPTION: grpcLen=%d actual=%d", payloadLen, actualPayload)
	}

	// Multi-chunk: pre-allocate from gRPC length prefix in first chunk.
	// Matches production MaterializeToBuffer(pool.Get(totalLength)).
	assembled := make([]byte, 0, 5+payloadLen)
	assembled = append(assembled, payload...)
	for len(assembled) < 5+payloadLen {
		fh3, payload3, err := readFrame(ctx, rx)
		if err != nil {
			return err
		}
		if fh3.Type != FrameTypeMESSAGE {
			return fmt.Errorf("unexpected frame type %d", fh3.Type)
		}
		assembled = append(assembled, payload3...)
	}
	return proto.Unmarshal(assembled[5:], msg)
}

// ---------------------------------------------------------------------------
// Unary round-trip benchmark (echo: write req → read req → write resp → read resp)
// ---------------------------------------------------------------------------

func benchUnary(b *testing.B, bodySize int, useZC bool) {
	// Skip very large payloads in short mode to avoid hangs.
	if testing.Short() && bodySize > 16*1024*1024 {
		b.Skipf("skipping %s in short mode", sizeLabel(bodySize))
	}
	tag := "copy"
	if useZC {
		tag = "zc"
	}
	segName := fmt.Sprintf("bp-%s-%d-%d", tag, bodySize, time.Now().UnixNano())
	seg, err := CreateSegment(segName, benchRingSize, benchRingSize)
	if err != nil {
		b.Fatalf("CreateSegment: %v", err)
	}
	defer func() { seg.Close(); RemoveSegment(segName) }()

	txA := NewShmRingFromSegment(seg.A, seg.Mem)
	rxA := NewShmRingFromSegment(seg.A, seg.Mem)
	txB := NewShmRingFromSegment(seg.B, seg.Mem)
	rxB := NewShmRingFromSegment(seg.B, seg.Mem)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	req := makePayload(bodySize)
	resp := makePayload(bodySize)
	b.SetBytes(int64(proto.Size(req) * 2))

	write := writeCopy
	read := readCopy
	if useZC {
		write = writeZC
		read = readZC
	}

	var wg sync.WaitGroup
	started := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		close(started)
		recv := &wrapperspb.BytesValue{}
		for i := 0; i < b.N; i++ {
			if err := read(ctx, rxA, recv); err != nil {
				return
			}
			if err := write(ctx, txB, 1, resp); err != nil {
				return
			}
		}
	}()

	<-started
	b.ResetTimer()

	recv := &wrapperspb.BytesValue{}
	for i := 0; i < b.N; i++ {
		if err := write(ctx, txA, 1, req); err != nil {
			b.Fatal(err)
		}
		if err := read(ctx, rxB, recv); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	txA.Close()
	txB.Close()
	wg.Wait()
}

func BenchmarkShmProtoUnaryZC(b *testing.B) {
	for _, s := range protoSizes() {
		b.Run(sizeLabel(s), func(b *testing.B) { benchUnary(b, s, true) })
	}
}

func BenchmarkShmProtoUnaryCopy(b *testing.B) {
	for _, s := range protoSizes() {
		b.Run(sizeLabel(s), func(b *testing.B) { benchUnary(b, s, false) })
	}
}

// ---------------------------------------------------------------------------
// Server streaming benchmark (1 req → N responses)
// ---------------------------------------------------------------------------

func benchStreaming(b *testing.B, msgCount, bodySize int, useZC bool) {
	if testing.Short() && bodySize > 16*1024*1024 {
		b.Skipf("skipping %s in short mode", sizeLabel(bodySize))
	}
	tag := "copy"
	if useZC {
		tag = "zc"
	}
	segName := fmt.Sprintf("bs-%s-%d-%d-%d", tag, msgCount, bodySize, time.Now().UnixNano())
	seg, err := CreateSegment(segName, benchRingSize, benchRingSize)
	if err != nil {
		b.Fatalf("CreateSegment: %v", err)
	}
	defer func() { seg.Close(); RemoveSegment(segName) }()

	txA := NewShmRingFromSegment(seg.A, seg.Mem)
	rxA := NewShmRingFromSegment(seg.A, seg.Mem)
	txB := NewShmRingFromSegment(seg.B, seg.Mem)
	rxB := NewShmRingFromSegment(seg.B, seg.Mem)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	req := makePayload(bodySize)
	resp := makePayload(bodySize)
	b.SetBytes(int64(proto.Size(req) * (msgCount + 1)))

	write := writeCopy
	read := readCopy
	if useZC {
		write = writeZC
		read = readZC
	}

	var wg sync.WaitGroup
	started := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		close(started)
		recv := &wrapperspb.BytesValue{}
		for i := 0; i < b.N; i++ {
			if err := read(ctx, rxA, recv); err != nil {
				return
			}
			for j := 0; j < msgCount; j++ {
				if err := write(ctx, txB, 1, resp); err != nil {
					return
				}
			}
		}
	}()

	<-started
	b.ResetTimer()

	recv := &wrapperspb.BytesValue{}
	for i := 0; i < b.N; i++ {
		if err := write(ctx, txA, 1, req); err != nil {
			b.Fatal(err)
		}
		for j := 0; j < msgCount; j++ {
			if err := read(ctx, rxB, recv); err != nil {
				b.Fatal(err)
			}
		}
	}
	b.StopTimer()
	txA.Close()
	txB.Close()
	wg.Wait()
}

func BenchmarkShmProtoStreamingZC(b *testing.B) {
	// Message count sweep with 1KB payload
	for _, n := range []int{10, 100, 1000} {
		b.Run(fmt.Sprintf("1KB/msgs=%d", n), func(b *testing.B) { benchStreaming(b, n, 1024, true) })
	}
	// Payload size sweep with 100 messages (like .NET's streaming benchmark)
	for _, size := range []int{64, 256, 1024, 4096, 16384, 64 * 1024, 256 * 1024, 1024 * 1024, 4 * 1024 * 1024, 16 * 1024 * 1024} {
		b.Run(fmt.Sprintf("%s/msgs=100", sizeLabel(size)), func(b *testing.B) { benchStreaming(b, 100, size, true) })
	}
	// Large payloads with fewer messages to keep total data manageable
	for _, size := range []int{64 * 1024 * 1024, 256 * 1024 * 1024} {
		b.Run(fmt.Sprintf("%s/msgs=10", sizeLabel(size)), func(b *testing.B) { benchStreaming(b, 10, size, true) })
	}
}

func BenchmarkShmProtoStreamingCopy(b *testing.B) {
	for _, n := range []int{10, 100, 1000} {
		b.Run(fmt.Sprintf("1KB/msgs=%d", n), func(b *testing.B) { benchStreaming(b, n, 1024, false) })
	}
	for _, size := range []int{64, 256, 1024, 4096, 16384, 64 * 1024, 256 * 1024, 1024 * 1024, 4 * 1024 * 1024, 16 * 1024 * 1024} {
		b.Run(fmt.Sprintf("%s/msgs=100", sizeLabel(size)), func(b *testing.B) { benchStreaming(b, 100, size, false) })
	}
	for _, size := range []int{64 * 1024 * 1024, 256 * 1024 * 1024} {
		b.Run(fmt.Sprintf("%s/msgs=10", sizeLabel(size)), func(b *testing.B) { benchStreaming(b, 10, size, false) })
	}
}

// ---------------------------------------------------------------------------
// Bidi streaming benchmark (concurrent ping-pong like .NET StreamingCall)
// ---------------------------------------------------------------------------

func benchBidi(b *testing.B, bodySize int, useZC bool) {
	if testing.Short() && bodySize > 16*1024*1024 {
		b.Skipf("skipping %s in short mode", sizeLabel(bodySize))
	}
	tag := "copy"
	if useZC {
		tag = "zc"
	}
	segName := fmt.Sprintf("bb-%s-%d-%d", tag, bodySize, time.Now().UnixNano())
	seg, err := CreateSegment(segName, benchRingSize, benchRingSize)
	if err != nil {
		b.Fatalf("CreateSegment: %v", err)
	}
	defer func() { seg.Close(); RemoveSegment(segName) }()

	txA := NewShmRingFromSegment(seg.A, seg.Mem)
	rxA := NewShmRingFromSegment(seg.A, seg.Mem)
	txB := NewShmRingFromSegment(seg.B, seg.Mem)
	rxB := NewShmRingFromSegment(seg.B, seg.Mem)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	req := makePayload(bodySize)
	resp := makePayload(bodySize)
	b.SetBytes(int64(proto.Size(req) * 2))

	write := writeCopy
	read := readCopy
	if useZC {
		write = writeZC
		read = readZC
	}

	var wg sync.WaitGroup
	started := make(chan struct{})

	// Echo server: receive one → send one (ping-pong like .NET StreamingCall)
	wg.Add(1)
	go func() {
		defer wg.Done()
		close(started)
		recv := &wrapperspb.BytesValue{}
		for i := 0; i < b.N; i++ {
			if err := read(ctx, rxA, recv); err != nil {
				return
			}
			if err := write(ctx, txB, 1, resp); err != nil {
				return
			}
		}
	}()

	<-started
	b.ResetTimer()

	recv := &wrapperspb.BytesValue{}
	for i := 0; i < b.N; i++ {
		if err := write(ctx, txA, 1, req); err != nil {
			b.Fatal(err)
		}
		if err := read(ctx, rxB, recv); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	txA.Close()
	txB.Close()
	wg.Wait()
}

// Bidi ping-pong with payload size sweep (matches .NET StreamingCall benchmark)
func BenchmarkShmProtoBidiZC(b *testing.B) {
	for _, size := range protoSizes() {
		b.Run(sizeLabel(size), func(b *testing.B) { benchBidi(b, size, true) })
	}
}

func BenchmarkShmProtoBidiCopy(b *testing.B) {
	for _, size := range protoSizes() {
		b.Run(sizeLabel(size), func(b *testing.B) { benchBidi(b, size, false) })
	}
}

func BenchmarkTCPProtoBidi(b *testing.B) {
	for _, bodySize := range protoSizes() {
		b.Run(sizeLabel(bodySize), func(b *testing.B) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				b.Fatal(err)
			}
			defer ln.Close()

			req := makePayload(bodySize)
			resp := makePayload(bodySize)
			b.SetBytes(int64(proto.Size(req) * 2))

			var wg sync.WaitGroup
			started := make(chan struct{})

			wg.Add(1)
			go func() {
				defer wg.Done()
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				close(started)
				recv := &wrapperspb.BytesValue{}
				for i := 0; i < b.N; i++ {
					if err := readProtoTCP(conn, recv); err != nil {
						return
					}
					if err := writeProtoTCP(conn, 1, resp); err != nil {
						return
					}
				}
			}()

			conn, err := net.Dial("tcp", ln.Addr().String())
			if err != nil {
				b.Fatal(err)
			}
			defer conn.Close()
			<-started
			b.ResetTimer()

			recv := &wrapperspb.BytesValue{}
			for i := 0; i < b.N; i++ {
				if err := writeProtoTCP(conn, 1, req); err != nil {
					b.Fatal(err)
				}
				if err := readProtoTCP(conn, recv); err != nil {
					b.Fatal(err)
				}
			}
			wg.Wait()
		})
	}
}

// ---------------------------------------------------------------------------
// TCP proto helpers and benchmarks
// ---------------------------------------------------------------------------

func benchTCPStreaming(b *testing.B, msgCount, bodySize int) {
	if testing.Short() && bodySize > 16*1024*1024 {
		b.Skipf("skipping %s in short mode", sizeLabel(bodySize))
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	defer ln.Close()

	req := makePayload(bodySize)
	resp := makePayload(bodySize)
	b.SetBytes(int64(proto.Size(req) * (msgCount + 1)))

	var wg sync.WaitGroup
	started := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		close(started)
		recv := &wrapperspb.BytesValue{}
		for i := 0; i < b.N; i++ {
			if err := readProtoTCP(conn, recv); err != nil {
				return
			}
			for j := 0; j < msgCount; j++ {
				if err := writeProtoTCP(conn, 1, resp); err != nil {
					return
				}
			}
		}
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		b.Fatal(err)
	}
	defer conn.Close()
	<-started
	b.ResetTimer()

	recv := &wrapperspb.BytesValue{}
	for i := 0; i < b.N; i++ {
		if err := writeProtoTCP(conn, 1, req); err != nil {
			b.Fatal(err)
		}
		for j := 0; j < msgCount; j++ {
			if err := readProtoTCP(conn, recv); err != nil {
				b.Fatal(err)
			}
		}
	}
	wg.Wait()
}

func BenchmarkTCPProtoStreaming(b *testing.B) {
	for _, size := range []int{64, 256, 1024, 4096, 16384, 64 * 1024, 256 * 1024, 1024 * 1024, 4 * 1024 * 1024, 16 * 1024 * 1024} {
		b.Run(fmt.Sprintf("%s/msgs=100", sizeLabel(size)), func(b *testing.B) { benchTCPStreaming(b, 100, size) })
	}
	for _, size := range []int{64 * 1024 * 1024, 256 * 1024 * 1024} {
		b.Run(fmt.Sprintf("%s/msgs=10", sizeLabel(size)), func(b *testing.B) { benchTCPStreaming(b, 10, size) })
	}
}

func writeProtoTCP(conn net.Conn, id uint32, msg proto.Message) error {
	out, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	buf := make([]byte, frameHeaderSize+5+len(out))
	var hdr [frameHeaderSize]byte
	encodeFrameHeaderTo(&hdr, FrameHeader{Type: FrameTypeMESSAGE, StreamID: id, Length: uint32(5 + len(out))})
	copy(buf, hdr[:])
	buf[frameHeaderSize] = 0
	binary.BigEndian.PutUint32(buf[frameHeaderSize+1:], uint32(len(out)))
	copy(buf[frameHeaderSize+5:], out)
	_, err = conn.Write(buf)
	return err
}

func readProtoTCP(conn net.Conn, msg proto.Message) error {
	var hdrBuf [frameHeaderSize]byte
	if _, err := io.ReadFull(conn, hdrBuf[:]); err != nil {
		return err
	}
	fh, err := decodeFrameHeader(hdrBuf[:])
	if err != nil {
		return err
	}
	payload := make([]byte, fh.Length)
	if _, err := io.ReadFull(conn, payload); err != nil {
		return err
	}
	if len(payload) < 5 {
		return fmt.Errorf("short")
	}
	return proto.Unmarshal(payload[5:], msg)
}

func BenchmarkTCPProtoUnary(b *testing.B) {
	for _, bodySize := range protoSizes() {
		b.Run(sizeLabel(bodySize), func(b *testing.B) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				b.Fatal(err)
			}
			defer ln.Close()

			req := makePayload(bodySize)
			resp := makePayload(bodySize)
			b.SetBytes(int64(proto.Size(req) * 2))

			var wg sync.WaitGroup
			started := make(chan struct{})

			wg.Add(1)
			go func() {
				defer wg.Done()
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				close(started)
				recv := &wrapperspb.BytesValue{}
				for i := 0; i < b.N; i++ {
					if err := readProtoTCP(conn, recv); err != nil {
						return
					}
					if err := writeProtoTCP(conn, 1, resp); err != nil {
						return
					}
				}
			}()

			conn, err := net.Dial("tcp", ln.Addr().String())
			if err != nil {
				b.Fatal(err)
			}
			defer conn.Close()
			<-started
			b.ResetTimer()

			recv := &wrapperspb.BytesValue{}
			for i := 0; i < b.N; i++ {
				if err := writeProtoTCP(conn, 1, req); err != nil {
					b.Fatal(err)
				}
				if err := readProtoTCP(conn, recv); err != nil {
					b.Fatal(err)
				}
			}
			wg.Wait()
		})
	}
}

// ---------------------------------------------------------------------------
// UDS (Unix Domain Socket) benchmarks - Linux baseline for local IPC
// ---------------------------------------------------------------------------

func udsPath(b *testing.B) string {
	dir := b.TempDir()
	return filepath.Join(dir, "bench.sock")
}

func benchUDSRoundtrip(b *testing.B, bodySize int) {
	sock := udsPath(b)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		b.Fatal(err)
	}
	defer ln.Close()
	defer os.Remove(sock)
	req := makePayload(bodySize)
	resp := makePayload(bodySize)
	b.SetBytes(int64(proto.Size(req) * 2))
	var wg sync.WaitGroup
	started := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		close(started)
		recv := &wrapperspb.BytesValue{}
		for i := 0; i < b.N; i++ {
			if err := readProtoTCP(conn, recv); err != nil {
				return
			}
			if err := writeProtoTCP(conn, 1, resp); err != nil {
				return
			}
		}
	}()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		b.Fatal(err)
	}
	defer conn.Close()
	<-started
	b.ResetTimer()
	recv := &wrapperspb.BytesValue{}
	for i := 0; i < b.N; i++ {
		if err := writeProtoTCP(conn, 1, req); err != nil {
			b.Fatal(err)
		}
		if err := readProtoTCP(conn, recv); err != nil {
			b.Fatal(err)
		}
	}
	wg.Wait()
}

func benchUDSStreaming(b *testing.B, msgCount, bodySize int) {
	sock := udsPath(b)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		b.Fatal(err)
	}
	defer ln.Close()
	defer os.Remove(sock)
	req := makePayload(bodySize)
	resp := makePayload(bodySize)
	b.SetBytes(int64(proto.Size(req) * (msgCount + 1)))
	var wg sync.WaitGroup
	started := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		close(started)
		recv := &wrapperspb.BytesValue{}
		for i := 0; i < b.N; i++ {
			if err := readProtoTCP(conn, recv); err != nil {
				return
			}
			for j := 0; j < msgCount; j++ {
				if err := writeProtoTCP(conn, 1, resp); err != nil {
					return
				}
			}
		}
	}()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		b.Fatal(err)
	}
	defer conn.Close()
	<-started
	b.ResetTimer()
	recv := &wrapperspb.BytesValue{}
	for i := 0; i < b.N; i++ {
		if err := writeProtoTCP(conn, 1, req); err != nil {
			b.Fatal(err)
		}
		for j := 0; j < msgCount; j++ {
			if err := readProtoTCP(conn, recv); err != nil {
				b.Fatal(err)
			}
		}
	}
	wg.Wait()
}

func BenchmarkUDSProtoUnary(b *testing.B) {
	for _, size := range protoSizes() {
		b.Run(sizeLabel(size), func(b *testing.B) { benchUDSRoundtrip(b, size) })
	}
}

func BenchmarkUDSProtoStreaming(b *testing.B) {
	for _, size := range []int{64, 256, 1024, 4096, 16384, 64 * 1024, 256 * 1024, 1024 * 1024, 4 * 1024 * 1024, 16 * 1024 * 1024} {
		b.Run(fmt.Sprintf("%s/msgs=100", sizeLabel(size)), func(b *testing.B) { benchUDSStreaming(b, 100, size) })
	}
	for _, size := range []int{64 * 1024 * 1024, 256 * 1024 * 1024} {
		b.Run(fmt.Sprintf("%s/msgs=10", sizeLabel(size)), func(b *testing.B) { benchUDSStreaming(b, 10, size) })
	}
}
