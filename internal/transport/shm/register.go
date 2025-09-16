//go:build linux && (amd64 || arm64)

package shm

import (
    "context"
    "fmt"
    "net/url"
    "strconv"
    "sync/atomic"

    "google.golang.org/grpc/internal/transport"
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
}

// ParseAddress parses shm URLs of the form: shm://name?cap=262144
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
    return ShmAddress{Name: name, Cap: capVal}, nil
}

// newShmServerFactory creates a server listener for the given shm address.
func newShmServerFactory(raw string) (*ShmListener, error) {
    addr, err := ParseAddress(raw)
    if err != nil {
        return nil, err
    }
    l, err := NewShmListener(&ShmAddr{Name: addr.Name}, DefaultSegmentSize, addr.Cap, addr.Cap)
    if err == nil {
        shmServerListenCount.Add(1)
    }
    return l, err
}

// newShmClientFactory dials/attaches to the server segment and returns a ready ClientTransport.
func newShmClientFactory(ctx context.Context, raw string) (transport.ClientTransport, error) {
    addr, err := ParseAddress(raw)
    if err != nil {
        return nil, err
    }
    // Open existing server segment and handshake.
    seg, err := OpenSegment(addr.Name)
    if err != nil {
        return nil, fmt.Errorf("open segment: %w", err)
    }
    // Signal client attached and wait for server ready (should already be set by listener).
    seg.H.SetClientReady(true)
    if err := seg.WaitForServer(ctx); err != nil {
        seg.Close()
        return nil, fmt.Errorf("wait server: %w", err)
    }
    ct, err := NewShmClientTransport(seg, &ShmAddr{Name: addr.Name + "_client"}, &ShmAddr{Name: addr.Name})
    if err == nil {
        shmClientConnectCount.Add(1)
    }
    return ct, err
}
