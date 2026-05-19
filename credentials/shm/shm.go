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

// Package shm implements shared memory transport credentials.
//
// Shared memory credentials provide process-level authentication by exchanging
// identity tokens during the transport handshake. Since shared memory is
// inherently local (same machine), this provides PrivacyAndIntegrity security
// level similar to Unix domain sockets.
//
// # Usage
//
// For insecure local shared memory communication (default):
//
//	creds := shm.NewCredentials()
//	conn, err := grpc.Dial("shm:segment_name", grpc.WithTransportCredentials(creds))
//
// For custom identity verification:
//
//	creds := shm.NewCredentialsWithOptions(shm.Options{
//	    Identity: "my-service",
//	    VerifyIdentity: func(remote string) error {
//	        if !strings.HasPrefix(remote, "trusted-") {
//	            return errors.New("untrusted identity")
//	        }
//	        return nil
//	    },
//	})
//
// # Experimental
//
// Notice: This package is EXPERIMENTAL and may be changed or removed in a
// later release.
package shm

import (
	"context"
	"fmt"
	"net"
	"os"

	"google.golang.org/grpc/credentials"
)

// Info contains the auth information for a shared memory connection.
// It implements the AuthInfo interface.
type Info struct {
	credentials.CommonAuthInfo
	// LocalIdentity is the identity of the local process
	LocalIdentity string
	// RemoteIdentity is the identity of the remote process
	RemoteIdentity string
}

// AuthType returns the type of Info as a string.
func (Info) AuthType() string {
	return "shm"
}

// ValidateAuthority allows any value to be overridden for the :authority
// header in shared memory connections.
func (Info) ValidateAuthority(string) error {
	return nil
}

// Options configures the shared memory credentials.
type Options struct {
	// Identity is the local identity token to present during handshake.
	// If empty, defaults to "pid:<process_id>".
	Identity string

	// VerifyIdentity is an optional function to validate the remote identity.
	// Returns nil if identity is valid, error otherwise.
	// If nil, all identities are accepted.
	VerifyIdentity func(remoteIdentity string) error
}

// shmTC is the credentials required to establish a shared memory connection.
type shmTC struct {
	info     credentials.ProtocolInfo
	identity string
	verify   func(remoteIdentity string) error
}

// Info provides the ProtocolInfo of this TransportCredentials.
func (c *shmTC) Info() credentials.ProtocolInfo {
	return c.info
}

// ClientHandshake performs the client-side handshake for shared memory transport.
//
// The actual identity exchange happens at the transport layer (see
// internal/transport.ShmSecurityHandshaker), which mutually exchanges
// per-side identity tokens during the SHM control-segment handshake.
// By the time this method runs, the conn already carries an AuthInfo
// (typically a transport.ShmAuthInfo) populated with the verified peer
// identity. This method simply forwards that AuthInfo and applies the
// caller-supplied VerifyIdentity callback, if any.
//
// If conn does not expose a transport-level AuthInfo (e.g. the conn was
// produced by a test harness that bypassed the handshake), this method
// returns Info with RemoteIdentity == "" and SecurityLevel
// PrivacyAndIntegrity. A non-nil VerifyIdentity callback configured via
// Options will be invoked with that empty string so the caller can
// decide whether to reject the connection.
func (c *shmTC) ClientHandshake(_ context.Context, _ string, conn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	// Verify this is a shared memory connection
	if conn.RemoteAddr().Network() != "shm" {
		return nil, nil, fmt.Errorf("shm credentials require shm network, got %q", conn.RemoteAddr().Network())
	}

	// Prefer the transport-layer AuthInfo if available (real handshake).
	var authInfo credentials.AuthInfo = Info{
		CommonAuthInfo: credentials.CommonAuthInfo{
			SecurityLevel: credentials.PrivacyAndIntegrity,
		},
		LocalIdentity: c.identity,
	}
	if p, ok := conn.(interface{ AuthInfo() credentials.AuthInfo }); ok {
		if ai := p.AuthInfo(); ai != nil {
			authInfo = ai
		}
	}

	if err := c.runVerify(authInfo); err != nil {
		return nil, nil, err
	}
	return conn, authInfo, nil
}

// ServerHandshake performs the server-side handshake for shared memory transport.
//
// See ClientHandshake for the model: the actual identity exchange happens
// at the transport layer; this method forwards the resulting AuthInfo
// and applies the caller-supplied VerifyIdentity callback, if any.
func (c *shmTC) ServerHandshake(conn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	// Verify this is a shared memory connection
	if conn.RemoteAddr().Network() != "shm" {
		return nil, nil, fmt.Errorf("shm credentials require shm network, got %q", conn.RemoteAddr().Network())
	}

	var authInfo credentials.AuthInfo = Info{
		CommonAuthInfo: credentials.CommonAuthInfo{
			SecurityLevel: credentials.PrivacyAndIntegrity,
		},
		LocalIdentity: c.identity,
	}
	if p, ok := conn.(interface{ AuthInfo() credentials.AuthInfo }); ok {
		if ai := p.AuthInfo(); ai != nil {
			authInfo = ai
		}
	}

	if err := c.runVerify(authInfo); err != nil {
		return nil, nil, err
	}
	return conn, authInfo, nil
}

// runVerify invokes the caller-supplied VerifyIdentity callback (if any)
// against the RemoteIdentity carried by the handshake's AuthInfo. The
// AuthInfo may be either this package's Info or the internal transport
// ShmAuthInfo (which exposes RemoteIdentity through the
// remoteIdentityCarrier duck-typed interface so this package does not
// have to import internal/transport).
func (c *shmTC) runVerify(ai credentials.AuthInfo) error {
	if c.verify == nil {
		return nil
	}
	var remote string
	switch v := ai.(type) {
	case Info:
		remote = v.RemoteIdentity
	case remoteIdentityCarrier:
		remote = v.GetRemoteIdentity()
	}
	return c.verify(remote)
}

// remoteIdentityCarrier is satisfied by any AuthInfo implementation that
// can surface a remote identity string. Notably the internal transport's
// ShmAuthInfo type implements this method, allowing this package to
// extract the verified peer identity without taking a build-time
// dependency on internal/transport.
type remoteIdentityCarrier interface {
	GetRemoteIdentity() string
}

// Clone makes a copy of shared memory credentials.
func (c *shmTC) Clone() credentials.TransportCredentials {
	return &shmTC{
		info:     c.info,
		identity: c.identity,
		verify:   c.verify,
	}
}

// OverrideServerName overrides the server name used in the handshake.
// For shared memory this doesn't have the same meaning as TLS SNI,
// but we store it for compatibility.
func (c *shmTC) OverrideServerName(serverNameOverride string) error {
	c.info.ServerName = serverNameOverride
	return nil
}

// NewCredentials returns shared memory credentials with default options.
// The default identity is "pid:<process_id>".
func NewCredentials() credentials.TransportCredentials {
	return NewCredentialsWithOptions(Options{})
}

// NewCredentialsWithOptions returns shared memory credentials with custom options.
func NewCredentialsWithOptions(opts Options) credentials.TransportCredentials {
	identity := opts.Identity
	if identity == "" {
		identity = fmt.Sprintf("pid:%d", os.Getpid())
	}

	return &shmTC{
		info: credentials.ProtocolInfo{
			SecurityProtocol: "shm",
		},
		identity: identity,
		verify:   opts.VerifyIdentity,
	}
}

// Identity returns the identity configured for these credentials.
func (c *shmTC) Identity() string {
	return c.identity
}

// VerifyFunc returns the identity verification function, or nil if none.
func (c *shmTC) VerifyFunc() func(string) error {
	return c.verify
}
