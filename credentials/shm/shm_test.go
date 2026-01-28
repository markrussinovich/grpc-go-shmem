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

package shm

import (
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc/credentials"
)

func TestNewCredentials(t *testing.T) {
	creds := NewCredentials()
	if creds == nil {
		t.Fatal("NewCredentials returned nil")
	}

	info := creds.Info()
	if info.SecurityProtocol != "shm" {
		t.Errorf("SecurityProtocol: got %q, want %q", info.SecurityProtocol, "shm")
	}
}

func TestNewCredentialsWithOptions(t *testing.T) {
	verifyFunc := func(remote string) error {
		if !strings.HasPrefix(remote, "trusted-") {
			return errors.New("untrusted")
		}
		return nil
	}

	creds := NewCredentialsWithOptions(Options{
		Identity:       "custom-identity",
		VerifyIdentity: verifyFunc,
	})

	tc := creds.(*shmTC)
	if tc.identity != "custom-identity" {
		t.Errorf("Identity: got %q, want %q", tc.identity, "custom-identity")
	}
	if tc.verify == nil {
		t.Error("Verify function should not be nil")
	}
}

func TestClone(t *testing.T) {
	creds := NewCredentialsWithOptions(Options{
		Identity: "test-identity",
	})

	cloned := creds.Clone()
	if cloned == nil {
		t.Fatal("Clone returned nil")
	}

	// Verify it's a different instance
	if creds == cloned {
		t.Error("Clone should return a new instance")
	}

	// Verify the info is copied
	if cloned.Info().SecurityProtocol != creds.Info().SecurityProtocol {
		t.Error("Clone should copy protocol info")
	}
}

func TestOverrideServerName(t *testing.T) {
	creds := NewCredentials()

	err := creds.OverrideServerName("custom-server")
	if err != nil {
		t.Errorf("OverrideServerName failed: %v", err)
	}

	if creds.Info().ServerName != "custom-server" {
		t.Errorf("ServerName: got %q, want %q", creds.Info().ServerName, "custom-server")
	}
}

func TestInfoAuthType(t *testing.T) {
	info := Info{
		CommonAuthInfo: credentials.CommonAuthInfo{
			SecurityLevel: credentials.PrivacyAndIntegrity,
		},
		LocalIdentity:  "local",
		RemoteIdentity: "remote",
	}

	if info.AuthType() != "shm" {
		t.Errorf("AuthType: got %q, want %q", info.AuthType(), "shm")
	}
}

func TestInfoValidateAuthority(t *testing.T) {
	info := Info{}

	testCases := []string{
		"localhost",
		"example.com:8080",
		"",
		"127.0.0.1",
	}

	for _, authority := range testCases {
		if err := info.ValidateAuthority(authority); err != nil {
			t.Errorf("ValidateAuthority(%q) failed: %v", authority, err)
		}
	}
}

func TestIdentityMethod(t *testing.T) {
	creds := NewCredentialsWithOptions(Options{
		Identity: "test-identity",
	})

	tc := creds.(*shmTC)
	if tc.Identity() != "test-identity" {
		t.Errorf("Identity: got %q, want %q", tc.Identity(), "test-identity")
	}
}

func TestDefaultIdentity(t *testing.T) {
	creds := NewCredentials()
	tc := creds.(*shmTC)

	// Default identity should start with "pid:"
	if !strings.HasPrefix(tc.Identity(), "pid:") {
		t.Errorf("Default identity should start with 'pid:', got %q", tc.Identity())
	}
}
