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

package shm

import (
	"testing"

	"google.golang.org/grpc/internal/transport"
	transportclient "google.golang.org/grpc/transport/client"
	transportserver "google.golang.org/grpc/transport/server"
)

// writeProtoFastPath mirrors the optional INLINE_TX capability that grpc-go core
// detects by assertion (marshal a protobuf message directly into transport
// memory). The plugin returns the engine's concrete streams DIRECTLY (no
// per-stream wrapper), so those streams must implement the capability for core
// to use marshal-into-ring.
type writeProtoFastPath interface {
	WriteProto(msg any, opts *transport.WriteOptions) (bool, error)
}

// TestEngineStreamsExposeOptionalWriteProto proves the streams the plugin hands
// to grpc-go core — the in-tree concrete engine streams, returned directly by
// the bridge transport WITHOUT a per-stream wrapper — implement the optional
// INLINE_TX capability, both as the core-internal shape (writeProtoFastPath) and
// as the exported transportclient/transportserver.ProtoWriteStream contract. So
// core's assertion succeeds and the plugin uses marshal-into-ring exactly like
// the monolithic transport. The real end-to-end path is exercised by
// TestRegistryPath* in registry_e2e_test.go.
func TestEngineStreamsExposeOptionalWriteProto(t *testing.T) {
	if _, ok := any((*transport.ClientStream)(nil)).(writeProtoFastPath); !ok {
		t.Fatal("*transport.ClientStream must implement the WriteProto fast path")
	}
	if _, ok := any((*transport.ClientStream)(nil)).(transportclient.ProtoWriteStream); !ok {
		t.Fatal("*transport.ClientStream must implement the exported transportclient.ProtoWriteStream capability")
	}
	if _, ok := any((*transport.ServerStream)(nil)).(writeProtoFastPath); !ok {
		t.Fatal("*transport.ServerStream must implement the WriteProto fast path")
	}
	if _, ok := any((*transport.ServerStream)(nil)).(transportserver.ProtoWriteStream); !ok {
		t.Fatal("*transport.ServerStream must implement the exported transportserver.ProtoWriteStream capability")
	}
}
