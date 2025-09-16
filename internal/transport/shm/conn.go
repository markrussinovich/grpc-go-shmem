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
	"errors"
	"sync/atomic"
)

var (
	ErrConnectionClosed = errors.New("connection closed")
)

// ShmConn models a duplex byte pipe backed by two rings.
// Server: read from ring A (client->server), write to ring B (server->client)
// Client: read from ring B (server->client), write to ring A (client->server)
type ShmConn struct {
	seg      *Segment
	readR    *ShmRing
	writeR   *ShmRing
	readView *ringView  // For accessing close/increment methods
	writeView *ringView // For accessing close/increment methods
	closed   atomic.Bool
	isServer bool // true if this is the server side
}

// NewServerConn creates a new server-side connection
func NewServerConn(seg *Segment) *ShmConn {
	return &ShmConn{
		seg:       seg,
		readR:     NewShmRingFromSegment(seg.A, seg.Mem), // Server reads from A (client->server)
		writeR:    NewShmRingFromSegment(seg.B, seg.Mem), // Server writes to B (server->client)
		readView:  seg.A,
		writeView: seg.B,
		isServer:  true,
	}
}

// NewClientConn creates a new client-side connection
func NewClientConn(seg *Segment) *ShmConn {
	return &ShmConn{
		seg:       seg,
		readR:     NewShmRingFromSegment(seg.B, seg.Mem), // Client reads from B (server->client)
		writeR:    NewShmRingFromSegment(seg.A, seg.Mem), // Client writes to A (client->server)
		readView:  seg.B,
		writeView: seg.A,
		isServer:  false,
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

// Write writes data to the connection
func (c *ShmConn) Write(p []byte) (int, error) {
	if c.closed.Load() {
		return 0, ErrConnectionClosed
	}
	
	err := c.writeR.WriteBlocking(p)
	if err != nil {
		// Check if the connection was closed while we were waiting
		if c.closed.Load() {
			return 0, ErrConnectionClosed
		}
		return 0, err
	}
	
	return len(p), nil
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
	
	// If this is the server (segment creator), it owns the segment and cleans up
	if c.isServer {
		return c.seg.Close()
	}
	
	// Client only unmaps, doesn't unlink the file
	return c.seg.Close()
}
