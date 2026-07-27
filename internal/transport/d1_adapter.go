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

package transport

// This file is the CORE-SIDE seam that lets grpc-go drive a self-contained,
// experimental "D1" transport (google.golang.org/grpc/experimental/transport)
// through the existing internal transport contract, WITHOUT the D1 transport or
// its engine ever importing internal/*. All translation between the internal
// kitchen-sink option/stream types and the purpose-built public D1 types happens
// here, in one place, on the core side. The D1 transport sees only public types.
//
// It is additive: nothing here changes the behavior of the stock HTTP/2 or the
// in-tree SHM transports. It is exercised only when the selection layer builds a
// D1-registered transport (see BuildD1Client / the server dispatch).

import (
	"context"
	"errors"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	expclient "google.golang.org/grpc/experimental/transport/client"
	expserver "google.golang.org/grpc/experimental/transport/server"
	"google.golang.org/grpc/internal/grpcutil"
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// ---------------------------------------------------------------------------
// Client side
// ---------------------------------------------------------------------------

// d1ClientTransport adapts an experimental client.ClientTransport to the
// internal ClientTransport contract.
type d1ClientTransport struct {
	inner expclient.ClientTransport
}

var _ ClientTransport = (*d1ClientTransport)(nil)

// newD1ClientTransport wraps an already-built D1 client transport.
func newD1ClientTransport(inner expclient.ClientTransport) *d1ClientTransport {
	return &d1ClientTransport{inner: inner}
}

func (t *d1ClientTransport) Close(err error)         { t.inner.Close(err) }
func (t *d1ClientTransport) GracefulClose()          { t.inner.GracefulClose() }
func (t *d1ClientTransport) Error() <-chan struct{}  { return t.inner.Error() }
func (t *d1ClientTransport) GoAway() <-chan struct{} { return t.inner.GoAway() }
func (t *d1ClientTransport) Peer() *peer.Peer        { return t.inner.Peer() }

func (t *d1ClientTransport) GetGoAwayReason() (GoAwayReason, string) {
	r, dbg := t.inner.GetGoAwayReason()
	return fromD1GoAwayReason(r), dbg
}

// NewStream translates the internal CallHdr into the public D1 CallHdr.
//
// Authority: an explicit per-call override (callHdr.Authority, from the
// CallAuthority call option or an LB picker) is validated HERE against the
// connection's negotiated AuthInfo — mirroring the HTTP/2 client exactly
// (credentials.AuthorityValidator; UNAVAILABLE on a missing validator or a
// validation failure) — and the resolved, already-validated effective authority
// is handed to the D1 transport, which writes it as-is. The default (Host) is
// used unvalidated when there is no override.
//
// KNOWN EXPERIMENTAL LIMITATION: the per-stream stats.Handler is not forwarded,
// so transport-timed stats events (OutHeader/InHeader/InTrailer) are NOT emitted
// for D1 transports (core still produces Begin/End/payload events). Faithful
// transport-level stats need a D1 stats hook and are tracked as a follow-up;
// this gap is documented (not silent) and does not affect the SHM data path.
func (t *d1ClientTransport) NewStream(ctx context.Context, callHdr *CallHdr, _ stats.Handler) (ClientStreamIface, error) {
	authority, err := t.resolveAuthority(callHdr.Authority, callHdr.Host)
	if err != nil {
		return nil, err
	}
	d1hdr := &expclient.CallHdr{
		Method:              callHdr.Method,
		Authority:           authority,
		ContentSubtype:      callHdr.ContentSubtype,
		SendCompress:        callHdr.SendCompress,
		AcceptedCompressors: resolveAcceptedCompressors(callHdr.AcceptedCompressors),
		CallCredentials:     callHdr.Creds,
		PreviousAttempts:    callHdr.PreviousAttempts,
		DoneFunc:            callHdr.DoneFunc,
	}
	s, err := t.inner.NewStream(ctx, d1hdr)
	if err != nil {
		return nil, translateNewStreamErr(err)
	}
	// A transport implementation lives outside grpc-go, so treat its output as
	// untrusted: a nil stream with a nil error violates the contract and would
	// otherwise be wrapped into a non-nil d1ClientStream that panics the moment
	// core touched it. Fail the attempt cleanly instead.
	if s == nil {
		return nil, &NewStreamError{Err: status.Errorf(codes.Internal, "grpc: transport %T returned a nil stream and a nil error from NewStream", t.inner)}
	}
	return &d1ClientStream{inner: s}, nil
}

// translateNewStreamErr maps a public experimental client.NewStreamError (the
// D1 transparent-retry signal) to the internal *NewStreamError that grpc-go's
// retry logic asserts on (stream.go). Other errors pass through unchanged. This
// is the ONLY place the transparent-retry bit crosses from the D1 boundary into
// core: a self-contained D1 transport cannot construct the internal type.
func translateNewStreamErr(err error) error {
	var nse *expclient.NewStreamError
	if errors.As(err, &nse) {
		return &NewStreamError{Err: nse.Err, AllowTransparentRetry: nse.AllowTransparentRetry}
	}
	return err
}

// resolveAuthority mirrors the HTTP/2 client's authority handling: an explicit
// override is validated against the negotiated AuthInfo's AuthorityValidator
// (UNAVAILABLE if the credentials do not implement it, or validation fails); the
// default host is used as-is when there is no override.
func (t *d1ClientTransport) resolveAuthority(override, host string) (string, error) {
	if override == "" {
		return host, nil
	}
	ai := t.inner.SecurityInfo().AuthInfo
	v, ok := ai.(credentials.AuthorityValidator)
	if !ok {
		authType := "insecure"
		if ai != nil {
			authType = ai.AuthType()
		}
		return "", &NewStreamError{Err: status.Errorf(codes.Unavailable, "credentials type %q does not implement the AuthorityValidator interface, but authority override %q was specified", authType, override)}
	}
	if err := v.ValidateAuthority(override); err != nil {
		return "", &NewStreamError{Err: status.Errorf(codes.Unavailable, "failed to validate authority %q: %v", override, err)}
	}
	return override, nil
}

// d1ClientStream adapts an experimental client.ClientStream to the internal
// ClientStreamIface, and forwards the optional INLINE_TX capability.
type d1ClientStream struct {
	inner expclient.ClientStream
}

var (
	_ ClientStreamIface = (*d1ClientStream)(nil)
	// The internal fast path detects INLINE_TX by asserting this method set
	// (see writeproto_fastpath.go). The adapter forwards to the D1 optional
	// capability when the underlying stream implements it.
	_ interface {
		WriteProto(msg any, opts *WriteOptions) (bool, error)
	} = (*d1ClientStream)(nil)
)

func (s *d1ClientStream) Write(hdr []byte, data mem.BufferSlice, opts *WriteOptions) error {
	return s.inner.Write(hdr, data, toD1WriteOptions(opts))
}

func (s *d1ClientStream) ReadMessageHeader(header []byte) error { return s.inner.ReadMessageHeader(header) }
func (s *d1ClientStream) Read(n int) (mem.BufferSlice, error)   { return s.inner.Read(n) }
func (s *d1ClientStream) RecvCompress() string                 { return s.inner.RecvCompress() }
func (s *d1ClientStream) Header() (metadata.MD, error)         { return s.inner.Header() }
func (s *d1ClientStream) Trailer() metadata.MD                 { return s.inner.Trailer() }
func (s *d1ClientStream) Status() *status.Status               { return s.inner.Status() }
func (s *d1ClientStream) Context() context.Context             { return s.inner.Context() }
func (s *d1ClientStream) Done() <-chan struct{}                { return s.inner.Done() }
func (s *d1ClientStream) Unprocessed() bool                    { return s.inner.Unprocessed() }
func (s *d1ClientStream) TrailersOnly() bool                   { return s.inner.TrailersOnly() }
func (s *d1ClientStream) BytesReceived() bool                  { return s.inner.BytesReceived() }
func (s *d1ClientStream) Close(err error)                      { s.inner.Close(err) }

// WriteProto forwards the internal INLINE_TX fast path to the D1 optional
// ProtoWriteStream capability. It computes the validated proto size (which the
// D1 contract requires) and asserts msg to proto.Message. If the underlying D1
// stream does not implement the capability, or msg is not a proto.Message, it
// declines cleanly so core uses the byte Write path.
func (s *d1ClientStream) WriteProto(msg any, opts *WriteOptions) (bool, error) {
	pw, ok := s.inner.(expclient.ProtoWriteStream)
	if !ok {
		return false, nil
	}
	pm, ok := msg.(proto.Message)
	if !ok {
		return false, nil
	}
	return pw.WriteProto(pm, proto.Size(pm), toD1WriteOptions(opts))
}

// ---------------------------------------------------------------------------
// Server side
// ---------------------------------------------------------------------------

// d1ServerTransport adapts an experimental server.ServerTransport to the
// internal ServerTransport contract.
type d1ServerTransport struct {
	inner expserver.ServerTransport
}

var _ ServerTransport = (*d1ServerTransport)(nil)

// newD1ServerTransport wraps an already-built D1 server transport.
func newD1ServerTransport(inner expserver.ServerTransport) *d1ServerTransport {
	return &d1ServerTransport{inner: inner}
}

func (t *d1ServerTransport) Close(err error)      { t.inner.Close(err) }
func (t *d1ServerTransport) Peer() *peer.Peer     { return t.inner.Peer() }
func (t *d1ServerTransport) Drain(debugData string) { t.inner.Drain(debugData) }

// HandleStreams wraps each accepted D1 server stream so core sees the internal
// ServerStreamIface.
func (t *d1ServerTransport) HandleStreams(ctx context.Context, handle func(ServerStreamIface)) {
	t.inner.HandleStreams(ctx, func(ds expserver.ServerStream) {
		// Defensive: a transport implementation lives outside grpc-go, so a nil
		// stream here would become a non-nil wrapper that panics in the handler.
		// Dropping it is the only safe action -- there is no stream to fail.
		if ds == nil {
			return
		}
		handle(&d1ServerStream{inner: ds})
	})
}

// d1ServerStream adapts an experimental server.ServerStream to the internal
// ServerStreamIface, and forwards the optional INLINE_TX capability.
type d1ServerStream struct {
	inner expserver.ServerStream
}

var (
	_ ServerStreamIface = (*d1ServerStream)(nil)
	_ interface {
		WriteProto(msg any, opts *WriteOptions) (bool, error)
	} = (*d1ServerStream)(nil)
)

func (s *d1ServerStream) ReadMessageHeader(header []byte) error { return s.inner.ReadMessageHeader(header) }
func (s *d1ServerStream) Read(n int) (mem.BufferSlice, error)   { return s.inner.Read(n) }
func (s *d1ServerStream) RecvCompress() string                 { return s.inner.RecvCompress() }

func (s *d1ServerStream) Write(hdr []byte, data mem.BufferSlice, opts *WriteOptions) error {
	return s.inner.Write(hdr, data, toD1ServerWriteOptions(opts))
}
func (s *d1ServerStream) WriteStatus(st *status.Status) error { return s.inner.WriteStatus(st) }
func (s *d1ServerStream) SendHeader(md metadata.MD) error     { return s.inner.SendHeader(md) }
func (s *d1ServerStream) SetHeader(md metadata.MD) error      { return s.inner.SetHeader(md) }
func (s *d1ServerStream) SetTrailer(md metadata.MD) error     { return s.inner.SetTrailer(md) }
func (s *d1ServerStream) Header() (metadata.MD, error)        { return s.inner.Header() }
func (s *d1ServerStream) Trailer() metadata.MD                { return s.inner.Trailer() }
func (s *d1ServerStream) HeaderWireLength() int               { return s.inner.HeaderWireLength() }
func (s *d1ServerStream) Method() string                      { return s.inner.Method() }
func (s *d1ServerStream) Context() context.Context            { return s.inner.Context() }
func (s *d1ServerStream) SetContext(ctx context.Context)      { s.inner.SetContext(ctx) }
func (s *d1ServerStream) SendCompress() string                { return s.inner.SendCompress() }
func (s *d1ServerStream) SetSendCompress(name string) error   { return s.inner.SetSendCompress(name) }
func (s *d1ServerStream) ContentSubtype() string              { return s.inner.ContentSubtype() }
func (s *d1ServerStream) ClientAdvertisedCompressors() []string {
	return s.inner.ClientAdvertisedCompressors()
}

func (s *d1ServerStream) WriteProto(msg any, opts *WriteOptions) (bool, error) {
	pw, ok := s.inner.(expserver.ProtoWriteStream)
	if !ok {
		return false, nil
	}
	pm, ok := msg.(proto.Message)
	if !ok {
		return false, nil
	}
	return pw.WriteProto(pm, proto.Size(pm), toD1ServerWriteOptions(opts))
}

// ---------------------------------------------------------------------------
// Translation helpers (pure)
// ---------------------------------------------------------------------------

func toD1WriteOptions(opts *WriteOptions) expclient.WriteOptions {
	if opts == nil {
		return expclient.WriteOptions{}
	}
	return expclient.WriteOptions{Last: opts.Last}
}

func toD1ServerWriteOptions(opts *WriteOptions) expserver.WriteOptions {
	if opts == nil {
		return expserver.WriteOptions{}
	}
	return expserver.WriteOptions{Last: opts.Last}
}

// splitAcceptedCompressors converts the internal single-valued grpc-accept-encoding
// override (*string, comma-joined, nil == use registry default) into the D1
// []string form (nil == registry default; non-nil incl. empty == explicit
// override).
func splitAcceptedCompressors(v *string) []string {
	if v == nil {
		return nil
	}
	if *v == "" {
		return []string{}
	}
	return strings.Split(*v, ",")
}

// resolveAcceptedCompressors produces the D1 grpc-accept-encoding list from the
// internal override. A nil override means "use the process-wide compressor
// registry" (the D1 CallHdr contract); the adapter resolves that here, on the
// core side, via grpcutil so the self-contained transport never needs the
// registry. An explicit override (including empty) is preserved verbatim.
func resolveAcceptedCompressors(v *string) []string {
	if v == nil {
		if rc := grpcutil.RegisteredCompressors(); rc != "" {
			return strings.Split(rc, ",")
		}
		return nil
	}
	if *v == "" {
		return []string{}
	}
	return strings.Split(*v, ",")
}

// fromD1GoAwayReason maps the public D1 GoAwayReason to the internal one.
func fromD1GoAwayReason(r expclient.GoAwayReason) GoAwayReason {
	switch r {
	case expclient.GoAwayNoReason:
		return GoAwayNoReason
	case expclient.GoAwayTooManyPings:
		return GoAwayTooManyPings
	default:
		return GoAwayInvalid
	}
}
