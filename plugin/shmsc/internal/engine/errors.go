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

package engine

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ErrIllegalHeaderWrite indicates that setting header is illegal because of the
// stream's state (headers already sent, or the stream is done).
var ErrIllegalHeaderWrite = status.Error(codes.Internal, "transport: SendHeader called multiple times")

// ErrConnClosing indicates that the transport is closing. It is a
// codes.Unavailable status error so that, when surfaced across the D1 boundary,
// grpc-go classifies the failed RPC as a retriable Unavailable rather than
// codes.Unknown (see the error-normalization guardrail).
var ErrConnClosing = status.Error(codes.Unavailable, "transport is closing")

// errStreamDrain indicates that the stream is rejected because the connection
// is draining (GOAWAY or balancer removing the address). Unavailable so grpc-go
// treats the failed attempt as retriable.
var errStreamDrain = status.Error(codes.Unavailable, "the connection is draining")

// ContextErr converts an error from the context package into a gRPC status
// error.
//
// Every error the engine surfaces back across the D1 boundary to grpc-go MUST
// be a status error: grpc-go's toRPCErr maps a non-status, non-recognized error
// to codes.Unknown, which would mask context cancellation and deadline
// outcomes. ContextErr is the single conversion point for context results, so
// callers propagate ctx.Err() through it rather than returning the raw context
// sentinel.
func ContextErr(err error) error {
	switch err {
	case context.DeadlineExceeded:
		return status.Error(codes.DeadlineExceeded, err.Error())
	case context.Canceled:
		return status.Error(codes.Canceled, err.Error())
	}
	return status.Errorf(codes.Internal, "Unexpected error from context packet: %v", err)
}
