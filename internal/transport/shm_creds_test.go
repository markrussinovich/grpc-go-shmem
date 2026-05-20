//go:build linux || windows

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

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// fakeTLSCreds is a stand-in TransportCredentials whose Info() reports
// a non-insecure SecurityProtocol so the assertion path treats it as
// TLS-equivalent. We never call its ClientHandshake; assertShmCompatibleCredentials
// rejects before reaching it.
type fakeTLSCreds struct {
	protocol string
}

func (f fakeTLSCreds) ClientHandshake(_ context.Context, _ string, _ net.Conn) (net.Conn, credentials.AuthInfo, error) {
	return nil, nil, nil
}
func (f fakeTLSCreds) ServerHandshake(_ net.Conn) (net.Conn, credentials.AuthInfo, error) {
	return nil, nil, nil
}
func (f fakeTLSCreds) Info() credentials.ProtocolInfo {
	return credentials.ProtocolInfo{SecurityProtocol: f.protocol}
}
func (f fakeTLSCreds) Clone() credentials.TransportCredentials { return f }
func (f fakeTLSCreds) OverrideServerName(_ string) error       { return nil }

// fakePerRPCCreds reports the requested RequireTransportSecurity()
// answer to drive the assertion path.
type fakePerRPCCreds struct {
	requireSec bool
}

func (f fakePerRPCCreds) GetRequestMetadata(_ context.Context, _ ...string) (map[string]string, error) {
	return nil, nil
}
func (f fakePerRPCCreds) RequireTransportSecurity() bool { return f.requireSec }

// TestAssertShmCompatibleCredentials_AcceptsInsecure verifies that the
// canonical "insecure.NewCredentials()" plus no PerRPCCredentials, plus
// a nil credentials.Bundle, is accepted.
func TestAssertShmCompatibleCredentials_AcceptsInsecure(t *testing.T) {
	cases := []struct {
		name string
		opts ConnectOptions
	}{
		{"nil-creds", ConnectOptions{}},
		{"insecure-creds", ConnectOptions{TransportCredentials: insecure.NewCredentials()}},
		{"insecure-plus-permissive-prc", ConnectOptions{
			TransportCredentials: insecure.NewCredentials(),
			PerRPCCredentials:    []credentials.PerRPCCredentials{fakePerRPCCreds{requireSec: false}},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := assertShmCompatibleCredentials(c.opts); err != nil {
				t.Errorf("assertShmCompatibleCredentials(%s) = %v; want nil", c.name, err)
			}
		})
	}
}

// TestAssertShmCompatibleCredentials_RejectsTLS verifies that any
// non-insecure transport credentials produce a structured ShmError
// the dialer can use to drive RFC-A73 fallback to HTTP/2.
func TestAssertShmCompatibleCredentials_RejectsTLS(t *testing.T) {
	cases := []struct {
		name string
		opts ConnectOptions
	}{
		{"tls", ConnectOptions{TransportCredentials: fakeTLSCreds{protocol: "tls"}}},
		{"alts", ConnectOptions{TransportCredentials: fakeTLSCreds{protocol: "alts"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := assertShmCompatibleCredentials(c.opts)
			if err == nil {
				t.Fatalf("assertShmCompatibleCredentials(%s) = nil; want error", c.name)
			}
			sErr, ok := err.(*ShmError)
			if !ok {
				t.Fatalf("error type = %T; want *ShmError", err)
			}
			if sErr.Code != ShmErrConnectionRefused {
				t.Errorf("error code = %v; want ShmErrConnectionRefused", sErr.Code)
			}
		})
	}
}

// TestAssertShmCompatibleCredentials_RejectsRequireTransportSecurity
// verifies that PerRPCCredentials whose RequireTransportSecurity() ==
// true are rejected even when the transport credentials are
// insecure-equivalent.
func TestAssertShmCompatibleCredentials_RejectsRequireTransportSecurity(t *testing.T) {
	opts := ConnectOptions{
		TransportCredentials: insecure.NewCredentials(),
		PerRPCCredentials: []credentials.PerRPCCredentials{
			fakePerRPCCreds{requireSec: true},
		},
	}
	err := assertShmCompatibleCredentials(opts)
	if err == nil {
		t.Fatal("assertShmCompatibleCredentials with RequireTransportSecurity=true returned nil; want error")
	}
	sErr, ok := err.(*ShmError)
	if !ok {
		t.Fatalf("error type = %T; want *ShmError", err)
	}
	if sErr.Code != ShmErrConnectionRefused {
		t.Errorf("error code = %v; want ShmErrConnectionRefused", sErr.Code)
	}
}
