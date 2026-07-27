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

package server

import "sync"

var (
	registryMu sync.RWMutex
	registry   = make(map[string]Builder)
)

// Register registers a server transport Builder under name. name identifies the
// transport type a plugin's listener tags accepted connections with.
//
// Register is expected to be called from a plugin's init(); it panics on a
// duplicate or empty name, or a nil Builder, to surface wiring mistakes at
// startup.
func Register(name string, b Builder) {
	if name == "" {
		panic("grpc/experimental/transport/server: Register called with empty name")
	}
	if b == nil {
		panic("grpc/experimental/transport/server: Register called with nil Builder for " + name)
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := registry[name]; dup {
		panic("grpc/experimental/transport/server: Register called twice for " + name)
	}
	registry[name] = b
}

// Get returns the Builder registered under name, or nil if none is registered.
func Get(name string) Builder {
	registryMu.RLock()
	defer registryMu.RUnlock()
	return registry[name]
}
