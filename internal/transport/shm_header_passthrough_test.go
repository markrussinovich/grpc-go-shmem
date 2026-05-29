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
	"bytes"
	"testing"
)

// TestTakeOrDecodeHeaders_StashFastPath exercises the PR #10 fast
// path: when the codec has stashed a HeadersV1 struct on the holder,
// takeOrDecodeHeaders returns it directly without parsing the wire
// payload (which is allowed to be nil / empty in that branch).
func TestTakeOrDecodeHeaders_StashFastPath(t *testing.T) {
	want := HeadersV1{
		Version:   1,
		HdrType:   0,
		Method:    "/svc.M/Stash",
		Authority: "stash.example",
		Metadata:  []KV{{Key: "k", Values: [][]byte{[]byte("v")}}},
	}
	holder := newHpackDecoderHolder()
	holder.stashDecodedHeader(want)

	got, err := takeOrDecodeHeaders(holder, nil)
	if err != nil {
		t.Fatalf("takeOrDecodeHeaders fast path: %v", err)
	}
	if got.Method != want.Method || got.Authority != want.Authority {
		t.Errorf("struct mismatch: got %+v, want %+v", got, want)
	}
	if len(got.Metadata) != 1 || got.Metadata[0].Key != "k" ||
		!bytes.Equal(got.Metadata[0].Values[0], []byte("v")) {
		t.Errorf("metadata mismatch: got %+v", got.Metadata)
	}
	// Slot must be cleared after take so the next read does not
	// surface stale data.
	if holder.decodedHeaderSet {
		t.Error("decodedHeaderSet still true after take")
	}
	if holder.decodedHeader.Method != "" {
		t.Errorf("decodedHeader not zeroed after take: %+v", holder.decodedHeader)
	}
}

// TestTakeOrDecodeHeaders_FallbackFromHolder verifies that when the
// stash slot is empty, the helper falls back to decoding the wire
// payload via decodeHeaders. This is the defensive path covering
// callers (e.g., handcrafted test frames) that bypass the codec's
// stash hook.
func TestTakeOrDecodeHeaders_FallbackFromHolder(t *testing.T) {
	src := HeadersV1{
		Version: 1, HdrType: 0, Method: "/svc.M/Fallback",
		Metadata: []KV{{Key: "x", Values: [][]byte{[]byte("y")}}},
	}
	holder := newHpackDecoderHolder()
	// slot deliberately NOT set
	got, err := takeOrDecodeHeaders(holder, encodeHeaders(src))
	if err != nil {
		t.Fatalf("takeOrDecodeHeaders fallback: %v", err)
	}
	if got.Method != src.Method {
		t.Errorf("fallback decode wrong method: got %q want %q", got.Method, src.Method)
	}
}

// TestTakeOrDecodeHeaders_FallbackNilHolder verifies the nil-holder
// guard in takeOrDecodeHeaders (defensive against callers that don't
// thread a holder through).
func TestTakeOrDecodeHeaders_FallbackNilHolder(t *testing.T) {
	src := HeadersV1{Version: 1, Method: "/svc.M/NilHolder"}
	got, err := takeOrDecodeHeaders(nil, encodeHeaders(src))
	if err != nil {
		t.Fatalf("nil-holder fallback: %v", err)
	}
	if got.Method != src.Method {
		t.Errorf("got %q want %q", got.Method, src.Method)
	}
}

// TestTakeOrDecodeHeaders_FallbackErrorPropagation verifies that
// errors from the underlying decodeHeaders surface unchanged (no
// silent zero-struct return).
func TestTakeOrDecodeHeaders_FallbackErrorPropagation(t *testing.T) {
	holder := newHpackDecoderHolder()
	if _, err := takeOrDecodeHeaders(holder, nil); err == nil {
		t.Error("expected error from decodeHeaders(nil), got nil")
	}
	if _, err := takeOrDecodeHeaders(nil, []byte{0xff}); err == nil {
		t.Error("expected error from decodeHeaders(garbage), got nil")
	}
}

// TestTakeOrDecodeTrailers_StashFastPath mirrors the headers fast-path
// coverage for the TRAILERS slot.
func TestTakeOrDecodeTrailers_StashFastPath(t *testing.T) {
	want := TrailersV1{
		Version:        1,
		GRPCStatusCode: 13, // Internal
		GRPCStatusMsg:  "boom",
		Metadata:       []KV{{Key: "t", Values: [][]byte{[]byte("v")}}},
	}
	holder := newHpackDecoderHolder()
	holder.stashDecodedTrailer(want)

	got, err := takeOrDecodeTrailers(holder, nil)
	if err != nil {
		t.Fatalf("takeOrDecodeTrailers fast path: %v", err)
	}
	if got.GRPCStatusCode != want.GRPCStatusCode || got.GRPCStatusMsg != want.GRPCStatusMsg {
		t.Errorf("status mismatch: got code=%d msg=%q want code=%d msg=%q",
			got.GRPCStatusCode, got.GRPCStatusMsg, want.GRPCStatusCode, want.GRPCStatusMsg)
	}
	if holder.decodedTrailerSet {
		t.Error("decodedTrailerSet still true after take")
	}
}

// TestTakeOrDecodeTrailers_FallbackFromHolder + nil-holder + error
// propagation, mirroring the headers test set.
func TestTakeOrDecodeTrailers_FallbackFromHolder(t *testing.T) {
	src := TrailersV1{Version: 1, GRPCStatusCode: 0, GRPCStatusMsg: "OK"}
	holder := newHpackDecoderHolder()
	got, err := takeOrDecodeTrailers(holder, encodeTrailers(src))
	if err != nil {
		t.Fatalf("fallback: %v", err)
	}
	if got.GRPCStatusMsg != "OK" {
		t.Errorf("got msg %q want OK", got.GRPCStatusMsg)
	}
}

func TestTakeOrDecodeTrailers_FallbackNilHolder(t *testing.T) {
	src := TrailersV1{Version: 1, GRPCStatusCode: 0}
	if _, err := takeOrDecodeTrailers(nil, encodeTrailers(src)); err != nil {
		t.Errorf("nil-holder fallback: %v", err)
	}
}

func TestTakeOrDecodeTrailers_FallbackErrorPropagation(t *testing.T) {
	holder := newHpackDecoderHolder()
	if _, err := takeOrDecodeTrailers(holder, nil); err == nil {
		t.Error("expected error from decodeTrailers(nil), got nil")
	}
}

// TestTakeOrDecode_HeaderTrailerIndependent verifies that the headers
// and trailers slots do not cross-contaminate (a stashed header does
// not affect takeOrDecodeTrailers and vice versa).
func TestTakeOrDecode_HeaderTrailerIndependent(t *testing.T) {
	holder := newHpackDecoderHolder()
	holder.stashDecodedHeader(HeadersV1{Version: 1, Method: "/m/H"})
	// take a trailer from nil payload + unset trailer slot → should error,
	// NOT return the stashed header coerced.
	if _, err := takeOrDecodeTrailers(holder, nil); err == nil {
		t.Error("expected error when only header is stashed, got nil")
	}
	// header slot still set (trailer take should not have touched it).
	if !holder.decodedHeaderSet {
		t.Error("decodedHeaderSet cleared by takeOrDecodeTrailers — slots must be independent")
	}
	// now take the header — should succeed.
	if _, err := takeOrDecodeHeaders(holder, nil); err != nil {
		t.Errorf("takeOrDecodeHeaders after trailer-only attempt: %v", err)
	}
}
