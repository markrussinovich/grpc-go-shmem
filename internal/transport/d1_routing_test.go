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
	"net"
	"testing"

	expclient "google.golang.org/grpc/experimental/transport/client"
	expserver "google.golang.org/grpc/experimental/transport/server"
	"google.golang.org/grpc/resolver"
)

// fakeConn is a net.Conn whose only interesting property is that Close is
// observable.
type fakeConn struct {
	net.Conn
	closed bool
}

func (c *fakeConn) Close() error {
	c.closed = true
	return nil
}

func (c *fakeConn) RemoteAddr() net.Addr { return fakeAddr{} }

type fakeAddr struct{}

func (fakeAddr) Network() string { return "fake" }
func (fakeAddr) String() string  { return "fake-addr" }

// TestBuildD1ServerByTypeUnregistered verifies that an unregistered transport
// type reports found==false, which is what lets the server fail closed instead
// of handing non-HTTP/2 bytes to the HTTP/2 parser.
func TestBuildD1ServerByTypeUnregistered(t *testing.T) {
	c := &fakeConn{}
	st, found, err := BuildD1ServerByType(c, "d1-never-registered", &ServerConfig{})
	if found {
		t.Fatalf("BuildD1ServerByType(unregistered) found = true; want false")
	}
	if st != nil || err != nil {
		t.Fatalf("BuildD1ServerByType(unregistered) = %v, %v; want nil, nil", st, err)
	}
}

// TestBuildD1ServerByTypeEmptyName verifies the empty transport type is treated
// as "not registered", so a connection that reports no type falls through to the
// default transport rather than being closed.
func TestBuildD1ServerByTypeEmptyName(t *testing.T) {
	c := &fakeConn{}
	_, found, err := BuildD1ServerByType(c, "", &ServerConfig{})
	if found || err != nil {
		t.Fatalf("BuildD1ServerByType(\"\") = found %v, err %v; want false, nil", found, err)
	}
}

// nilServerBuilder is a hostile plugin: it violates the contract by returning a
// nil transport together with a nil error.
type nilServerBuilder struct{}

func (nilServerBuilder) Build(net.Conn, expserver.BuildOptions) (expserver.ServerTransport, error) {
	return nil, nil
}

// nilClientBuilder is the client-side equivalent hostile plugin.
type nilClientBuilder struct{}

func (nilClientBuilder) Build(context.Context, context.Context, resolver.Address, expclient.BuildOptions) (expclient.ClientTransport, error) {
	return nil, nil
}

// TestBuildD1ByTypeRejectsNilTransport pins the trust boundary: a builder that
// returns (nil, nil) must produce an error rather than a non-nil adapter that
// panics the first time grpc-go touches it.
func TestBuildD1ByTypeRejectsNilTransport(t *testing.T) {
	expserver.Register("d1-nil-server", nilServerBuilder{})
	expclient.Register("d1-nil-client", nilClientBuilder{})

	st, found, err := BuildD1ServerByType(&fakeConn{}, "d1-nil-server", &ServerConfig{})
	if !found {
		t.Fatalf("BuildD1ServerByType(registered) found = false; want true")
	}
	if err == nil {
		t.Fatalf("BuildD1ServerByType with a nil-returning builder = %v, nil; want an error", st)
	}
	if st != nil {
		t.Fatalf("BuildD1ServerByType returned transport %v alongside an error; want nil", st)
	}

	ctx := context.Background()
	ct, found, err := BuildD1ClientByType(ctx, ctx, "d1-nil-client", "authority", resolver.Address{Addr: "a"}, ConnectOptions{}, nil)
	if !found {
		t.Fatalf("BuildD1ClientByType(registered) found = false; want true")
	}
	if err == nil {
		t.Fatalf("BuildD1ClientByType with a nil-returning builder = %v, nil; want an error", ct)
	}
	if ct != nil {
		t.Fatalf("BuildD1ClientByType returned transport %v alongside an error; want nil", ct)
	}
}
