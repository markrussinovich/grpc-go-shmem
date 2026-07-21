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

// Package client is the exported selection seam for pluggable client-side gRPC
// transports, modeled structurally on the L37 "Go Custom Transports" proposal
// (grpc/proposal#103): a transport registers a Builder under a name, and grpc-go
// selects it by resolver.Address.TransportType instead of a hardcoded branch.
//
// POC scope: a Builder returns the existing transport.ClientTransport interface,
// so an in-module plugin (e.g. the SHM transport) is reused unchanged. The
// external end-state replaces that with an exported transport/stream contract so
// the plugin needs no internal/ dependency; see plugin/DESIGN.md §4 and §9.
package client

import (
	"context"
	"sync"

	"google.golang.org/grpc/resolver"
)

// A Builder builds a client-side transport connected to a single address.
type Builder interface {
	// Build connects to addr and returns a ready ClientTransport. connectCtx
	// carries the per-attempt connect deadline; ctx is the long-lived ClientConn
	// context that bounds the transport's lifetime (mirroring grpc-go's built-in
	// transport constructors). An error is propagated to the connection-failure
	// path like any transport dial error; there is no automatic fallback to the
	// HTTP/2 transport once an address has selected a registered Builder.
	Build(connectCtx, ctx context.Context, addr resolver.Address, opts BuildOptions) (ClientTransport, error)
}

// BuildOptions carries the inputs a transport needs at dial time. It mirrors the
// subset of ConnectOptions a custom transport consumes.
type BuildOptions struct {
	// ConnectOptions are the per-connection options grpc-go would pass to its
	// built-in transport (credentials, keepalive, window sizes, stats, tap, ...).
	ConnectOptions ConnectOptions
	// OnClose is invoked when the transport terminates (GOAWAY / drain).
	OnClose OnCloseFunc
}

var (
	mu       sync.RWMutex
	registry = make(map[string]Builder)
)

// Register installs b under name. name is matched against
// resolver.Address.TransportType during transport selection. Register is
// intended to be called from a plugin's init and is safe for concurrent use.
func Register(name string, b Builder) {
	mu.Lock()
	defer mu.Unlock()
	registry[name] = b
}

// Get returns the Builder registered under name, or nil if none is registered.
func Get(name string) Builder {
	mu.RLock()
	defer mu.RUnlock()
	return registry[name]
}
