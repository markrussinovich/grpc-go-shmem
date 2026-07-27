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

package shmsc

import (
	"context"
	"net"

	transportclient "google.golang.org/grpc/experimental/transport/client"
	transportserver "google.golang.org/grpc/experimental/transport/server"
	"google.golang.org/grpc/plugin/shmsc/internal/engine"
	"google.golang.org/grpc/resolver"
)

// clientBuilder is the D1 client transport Builder for the self-contained SHM
// transport. grpc-go selects it when a resolver.Address carries
// TransportType == Name; it dials the segment named by addr.Addr.
type clientBuilder struct{}

func (clientBuilder) Build(connectCtx, _ context.Context, addr resolver.Address, opts transportclient.BuildOptions) (transportclient.ClientTransport, error) {
	return engine.DialClient(connectCtx, addr.Addr, opts)
}

// serverBuilder is the D1 server transport Builder. It adopts the server
// transport carried by an accepted shared-memory connection (see Listener).
type serverBuilder struct{}

func (serverBuilder) Build(conn net.Conn, opts transportserver.BuildOptions) (transportserver.ServerTransport, error) {
	return engine.BuildServer(conn, opts)
}

func init() {
	transportclient.Register(Name, clientBuilder{})
	transportserver.Register(Name, serverBuilder{})
}

// Listener wraps a shared-memory net.Listener so grpc-go routes its accepted
// connections to this plugin's server Builder. grpc-go's server dispatch selects
// a registered server Builder by the accepted connection's TransportType(); this
// wrapper tags each connection with TransportType() == Name and exposes the
// underlying connection via Unwrap so the engine's server Builder can recover it.
//
// Usage: grpcServer.Serve(shmsc.NewListener(shmListener)).
type Listener struct {
	net.Listener
}

// NewListener wraps a shared-memory net.Listener so its connections are tagged
// for the shmsc server Builder.
func NewListener(inner net.Listener) *Listener {
	return &Listener{Listener: inner}
}

// Listen creates a shared-memory listener for the given segment name and wraps
// it so grpc-go's server dispatch routes accepted connections to this plugin's
// server Builder. Pass the result to grpc.Server.Serve.
func Listen(name string) (net.Listener, error) {
	lis, err := engine.NewShmListener(&engine.ShmAddr{Name: name}, engine.DefaultSegmentSize, engine.DefaultRingASize, engine.DefaultRingBSize)
	if err != nil {
		return nil, err
	}
	return NewListener(lis), nil
}

// Accept accepts the next connection and tags it with the shmsc transport type.
func (l *Listener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return taggedConn{Conn: c}, nil
}

// taggedConn tags a shared-memory connection with the shmsc transport type and
// exposes the underlying connection via Unwrap.
type taggedConn struct {
	net.Conn
}

// TransportType reports the transport type grpc-go's server dispatch matches
// against the registered server Builder.
func (taggedConn) TransportType() string { return Name }

// Unwrap returns the underlying shared-memory connection so the engine's server
// Builder can recover the carried server transport.
func (c taggedConn) Unwrap() net.Conn { return c.Conn }
