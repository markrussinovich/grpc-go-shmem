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
	"sync"
	"testing"
)

type testBuilder struct{ name string }

func (testBuilder) Build(net.Conn, BuildOptions) (ServerTransport, error) {
	return nil, nil
}

func TestRegisterAndGet(t *testing.T) {
	b := testBuilder{name: "tb1"}
	Register("test-registry-get", b)

	got := Get("test-registry-get")
	if got == nil {
		t.Fatalf("Get(%q) = nil; want the registered Builder", "test-registry-get")
	}
	if got.(testBuilder).name != "tb1" {
		t.Fatalf("Get(%q) returned %+v; want %+v", "test-registry-get", got, b)
	}
}

func TestGetUnregisteredReturnsNil(t *testing.T) {
	if got := Get("test-registry-never-registered"); got != nil {
		t.Fatalf("Get(unregistered) = %v; want nil (callers rely on nil to fail closed)", got)
	}
}

func TestRegisterPanics(t *testing.T) {
	tests := []struct {
		desc    string
		name    string
		builder Builder
	}{
		{desc: "empty name", name: "", builder: testBuilder{}},
		{desc: "nil builder", name: "test-registry-nil-builder", builder: nil},
	}
	for _, test := range tests {
		t.Run(test.desc, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("Register(%q, %v) did not panic; want panic", test.name, test.builder)
				}
			}()
			Register(test.name, test.builder)
		})
	}
}

func TestRegisterDuplicatePanics(t *testing.T) {
	Register("test-registry-dup", testBuilder{})
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("second Register with a duplicate name did not panic; want panic")
		}
	}()
	Register("test-registry-dup", testBuilder{})
}

// TestGetConcurrent exercises Get under concurrency; the race detector is the
// real assertion here (registry access is RWMutex-guarded).
func TestGetConcurrent(t *testing.T) {
	Register("test-registry-concurrent", testBuilder{name: "conc"})

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if got := Get("test-registry-concurrent"); got == nil {
					t.Errorf("Get returned nil for a registered name")
					return
				}
			}
		}()
	}
	wg.Wait()
}
