//go:build !windows

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
	"time"
)

// RingEvents is a no-op on Linux where futex works natively across mappings.
type RingEvents struct{}

// CreateRingEvents returns nil on Linux - futex is used directly.
func CreateRingEvents(_ string, _ string) (*RingEvents, error) {
	return nil, nil
}

// OpenRingEvents returns nil on Linux - futex is used directly.
func OpenRingEvents(_ string, _ string) (*RingEvents, error) {
	return nil, nil
}

// Close is a no-op on Linux.
func (e *RingEvents) Close() error {
	return nil
}

// SignalData is a no-op on Linux - futex wake is used directly.
func (e *RingEvents) SignalData() {}

// SignalSpace is a no-op on Linux - futex wake is used directly.
func (e *RingEvents) SignalSpace() {}

// SignalContig is a no-op on Linux - futex wake is used directly.
func (e *RingEvents) SignalContig() {}

// WaitData is not used on Linux - futex wait is used directly.
func (e *RingEvents) WaitData(_ *uint32, _ uint32, _ time.Duration) error {
	return nil
}

// WaitSpace is not used on Linux - futex wait is used directly.
func (e *RingEvents) WaitSpace(_ *uint32, _ uint32, _ time.Duration) error {
	return nil
}

// WaitContig is not used on Linux - futex wait is used directly.
func (e *RingEvents) WaitContig(_ *uint32, _ uint32, _ time.Duration) error {
	return nil
}

// GetRingEvents returns nil on Linux - events are not used.
func GetRingEvents(_ string, _ string) *RingEvents {
	return nil
}

// RingEventHandle is a no-op on Linux.
type RingEventHandle struct{}

// NewRingEventHandle returns an empty handle on Linux.
func NewRingEventHandle(_ *RingEvents) RingEventHandle {
	return RingEventHandle{}
}

// Events returns nil on Linux.
func (h RingEventHandle) Events() *RingEvents {
	return nil
}

// HandshakeEvents is a no-op on Linux where futex works natively.
type HandshakeEvents struct{}

// CreateHandshakeEvents returns nil on Linux - futex is used directly.
func CreateHandshakeEvents(_ string) (*HandshakeEvents, error) {
	return nil, nil
}

// OpenHandshakeEvents returns nil on Linux - futex is used directly.
func OpenHandshakeEvents(_ string) (*HandshakeEvents, error) {
	return nil, nil
}

// SignalClientReady is a no-op on Linux - futex wake is used directly.
func SignalClientReady(_ string) {}

// SignalServerReady is a no-op on Linux - futex wake is used directly.
func SignalServerReady(_ string) {}

// WaitClientReady is a no-op on Linux - futex wait is used directly.
func WaitClientReady(_ context.Context, _ string) error {
	return nil
}

// WaitServerReady is a no-op on Linux - futex wait is used directly.
func WaitServerReady(_ context.Context, _ string) error {
	return nil
}

// CloseHandshakeEvents is a no-op on Linux.
func CloseHandshakeEvents(_ string) {}
