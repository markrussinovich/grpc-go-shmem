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

// Core-side "build" layer for the experimental D1 transport: it translates the
// internal kitchen-sink options into the purpose-built public D1 BuildOptions,
// looks a D1 builder up by transport type, builds it, and wraps the result as an
// internal transport (via the d1_adapter.go adapters). This keeps ALL D1<->
// internal translation on the core side; the D1 transport/engine sees only
// public types.
//
// Additive: these helpers are dead until the selection layer (clientconn.go /
// server.go) calls them for an experimental transport type. Stock HTTP/2 and the
// in-tree SHM transport are unaffected.

import (
	"context"
	"fmt"
	"net"

	"google.golang.org/grpc/credentials"
	expclient "google.golang.org/grpc/experimental/transport/client"
	expserver "google.golang.org/grpc/experimental/transport/server"
	"google.golang.org/grpc/resolver"
)

// normalizeWindow converts an internal int32 flow-control window to the public
// uint32 form, applying grpc-go's documented rule that a window BELOW the 64 KiB
// default is ignored (treated as 0 == "transport default"). This mirrors the
// HTTP/2 client, which applies InitialWindowSize only when it is at least
// defaultWindowSize (http2_client.go), and the WithInitial[Conn]WindowSize
// dial-option promise (dialoptions.go).
func normalizeWindow(v int32) uint32 {
	if v < defaultWindowSize {
		return 0
	}
	return uint32(v)
}

// toD1ClientBuildOptions maps internal ConnectOptions to the public D1
// BuildOptions. It normalizes a credentials.Bundle into TransportCredentials +
// PerRPCCredentials (the D1 contract never sees a Bundle), bridges the internal
// OnCloseFunc, and drops above-the-seam concerns (stats handlers, socket buffer
// sizes, shared write buffer, channelz, static-window policy).
func toD1ClientBuildOptions(copts ConnectOptions, authority string, onClose OnCloseFunc) expclient.BuildOptions {
	tc := copts.TransportCredentials
	perRPC := copts.PerRPCCredentials
	if copts.CredsBundle != nil {
		// Only override channel transport credentials when the bundle actually
		// supplies them; a per-RPC-only bundle must preserve the existing channel
		// credentials (matches the HTTP/2 client).
		if btc := copts.CredsBundle.TransportCredentials(); btc != nil {
			tc = btc
		}
		if pr := copts.CredsBundle.PerRPCCredentials(); pr != nil {
			perRPC = append(append([]credentials.PerRPCCredentials{}, perRPC...), pr)
		}
	}
	var onCloseD1 func(expclient.CloseInfo)
	if onClose != nil {
		onCloseD1 = func(ci expclient.CloseInfo) { onClose(GoAwayInfo{Err: ci.Err}) }
	}
	return expclient.BuildOptions{
		Authority:             authority,
		UserAgent:             copts.UserAgent,
		Dialer:                copts.Dialer,
		TransportCredentials:  tc,
		PerRPCCredentials:     perRPC,
		Keepalive:             copts.KeepaliveParams,
		InitialWindowSize:     normalizeWindow(copts.InitialWindowSize),
		InitialConnWindowSize: normalizeWindow(copts.InitialConnWindowSize),
		MaxHeaderListSize:     copts.MaxHeaderListSize,
		BufferPool:            copts.BufferPool,
		OnClose:               onCloseD1,
	}
}

// BuildD1ClientByType looks up an experimental client transport builder for
// transportType and, if found, builds and wraps it as an internal
// ClientTransport. The bool reports whether a builder was found; the caller
// treats false as fail-closed for a non-empty transport type, and an empty type
// never reaches here.
func BuildD1ClientByType(connectCtx, ctx context.Context, transportType, authority string, addr resolver.Address, copts ConnectOptions, onClose OnCloseFunc) (ClientTransport, bool, error) {
	b := expclient.Get(transportType)
	if b == nil {
		return nil, false, nil
	}
	d1, err := b.Build(connectCtx, ctx, addr, toD1ClientBuildOptions(copts, authority, onClose))
	if err != nil {
		return nil, true, err
	}
	// A Builder lives outside grpc-go, so treat its output as untrusted: a nil
	// transport with a nil error violates the contract and would be wrapped into
	// a non-nil adapter that panics on first use.
	if d1 == nil {
		return nil, true, fmt.Errorf("grpc: transport builder for %q returned a nil transport and a nil error", transportType)
	}
	return newD1ClientTransport(d1), true, nil
}

// toD1ServerBuildOptions maps an internal ServerConfig to the public D1 server
// BuildOptions, dropping above-the-seam concerns (tap, stats, socket buffers,
// shared write buffer, channelz, static-window policy).
func toD1ServerBuildOptions(config *ServerConfig) expserver.BuildOptions {
	if config == nil {
		return expserver.BuildOptions{}
	}
	return expserver.BuildOptions{
		Credentials:           config.Credentials,
		ConnectionTimeout:     config.ConnectionTimeout,
		MaxConcurrentStreams:  config.MaxStreams,
		Keepalive:             config.KeepaliveParams,
		KeepalivePolicy:       config.KeepalivePolicy,
		InitialWindowSize:     normalizeWindow(config.InitialWindowSize),
		InitialConnWindowSize: normalizeWindow(config.InitialConnWindowSize),
		MaxHeaderListSize:     config.MaxHeaderListSize,
		HeaderTableSize:       config.HeaderTableSize,
		BufferPool:            config.BufferPool,
	}
}

// BuildD1ServerByType looks up an experimental server transport builder for
// transportType and, if found, builds and wraps it as an internal
// ServerTransport. The bool reports whether a builder was found; the caller
// treats false as fail-closed for a non-empty transport type.
func BuildD1ServerByType(conn net.Conn, transportType string, config *ServerConfig) (ServerTransport, bool, error) {
	b := expserver.Get(transportType)
	if b == nil {
		return nil, false, nil
	}
	st, err := b.Build(conn, toD1ServerBuildOptions(config))
	if err != nil {
		return nil, true, err
	}
	// See BuildD1ClientByType: builder output is untrusted.
	if st == nil {
		return nil, true, fmt.Errorf("grpc: transport builder for %q returned a nil transport and a nil error", transportType)
	}
	return newD1ServerTransport(st), true, nil
}
