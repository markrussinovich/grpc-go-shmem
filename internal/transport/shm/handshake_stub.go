//go:build !linux || !(amd64 || arm64)

package shm

import (
	"context"
)

// WaitForClient waits for the client to mark itself as ready.
// The server calls this after creating the segment to wait for a client connection.
func (s *Segment) WaitForClient(ctx context.Context) error {
	return ErrUnsupported
}

// WaitForServer waits for the server to mark itself as ready.
// The client calls this after opening a segment to wait for server confirmation.
func (s *Segment) WaitForServer(ctx context.Context) error {
	return ErrUnsupported
}