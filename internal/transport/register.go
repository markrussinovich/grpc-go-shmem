//go:build linux

package transport

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"sync/atomic"
	"time"
)

// Instrumentation counters for selection tests.
var (
	shmClientConnectCount atomic.Uint64
	shmServerListenCount  atomic.Uint64
)

// ShmAddress is a parsed shm:// address.
type ShmAddress struct {
	Name string
	Cap  uint64
	// MaxStreams limits concurrent streams per transport.
	// A value of 0 indicates unlimited.
	MaxStreams uint32
}

// ParseAddress parses shm URLs of the form: shm://name?cap=262144&maxstreams=1
func ParseAddress(raw string) (ShmAddress, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return ShmAddress{}, fmt.Errorf("parse shm address: %w", err)
	}
	if u.Scheme != "shm" {
		return ShmAddress{}, fmt.Errorf("unsupported scheme: %s", u.Scheme)
	}
	name := u.Host
	if name == "" {
		// Allow shm://name via path
		name = u.Path
		if len(name) > 0 && name[0] == '/' {
			name = name[1:]
		}
	}
	if name == "" {
		return ShmAddress{}, fmt.Errorf("missing shm name")
	}
	capVal := uint64(DefaultRingASize)
	if c := u.Query().Get("cap"); c != "" {
		v, err := strconv.ParseUint(c, 10, 64)
		if err != nil {
			return ShmAddress{}, fmt.Errorf("invalid cap: %w", err)
		}
		if !IsPowerOfTwo(v) {
			return ShmAddress{}, fmt.Errorf("cap must be power of two: %d", v)
		}
		capVal = uint64(v)
	}

	maxStreamsVal := uint32(math.MaxUint32)
	if ms := u.Query().Get("maxstreams"); ms != "" {
		v, err := strconv.ParseUint(ms, 10, 32)
		if err != nil {
			return ShmAddress{}, fmt.Errorf("invalid maxstreams: %w", err)
		}
		if v == 0 {
			maxStreamsVal = uint32(math.MaxUint32)
		} else {
			maxStreamsVal = uint32(v)
		}
	}

	return ShmAddress{Name: name, Cap: capVal, MaxStreams: maxStreamsVal}, nil
}

// newShmServerFactory creates a server listener for the given shm address.
func newShmServerFactory(raw string) (*ShmListener, error) {
	addr, err := ParseAddress(raw)
	if err != nil {
		return nil, err
	}
	l, err := NewShmListener(&ShmAddr{Name: addr.Name}, DefaultSegmentSize, addr.Cap, addr.Cap)
	if err == nil {
		l.SetMaxStreams(addr.MaxStreams)
		shmServerListenCount.Add(1)
	}
	return l, err
}

// newShmClientFactory dials/attaches to the server segment and returns a ready ClientTransport.
func newShmClientFactory(ctx context.Context, raw string) (ClientTransport, error) {
	addr, err := ParseAddress(raw)
	if err != nil {
		return nil, err
	}

	// Use DialShm which handles the new multi-segment pattern
	opts := &DialOptions{
		SegmentSize:    DefaultSegmentSize,
		RingASize:      addr.Cap,
		RingBSize:      addr.Cap,
		ConnectTimeout: 5 * time.Second,
	}

	ct, err := DialShm(ctx, addr.Name, opts)
	if err == nil {
		shmClientConnectCount.Add(1)
	}
	return ct, err
}
