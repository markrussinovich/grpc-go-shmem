/*
 *
 * Copyright 2024 gRPC authors.
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

// Package proto defines the protobuf codec. Importing this package will
// register the codec.
package proto

import (
	"fmt"

	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/mem"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/protoadapt"
)

// Name is the name registered for the proto compressor.
const Name = "proto"

func init() {
	encoding.RegisterCodecV2(&codecV2{})
}

// codec is a CodecV2 implementation with protobuf. It is the default codec for
// gRPC.
type codecV2 struct{}

// IsBuiltin reports whether c is the gRPC built-in proto codec
// registered by this package. Used by transport fast paths (e.g. the
// shared-memory transport's WriteProto bypass) that need an
// unspoofable identity check to safely skip the user-supplied codec
// and call protobuf marshal/unmarshal directly.
//
// Why an exported identity function rather than a capability
// interface like bufferPoolMarshaler: capability-based detection can
// be silently satisfied by a third-party codec that registers under
// the name "proto" and happens to implement the same capability,
// which would cause the SHM bypass path to marshal via the built-in
// protobuf library instead of the user's codec — silent wire-format
// divergence. IsBuiltin is a pointer-identity test against the
// concrete *codecV2 registered by this package and cannot be spoofed.
//
// Accepts any (rather than encoding.CodecV2) so callers can pass
// their internal baseCodec/CodecV1Bridge interface values without an
// up-cast; non-codec values cleanly return false.
//
// Returns false for nil receivers.
func IsBuiltin(c any) bool {
	_, ok := c.(*codecV2)
	return ok
}

func (c *codecV2) Marshal(v any) (data mem.BufferSlice, err error) {
	return c.marshalWithPool(v, mem.DefaultBufferPool())
}

// MarshalWithPool is an optional extension of the CodecV2 interface that
// allows the caller to supply the mem.BufferPool used for the marshal
// destination buffer. Production wiring lives in (rpc_util).encode, which
// dispatches to this method when the caller (a stream / transport)
// configures a per-channel BufferPool (e.g. for the shared-memory
// transport, which provides a tightly sized pool to avoid the default
// tiered pool's per-Get overshoot).
//
// Behaviour is identical to Marshal when pool is nil — the default
// pool is substituted, preserving backwards-compatible semantics for
// any caller that has not opted into the pool-injection path.
func (c *codecV2) MarshalWithPool(v any, pool mem.BufferPool) (data mem.BufferSlice, err error) {
	if pool == nil {
		pool = mem.DefaultBufferPool()
	}
	return c.marshalWithPool(v, pool)
}

func (c *codecV2) marshalWithPool(v any, pool mem.BufferPool) (data mem.BufferSlice, err error) {
	vv := messageV2Of(v)
	if vv == nil {
		return nil, fmt.Errorf("proto: failed to marshal, message is %T, want proto.Message", v)
	}

	// Important: if we remove this Size call then we cannot use
	// UseCachedSize in MarshalOptions below.
	size := proto.Size(vv)

	// MarshalOptions with UseCachedSize allows reusing the result from the
	// previous Size call. This is safe here because:
	//
	// 1. We just computed the size.
	// 2. We assume the message is not being mutated concurrently.
	//
	// Important: If the proto.Size call above is removed, using UseCachedSize
	// becomes unsafe and may lead to incorrect marshaling.
	//
	// For more details, see the doc of UseCachedSize:
	// https://pkg.go.dev/google.golang.org/protobuf/proto#MarshalOptions
	marshalOptions := proto.MarshalOptions{UseCachedSize: true}

	if mem.IsBelowBufferPoolingThreshold(size) {
		buf, err := marshalOptions.Marshal(vv)
		if err != nil {
			return nil, err
		}
		data = append(data, mem.SliceBuffer(buf))
	} else {
		buf := pool.Get(size)
		if _, err := marshalOptions.MarshalAppend((*buf)[:0], vv); err != nil {
			pool.Put(buf)
			return nil, err
		}
		data = append(data, mem.NewBuffer(buf, pool))
	}

	return data, nil
}

func (c *codecV2) Unmarshal(data mem.BufferSlice, v any) (err error) {
	vv := messageV2Of(v)
	if vv == nil {
		return fmt.Errorf("failed to unmarshal, message is %T, want proto.Message", v)
	}

	buf := data.MaterializeToBuffer(mem.DefaultBufferPool())
	defer buf.Free()
	// TODO: Upgrade proto.Unmarshal to support mem.BufferSlice. Right now, it's not
	//  really possible without a major overhaul of the proto package, but the
	//  vtprotobuf library may be able to support this.
	return proto.Unmarshal(buf.ReadOnlyData(), vv)
}

func messageV2Of(v any) proto.Message {
	switch v := v.(type) {
	case protoadapt.MessageV1:
		return protoadapt.MessageV2Of(v)
	case protoadapt.MessageV2:
		return v
	}

	return nil
}

func (c *codecV2) Name() string {
	return Name
}
