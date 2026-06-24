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

import "google.golang.org/grpc/internal/transport"

// WriteProto (INLINE_TX: marshal a proto message directly into the transport's
// send buffer) is an OPTIONAL, non-portable fast path. It is deliberately NOT
// part of the byte-based pluggable transport interface
// (transport.ClientStreamIface / ServerStreamIface), because "marshal an
// application message" is a codec responsibility that is not portable across
// gRPC languages. Core therefore detects it by assertion and uses it only when
// the concrete transport stream offers it: the first-party monolithic transport
// keeps INLINE_TX, while a byte-only plugin stream does not implement WriteProto
// and transparently falls back to the standard Write path.

// writeProtoCapable is the optional INLINE_TX capability. The client and server
// concrete streams share the identical signature, so one interface covers both.
type writeProtoCapable interface {
	WriteProto(msg any, opts *transport.WriteOptions) (bool, error)
}

// tryClientWriteProto invokes the optional INLINE_TX fast path when available.
// It returns (false, nil) when the stream does not implement it, signalling the
// caller to use the standard Write path.
func tryClientWriteProto(s transport.ClientStreamIface, msg any, opts *transport.WriteOptions) (handled bool, err error) {
	if wp, ok := s.(writeProtoCapable); ok {
		return wp.WriteProto(msg, opts)
	}
	return false, nil
}

// tryServerWriteProto is the server-side analogue of tryClientWriteProto.
func tryServerWriteProto(s transport.ServerStreamIface, msg any, opts *transport.WriteOptions) (handled bool, err error) {
	if wp, ok := s.(writeProtoCapable); ok {
		return wp.WriteProto(msg, opts)
	}
	return false, nil
}
