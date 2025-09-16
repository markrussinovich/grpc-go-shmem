//go:build linux && (amd64 || arm64)

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

package shm

import (
    "context"
    "errors"
    "sync"
    "sync/atomic"
)

// ShmUnaryClient is a minimal unary client built over SMF v1 and ShmRings.
// It is intended for step-3 bring-up and tests; it does not wire into the
// grpc-go transport.ClientTransport yet.
type ShmUnaryClient struct {
    seg *Segment
    tx  *ShmRing // client -> server
    rx  *ShmRing // server -> client

    nextID   uint32 // odd stream IDs
    streams  map[uint32]*unaryStream
    streamsM sync.Mutex

    writeMu sync.Mutex // serialize frame writes to preserve ordering

    readerOnce sync.Once
    readerDone chan struct{}
    closed     atomic.Bool
    cancelOnce sync.Once
}

type unaryStream struct {
    hdrCh     chan HeadersV1
    msgCh     chan []byte // delivers exactly one complete gRPC message payload
    trCh      chan TrailersV1
    errCh     chan error
    completed atomic.Bool
}

// NewShmUnaryClient constructs a unary client over an existing segment.
func NewShmUnaryClient(seg *Segment) *ShmUnaryClient {
    c := &ShmUnaryClient{
        seg:        seg,
        tx:         NewShmRingFromSegment(seg.A, seg.Mem),
        rx:         NewShmRingFromSegment(seg.B, seg.Mem),
        nextID:     1,
        streams:    make(map[uint32]*unaryStream),
        readerDone: make(chan struct{}),
    }
    return c
}

// Close closes the client and underlying segment mapping.
func (c *ShmUnaryClient) Close() error {
    if !c.closed.CompareAndSwap(false, true) {
        return nil
    }
    close(c.readerDone)
    return c.seg.Close()
}

// startReader starts the event-driven frame reader once.
func (c *ShmUnaryClient) startReader() {
    c.readerOnce.Do(func() {
        go func() {
            // Single-threaded demux of frames.
            for !c.closed.Load() {
                fh, payload, err := readFrame(c.rx)
                if err != nil {
                    return
                }
                switch fh.Type {
                case FrameTypeHEADERS:
                    hdr, err := decodeHeaders(payload)
                    c.dispatchHeaders(fh.StreamID, hdr, err)
                case FrameTypeMESSAGE:
                    // Deliver raw bytes (includes 5-byte gRPC prefix)
                    c.dispatchMessage(fh.StreamID, payload)
                case FrameTypeTRAILERS:
                    tr, err := decodeTrailers(payload)
                    c.dispatchTrailers(fh.StreamID, tr, err)
                case FrameTypePING:
                    // Immediately reply with PONG
                    c.writeMu.Lock()
                    _ = writeFrame(c.tx, FrameHeader{StreamID: fh.StreamID, Type: FrameTypePONG}, payload)
                    c.writeMu.Unlock()
                case FrameTypeGOAWAY:
                    // Ignore in unary bring-up
                default:
                    // Ignore unknown for now
                }
            }
        }()
    })
}

func (c *ShmUnaryClient) dispatchHeaders(id uint32, h HeadersV1, err error) {
    c.streamsM.Lock()
    s := c.streams[id]
    c.streamsM.Unlock()
    if s == nil {
        return
    }
    if err != nil {
        select { case s.errCh <- err: default: }
        return
    }
    select { case s.hdrCh <- h: default: }
}

func (c *ShmUnaryClient) dispatchMessage(id uint32, p []byte) {
    c.streamsM.Lock()
    s := c.streams[id]
    c.streamsM.Unlock()
    if s == nil { return }
    select { case s.msgCh <- append([]byte(nil), p...): default: }
}

func (c *ShmUnaryClient) dispatchTrailers(id uint32, tr TrailersV1, err error) {
    c.streamsM.Lock()
    s := c.streams[id]
    delete(c.streams, id)
    c.streamsM.Unlock()
    if s == nil { return }
    if err != nil {
        select { case s.errCh <- err: default: }
        return
    }
    s.completed.Store(true)
    select { case s.trCh <- tr: default: }
}

// UnaryCall sends a unary request and waits for the unary response.
// payload must contain the 5-byte gRPC message prefix and message bytes.
func (c *ShmUnaryClient) UnaryCall(ctx context.Context, method, authority string, md []KV, payload []byte) (HeadersV1, []byte, TrailersV1, error) {
    if c.closed.Load() {
        return HeadersV1{}, nil, TrailersV1{}, errors.New("closed")
    }

    c.startReader()

    // Allocate odd stream ID.
    id := atomic.AddUint32(&c.nextID, 2) - 2
    if id == 0 { id = 1 }

    s := &unaryStream{
        hdrCh: make(chan HeadersV1, 1),
        msgCh: make(chan []byte, 1),
        trCh:  make(chan TrailersV1, 1),
        errCh: make(chan error, 1),
    }
    c.streamsM.Lock()
    c.streams[id] = s
    c.streamsM.Unlock()

    // Send HEADERS
    hdr := HeadersV1{Version: 1, HdrType: 0, Method: method, Authority: authority, Metadata: md}
    hbytes := encodeHeaders(hdr)
    c.writeMu.Lock()
    if err := writeFrame(c.tx, FrameHeader{StreamID: id, Type: FrameTypeHEADERS, Flags: HeadersFlagINITIAL}, hbytes); err != nil {
        c.writeMu.Unlock()
        return HeadersV1{}, nil, TrailersV1{}, err
    }
    // Send MESSAGE (single frame for unary)
    if err := writeFrame(c.tx, FrameHeader{StreamID: id, Type: FrameTypeMESSAGE}, payload); err != nil {
        c.writeMu.Unlock()
        return HeadersV1{}, nil, TrailersV1{}, err
    }
    c.writeMu.Unlock()

    // Wait for HEADERS, MESSAGE, TRAILERS or ctx done.
    var rh HeadersV1
    var rm []byte
    var rt TrailersV1
    _, haveMsg, haveTr := false, false, false

    for {
        // If we have both message and trailers, we can return.
        if haveMsg && haveTr {
            return rh, rm, rt, nil
        }
        select {
        case <-ctx.Done():
            // Best-effort CANCEL
            c.writeMu.Lock()
            _ = writeFrame(c.tx, FrameHeader{StreamID: id, Type: FrameTypeCANCEL}, []byte{1})
            c.writeMu.Unlock()
            return HeadersV1{}, nil, TrailersV1{}, ctx.Err()
        case e := <-s.errCh:
            return HeadersV1{}, nil, TrailersV1{}, e
        case h := <-s.hdrCh:
            rh = h
        case m := <-s.msgCh:
            rm = append([]byte(nil), m...)
            haveMsg = true
        case tr := <-s.trCh:
            rt = tr
            haveTr = true
        }
    }
}
