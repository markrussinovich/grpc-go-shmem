//go:build windows

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
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// RingEvents holds the Windows event handles for a ring buffer.
// These named events enable cross-mapping synchronization since
// Windows WaitOnAddress/WakeByAddress only work within the same
// virtual address mapping.
type RingEvents struct {
	// dataEvent is signaled when new data is available (writer -> reader)
	dataEvent windows.Handle
	// spaceEvent is signaled when space becomes available (reader -> writer)
	spaceEvent windows.Handle
	// contigEvent is signaled when contiguous space improves
	contigEvent windows.Handle

	// eventName prefix for cleanup
	namePrefix string
}

// Global registry of ring events by segment+ring name.
// This allows the ring to look up its events without storing handles
// in shared memory (which wouldn't work across processes).
var (
	ringEventsRegistry = make(map[string]*RingEvents)
	ringEventsMu       sync.RWMutex
)

// eventNamePrefix returns the naming prefix for events.
// Format: "Local\grpc_shm_<segment>_<ring>"
// Using "Local\" namespace for same-session IPC (faster than "Global\").
func eventNamePrefix(segmentName string, ringID string) string {
	return fmt.Sprintf("Local\\grpc_shm_%s_%s", segmentName, ringID)
}

// CreateRingEvents creates the named events for a ring.
// Called by the segment creator (server side).
// Uses openOrCreate for idempotency - handles case where client already created them.
func CreateRingEvents(segmentName string, ringID string) (*RingEvents, error) {
	prefix := eventNamePrefix(segmentName, ringID)
	shmDebugf("[DEBUG] CreateRingEvents: segment=%s ring=%s prefix=%s", segmentName, ringID, prefix)

	// Check if already in registry (another goroutine might have created them)
	ringEventsMu.RLock()
	if events, ok := ringEventsRegistry[prefix]; ok {
		ringEventsMu.RUnlock()
		shmDebugf("[DEBUG] CreateRingEvents: found existing events in registry")
		return events, nil
	}
	ringEventsMu.RUnlock()

	// Create or open events - handles race where client created them first
	dataEvent, err := openOrCreateNamedEvent(prefix + "_data")
	if err != nil {
		shmDebugf("[DEBUG] CreateRingEvents: failed to create/open data event: %v", err)
		return nil, fmt.Errorf("create data event: %w", err)
	}

	spaceEvent, err := openOrCreateNamedEvent(prefix + "_space")
	if err != nil {
		windows.CloseHandle(dataEvent)
		return nil, fmt.Errorf("create space event: %w", err)
	}

	contigEvent, err := openOrCreateNamedEvent(prefix + "_contig")
	if err != nil {
		windows.CloseHandle(dataEvent)
		windows.CloseHandle(spaceEvent)
		return nil, fmt.Errorf("create contig event: %w", err)
	}

	shmDebugf("[DEBUG] CreateRingEvents: created/opened events data=%v space=%v contig=%v", dataEvent, spaceEvent, contigEvent)

	events := &RingEvents{
		dataEvent:   dataEvent,
		spaceEvent:  spaceEvent,
		contigEvent: contigEvent,
		namePrefix:  prefix,
	}

	// Register in global registry
	ringEventsMu.Lock()
	ringEventsRegistry[prefix] = events
	ringEventsMu.Unlock()

	return events, nil
}

// OpenRingEvents opens existing named events for a ring.
// Called by the segment opener (client side).
// If events don't exist yet, creates them (handles race conditions).
func OpenRingEvents(segmentName string, ringID string) (*RingEvents, error) {
	prefix := eventNamePrefix(segmentName, ringID)
	shmDebugf("[DEBUG] OpenRingEvents: segment=%s ring=%s prefix=%s", segmentName, ringID, prefix)

	// Check if already opened in this process
	ringEventsMu.RLock()
	if events, ok := ringEventsRegistry[prefix]; ok {
		ringEventsMu.RUnlock()
		shmDebugf("[DEBUG] OpenRingEvents: found existing events in registry")
		return events, nil
	}
	ringEventsMu.RUnlock()

	// Open or create events (handles race condition where client starts before server)
	dataEvent, err := openOrCreateNamedEvent(prefix + "_data")
	if err != nil {
		shmDebugf("[DEBUG] OpenRingEvents: failed to open/create data event: %v", err)
		return nil, fmt.Errorf("open data event: %w", err)
	}

	spaceEvent, err := openOrCreateNamedEvent(prefix + "_space")
	if err != nil {
		windows.CloseHandle(dataEvent)
		return nil, fmt.Errorf("open space event: %w", err)
	}

	contigEvent, err := openOrCreateNamedEvent(prefix + "_contig")
	if err != nil {
		windows.CloseHandle(dataEvent)
		windows.CloseHandle(spaceEvent)
		return nil, fmt.Errorf("open contig event: %w", err)
	}

	shmDebugf("[DEBUG] OpenRingEvents: opened/created events data=%v space=%v contig=%v", dataEvent, spaceEvent, contigEvent)

	events := &RingEvents{
		dataEvent:   dataEvent,
		spaceEvent:  spaceEvent,
		contigEvent: contigEvent,
		namePrefix:  prefix,
	}

	// Register in global registry
	ringEventsMu.Lock()
	ringEventsRegistry[prefix] = events
	ringEventsMu.Unlock()

	return events, nil
}

// Close closes all event handles and removes from registry.
func (e *RingEvents) Close() error {
	ringEventsMu.Lock()
	delete(ringEventsRegistry, e.namePrefix)
	ringEventsMu.Unlock()

	var firstErr error
	if e.dataEvent != 0 {
		if err := windows.CloseHandle(e.dataEvent); err != nil && firstErr == nil {
			firstErr = err
		}
		e.dataEvent = 0
	}
	if e.spaceEvent != 0 {
		if err := windows.CloseHandle(e.spaceEvent); err != nil && firstErr == nil {
			firstErr = err
		}
		e.spaceEvent = 0
	}
	if e.contigEvent != 0 {
		if err := windows.CloseHandle(e.contigEvent); err != nil && firstErr == nil {
			firstErr = err
		}
		e.contigEvent = 0
	}
	return firstErr
}

// SignalData signals that new data is available in the ring.
func (e *RingEvents) SignalData() {
	if e.dataEvent != 0 {
		shmDebugf("[DEBUG] SignalData: setting event=%v", e.dataEvent)
		windows.SetEvent(e.dataEvent)
	}
}

// SignalSpace signals that space has become available in the ring.
func (e *RingEvents) SignalSpace() {
	if e.spaceEvent != 0 {
		windows.SetEvent(e.spaceEvent)
	}
}

// SignalContig signals that contiguous space has improved.
func (e *RingEvents) SignalContig() {
	if e.contigEvent != 0 {
		windows.SetEvent(e.contigEvent)
	}
}

// WaitData waits for the data event or value change.
// Returns nil if val changed, ErrFutexTimeout on timeout.
func (e *RingEvents) WaitData(addr *uint32, val uint32, timeout time.Duration) error {
	return waitOnEventWithValue(e.dataEvent, addr, val, timeout)
}

// WaitSpace waits for the space event or value change.
func (e *RingEvents) WaitSpace(addr *uint32, val uint32, timeout time.Duration) error {
	return waitOnEventWithValue(e.spaceEvent, addr, val, timeout)
}

// WaitContig waits for the contig event or value change.
func (e *RingEvents) WaitContig(addr *uint32, val uint32, timeout time.Duration) error {
	return waitOnEventWithValue(e.contigEvent, addr, val, timeout)
}

// waitOnEventWithValue waits on an event handle while checking if the atomic value changed.
// This combines the Windows event wait with the futex-like value check semantics.
func waitOnEventWithValue(event windows.Handle, addr *uint32, expectedVal uint32, timeout time.Duration) error {
	// Fast-path: check if value already changed
	if atomic.LoadUint32(addr) != expectedVal {
		return nil
	}

	// Calculate timeout in milliseconds
	var timeoutMs uint32 = windows.INFINITE
	if timeout > 0 {
		ms := timeout.Milliseconds()
		if ms > int64(windows.INFINITE-1) {
			timeoutMs = windows.INFINITE - 1
		} else if ms > 0 {
			timeoutMs = uint32(ms)
		} else {
			timeoutMs = 1 // At least 1ms
		}
	}

	// Wait on the event
	ret, err := windows.WaitForSingleObject(event, timeoutMs)
	if err != nil {
		return fmt.Errorf("WaitForSingleObject: %w", err)
	}

	// WAIT_TIMEOUT = 0x00000102 = 258
	const waitTimeout = 0x00000102

	switch ret {
	case windows.WAIT_OBJECT_0:
		// Event was signaled - check if value actually changed
		if atomic.LoadUint32(addr) != expectedVal {
			return nil
		}
		// Spurious wake (value didn't change) - caller should retry
		return nil
	case waitTimeout:
		// Check value one more time before returning timeout
		if atomic.LoadUint32(addr) != expectedVal {
			return nil
		}
		return ErrFutexTimeout
	case windows.WAIT_ABANDONED:
		return fmt.Errorf("event abandoned")
	default:
		return fmt.Errorf("unexpected wait result: %d", ret)
	}
}

// createNamedEvent creates a new named auto-reset event.
func createNamedEvent(name string) (windows.Handle, error) {
	namePtr, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return 0, err
	}

	// CreateEventW with:
	// - lpEventAttributes: nil (default security)
	// - bManualReset: 0 (FALSE = auto-reset)
	// - bInitialState: 0 (FALSE = non-signaled)
	// - lpName: event name
	handle, err := windows.CreateEvent(nil, 0, 0, namePtr)
	if err != nil {
		return 0, err
	}
	return handle, nil
}

// openNamedEvent opens an existing named event.
func openNamedEvent(name string) (windows.Handle, error) {
	namePtr, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return 0, err
	}

	// OpenEvent with SYNCHRONIZE | EVENT_MODIFY_STATE access
	const eventModifyState = 0x0002
	handle, err := windows.OpenEvent(windows.SYNCHRONIZE|eventModifyState, false, namePtr)
	if err != nil {
		return 0, err
	}
	return handle, nil
}

// openOrCreateNamedEvent tries to open an existing event, or creates it if it doesn't exist.
// This allows either side (server or client) to be the first to create the events.
func openOrCreateNamedEvent(name string) (windows.Handle, error) {
	// Try to open first
	handle, err := openNamedEvent(name)
	if err == nil {
		return handle, nil
	}

	// If open failed, try to create
	handle, err = createNamedEvent(name)
	if err == nil {
		return handle, nil
	}

	// If create failed, maybe it was created between our open and create attempts
	// Try to open one more time
	handle, err = openNamedEvent(name)
	if err == nil {
		return handle, nil
	}

	return 0, fmt.Errorf("failed to open or create event %s: %w", name, err)
}

// GetRingEvents retrieves events from the registry.
func GetRingEvents(segmentName string, ringID string) *RingEvents {
	prefix := eventNamePrefix(segmentName, ringID)
	ringEventsMu.RLock()
	events := ringEventsRegistry[prefix]
	ringEventsMu.RUnlock()
	return events
}

// RingEventHandle is an opaque type representing event handles.
// Used to pass events to ShmRing without exposing Windows types.
type RingEventHandle struct {
	ptr unsafe.Pointer
}

// NewRingEventHandle wraps RingEvents for passing to ShmRing.
func NewRingEventHandle(events *RingEvents) RingEventHandle {
	return RingEventHandle{ptr: unsafe.Pointer(events)}
}

// Events returns the underlying RingEvents.
func (h RingEventHandle) Events() *RingEvents {
	if h.ptr == nil {
		return nil
	}
	return (*RingEvents)(h.ptr)
}

// HandshakeEvents holds the Windows event handles for segment handshake.
type HandshakeEvents struct {
	clientReadyEvent windows.Handle
	serverReadyEvent windows.Handle
	segmentName      string
}

// Global registry of handshake events by segment name.
var (
	handshakeEventsRegistry = make(map[string]*HandshakeEvents)
	handshakeEventsMu       sync.RWMutex
)

// CreateHandshakeEvents creates the named events for segment handshake.
// Called by the segment creator (server side).
func CreateHandshakeEvents(segmentName string) (*HandshakeEvents, error) {
	handshakeEventsMu.Lock()
	defer handshakeEventsMu.Unlock()

	// Check if already exists
	if events, ok := handshakeEventsRegistry[segmentName]; ok {
		return events, nil
	}

	clientEventName := fmt.Sprintf("Local\\grpc_shm_%s_clientReady", segmentName)
	serverEventName := fmt.Sprintf("Local\\grpc_shm_%s_serverReady", segmentName)

	clientEvent, err := openOrCreateNamedEvent(clientEventName)
	if err != nil {
		return nil, fmt.Errorf("create client ready event: %w", err)
	}

	serverEvent, err := openOrCreateNamedEvent(serverEventName)
	if err != nil {
		windows.CloseHandle(clientEvent)
		return nil, fmt.Errorf("create server ready event: %w", err)
	}

	events := &HandshakeEvents{
		clientReadyEvent: clientEvent,
		serverReadyEvent: serverEvent,
		segmentName:      segmentName,
	}

	handshakeEventsRegistry[segmentName] = events
	return events, nil
}

// OpenHandshakeEvents opens existing named events for segment handshake.
// Called by the segment opener (client side).
func OpenHandshakeEvents(segmentName string) (*HandshakeEvents, error) {
	handshakeEventsMu.Lock()
	defer handshakeEventsMu.Unlock()

	// Check if already exists in registry
	if events, ok := handshakeEventsRegistry[segmentName]; ok {
		return events, nil
	}

	clientEventName := fmt.Sprintf("Local\\grpc_shm_%s_clientReady", segmentName)
	serverEventName := fmt.Sprintf("Local\\grpc_shm_%s_serverReady", segmentName)

	clientEvent, err := openOrCreateNamedEvent(clientEventName)
	if err != nil {
		return nil, fmt.Errorf("open client ready event: %w", err)
	}

	serverEvent, err := openOrCreateNamedEvent(serverEventName)
	if err != nil {
		windows.CloseHandle(clientEvent)
		return nil, fmt.Errorf("open server ready event: %w", err)
	}

	events := &HandshakeEvents{
		clientReadyEvent: clientEvent,
		serverReadyEvent: serverEvent,
		segmentName:      segmentName,
	}

	handshakeEventsRegistry[segmentName] = events
	return events, nil
}

// SignalClientReady signals that the client is ready.
func SignalClientReady(segmentName string) {
	handshakeEventsMu.RLock()
	events, ok := handshakeEventsRegistry[segmentName]
	handshakeEventsMu.RUnlock()
	if ok && events != nil && events.clientReadyEvent != 0 {
		windows.SetEvent(events.clientReadyEvent)
	}
}

// SignalServerReady signals that the server is ready.
func SignalServerReady(segmentName string) {
	handshakeEventsMu.RLock()
	events, ok := handshakeEventsRegistry[segmentName]
	handshakeEventsMu.RUnlock()
	if ok && events != nil && events.serverReadyEvent != 0 {
		windows.SetEvent(events.serverReadyEvent)
	}
}

// WaitClientReady waits for the client ready signal with optional timeout.
func WaitClientReady(ctx context.Context, segmentName string) error {
	handshakeEventsMu.RLock()
	events, ok := handshakeEventsRegistry[segmentName]
	handshakeEventsMu.RUnlock()
	if !ok || events == nil {
		return fmt.Errorf("no handshake events for segment %s", segmentName)
	}

	// Calculate timeout
	var timeout uint32 = windows.INFINITE
	if dl, ok := ctx.Deadline(); ok {
		remaining := time.Until(dl)
		if remaining <= 0 {
			return context.DeadlineExceeded
		}
		timeout = uint32(remaining.Milliseconds())
	}

	result, err := windows.WaitForSingleObject(events.clientReadyEvent, timeout)
	if err != nil {
		return err
	}
	switch result {
	case windows.WAIT_OBJECT_0:
		return nil
	case uint32(windows.WAIT_TIMEOUT):
		return context.DeadlineExceeded
	default:
		return fmt.Errorf("WaitForSingleObject returned %d", result)
	}
}

// WaitServerReady waits for the server ready signal with optional timeout.
func WaitServerReady(ctx context.Context, segmentName string) error {
	handshakeEventsMu.RLock()
	events, ok := handshakeEventsRegistry[segmentName]
	handshakeEventsMu.RUnlock()
	if !ok || events == nil {
		return fmt.Errorf("no handshake events for segment %s", segmentName)
	}

	// Calculate timeout
	var timeout uint32 = windows.INFINITE
	if dl, ok := ctx.Deadline(); ok {
		remaining := time.Until(dl)
		if remaining <= 0 {
			return context.DeadlineExceeded
		}
		timeout = uint32(remaining.Milliseconds())
	}

	result, err := windows.WaitForSingleObject(events.serverReadyEvent, timeout)
	if err != nil {
		return err
	}
	switch result {
	case windows.WAIT_OBJECT_0:
		return nil
	case uint32(windows.WAIT_TIMEOUT):
		return context.DeadlineExceeded
	default:
		return fmt.Errorf("WaitForSingleObject returned %d", result)
	}
}

// CloseHandshakeEvents closes and removes handshake events from registry.
func CloseHandshakeEvents(segmentName string) {
	handshakeEventsMu.Lock()
	events, ok := handshakeEventsRegistry[segmentName]
	if ok {
		delete(handshakeEventsRegistry, segmentName)
	}
	handshakeEventsMu.Unlock()

	if events != nil {
		if events.clientReadyEvent != 0 {
			windows.CloseHandle(events.clientReadyEvent)
		}
		if events.serverReadyEvent != 0 {
			windows.CloseHandle(events.serverReadyEvent)
		}
	}
}
