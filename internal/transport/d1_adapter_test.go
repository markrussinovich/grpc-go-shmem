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

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"google.golang.org/grpc/codes"
	expclient "google.golang.org/grpc/experimental/transport/client"
	expserver "google.golang.org/grpc/experimental/transport/server"
	"google.golang.org/grpc/internal/grpcutil"
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// fakeAuthInfo implements credentials.AuthInfo and, optionally,
// credentials.AuthorityValidator.
type fakeAuthInfo struct{ rejectAuthority bool }

func (fakeAuthInfo) AuthType() string { return "fake" }
func (a fakeAuthInfo) ValidateAuthority(authority string) error {
	if a.rejectAuthority {
		return fmt.Errorf("rejected authority %q", authority)
	}
	return nil
}

func unavailable(t *testing.T, err error) {
	t.Helper()
	nse, ok := err.(*NewStreamError)
	if !ok {
		t.Fatalf("expected *NewStreamError, got %T: %v", err, err)
	}
	if status.Code(nse.Err) != codes.Unavailable {
		t.Fatalf("expected UNAVAILABLE, got %v", nse.Err)
	}
}

func TestD1TranslateNewStreamErr(t *testing.T) {
	inner := status.Error(codes.Unavailable, "boom")

	// Public retryable NewStreamError -> internal *NewStreamError, bit preserved.
	got := translateNewStreamErr(&expclient.NewStreamError{Err: inner, AllowTransparentRetry: true})
	nse, ok := got.(*NewStreamError)
	if !ok {
		t.Fatalf("expected *NewStreamError, got %T", got)
	}
	if !nse.AllowTransparentRetry || nse.Err != inner {
		t.Errorf("translation lost fields: retry=%v err=%v", nse.AllowTransparentRetry, nse.Err)
	}

	// Non-retryable preserved as false.
	if nse2, ok := translateNewStreamErr(&expclient.NewStreamError{Err: inner}).(*NewStreamError); !ok || nse2.AllowTransparentRetry {
		t.Errorf("non-retry translation wrong")
	}

	// Wrapped: errors.As must still find and translate it.
	wrapped := fmt.Errorf("ctx: %w", &expclient.NewStreamError{Err: inner, AllowTransparentRetry: true})
	if nse3, ok := translateNewStreamErr(wrapped).(*NewStreamError); !ok || !nse3.AllowTransparentRetry {
		t.Errorf("wrapped translation failed")
	}

	// Plain error passes through unchanged.
	plain := errors.New("plain")
	if translateNewStreamErr(plain) != plain {
		t.Errorf("plain error must pass through unchanged")
	}

	// Public error Error()/Unwrap() behavior.
	pub := &expclient.NewStreamError{Err: inner, AllowTransparentRetry: true}
	if pub.Error() != inner.Error() || !errors.Is(pub, inner) {
		t.Errorf("NewStreamError Error()/Unwrap() broken: %q is=%v", pub.Error(), errors.Is(pub, inner))
	}
}

func TestD1ResolveAuthority(t *testing.T) {
	// no override -> host, no validation
	if got, err := newD1ClientTransport(&fakeD1ClientTransport{}).resolveAuthority("", "default-host"); err != nil || got != "default-host" {
		t.Errorf("no override: got (%q,%v), want (default-host,nil)", got, err)
	}
	// override + valid validator -> override
	okT := newD1ClientTransport(&fakeD1ClientTransport{sec: expclient.SecurityInfo{AuthInfo: fakeAuthInfo{}}})
	if got, err := okT.resolveAuthority("override", "h"); err != nil || got != "override" {
		t.Errorf("valid override: got (%q,%v), want (override,nil)", got, err)
	}
	// override + no validator (nil AuthInfo) -> UNAVAILABLE
	_, err := newD1ClientTransport(&fakeD1ClientTransport{}).resolveAuthority("override", "h")
	unavailable(t, err)
	// override + failing validator -> UNAVAILABLE
	_, err = newD1ClientTransport(&fakeD1ClientTransport{sec: expclient.SecurityInfo{AuthInfo: fakeAuthInfo{rejectAuthority: true}}}).resolveAuthority("bad", "h")
	unavailable(t, err)
}

func TestD1SplitAcceptedCompressors(t *testing.T) {
	if got := splitAcceptedCompressors(nil); got != nil {
		t.Errorf("nil override must map to nil (registry default), got %v", got)
	}
	empty := ""
	if got := splitAcceptedCompressors(&empty); got == nil || len(got) != 0 {
		t.Errorf("empty override must map to non-nil empty slice, got %v (nil=%v)", got, got == nil)
	}
	val := "gzip,snappy"
	got := splitAcceptedCompressors(&val)
	if len(got) != 2 || got[0] != "gzip" || got[1] != "snappy" {
		t.Errorf("comma-joined override mis-split: got %v", got)
	}
}

func TestD1ResolveAcceptedCompressors(t *testing.T) {
	// Explicit overrides pass through unchanged (nil-vs-empty preserved).
	empty := ""
	if got := resolveAcceptedCompressors(&empty); got == nil || len(got) != 0 {
		t.Errorf("explicit empty must be non-nil empty slice, got %v (nil=%v)", got, got == nil)
	}
	val := "gzip,snappy"
	if got := resolveAcceptedCompressors(&val); len(got) != 2 || got[0] != "gzip" {
		t.Errorf("explicit override mis-split: got %v", got)
	}

	// A nil override resolves the process-wide compressor registry on the core
	// side, so the self-contained transport receives an explicit list.
	saved := grpcutil.RegisteredCompressorNames
	defer func() { grpcutil.RegisteredCompressorNames = saved }()
	grpcutil.RegisteredCompressorNames = []string{"gzip", "deflate"}
	if got := resolveAcceptedCompressors(nil); len(got) != 2 || got[0] != "gzip" || got[1] != "deflate" {
		t.Errorf("nil override must resolve registry, got %v", got)
	}
	grpcutil.RegisteredCompressorNames = nil
	if got := resolveAcceptedCompressors(nil); got != nil {
		t.Errorf("nil override with empty registry must be nil, got %v", got)
	}
}

func TestD1WriteOptionsTranslation(t *testing.T) {
	if got := toD1WriteOptions(nil); got.Last {
		t.Errorf("nil opts must map to zero (Last=false)")
	}
	if got := toD1WriteOptions(&WriteOptions{Last: true}); !got.Last {
		t.Errorf("Last must pass through")
	}
	if got := toD1ServerWriteOptions(&WriteOptions{Last: true}); !got.Last {
		t.Errorf("server Last must pass through")
	}
}

func TestD1GoAwayReasonMapping(t *testing.T) {
	cases := []struct {
		in   expclient.GoAwayReason
		want GoAwayReason
	}{
		{expclient.GoAwayNoReason, GoAwayNoReason},
		{expclient.GoAwayTooManyPings, GoAwayTooManyPings},
		{expclient.GoAwayInvalid, GoAwayInvalid},
	}
	for _, c := range cases {
		if got := fromD1GoAwayReason(c.in); got != c.want {
			t.Errorf("fromD1GoAwayReason(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

// --- adapter behavior via capturing fakes ---

type fakeD1ClientStream struct{}

func (s *fakeD1ClientStream) Write(hdr []byte, data mem.BufferSlice, opts expclient.WriteOptions) error {
	return nil
}
func (s *fakeD1ClientStream) ReadMessageHeader(header []byte) error { return nil }
func (s *fakeD1ClientStream) Read(n int) (mem.BufferSlice, error)   { return nil, nil }
func (s *fakeD1ClientStream) RecvCompress() string                 { return "" }
func (s *fakeD1ClientStream) Header() (metadata.MD, error)         { return nil, nil }
func (s *fakeD1ClientStream) Trailer() metadata.MD                 { return nil }
func (s *fakeD1ClientStream) Status() *status.Status               { return nil }
func (s *fakeD1ClientStream) Context() context.Context             { return context.Background() }
func (s *fakeD1ClientStream) Done() <-chan struct{}                { return nil }
func (s *fakeD1ClientStream) Unprocessed() bool                    { return false }
func (s *fakeD1ClientStream) TrailersOnly() bool                   { return false }
func (s *fakeD1ClientStream) BytesReceived() bool                  { return false }
func (s *fakeD1ClientStream) Close(err error)                      {}

// protoFakeD1ClientStream additionally implements the optional capability.
type protoFakeD1ClientStream struct {
	fakeD1ClientStream
	gotMsg  proto.Message
	gotSize int
	gotLast bool
	retErr  error
}

func (s *protoFakeD1ClientStream) WriteProto(msg proto.Message, size int, opts expclient.WriteOptions) (bool, error) {
	s.gotMsg = msg
	s.gotSize = size
	s.gotLast = opts.Last
	return true, s.retErr
}

type fakeD1ClientTransport struct {
	gotHdr *expclient.CallHdr
	stream expclient.ClientStream
	reason expclient.GoAwayReason
	sec    expclient.SecurityInfo
}

func (t *fakeD1ClientTransport) NewStream(ctx context.Context, callHdr *expclient.CallHdr) (expclient.ClientStream, error) {
	t.gotHdr = callHdr
	return t.stream, nil
}
func (t *fakeD1ClientTransport) Close(err error)                      {}
func (t *fakeD1ClientTransport) GracefulClose()                       {}
func (t *fakeD1ClientTransport) Error() <-chan struct{}               { return nil }
func (t *fakeD1ClientTransport) GoAway() <-chan struct{}              { return nil }
func (t *fakeD1ClientTransport) GetGoAwayReason() (expclient.GoAwayReason, string) {
	return t.reason, "dbg"
}
func (t *fakeD1ClientTransport) Peer() *peer.Peer { return nil }
func (t *fakeD1ClientTransport) SecurityInfo() expclient.SecurityInfo {
	return t.sec
}

func TestD1ClientNewStreamTranslatesCallHdr(t *testing.T) {
	acc := "gzip"
	fake := &fakeD1ClientTransport{
		stream: &fakeD1ClientStream{},
		sec:    expclient.SecurityInfo{AuthInfo: fakeAuthInfo{}}, // validator accepts overrides
	}
	adapter := newD1ClientTransport(fake)

	_, err := adapter.NewStream(context.Background(), &CallHdr{
		Host:                "default-host",
		Authority:           "override-authority",
		Method:              "/svc/M",
		SendCompress:        "gzip",
		AcceptedCompressors: &acc,
		ContentSubtype:      "proto",
		PreviousAttempts:    2,
	}, nil)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	got := fake.gotHdr
	if got == nil {
		t.Fatal("D1 transport did not receive a CallHdr")
	}
	if got.Authority != "override-authority" {
		t.Errorf("authority not honored (bug regression): got %q, want %q", got.Authority, "override-authority")
	}
	if got.Method != "/svc/M" {
		t.Errorf("method: got %q", got.Method)
	}
	if len(got.AcceptedCompressors) != 1 || got.AcceptedCompressors[0] != "gzip" {
		t.Errorf("AcceptedCompressors mis-translated: %v", got.AcceptedCompressors)
	}
	if got.PreviousAttempts != 2 {
		t.Errorf("PreviousAttempts: got %d", got.PreviousAttempts)
	}
}

func TestD1ClientWriteProtoForwardsWithSize(t *testing.T) {
	msg := wrapperspb.String("hello inline")
	wantSize := proto.Size(msg)

	// Stream implementing the optional capability: WriteProto is forwarded with
	// the validated size and reports handled=true.
	ps := &protoFakeD1ClientStream{}
	cs := &d1ClientStream{inner: ps}
	handled, err := cs.WriteProto(msg, &WriteOptions{Last: true})
	if err != nil {
		t.Fatalf("WriteProto: %v", err)
	}
	if !handled {
		t.Fatal("expected handled=true when the D1 stream implements ProtoWriteStream")
	}
	if ps.gotSize != wantSize {
		t.Errorf("forwarded size: got %d, want proto.Size %d", ps.gotSize, wantSize)
	}
	if !proto.Equal(ps.gotMsg, msg) {
		t.Errorf("forwarded msg mismatch")
	}

	// Stream WITHOUT the capability: decline cleanly so core uses byte Write.
	cs2 := &d1ClientStream{inner: &fakeD1ClientStream{}}
	handled2, err2 := cs2.WriteProto(msg, nil)
	if handled2 || err2 != nil {
		t.Errorf("no-capability stream must decline cleanly: handled=%v err=%v", handled2, err2)
	}
}

func TestD1ClientGetGoAwayReason(t *testing.T) {
	fake := &fakeD1ClientTransport{reason: expclient.GoAwayTooManyPings}
	adapter := newD1ClientTransport(fake)
	r, dbg := adapter.GetGoAwayReason()
	if r != GoAwayTooManyPings {
		t.Errorf("GoAwayReason mapping: got %v", r)
	}
	if dbg != "dbg" {
		t.Errorf("debug string not forwarded: %q", dbg)
	}
}

// --- server adapter ---

type fakeD1ServerStream struct{}

func (fakeD1ServerStream) ReadMessageHeader(header []byte) error                                { return nil }
func (fakeD1ServerStream) Read(n int) (mem.BufferSlice, error)                                  { return nil, nil }
func (fakeD1ServerStream) RecvCompress() string                                                { return "" }
func (fakeD1ServerStream) Write(hdr []byte, data mem.BufferSlice, opts expserver.WriteOptions) error { return nil }
func (fakeD1ServerStream) WriteStatus(st *status.Status) error                                  { return nil }
func (fakeD1ServerStream) SendHeader(md metadata.MD) error                                      { return nil }
func (fakeD1ServerStream) SetHeader(md metadata.MD) error                                       { return nil }
func (fakeD1ServerStream) SetTrailer(md metadata.MD) error                                      { return nil }
func (fakeD1ServerStream) Header() (metadata.MD, error)                                         { return nil, nil }
func (fakeD1ServerStream) Trailer() metadata.MD                                                 { return nil }
func (fakeD1ServerStream) HeaderWireLength() int                                                { return 0 }
func (fakeD1ServerStream) Method() string                                                       { return "/svc/M" }
func (fakeD1ServerStream) Context() context.Context                                             { return context.Background() }
func (fakeD1ServerStream) SetContext(ctx context.Context)                                       {}
func (fakeD1ServerStream) SendCompress() string                                                { return "" }
func (fakeD1ServerStream) SetSendCompress(name string) error                                    { return nil }
func (fakeD1ServerStream) ContentSubtype() string                                              { return "" }
func (fakeD1ServerStream) ClientAdvertisedCompressors() []string                                { return nil }

type fakeD1ServerTransport struct{}

func (fakeD1ServerTransport) HandleStreams(ctx context.Context, onStream func(expserver.ServerStream)) {
	onStream(fakeD1ServerStream{})
}
func (fakeD1ServerTransport) Drain(debugData string) {}
func (fakeD1ServerTransport) Close(err error)        {}
func (fakeD1ServerTransport) Peer() *peer.Peer       { return nil }

func TestD1ServerHandleStreamsWraps(t *testing.T) {
	adapter := newD1ServerTransport(fakeD1ServerTransport{})
	var got ServerStreamIface
	adapter.HandleStreams(context.Background(), func(s ServerStreamIface) { got = s })
	if got == nil {
		t.Fatal("handler did not receive a wrapped server stream")
	}
	if _, ok := got.(*d1ServerStream); !ok {
		t.Errorf("expected *d1ServerStream, got %T", got)
	}
	if got.Method() != "/svc/M" {
		t.Errorf("method forwarded: %q", got.Method())
	}
}

func TestD1ClientNewStreamHostFallback(t *testing.T) {
	fake := &fakeD1ClientTransport{stream: &fakeD1ClientStream{}}
	if _, err := newD1ClientTransport(fake).NewStream(context.Background(), &CallHdr{Host: "default-host", Method: "/s/M"}, nil); err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	if fake.gotHdr.Authority != "default-host" {
		t.Errorf("no-override should pass Host as effective authority, got %q", fake.gotHdr.Authority)
	}
}

func TestD1ClientWriteProtoErrPropagation(t *testing.T) {
	msg := wrapperspb.String("x")
	sentinel := fmt.Errorf("boom")
	ps := &protoFakeD1ClientStream{retErr: sentinel}
	cs := &d1ClientStream{inner: ps}
	handled, err := cs.WriteProto(msg, &WriteOptions{Last: true})
	if !handled || err != sentinel {
		t.Errorf("ownership: handled=%v err=%v, want handled=true err=boom", handled, err)
	}
	if !ps.gotLast {
		t.Error("Last not forwarded to WriteProto")
	}
}

type protoFakeD1ServerStream struct {
	fakeD1ServerStream
	gotSize int
	gotLast bool
}

func (s *protoFakeD1ServerStream) WriteProto(msg proto.Message, size int, opts expserver.WriteOptions) (bool, error) {
	s.gotSize = size
	s.gotLast = opts.Last
	return true, nil
}

func TestD1ServerWriteProto(t *testing.T) {
	msg := wrapperspb.String("srv")
	ps := &protoFakeD1ServerStream{}
	ss := &d1ServerStream{inner: ps}
	handled, err := ss.WriteProto(msg, &WriteOptions{Last: true})
	if !handled || err != nil {
		t.Fatalf("server WriteProto: handled=%v err=%v", handled, err)
	}
	if ps.gotSize != proto.Size(msg) || !ps.gotLast {
		t.Errorf("server forward: size=%d (want %d) last=%v", ps.gotSize, proto.Size(msg), ps.gotLast)
	}
	// decline path: server stream without the capability.
	if h, e := (&d1ServerStream{inner: fakeD1ServerStream{}}).WriteProto(msg, nil); h || e != nil {
		t.Errorf("no-capability server must decline: h=%v e=%v", h, e)
	}
}
