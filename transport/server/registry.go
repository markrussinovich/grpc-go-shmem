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

// Package server is the exported selection seam for pluggable server-side gRPC
// transports, modeled structurally on the L37 "Go Custom Transports" proposal
// (grpc/proposal#103). A transport registers a Builder under a name; the
// resulting ServerTransport is driven by grpc-go's push model
// (ServerTransport.HandleStreams), matching how grpc.Server drives transports
// today. (L37's pull-model ServeTransport/Accept is the eventual target.)
//
// POC scope: a Builder returns the existing transport.ServerTransport interface,
// so an in-module plugin (e.g. the SHM transport) is reused unchanged. The
// external end-state replaces that with an exported transport/stream contract;
// see plugin/DESIGN.md §4 and §9.
package server

import (
	"net"
	"sync"
)

// A Builder wraps an accepted connection into a server-side transport.
type Builder interface {
	// Build wraps conn (the connection the plugin's listener accepted; for SHM
	// the bootstrap shim carrying the segment) into a ServerTransport, mirroring
	// today's transport.NewServerTransport(conn, config).
	Build(conn net.Conn, opts BuildOptions) (ServerTransport, error)
}

// BuildOptions carries the server-side configuration grpc-go would pass to its
// built-in transport (stats, tap, keepalive, window sizes, credentials, ...).
type BuildOptions struct {
	Config *ServerConfig
}

var (
	mu       sync.RWMutex
	registry = make(map[string]Builder)
)

// Register installs b under name. Register is intended to be called from a
// plugin's init and is safe for concurrent use.
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
