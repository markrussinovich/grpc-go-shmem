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

package server

import (
	"net"
	"testing"
)

type fakeServerBuilder struct{ tag string }

func (fakeServerBuilder) Build(_ net.Conn, _ BuildOptions) (ServerTransport, error) {
	return nil, nil
}

func TestGetUnknownReturnsNil(t *testing.T) {
	if got := Get("server-builder-does-not-exist"); got != nil {
		t.Fatalf("Get(unknown) = %v, want nil", got)
	}
}

func TestRegisterAndGet(t *testing.T) {
	const name = "test-server-builder"
	Register(name, fakeServerBuilder{tag: "y"})
	got := Get(name)
	b, ok := got.(fakeServerBuilder)
	if !ok {
		t.Fatalf("Get(%q) = %T, want fakeServerBuilder", name, got)
	}
	if b.tag != "y" {
		t.Fatalf("Get(%q) returned builder with tag %q, want %q", name, b.tag, "y")
	}
}
