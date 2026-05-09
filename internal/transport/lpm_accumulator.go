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
	"encoding/binary"
	"errors"
	"fmt"
)

// lpmAccumulator reassembles a single in-progress gRPC length-prefixed
// message (LPM) across multiple HTTP/2 DATA frames. HTTP/2 DATA frames
// carry a byte stream of LPM-prefixed messages (gRFC §"Length-Prefixed-
// Message"); one DATA frame may contain a fragment of one message,
// multiple complete messages, or a mix.
//
// The accumulator emits "1 internal MESSAGE = 1 complete app message" so
// upper layers see the same model used by Custom16. The LPM header (5
// bytes) is preserved at the start of the emitted body for compatibility
// with downstream readers that expect to strip it themselves.
//
// LPM wire format (gRFC):
//
//	+--------+--------+--------+--------+--------+...
//	| 1 byte | 4 byte big-endian length  | length bytes of body
//	|comp flg|  uncompressed body length |
//	+--------+---------------------------+...
//
// State machine:
//
//	state = empty:
//	  - header bytes seen 0..4 → keep collecting in headerBuf
//	  - header bytes seen == 5 → parse expected body length, allocate buffer
//	state = collecting:
//	  - append body bytes until pos == expectedTotal → emit
//
// Single-frame fast path: when the accumulator is empty AND the entire
// LPM (5+body) is contained in a single DATA frame body, the codec
// bypasses the accumulator and returns the body slice directly. This
// preserves the ZC opportunity for the common case.
type lpmAccumulator struct {
	headerBuf       [5]byte // partial LPM header bytes collected so far
	headerBytesSeen int     // 0..5 — header bytes written to headerBuf
	expectedTotal   int     // 5 + body length, set once header complete
	pos             int     // bytes written into buf (includes 5-byte header)
	buf             []byte  // accumulated frame, len == pos, cap >= expectedTotal
}

// inProgress reports whether the accumulator is mid-message.
func (a *lpmAccumulator) inProgress() bool {
	return a.headerBytesSeen > 0 || a.pos > 0
}

// feed consumes data from a DATA frame body. Returns:
//   - msg != nil: a complete LPM (header+body) is ready; caller should
//     emit it as a MESSAGE frame. The returned slice is a fresh allocation
//     owned by the caller.
//   - leftover: bytes from data not yet consumed (start of the next LPM
//     or partial header). The caller should re-feed leftover on the next
//     iteration if it's non-empty. (This is rare in practice — a single
//     DATA frame typically carries exactly one LPM.)
//   - err: protocol violation (e.g., LPM body length exceeds maxBody).
//
// maxBody is the maximum allowed body length (unencoded) — payload too
// large is rejected per gRPC's MaxReceiveMessageSize semantics. Pass 0
// to disable the size cap (enforced upstream).
func (a *lpmAccumulator) feed(data []byte, maxBody int) (msg []byte, leftover []byte, err error) {
	// Phase 1: complete the LPM header if not yet parsed.
	if a.headerBytesSeen < 5 {
		need := 5 - a.headerBytesSeen
		if len(data) < need {
			n := copy(a.headerBuf[a.headerBytesSeen:], data)
			a.headerBytesSeen += n
			return nil, nil, nil
		}
		copy(a.headerBuf[a.headerBytesSeen:], data[:need])
		a.headerBytesSeen = 5
		data = data[need:]

		bodyLen := int(binary.BigEndian.Uint32(a.headerBuf[1:5]))
		if bodyLen < 0 {
			// Reset header-parse state so a subsequent feed call (in
			// principle: caller treats this as connection-fatal, but
			// defense-in-depth) doesn't silently mis-parse on stale
			// state.
			a.headerBytesSeen = 0
			return nil, nil, errors.New("h2 LPM: negative body length")
		}
		if maxBody > 0 && bodyLen > maxBody {
			a.headerBytesSeen = 0
			return nil, nil, fmt.Errorf("h2 LPM: body length %d exceeds max %d", bodyLen, maxBody)
		}
		a.expectedTotal = 5 + bodyLen
		// Allocate incrementally rather than to the full declared
		// expectedTotal: a peer-controlled tiny DATA frame that
		// declares hundreds of MiB in the LPM header would otherwise
		// force an immediate huge allocation before any per-RPC
		// receive limit can reject it. Append below grows the slice
		// amortised-O(N) on actual received bytes; the cap above
		// (maxBody) provides a hard upper bound on declared size, but
		// we never trust it for preallocation.
		//
		// Initial cap is min(8 KiB, expectedTotal) — enough to absorb
		// the small-message hot path without a second growth, while
		// keeping the worst-case (511 MiB declared, 5 byte body sent)
		// allocation bounded by what was actually received.
		const initialBufHint = 8 * 1024
		initialCap := a.expectedTotal
		if initialCap > initialBufHint {
			initialCap = initialBufHint
		}
		a.buf = make([]byte, 0, initialCap)
		a.buf = append(a.buf, a.headerBuf[:]...)
		a.pos = 5
	}

	// Phase 2: append body bytes.
	remaining := a.expectedTotal - a.pos
	if remaining > len(data) {
		a.buf = append(a.buf, data...)
		a.pos += len(data)
		return nil, nil, nil
	}

	// Complete the message.
	a.buf = append(a.buf, data[:remaining]...)
	a.pos += remaining
	leftover = data[remaining:]
	msg = a.buf

	// Reset for the next message.
	a.headerBytesSeen = 0
	a.expectedTotal = 0
	a.pos = 0
	a.buf = nil

	return msg, leftover, nil
}
