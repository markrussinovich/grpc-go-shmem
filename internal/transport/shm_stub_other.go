//go:build !linux && !windows

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

// SHM transport stubs for platforms that do not build the shared-memory
// transport (anything other than linux / windows). The real
// implementations live alongside the platform-specific transport in
// shm_attributes.go, shm_aware_dialer.go, and shm_fallback.go behind
// `//go:build linux || windows` tags. Without these stubs the
// platform-neutral clientconn.go would fail to compile on Darwin /
// FreeBSD / other Go-supported targets.
//
// All stubs treat SHM as universally unavailable: IsShmEnabled returns
// false (so the RFC-A73 selection path always takes the standard HTTP/2
// branch), NewShmClient returns a non-retryable error, IsFallbackAllowed
// returns true (so clientconn's fallback logic still routes to HTTP/2
// even if some unusual code path called NewShmClient explicitly), and
// the error-classification helpers return false because no real
// ShmError is ever produced on these platforms.

package transport

import (
	"context"
	"errors"
	"time"

	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/resolver"
)

const (
	// DefaultSegmentSize matches the linux/windows SHM default so callers
	// can build portable configs even when SHM is unavailable.
	DefaultSegmentSize = 136 * 1024 * 1024
	// DefaultRingASize matches the linux/windows client-to-server ring size.
	DefaultRingASize = 64 * 1024 * 1024
	// DefaultRingBSize matches the linux/windows server-to-client ring size.
	DefaultRingBSize = 64 * 1024 * 1024
)

// Segment is a placeholder type so non-linux/windows builds can declare
// SHM-related methods (handshake_stub.go defines WaitForClient and
// WaitForServer on it). The real Segment is defined in shm_segment.go
// behind `//go:build linux || windows`. No code on this platform
// instantiates a Segment — anything trying to use it would have
// failed at IsShmEnabled / NewShmClient earlier.
type Segment struct{}

// ShmSecurityHandshaker is a placeholder for the linux/windows security
// handshaker type so portable DialOptions declarations compile.
type ShmSecurityHandshaker struct{}

// DialOptions contains options for dialing a shared memory connection.
// On unsupported platforms it is only a configuration placeholder used
// by the experimental/shm public API.
type DialOptions struct {
	SegmentSize           uint64
	RingASize             uint64
	RingBSize             uint64
	ConnectTimeout        time.Duration
	KeepaliveParams       keepalive.ClientParameters
	Handshaker            *ShmSecurityHandshaker
	SingleStreamMode      bool
	InitialWindowSize     int32
	InitialConnWindowSize int32
}

// DefaultDialOptions returns portable defaults for unsupported platforms.
func DefaultDialOptions() *DialOptions {
	return &DialOptions{
		SegmentSize:    DefaultSegmentSize,
		RingASize:      DefaultRingASize,
		RingBSize:      DefaultRingBSize,
		ConnectTimeout: 30 * time.Second,
	}
}

// unmapMemory is a no-op stub matching the linux/windows signature
// (a package-level function variable assigned in mmap init blocks).
// On platforms without SHM support no init writes to it, so the
// default no-op stands.
var unmapMemory = func(_ []byte) error { return nil }

// shmDebugf is a no-op stub matching the ring.go debug helper.
// SHM is not built on this platform; the http2 transport's
// ServerTransportProvider check still compiles via this stub but
// emits no diagnostics.
func shmDebugf(_ string, _ ...any) {}

// IsShmEnabled reports whether the resolver.Address carries SHM
// capability attributes that should cause clientconn to dial via the
// SHM transport. SHM is not built on this platform, so the answer is
// always false and clientconn takes the HTTP/2 path.
func IsShmEnabled(_ resolver.Address) bool {
	return false
}

// IsFallbackAllowed reports whether the resolver.Address permits a
// fallback to the standard HTTP/2 transport when the SHM dial fails.
// SHM is unavailable on this platform; we always allow fallback so
// any explicit NewShmClient caller still gets a working HTTP/2 dial.
func IsFallbackAllowed(_ resolver.Address) bool {
	return true
}

// NewShmClient is a stub that always returns an error indicating that
// SHM is not supported on this platform. clientconn's RFC-A73 path
// consults IsFallbackAllowed (true above) and dials HTTP/2 instead.
func NewShmClient(_, _ context.Context, _ resolver.Address, _ ConnectOptions, _ OnCloseFunc) (ClientTransport, error) {
	return nil, errors.New("shm: shared-memory transport not supported on this platform; build for linux or windows")
}

// IsShmErrorRetryable is a stub: this platform never produces SHM
// errors, so nothing is retryable.
func IsShmErrorRetryable(_ error) bool {
	return false
}

// IsShmErrorPermanent is a stub: this platform never produces SHM
// errors, so we report false (the caller will pick whatever error
// classification it uses for non-SHM errors).
func IsShmErrorPermanent(_ error) bool {
	return false
}
