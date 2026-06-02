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

package grpc

import (
	"testing"

	"google.golang.org/grpc/encoding"
	protoenc "google.golang.org/grpc/encoding/proto"
	"google.golang.org/grpc/mem"
)

// spoofProtoCodec is a deliberately adversarial codec: it registers
// itself under the name "proto" AND implements the bufferPoolMarshaler
// capability. Before the identity-marker fix it would have been silently
// matched by the SHM WriteProto bypass paths via the
// `c.(bufferPoolMarshaler)` capability sniff, which would have caused
// the transport to marshal via the built-in protobuf library instead of
// this codec — i.e. silent wire-format divergence for any user who
// relied on a custom "proto"-named v2 codec.
//
// It does NOT satisfy encoding/proto.IsBuiltin (which is a pointer
// identity check against the package-private *codecV2 type), so it
// MUST NOT match the SHM bypass identity check.
type spoofProtoCodec struct{}

func (spoofProtoCodec) Marshal(any) (mem.BufferSlice, error) { return nil, nil }
func (spoofProtoCodec) Unmarshal(mem.BufferSlice, any) error { return nil }
func (spoofProtoCodec) Name() string                         { return "proto-spoof-test-only" }
func (spoofProtoCodec) MarshalWithPool(any, mem.BufferPool) (mem.BufferSlice, error) {
	return nil, nil
}

// TestBuiltinProtoCodecMarker_NotSpoofable verifies that the
// proto.IsBuiltin identity check cannot be satisfied by a third-
// party codec that merely implements the bufferPoolMarshaler
// capability. The two mechanisms serve different purposes and must
// not be conflated:
//
//   - bufferPoolMarshaler is a CAPABILITY (pool-aware Marshal). Used by
//     encode() to pass the channel's BufferPool through to the user's
//     codec.
//   - proto.IsBuiltin is an IDENTITY test. Used by SHM transport fast
//     paths that BYPASS the user's codec and call protobuf
//     marshal / unmarshal directly. Silent wire-format divergence if
//     this identity check accepts a non-builtin codec.
func TestBuiltinProtoCodecMarker_NotSpoofable(t *testing.T) {
	// Built-in proto codec must satisfy the bufferPoolMarshaler
	// capability AND the identity check.
	builtin := encoding.GetCodecV2("proto")
	if builtin == nil {
		t.Fatal("encoding.GetCodecV2(\"proto\") returned nil; built-in proto codec missing")
	}
	if _, ok := builtin.(bufferPoolMarshaler); !ok {
		t.Errorf("built-in proto codec does not implement bufferPoolMarshaler; SHM pool injection regressed")
	}
	if !protoenc.IsBuiltin(builtin) {
		t.Errorf("built-in proto codec does not satisfy proto.IsBuiltin; SHM bypass paths will reject it")
	}

	// Spoofing v2 codec must satisfy bufferPoolMarshaler but NOT
	// proto.IsBuiltin. If it satisfied the identity check, a
	// third-party could trick the SHM WriteProto bypass path into
	// marshalling via the built-in protobuf library instead of
	// the user's codec.
	spoof := spoofProtoCodec{}
	if _, ok := any(spoof).(bufferPoolMarshaler); !ok {
		t.Fatal("spoofProtoCodec test setup: must implement bufferPoolMarshaler")
	}
	if protoenc.IsBuiltin(spoof) {
		t.Fatal("REGRESSION: spoofProtoCodec satisfies proto.IsBuiltin; identity check is spoofable")
	}
}
