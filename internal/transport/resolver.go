//go:build linux

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

// Package shm implements a shared memory resolver for gRPC.
// It resolves addresses of the form "shm://segment_name" to shared memory connections.
package transport

import (
	"errors"
	"fmt"

	"google.golang.org/grpc/resolver"
)

const scheme = "shm"

// shmResolverBuilder implements resolver.Builder for shared memory connections.
type shmResolverBuilder struct{}

// Build creates a new shared memory resolver.
// The target should be in the format "shm://segment_name" where segment_name
// is the name of the shared memory segment to connect to.
func (*shmResolverBuilder) Build(target resolver.Target, cc resolver.ClientConn, opts resolver.BuildOptions) (resolver.Resolver, error) {
	if target.Endpoint() == "" {
		return nil, errors.New("shm: received empty target in Build()")
	}

	// The endpoint is the segment name
	segmentName := target.Endpoint()

	// Create a resolver that will resolve to the shared memory address
	r := &shmResolver{
		target:      target,
		cc:          cc,
		segmentName: segmentName,
	}

	// Immediately resolve to the shared memory address
	r.start()
	return r, nil
}

// Scheme returns "shm" as the scheme handled by this resolver.
func (*shmResolverBuilder) Scheme() string {
	return scheme
}

// shmResolver implements resolver.Resolver for shared memory connections.
type shmResolver struct {
	target      resolver.Target
	cc          resolver.ClientConn
	segmentName string
}

// start resolves the target to a shared memory address.
// For shared memory, the "address" is just the segment name,
// which will be used by the dialer to create/open the segment.
func (r *shmResolver) start() {
	// For shared memory, we resolve to an address that uses the segment name
	// The Addr field will be used by our custom dialer
	addr := resolver.Address{
		Addr:       fmt.Sprintf("shm:%s", r.segmentName),
		ServerName: r.segmentName,
	}

	// Update the ClientConn with the resolved address
	r.cc.UpdateState(resolver.State{
		Addresses: []resolver.Address{addr},
	})
}

// ResolveNow is a no-op for shared memory resolver since the address doesn't change.
func (*shmResolver) ResolveNow(resolver.ResolveNowOptions) {}

// Close closes the resolver.
func (*shmResolver) Close() {}

// init registers the shared memory resolver with gRPC.
func init() {
	resolver.Register(&shmResolverBuilder{})
}
