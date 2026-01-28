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
// For shared memory, authentication is implicit - both processes can access
// the same memory segment has already proved locality.
func (c *shmTC) ClientHandshake(ctx context.Context, authority string, conn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	// Verify this is a shared memory connection
	if conn.RemoteAddr().Network() != "shm" {
		return nil, nil, fmt.Errorf("shm credentials require shm network, got %q", conn.RemoteAddr().Network())
	}

	// For shared memory, the handshake is performed at the transport level
	// using the ShmSecurityHandshaker. Here we just return the AuthInfo.
	// The actual handshake frames are exchanged in the transport layer.

	// Check if AuthInfo was already set by transport-level handshake
	if shmConn, ok := conn.(interface{ AuthInfo() credentials.AuthInfo }); ok {
		if authInfo := shmConn.AuthInfo(); authInfo != nil {
			return conn, authInfo, nil
		}
	}

	// Return basic auth info - transport layer handles the actual handshake
	return conn, Info{
		CommonAuthInfo: credentials.CommonAuthInfo{
			SecurityLevel: credentials.PrivacyAndIntegrity,
		},
		LocalIdentity:  c.identity,
		RemoteIdentity: "", // Will be set by transport handshake
	}, nil
}

// ServerHandshake performs the server-side handshake for shared memory transport.
func (c *shmTC) ServerHandshake(conn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	// Verify this is a shared memory connection
	if conn.RemoteAddr().Network() != "shm" {
		return nil, nil, fmt.Errorf("shm credentials require shm network, got %q", conn.RemoteAddr().Network())
	}

	// Check if AuthInfo was already set by transport-level handshake
	if shmConn, ok := conn.(interface{ AuthInfo() credentials.AuthInfo }); ok {
		if authInfo := shmConn.AuthInfo(); authInfo != nil {
			return conn, authInfo, nil
		}
	}

	// Return basic auth info - transport layer handles the actual handshake
	return conn, Info{
		CommonAuthInfo: credentials.CommonAuthInfo{
			SecurityLevel: credentials.PrivacyAndIntegrity,
		},
		LocalIdentity:  c.identity,
		RemoteIdentity: "", // Will be set by transport handshake
	}, nil
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
