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
	"context"
	"reflect"
	"testing"

	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/metadata"
)

// ctxStreamStub satisfies both ServerTransportStream (so it can be stored in the
// handler context) and the narrow compressorCapableStream surface, WITHOUT being
// the concrete *transport.ServerStream. It stands in for a pluggable transport's
// wrapped stream.
type ctxStreamStub struct {
	advertised  []string
	setCompress string
}

func (s *ctxStreamStub) Method() string                  { return "/svc/method" }
func (s *ctxStreamStub) SetHeader(md metadata.MD) error  { return nil }
func (s *ctxStreamStub) SendHeader(md metadata.MD) error { return nil }
func (s *ctxStreamStub) SetTrailer(md metadata.MD) error { return nil }
func (s *ctxStreamStub) ClientAdvertisedCompressors() []string {
	return s.advertised
}
func (s *ctxStreamStub) SetSendCompress(name string) error {
	s.setCompress = name
	return nil
}

// TestServerCompressorAPIsAcceptNonConcreteStream verifies that the server-side
// compressor helpers work for a stream that satisfies the narrow capability
// interface but is NOT the concrete *transport.ServerStream — i.e. a pluggable
// transport's wrapped stream. This pins the assertion widening in
// SetSendCompressor / ClientSupportedCompressors.
func (s) TestServerCompressorAPIsAcceptNonConcreteStream(t *testing.T) {
	stub := &ctxStreamStub{advertised: []string{"gzip", "snappy"}}
	ctx := NewContextWithServerTransportStream(context.Background(), stub)

	got, err := ClientSupportedCompressors(ctx)
	if err != nil {
		t.Fatalf("ClientSupportedCompressors on wrapped stream: %v", err)
	}
	if want := []string{"gzip", "snappy"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ClientSupportedCompressors = %v, want %v", got, want)
	}

	// SetSendCompressor must also accept the non-concrete stream. encoding.Identity
	// bypasses the registered-compressor check, so this exercises the (widened)
	// stream assertion plus the SetSendCompress forward without needing a
	// registered compressor.
	if err := SetSendCompressor(ctx, encoding.Identity); err != nil {
		t.Fatalf("SetSendCompressor(identity) on wrapped stream: %v", err)
	}
	if stub.setCompress != encoding.Identity {
		t.Fatalf("SetSendCompress not forwarded to the wrapped stream (got %q)", stub.setCompress)
	}
}
