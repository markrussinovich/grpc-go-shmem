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
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// ErrConnectionClosed is returned when operations are attempted on a closed connection.
var ErrConnectionClosed = errors.New("connection closed")

// ShmConn models a duplex byte pipe backed by two rings.
// Server: read from ring A (client->server), write to ring B (server->client)
// Client: read from ring B (server->client), write to ring A (client->server)
type ShmConn struct {
	seg         *Segment
	readR       *ShmRing
	writeR      *ShmRing
	readView    *ringView // For accessing close/increment methods
	writeView   *ringView // For accessing close/increment methods
	readEvents  *RingEvents
	writeEvents *RingEvents
	closed      atomic.Bool
	isServer    bool   // true if this is the server side
	segmentName string // segment name for event naming
}

// extractSegmentName extracts the segment name from the file path.
// e.g., "/dev/shm/grpc_shm_foo" or "C:\Temp\grpc_shm_foo" -> "foo"
func extractSegmentName(path string) string {
	base := filepath.Base(path)
	const prefix = "grpc_shm_"
	if strings.HasPrefix(base, prefix) {
		return base[len(prefix):]
	}
	return base
}

// NewServerConn creates a new server-side connection
func NewServerConn(seg *Segment) *ShmConn {
	segmentName := extractSegmentName(seg.Path)

	readR := NewShmRingFromSegment(seg.A, seg.Mem)
	writeR := NewShmRingFromSegment(seg.B, seg.Mem)
	readR.SetSegmentID(seg.Path)
	writeR.SetSegmentID(seg.Path)
	seg.RegisterRing(readR)
	seg.RegisterRing(writeR)

	// Create events for cross-mapping synchronization (Windows).
	// Server creates events. On Linux, these are no-ops.
	readEvents, _ := CreateRingEvents(segmentName, "A")
	writeEvents, _ := CreateRingEvents(segmentName, "B")

	// Attach events to rings
	readR.SetEvents(readEvents)
	writeR.SetEvents(writeEvents)

	return &ShmConn{
		seg:         seg,
		readR:       readR,
		writeR:      writeR,
		readView:    seg.A,
		writeView:   seg.B,
		readEvents:  readEvents,
		writeEvents: writeEvents,
		isServer:    true,
		segmentName: segmentName,
	}
}

// NewClientConn creates a new client-side connection
func NewClientConn(seg *Segment) *ShmConn {
	segmentName := extractSegmentName(seg.Path)

	readR := NewShmRingFromSegment(seg.B, seg.Mem)
	writeR := NewShmRingFromSegment(seg.A, seg.Mem)
	readR.SetSegmentID(seg.Path)
	writeR.SetSegmentID(seg.Path)
	seg.RegisterRing(readR)
	seg.RegisterRing(writeR)

	// Open events for cross-mapping synchronization (Windows).
	// Client opens existing events created by server.
	// Note: Client reads from B, writes to A (opposite of server).
	readEvents, _ := OpenRingEvents(segmentName, "B")
	writeEvents, _ := OpenRingEvents(segmentName, "A")

	// Attach events to rings
	readR.SetEvents(readEvents)
	writeR.SetEvents(writeEvents)

	return &ShmConn{
		seg:         seg,
		readR:       readR,
		writeR:      writeR,
		readView:    seg.B,
		writeView:   seg.A,
		readEvents:  readEvents,
		writeEvents: writeEvents,
		isServer:    false,
		segmentName: segmentName,
	}
}

// Read reads data from the connection
func (c *ShmConn) Read(p []byte) (int, error) {
	if c.closed.Load() {
		return 0, ErrConnectionClosed
	}

	n, err := c.readR.ReadBlocking(p)
	if err != nil {
		// Check if the connection was closed while we were waiting
		if c.closed.Load() {
			return 0, ErrConnectionClosed
		}
		return n, err
	}

	return n, nil
}

// ReadContext reads data from the connection with context timeout support
func (c *ShmConn) ReadContext(ctx context.Context, p []byte) (int, error) {
	if c.closed.Load() {
		return 0, ErrConnectionClosed
	}

	n, err := c.readR.ReadBlockingContext(ctx, p)
	if err != nil {
		// Check if the connection was closed while we were waiting
		if c.closed.Load() {
			return 0, ErrConnectionClosed
		}
		return n, err
	}

	return n, nil
}

// Write writes data to the connection
func (c *ShmConn) Write(p []byte) (int, error) {
	if c.closed.Load() {
		return 0, ErrConnectionClosed
	}

	// Handle large writes by chunking them to fit within ring capacity
	totalWritten := 0
	ringCapacity := int(c.writeR.Capacity())

	for totalWritten < len(p) {
		// Determine chunk size (remaining data or ring capacity, whichever is smaller)
		remaining := len(p) - totalWritten
		chunkSize := remaining
		if chunkSize > ringCapacity {
			chunkSize = ringCapacity
		}

		// Write this chunk
		chunk := p[totalWritten : totalWritten+chunkSize]
		err := c.writeR.WriteBlocking(chunk)
		if err != nil {
			// Check if the connection was closed while we were waiting
			if c.closed.Load() {
				return totalWritten, ErrConnectionClosed
			}
			return totalWritten, err
		}

		totalWritten += chunkSize
	}

	return totalWritten, nil
}

// WriteContext writes data to the connection with context timeout support
func (c *ShmConn) WriteContext(ctx context.Context, p []byte) (int, error) {
	if c.closed.Load() {
		return 0, ErrConnectionClosed
	}

	// Handle large writes by chunking them to fit within ring capacity
	totalWritten := 0
	ringCapacity := int(c.writeR.Capacity())

	for totalWritten < len(p) {
		// Determine chunk size (remaining data or ring capacity, whichever is smaller)
		remaining := len(p) - totalWritten
		chunkSize := remaining
		if chunkSize > ringCapacity {
			chunkSize = ringCapacity
		}

		// Write this chunk
		chunk := p[totalWritten : totalWritten+chunkSize]
		err := c.writeR.WriteBlockingContext(ctx, chunk)
		if err != nil {
			// Check if the connection was closed while we were waiting
			if c.closed.Load() {
				return totalWritten, ErrConnectionClosed
			}
			return totalWritten, err
		}

		totalWritten += chunkSize
	}

	return totalWritten, nil
}

// Close closes the connection
func (c *ShmConn) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil // Already closed
	}

	// Set both rings as closed to notify the other side
	c.readR.Close()
	c.writeR.Close()

	// Set the segment header closed flag
	c.seg.H.SetClosed(true)

	// Increment data sequence numbers and wake any waiters
	c.readView.IncrementDataSequence()
	c.writeView.IncrementDataSequence()

	// Close the named events (Windows)
	if c.readEvents != nil {
		c.readEvents.Close()
	}
	if c.writeEvents != nil {
		c.writeEvents.Close()
	}

	// If this is the server (segment creator), it owns the segment and cleans up
	if c.isServer {
		return c.seg.Close()
	}

	// Client only unmaps, doesn't unlink the file
	return c.seg.Close()
}
