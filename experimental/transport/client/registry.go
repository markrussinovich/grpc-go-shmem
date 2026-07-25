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

package client

import "sync"

var (
	registryMu sync.RWMutex
	registry   = make(map[string]Builder)
)

// Register registers a client transport Builder under name. name is matched
// against resolver.Address.TransportType during connection establishment.
//
// Register is expected to be called from a plugin's init(); it panics on a
// duplicate name or an empty name to surface wiring mistakes at startup.
func Register(name string, b Builder) {
	if name == "" {
		panic("grpc/experimental/transport/client: Register called with empty name")
	}
	if b == nil {
		panic("grpc/experimental/transport/client: Register called with nil Builder for " + name)
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := registry[name]; dup {
		panic("grpc/experimental/transport/client: Register called twice for " + name)
	}
	registry[name] = b
}

// Get returns the Builder registered under name, or nil if none is registered.
//
// Selection is fail-closed: a non-empty resolver.Address.TransportType that is
// not registered here is a hard connection error rather than a silent fallback
// to the default HTTP/2 transport, so an explicit transport selector can never
// silently change the protocol used for a connection.
func Get(name string) Builder {
	registryMu.RLock()
	defer registryMu.RUnlock()
	return registry[name]
}
