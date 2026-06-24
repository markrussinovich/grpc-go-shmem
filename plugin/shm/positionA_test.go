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
)

// writeProtoFastPath mirrors the optional, non-portable INLINE_TX capability
// that grpc-go core detects by assertion. The first-party concrete streams
// implement it; a byte-only plugin stream must not.
type writeProtoFastPath interface {
	WriteProto(msg any, opts *transport.WriteOptions) (bool, error)
}

// TestPluginStreamsDoNotExposeWriteProto proves Position A structurally: the
// streams the plugin hands to grpc-go core present ONLY the byte-based
// interface, so core's optional INLINE_TX (marshal-into-ring) capability
// assertion fails and the portable Write path is used. INLINE_TX therefore
// remains a first-party-monolithic-only optimization that the plugin does not
// (and cannot, over the exported contract) use.
func TestPluginStreamsDoNotExposeWriteProto(t *testing.T) {
	var cs transport.ClientStreamIface = bridgeClientStream{}
	if _, ok := cs.(writeProtoFastPath); ok {
		t.Fatal("bridgeClientStream must NOT expose WriteProto: INLINE_TX is a first-party-only optimization, not part of the byte-based pluggable contract")
	}

	var ss transport.ServerStreamIface = bridgeServerStream{}
	if _, ok := ss.(writeProtoFastPath); ok {
		t.Fatal("bridgeServerStream must NOT expose WriteProto: INLINE_TX is a first-party-only optimization, not part of the byte-based pluggable contract")
	}
}

// TestConcreteStreamsDoExposeWriteProto is the companion: the in-tree concrete
// streams DO implement the optional fast path, which is exactly why the
// monolithic (non-plugin) path keeps INLINE_TX. This pins the asymmetry the
// benchmark measures.
func TestConcreteStreamsDoExposeWriteProto(t *testing.T) {
	if _, ok := any((*transport.ClientStream)(nil)).(writeProtoFastPath); !ok {
		t.Fatal("expected concrete *transport.ClientStream to implement the WriteProto fast path")
	}
	if _, ok := any((*transport.ServerStream)(nil)).(writeProtoFastPath); !ok {
		t.Fatal("expected concrete *transport.ServerStream to implement the WriteProto fast path")
	}
}
