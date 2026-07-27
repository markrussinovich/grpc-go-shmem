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
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	expclient "google.golang.org/grpc/experimental/transport/client"
	expserver "google.golang.org/grpc/experimental/transport/server"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/resolver"
)

type fakePerRPC struct{}

func (fakePerRPC) GetRequestMetadata(ctx context.Context, uri ...string) (map[string]string, error) {
	return nil, nil
}
func (fakePerRPC) RequireTransportSecurity() bool { return false }

type fakeBundle struct {
	tc credentials.TransportCredentials
	pr credentials.PerRPCCredentials
}

func (b fakeBundle) TransportCredentials() credentials.TransportCredentials { return b.tc }
func (b fakeBundle) PerRPCCredentials() credentials.PerRPCCredentials       { return b.pr }
func (b fakeBundle) NewWithMode(mode string) (credentials.Bundle, error)    { return b, nil }

func TestToD1ClientBuildOptionsBasic(t *testing.T) {
	mhl := uint32(1024)
	var closedErr error
	opts := toD1ClientBuildOptions(ConnectOptions{
		UserAgent:             "ua",
		InitialWindowSize:     65535,
		InitialConnWindowSize: 1048576,
		MaxHeaderListSize:     &mhl,
	}, "auth", func(gi GoAwayInfo) { closedErr = gi.Err })

	if opts.Authority != "auth" || opts.UserAgent != "ua" {
		t.Errorf("authority/ua: got %q/%q", opts.Authority, opts.UserAgent)
	}
	if opts.InitialWindowSize != 65535 || opts.InitialConnWindowSize != 1048576 {
		t.Errorf("windows: got %d/%d", opts.InitialWindowSize, opts.InitialConnWindowSize)
	}
	if opts.MaxHeaderListSize != &mhl {
		t.Errorf("MaxHeaderListSize pointer not passed through")
	}
	// OnClose bridging: D1 CloseInfo.Err -> internal GoAwayInfo.Err.
	sentinel := fmt.Errorf("boom")
	opts.OnClose(expclient.CloseInfo{Err: sentinel})
	if closedErr != sentinel {
		t.Errorf("onClose bridge: got %v, want boom", closedErr)
	}
}

func TestToD1ClientBuildOptionsWindowNormalization(t *testing.T) {
	cases := []struct {
		in   int32
		want uint32
	}{
		{-1, 0},            // negative -> transport default
		{1024, 0},          // positive but below the 64 KiB default -> ignored
		{65535, 65535},     // exactly the default -> applied
		{1048576, 1048576}, // above default -> applied
	}
	for _, c := range cases {
		opts := toD1ClientBuildOptions(ConnectOptions{InitialWindowSize: c.in, InitialConnWindowSize: c.in}, "", nil)
		if opts.InitialWindowSize != c.want || opts.InitialConnWindowSize != c.want {
			t.Errorf("window %d: got stream=%d conn=%d, want %d", c.in, opts.InitialWindowSize, opts.InitialConnWindowSize, c.want)
		}
	}
	if opts := toD1ClientBuildOptions(ConnectOptions{}, "", nil); opts.OnClose != nil {
		t.Errorf("nil onClose must yield nil OnClose")
	}
}

func TestToD1ClientBuildOptionsBundleNormalized(t *testing.T) {
	opts := toD1ClientBuildOptions(ConnectOptions{
		CredsBundle:       fakeBundle{pr: fakePerRPC{}},
		PerRPCCredentials: nil,
	}, "", nil)
	if len(opts.PerRPCCredentials) != 1 {
		t.Fatalf("bundle PerRPCCredentials not normalized in: got %d", len(opts.PerRPCCredentials))
	}
}

func TestToD1ClientBuildOptionsBundleNilTCPreservesChannelCreds(t *testing.T) {
	channelTC := insecure.NewCredentials()
	opts := toD1ClientBuildOptions(ConnectOptions{
		TransportCredentials: channelTC,
		CredsBundle:          fakeBundle{tc: nil, pr: fakePerRPC{}}, // per-RPC-only bundle
	}, "", nil)
	if opts.TransportCredentials != channelTC {
		t.Errorf("per-RPC-only bundle must preserve channel TransportCredentials")
	}
	if len(opts.PerRPCCredentials) != 1 {
		t.Errorf("bundle per-RPC creds not included: %d", len(opts.PerRPCCredentials))
	}
}

var d1WiringRegisterOnce sync.Once

type fakeD1Builder struct{}

func (fakeD1Builder) Build(connectCtx, ctx context.Context, addr resolver.Address, opts expclient.BuildOptions) (expclient.ClientTransport, error) {
	return &fakeD1ClientTransport{stream: &fakeD1ClientStream{}}, nil
}

type fakeD1ErrBuilder struct{}

func (fakeD1ErrBuilder) Build(connectCtx, ctx context.Context, addr resolver.Address, opts expclient.BuildOptions) (expclient.ClientTransport, error) {
	return nil, fmt.Errorf("build failed")
}

func TestBuildD1ClientByType(t *testing.T) {
	// Unregistered type: not found, no error, so the caller falls through.
	_, found, err := BuildD1ClientByType(context.Background(), context.Background(), "nope-unregistered", "", resolver.Address{}, ConnectOptions{}, nil)
	if found || err != nil {
		t.Errorf("unregistered type: found=%v err=%v, want false/nil", found, err)
	}

	d1WiringRegisterOnce.Do(func() {
		expclient.Register("test-d1-wiring", fakeD1Builder{})
		expclient.Register("test-d1-err", fakeD1ErrBuilder{})
	})

	// Registered type: found, built, and wrapped as an internal transport.
	tr, found2, err2 := BuildD1ClientByType(context.Background(), context.Background(), "test-d1-wiring", "a", resolver.Address{}, ConnectOptions{}, nil)
	if !found2 || err2 != nil {
		t.Fatalf("registered type: found=%v err=%v", found2, err2)
	}
	if _, ok := tr.(*d1ClientTransport); !ok {
		t.Errorf("expected wrapped *d1ClientTransport, got %T", tr)
	}

	// Registered builder that fails: found=true with the build error (fail closed).
	tr3, found3, err3 := BuildD1ClientByType(context.Background(), context.Background(), "test-d1-err", "", resolver.Address{}, ConnectOptions{}, nil)
	if !found3 || err3 == nil || tr3 != nil {
		t.Errorf("failing builder: found=%v err=%v tr=%v, want found=true/err!=nil/tr=nil", found3, err3, tr3)
	}
}

func TestToD1ServerBuildOptions(t *testing.T) {
	if got := toD1ServerBuildOptions(nil); got.MaxConcurrentStreams != 0 || got.InitialWindowSize != 0 {
		t.Errorf("nil config must yield zero options, got %+v", got)
	}
	mhl := uint32(2048)
	hts := uint32(4096)
	tc := insecure.NewCredentials()
	pool := mem.DefaultBufferPool()
	got := toD1ServerBuildOptions(&ServerConfig{
		MaxStreams:            100,
		ConnectionTimeout:     5 * time.Second,
		Credentials:           tc,
		KeepaliveParams:       keepalive.ServerParameters{MaxConnectionIdle: 7 * time.Second},
		KeepalivePolicy:       keepalive.EnforcementPolicy{MinTime: 3 * time.Second},
		InitialWindowSize:     1024, // sub-default -> 0
		InitialConnWindowSize: 1048576,
		MaxHeaderListSize:     &mhl,
		HeaderTableSize:       &hts,
		BufferPool:            pool,
	})
	if got.MaxConcurrentStreams != 100 {
		t.Errorf("MaxStreams->MaxConcurrentStreams: got %d", got.MaxConcurrentStreams)
	}
	if got.ConnectionTimeout != 5*time.Second {
		t.Errorf("ConnectionTimeout: got %v", got.ConnectionTimeout)
	}
	if got.Credentials != tc {
		t.Errorf("Credentials not passed through")
	}
	if got.Keepalive.MaxConnectionIdle != 7*time.Second {
		t.Errorf("Keepalive: got %+v", got.Keepalive)
	}
	if got.KeepalivePolicy.MinTime != 3*time.Second {
		t.Errorf("KeepalivePolicy: got %+v", got.KeepalivePolicy)
	}
	if got.InitialWindowSize != 0 {
		t.Errorf("sub-default window must normalize to 0, got %d", got.InitialWindowSize)
	}
	if got.InitialConnWindowSize != 1048576 {
		t.Errorf("conn window: got %d", got.InitialConnWindowSize)
	}
	if got.MaxHeaderListSize != &mhl {
		t.Errorf("MaxHeaderListSize pointer not passed through")
	}
	if got.HeaderTableSize != &hts {
		t.Errorf("HeaderTableSize pointer not passed through")
	}
	if got.BufferPool == nil {
		t.Errorf("BufferPool not passed through")
	}
}

var d1ServerRegisterOnce sync.Once

type fakeD1ServerBuilder struct{}

func (fakeD1ServerBuilder) Build(conn net.Conn, opts expserver.BuildOptions) (expserver.ServerTransport, error) {
	return fakeD1ServerTransport{}, nil
}

func TestBuildD1ServerByType(t *testing.T) {
	// Unregistered: not found, no error -> caller falls through.
	if _, found, err := BuildD1ServerByType(nil, "nope-srv-unregistered", nil); found || err != nil {
		t.Errorf("unregistered server type: found=%v err=%v", found, err)
	}
	d1ServerRegisterOnce.Do(func() { expserver.Register("test-d1-srv", fakeD1ServerBuilder{}) })
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	st, found2, err2 := BuildD1ServerByType(c1, "test-d1-srv", &ServerConfig{})
	if !found2 || err2 != nil {
		t.Fatalf("registered server type: found=%v err=%v", found2, err2)
	}
	if _, ok := st.(*d1ServerTransport); !ok {
		t.Errorf("expected wrapped *d1ServerTransport, got %T", st)
	}
}
