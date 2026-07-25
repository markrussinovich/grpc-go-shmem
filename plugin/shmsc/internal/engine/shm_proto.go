//go:build linux || windows

/*
 *
 * Copyright 2025 gRPC authors.
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

package engine

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/proto"
)

// protoMessage is an interface satisfied by all protobuf v2 messages.
type protoMessage = proto.Message

// protoSize returns the serialized size of a proto message.
func protoSize(msg proto.Message) int {
	return proto.Size(msg)
}

// protoMarshalAppend serializes msg and appends the result to dst.
func protoMarshalAppend(dst []byte, msg proto.Message) ([]byte, error) {
	return proto.MarshalOptions{UseCachedSize: true}.MarshalAppend(dst, msg)
}

// marshalProtoForAsync marshals pm into a pooled buffer for the async
// writeProto fallback and verifies the encoded length matches the
// pre-computed pSize. It is called on the sender's SendMsg goroutine so
// the live proto.Message is fully serialized BEFORE writeProto returns;
// the writer goroutine then copies the returned owned bytes into the
// ring (see writeProtoBytesToRingH2Blocking). On success the caller owns
// the buffer and MUST release it via putAsyncProtoBuf on every terminal
// path; on error the buffer is already released.
func marshalProtoForAsync(pm proto.Message, pSize int) ([]byte, error) {
	buf := getAsyncProtoBuf(pSize)
	out, err := protoMarshalAppend(buf[:0], pm)
	if err != nil {
		putAsyncProtoBuf(buf)
		return nil, err
	}
	if len(out) != pSize {
		putAsyncProtoBuf(out)
		return nil, fmt.Errorf("shm writeProto: marshal size mismatch: %d vs %d", pSize, len(out))
	}
	return out, nil
}

// writeProtoToRing serializes a proto.Message directly into the ring buffer
// using the HTTP/2 wire codec. The H2 path emits an H2 DATA frame whose body
// is the gRPC LPM (5B header + protobuf payload).
//
// Returns false if the message cannot fit contiguously at this moment. The
// caller should retry via the queued frame writer path which will block until
// space is available.
//
// pSize is the pre-computed proto.Size result. Passing it avoids a redundant
// proto.Size call (the caller typically already computed it for flow control).
// Pass -1 to compute it internally.
func writeProtoToRing(ctx context.Context, tx *ShmRing, streamID uint32, msg proto.Message, pSize int, flags uint8) (bool, error) {
	if pSize < 0 {
		pSize = proto.Size(msg)
	}
	return writeProtoToRingH2(ctx, tx, streamID, msg, pSize, flags)
}
