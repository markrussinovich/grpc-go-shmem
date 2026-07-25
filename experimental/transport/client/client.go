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

// Package client defines an EXPERIMENTAL, purpose-built public contract for a
// pluggable client-side gRPC transport. It lets an out-of-tree transport (for
// example a shared-memory transport) be selected and driven by grpc-go WITHOUT
// importing google.golang.org/grpc/internal/*.
//
// # Stability
//
// EXPERIMENTAL. The shapes here are deliberately minimal and grpc-go-native.
// No compatibility is promised until at least one independent transport and one
// fake transport pass the conformance suite. Do NOT depend on this from
// production code.
//
// # Design
//
// The mandatory send path is BYTE-BASED: ClientStream.Write takes already-framed
// bytes, ReadMessageHeader+Read hand ring/buffer-backed bytes upward (Read
// returns a ref-counted mem.BufferSlice, which is how read-side zero-copy
// survives a clean transport boundary). Marshalling an application message
// directly into transport-owned memory (the INLINE_TX fast path) is an OPTIONAL
// capability, ProtoWriteStream, detected by interface assertion; grpc-go
// transparently falls back to Write when it is absent or declines.
//
// The option types (BuildOptions, CallHdr, WriteOptions) are PURPOSE-BUILT
// by-value structs carrying only what a transport legitimately consumes. They
// are intentionally NOT aliases of any internal option struct: aliasing would
// freeze internal layout as a public compatibility obligation. Concerns above
// the transport seam (channelz, RPC stats, tap, socket buffer tuning) are
// deliberately excluded.
package client

import (
	"context"
	"errors"
	"net"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// ErrNotApplicable is returned (or wrapped, detectable via errors.Is) by a
// Builder.Build to decline an address because of a negotiation/applicability
// mismatch, as opposed to an authentication, resource, or timeout failure.
//
// CURRENT BEHAVIOR: grpc-go does NOT fall back to another transport on
// ErrNotApplicable. Selection is fail-closed, so an explicitly selected
// transport that declines fails the connection like any other build error. The
// sentinel exists so a Builder can express the distinction, and so
// resolver-authorized fallback can be added later without changing this API;
// until then, returning it is equivalent to returning any other error.
//
// A Builder that returns ErrNotApplicable MUST have released every resource it
// acquired, published no transport or stream, and left no externally visible
// protocol side effect that would make a future fallback unsafe. Probing
// (dialing / negotiating) before discovering non-applicability is permitted
// provided this clean-teardown guarantee holds.
var ErrNotApplicable = errors.New("grpc: transport not applicable for this address")

// NewStreamError is an OPTIONAL structured error a ClientTransport MAY return
// from NewStream to signal whether the failed stream attempt is eligible for
// TRANSPARENT retry — i.e. the RPC provably never left the client, so grpc-go
// may safely re-attempt it (on another transport) without violating at-most-once
// semantics. grpc-go unwraps Err for status classification and honors
// AllowTransparentRetry.
//
// A transport that returns a plain error (not this type) is treated as NOT
// transparently retryable.
type NewStreamError struct {
	// Err is the underlying failure. It SHOULD be a gRPC status error so grpc-go
	// can classify it; a connection-level failure (closed/draining/GOAWAY)
	// SHOULD be codes.Unavailable, a context failure codes.Canceled /
	// codes.DeadlineExceeded.
	Err error
	// AllowTransparentRetry reports whether the attempt may be transparently
	// retried. Set true ONLY when the RPC provably did not reach the server.
	AllowTransparentRetry bool
}

func (e *NewStreamError) Error() string { return e.Err.Error() }

func (e *NewStreamError) Unwrap() error { return e.Err }

// GoAwayReason describes why a client transport received a drain (GOAWAY)
// signal.
type GoAwayReason int

const (
	// GoAwayInvalid is the zero value and means no/unknown reason.
	GoAwayInvalid GoAwayReason = iota
	// GoAwayNoReason is the transport-drained-without-a-specific-reason case.
	GoAwayNoReason
	// GoAwayTooManyPings indicates the server drained due to excessive pings.
	GoAwayTooManyPings
)

// SecurityInfo reports the security properties a transport negotiated for a
// connection. grpc-go uses it to enforce RequireTransportSecurity for per-RPC
// credentials without reaching into transport internals.
//
// SecurityLevel is AUTHORITATIVE for enforcement and MUST be set explicitly by
// the transport to the negotiated level; it MUST be consistent with any
// CommonAuthInfo.SecurityLevel carried by AuthInfo. A value of
// credentials.InvalidSecurityLevel (the zero value) is treated as insecure and
// MUST fail any credential requiring transport security: enforcement is
// fail-closed and is never satisfied by absent or unknown information.
type SecurityInfo struct {
	// AuthInfo is the authenticated peer information for the connection, or nil
	// for an insecure transport. It MUST describe the same negotiated identity as
	// ClientTransport.Peer().AuthInfo.
	AuthInfo credentials.AuthInfo
	// SecurityLevel is the authoritative negotiated transport security level.
	SecurityLevel credentials.SecurityLevel
}

// CloseInfo carries the reason a transport terminated, delivered to
// BuildOptions.OnClose.
type CloseInfo struct {
	// Err is the terminal error, or nil for a graceful close.
	Err error
}

// CallHdr carries the per-RPC information grpc-go passes to NewStream. It is a
// minimal, transport-facing subset of the internal call header.
type CallHdr struct {
	// Method is the RPC method name (e.g. "/service/Method").
	Method string
	// Authority is the :authority pseudo-header for the RPC. Precedence:
	// CallHdr.Authority (per-call override) over resolver.Address.ServerName over
	// BuildOptions.Authority. When non-empty it MUST be validated against the
	// connection's negotiated identity via ClientTransport.SecurityInfo().AuthInfo
	// when that AuthInfo implements credentials.AuthorityValidator; if an override
	// is supplied but no validator is available, NewStream MUST fail.
	Authority string
	// ContentSubtype is the gRPC content sub-type (e.g. "proto"); empty means the
	// default.
	ContentSubtype string
	// SendCompress names the outbound message compressor, or "" for none.
	SendCompress string
	// AcceptedCompressors overrides the response compressors advertised via
	// grpc-accept-encoding for this RPC. nil means "use the process-wide
	// compressor registry" (the default); a non-nil slice (including an empty
	// slice) is an explicit override. The nil-vs-empty distinction is
	// significant.
	AcceptedCompressors []string
	// CallCredentials are per-call credentials supplied via a CallOption, applied
	// IN ADDITION to BuildOptions.PerRPCCredentials when forming outgoing request
	// metadata. It is nil when the call carries no call-level credentials. A
	// credential whose RequireTransportSecurity is true MUST be rejected unless
	// SecurityInfo reports a secure connection.
	CallCredentials credentials.PerRPCCredentials
	// PreviousAttempts is the number of prior transparent/retry attempts for this
	// RPC, used to populate grpc-previous-rpc-attempts.
	PreviousAttempts int
	// DoneFunc, if non-nil, is called when the stream terminates.
	DoneFunc func()
}

// WriteOptions carries per-write flags.
type WriteOptions struct {
	// Last indicates this is the final write on the stream (half-close).
	Last bool
}

// BuildOptions carries the per-connection inputs grpc-go supplies to a client
// transport Builder. It is passed by value and carries only public types; no
// internal types leak through. Some fields are reference-bearing (slices,
// pointers, interfaces); see the package "Concurrency and lifetime" section for
// their retention rules.
type BuildOptions struct {
	// Authority is the default :authority for RPCs on this connection.
	Authority string
	// UserAgent is the User-Agent header value.
	UserAgent string
	// Dialer, if set, produces the underlying connection for transports that
	// bootstrap over a net.Conn (e.g. a UDS control channel for SHM).
	Dialer func(ctx context.Context, addr string) (net.Conn, error)
	// TransportCredentials performs the transport-security handshake. It is nil
	// for an insecure connection.
	TransportCredentials credentials.TransportCredentials
	// PerRPCCredentials are the call credentials to attach to outgoing RPCs.
	// grpc-go normalizes any credentials.Bundle into this + TransportCredentials
	// so the transport never sees a Bundle.
	PerRPCCredentials []credentials.PerRPCCredentials
	// Keepalive configures client-side keepalive pings.
	Keepalive keepalive.ClientParameters
	// InitialWindowSize is the initial per-stream flow-control window, or 0 for
	// the transport default.
	InitialWindowSize uint32
	// InitialConnWindowSize is the initial connection-level flow-control window,
	// or 0 for the transport default.
	InitialConnWindowSize uint32
	// MaxHeaderListSize bounds the decoded header list size, or nil for the
	// transport default.
	MaxHeaderListSize *uint32
	// BufferPool is the pool the transport should use for read/write buffers.
	BufferPool mem.BufferPool
	// OnClose is invoked once when the transport terminates.
	OnClose func(CloseInfo)
}

// ClientStream is the per-RPC stream a ClientTransport produces. The mandatory
// contract is byte-based; the optional INLINE_TX fast path is ProtoWriteStream.
//
// # Validity requirements
//
// grpc-go drives these values directly, so an implementation MUST satisfy the
// following or it will hang or crash the calling RPC:
//
//   - NewStream MUST NOT return a nil ClientStream together with a nil error.
//     grpc-go rejects that combination with an Internal error rather than
//     dereferencing it, but it is a contract violation.
//   - Done MUST return a non-nil channel that is closed exactly once, when the
//     stream is finished. grpc-go BLOCKS on it while waiting for trailers; a nil
//     channel blocks forever and deadlocks the RPC.
//   - Status MUST return a non-nil *status.Status once Done is closed.
//   - Context MUST return a non-nil context for the life of the stream.
type ClientStream interface {
	// Write writes the pre-framed hdr and data bytes to the stream. The
	// implementation MUST NOT retain hdr or data beyond the call; data (a
	// ref-counted mem.BufferSlice) may be kept alive only by taking a Ref.
	Write(hdr []byte, data mem.BufferSlice, opts WriteOptions) error

	// ReadMessageHeader and Read form the parser contract. Read returns a
	// ref-counted mem.BufferSlice that MAY be backed by transport memory.
	ReadMessageHeader(header []byte) error
	Read(n int) (mem.BufferSlice, error)
	// RecvCompress reports the inbound message compression algorithm.
	RecvCompress() string

	// Header and Trailer expose received header/trailer metadata; Status is the
	// RPC status received from the server.
	Header() (metadata.MD, error)
	Trailer() metadata.MD
	Status() *status.Status

	// Context, Done and the retry predicates are what grpc-go's finish/retry
	// logic calls.
	Context() context.Context
	Done() <-chan struct{}
	Unprocessed() bool
	TrailersOnly() bool
	BytesReceived() bool
	Close(err error)
}

// ProtoWriteStream is the OPTIONAL INLINE_TX capability a ClientStream MAY
// additionally implement to marshal a protobuf message DIRECTLY into
// transport-owned memory, skipping the intermediate marshal buffer and payload
// copy the mandatory byte Write path incurs.
//
// grpc-go attempts it ONLY for an uncompressed protobuf message using the
// built-in codec and within the configured max send size. size is the validated
// proto.Size(msg) for the exact message state passed (size >= 0) and may be
// trusted without recomputation.
//
// The result reports OWNERSHIP, not merely success:
//   - handled=false MUST mean the implementation declined cleanly: it wrote NO
//     message DATA, consumed NO message flow-control quota, made NO terminal
//     write-state transition, and left NO externally visible side effect that
//     would make the byte Write fallback unsafe. It MUST return err=nil; grpc-go
//     IGNORES any error paired with handled=false and uses the byte Write path.
//     (Stream-opening HEADERS already emitted by NewStream are unaffected.)
//   - handled=true means the implementation TOOK OWNERSHIP and grpc-go MUST NOT
//     fall back: err=nil means the message was fully serialized; err!=nil means
//     the attempt failed and grpc-go propagates err without a byte-path retry.
//
// If the implementation performs any fallible, externally visible operation
// (e.g. flushing pending HEADERS) and it fails, it MUST report handled=true with
// that error rather than handled=false, so grpc-go does not double-write.
//
// The implementation MUST finish reading msg BEFORE returning and MUST NOT
// retain or access msg afterward, so the caller may reuse it.
type ProtoWriteStream interface {
	WriteProto(msg proto.Message, size int, opts WriteOptions) (handled bool, err error)
}

// ClientTransport is a client-side gRPC transport produced by a Builder.
type ClientTransport interface {
	// NewStream starts a new RPC stream.
	NewStream(ctx context.Context, callHdr *CallHdr) (ClientStream, error)
	// Close tears the transport down with the given error.
	Close(err error)
	// GracefulClose drains the transport, allowing in-flight RPCs to finish.
	GracefulClose()
	// Error returns a channel closed when the transport hits a fatal error.
	Error() <-chan struct{}
	// GoAway returns a channel closed when the transport receives a drain
	// signal.
	GoAway() <-chan struct{}
	// GetGoAwayReason reports why a drain signal was received.
	GetGoAwayReason() (GoAwayReason, string)
	// Peer returns the connection peer, including SecurityInfo-derived AuthInfo.
	Peer() *peer.Peer
	// SecurityInfo reports the negotiated connection security, for
	// RequireTransportSecurity enforcement.
	SecurityInfo() SecurityInfo
}

// Builder builds client transports for addresses selected by TransportType.
type Builder interface {
	// Build dials and returns a client transport for addr. connectCtx bounds the
	// dial/handshake; ctx bounds the transport lifetime. Returning
	// ErrNotApplicable declines the address cleanly (see ErrNotApplicable).
	Build(connectCtx, ctx context.Context, addr resolver.Address, opts BuildOptions) (ClientTransport, error)
}
