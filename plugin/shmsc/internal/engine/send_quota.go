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

package engine

import "sync/atomic"

// tryReserveSendQuota attempts a single lock-free two-resource CAS reservation
// of n bytes from both the connection and per-stream send quotas. Returns true
// on success; false if either quota is insufficient OR a concurrent CAS won the
// race (caller should retry / defer as appropriate). On false, no quota is held.
//
// The rollback path runs when the stream CAS succeeded but the conn CAS failed —
// we Add(n) back to the stream quota. This may transiently push the stream quota
// above its current ceiling if a concurrent addSendQuota also incremented in the
// same window; HTTP/2 semantics permit this (outbound quota is bounded only by
// the protocol max of 2^31-1, which addSendQuota never approaches).
func tryReserveSendQuota(connQuota, streamQuota *atomic.Int64, n int64) bool {
	streamQ := streamQuota.Load()
	if streamQ < n {
		return false
	}
	connQ := connQuota.Load()
	if connQ < n {
		return false
	}
	if !streamQuota.CompareAndSwap(streamQ, streamQ-n) {
		return false
	}
	if !connQuota.CompareAndSwap(connQ, connQ-n) {
		// Conn CAS lost the race — restore stream quota.
		streamQuota.Add(n)
		shmCASRollback.Add(1)
		return false
	}
	return true
}
