//go:build linux

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
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// SCM_RIGHTS-based file-descriptor handoff for the per-data-segment
// eventfd waker.
//
// The eventfd waker requires both peers to share the same kernel
// eventfd objects. Within a single process the creator stashes the
// peer endpoint in a package-private map for the opener to claim;
// cross-process openers cannot reach that map. To make the eventfd
// path work across processes, the creator additionally exposes the
// pair via a per-segment Unix domain socket at "<seg.Path>.fds.sock";
// the opener, after mmap'ing the segment but before any data-plane
// activity, connects to that socket and receives the two eventfd
// file descriptors via SCM_RIGHTS. The kernel duplicates the fds
// into the opener's FD table, so the opener obtains valid eventfds
// for the same underlying kernel objects.
//
// The wire format on the FD-pass socket is intentionally minimal:
// the server accepts a connection and immediately sends a fixed
// 4-byte token ("FDS\n") plus the two eventfd fds in an SCM_RIGHTS
// cmsg in a single sendmsg call, then closes the connection. The
// token lets the opener detect protocol mismatch (e.g., older
// server that doesn't speak this); future revisions can extend the
// payload as a length-prefixed framed message under the same token.
//
// Socket path: derived from the segment's filesystem path by
// appending ".fds.sock". For a segment at /dev/shm/grpc_shm_NAME
// the socket lives at /dev/shm/grpc_shm_NAME.fds.sock. The socket
// is single-shot per opener: after sendmsg the server closes the
// connection; the server stays listening to handle re-dials (e.g.,
// transient peer crashes) but each accept hands out the same fd
// pair (the kernel objects are shared, dup is cheap). Segment.Close
// invokes fdpassStop to shut the server down and unlink the socket
// file.
//
// Lifecycle:
//
//   - CreateSegment (Linux): after newShmDataSegWakerPair allocates
//     the eventfds and the local stash records the peer endpoint,
//     spawn a goroutine that binds the FD-pass socket and serves
//     SCM_RIGHTS handoffs until Segment.Close cancels it.
//   - OpenSegment (Linux): claim from the local stash first
//     (same-process fast path, zero syscalls); on miss, dial the
//     FD-pass socket and recvmsg the eventfd pair. If recv fails
//     (no server, timeout, version mismatch), opener falls through
//     to the futex / WaitOnAddress wake path.
//   - Segment.Close: invoke fdpassStop which closes the listener,
//     unblocks the accept goroutine, and unlinks the socket file.
//
// Trust model: the socket inherits the directory permissions of its
// parent directory (typically /dev/shm with mode 1777 + sticky, or
// $TMPDIR with the user's umask). Only processes that can access
// the segment file itself can connect, matching the existing access
// control story for /dev/shm-based IPC.

// fdpassSocketPath returns the Unix-domain socket path that the
// fd-pass server binds for the given segment.
func fdpassSocketPath(segPath string) string {
	return segPath + ".fds.sock"
}

// fdpassHandshakeToken is sent as the first 4 bytes of every
// SCM_RIGHTS payload so the opener can detect a wrong-protocol
// server (or, conversely, an old client connecting to a new
// server). Bumping this token signals a non-backward-compatible
// change in the fd-pass wire format.
var fdpassHandshakeToken = [4]byte{'F', 'D', 'S', '\n'}

// fdpassRecvTimeout caps how long an opener waits for the server's
// SCM_RIGHTS reply. Generous (the server's accept handler runs in
// a goroutine and replies synchronously) but bounded so a hung
// peer doesn't stall Dial indefinitely.
const fdpassRecvTimeout = 5 * time.Second

// serveEventfdsForCreatorWaker binds a Unix-domain listener at the
// per-segment fd-pass socket path and serves SCM_RIGHTS handoffs of
// the creator-side eventfd pair extracted from w. It returns a stop
// function that the caller MUST invoke on segment close so the
// listener exits cleanly and the socket file is unlinked.
//
// The eventfds owned by w are not closed by this function; w
// retains ownership. If w is closed (e.g., via Segment.Close ->
// dataSegWaker.Close) the listener remains harmless (subsequent
// sendmsg returns EBADF and the connection is dropped). Callers
// SHOULD invoke the returned stop before closing w to avoid the
// EBADF window.
func serveEventfdsForCreatorWaker(segPath string, w *shmDataSegWaker) (stop func(), err error) {
	if w == nil {
		return func() {}, errors.New("serveEventfdsForCreatorWaker: nil waker")
	}
	fds := []int{w.myReadRawFd, w.peerReadFd}
	sockPath := fdpassSocketPath(segPath)
	// Best-effort unlink in case a prior server crashed mid-life.
	_ = os.Remove(sockPath)

	addr := &net.UnixAddr{Name: sockPath, Net: "unix"}
	listener, err := net.ListenUnix("unix", addr)
	if err != nil {
		return nil, fmt.Errorf("fdpass: listen %s: %w", sockPath, err)
	}

	// Tighten the socket's permissions to 0600 so only the segment
	// owner can connect. The default umask of 022 on /dev/shm would
	// otherwise create a world-readable / world-writable socket,
	// letting any local process receive the eventfd duplicates and
	// flood the segment with spurious wakes (DoS). The eventfds carry
	// no data, but the wake budget belongs to the legitimate peer.
	if chmodErr := os.Chmod(sockPath, 0o600); chmodErr != nil {
		_ = listener.Close()
		_ = os.Remove(sockPath)
		return nil, fmt.Errorf("fdpass: chmod %s 0600: %w", sockPath, chmodErr)
	}

	// Snapshot our UID for the per-accept SO_PEERCRED check below.
	ownerUID := uint32(os.Getuid())

	// Copy the fd slice so callers can free their backing array.
	// We send the same kernel objects to every opener via dup
	// (SCM_RIGHTS does the dup; our retained ints stay valid).
	sendFds := make([]int, len(fds))
	copy(sendFds, fds)

	var (
		stoppedMu sync.Mutex
		stopped   bool
	)
	stopFn := func() {
		stoppedMu.Lock()
		if stopped {
			stoppedMu.Unlock()
			return
		}
		stopped = true
		stoppedMu.Unlock()
		_ = listener.Close()
		_ = os.Remove(sockPath)
	}

	go func() {
		for {
			conn, accErr := listener.AcceptUnix()
			if accErr != nil {
				stoppedMu.Lock()
				done := stopped
				stoppedMu.Unlock()
				if done {
					return
				}
				// Transient accept failures: brief backoff and retry.
				time.Sleep(10 * time.Millisecond)
				continue
			}
			// Hand off this connection in a goroutine so a slow
			// opener doesn't block other concurrent opens.
			go func(c *net.UnixConn) {
				defer c.Close()
				// Reject peers whose UID does not match ours. The
				// 0600 chmod above narrows the door to same-UID
				// processes; SO_PEERCRED is the second-line check
				// that survives any future tightening of the
				// directory permission story (e.g., a private
				// 0700 directory under TempDir on systems without
				// /dev/shm).
				if !peerUIDMatches(c, ownerUID) {
					return
				}
				_ = c.SetWriteDeadline(time.Now().Add(fdpassRecvTimeout))
				rights := unix.UnixRights(sendFds...)
				_, _, _ = c.WriteMsgUnix(fdpassHandshakeToken[:], rights, nil)
			}(conn)
		}
	}()

	return stopFn, nil
}

// recvEventfdsFromCreator dials the per-segment fd-pass socket and
// retrieves the eventfd pair via SCM_RIGHTS. It returns the received
// file descriptors in the same order the creator sent them: index 0
// is the creator's own read fd (= opener's peer write target) and
// index 1 is the creator's peer write fd (= opener's own read fd).
// Callers SHOULD wrap the returned fds in *os.File via os.NewFile
// for netpoll integration.
//
// Returns an error if the socket cannot be reached (server not
// running, permission denied, timeout) or if the handshake token
// does not match. Callers MUST close the returned fds on success
// when the Segment is being torn down; on error the function
// returns no fds, so no cleanup is needed.
//
// Fast-fail behavior: if the socket file does not exist, the
// function returns immediately without retry. This is the common
// asymmetric-config case (creator did not enable the eventfd waker)
// where waiting for fdpassRecvTimeout would block OpenSegment and
// stall downstream WaitForServer / WaitForClient gates.
func recvEventfdsFromCreator(segPath string) ([]int, error) {
	sockPath := fdpassSocketPath(segPath)
	if _, err := os.Stat(sockPath); err != nil {
		// No fd-pass server bound -- creator either disabled the
		// eventfd waker or hasn't started yet. Either way, the
		// caller falls through to the futex / events path; we
		// return immediately so OpenSegment doesn't stall.
		return nil, fmt.Errorf("fdpass: socket %s not present: %w", sockPath, err)
	}
	deadline := time.Now().Add(fdpassRecvTimeout)
	addr := &net.UnixAddr{Name: sockPath, Net: "unix"}

	// Retry briefly: the server's listener goroutine may not have
	// reached Accept by the time the opener dials in same-process
	// race scenarios.
	var (
		conn *net.UnixConn
		err  error
	)
	for {
		conn, err = net.DialUnix("unix", nil, addr)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("fdpass: dial %s: %w", sockPath, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(deadline)

	buf := make([]byte, len(fdpassHandshakeToken))
	// 2 file descriptors at 4 bytes each in the cmsg payload.
	oob := make([]byte, unix.CmsgSpace(2*4))
	n, oobn, _, _, err := conn.ReadMsgUnix(buf, oob)
	if err != nil {
		return nil, fmt.Errorf("fdpass: read: %w", err)
	}
	if n != len(fdpassHandshakeToken) || !bytes.Equal(buf, fdpassHandshakeToken[:]) {
		return nil, fmt.Errorf("fdpass: bad handshake token: %q", buf[:n])
	}
	cmsgs, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return nil, fmt.Errorf("fdpass: ParseSocketControlMessage: %w", err)
	}
	var got []int
	for _, c := range cmsgs {
		fds, err := unix.ParseUnixRights(&c)
		if err != nil {
			// Close any descriptors collected before the failure so we
			// do not leak them to the caller's process.
			for _, fd := range got {
				unix.Close(fd)
			}
			return nil, fmt.Errorf("fdpass: ParseUnixRights: %w", err)
		}
		got = append(got, fds...)
	}
	if len(got) == 0 {
		return nil, errors.New("fdpass: server sent no fds")
	}
	return got, nil
}

// newShmDataSegWakerFromOpenerFds constructs an opener-side waker
// from a pair of file descriptors received via SCM_RIGHTS from the
// segment's creator. The fd ordering matches newShmDataSegWakerPair:
// fds[0] is the creator's read fd (the opener writes there to wake
// the creator) and fds[1] is the creator's peer write fd / the
// opener's own read fd (the opener parks here for incoming wakes).
//
// On any failure the function closes the supplied fds so the caller
// does not have to handle partial ownership.
func newShmDataSegWakerFromOpenerFds(fds []int) (*shmDataSegWaker, error) {
	if len(fds) != 2 {
		for _, fd := range fds {
			unix.Close(fd)
		}
		return nil, fmt.Errorf("newShmDataSegWakerFromOpenerFds: expected 2 fds, got %d", len(fds))
	}
	// fds[0] = creator's read (opener writes here to wake)
	// fds[1] = opener's read (opener parks here)
	peerReadFd := fds[0]
	myReadFd := fds[1]

	f := os.NewFile(uintptr(myReadFd), "shm-dataseg-efd-opener")
	if f == nil {
		unix.Close(peerReadFd)
		unix.Close(myReadFd)
		return nil, errors.New("newShmDataSegWakerFromOpenerFds: os.NewFile failed")
	}
	return &shmDataSegWaker{
		myReadFile:  f,
		myReadRawFd: myReadFd,
		peerReadFd:  peerReadFd,
	}, nil
}

// peerUIDMatches verifies via SO_PEERCRED that the connected peer is
// owned by the expected UID. Returns true on match, false on any
// mismatch or error (defensive: deny on failure to read credentials).
//
// SO_PEERCRED is set at connect()/accept() time and is immutable for
// the life of the socket, so the check is reliable against TOCTOU
// races where a peer might fork after connecting.
func peerUIDMatches(c *net.UnixConn, expected uint32) bool {
	raw, err := c.SyscallConn()
	if err != nil {
		return false
	}
	var ucred *unix.Ucred
	var inner error
	if controlErr := raw.Control(func(fd uintptr) {
		ucred, inner = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); controlErr != nil || inner != nil || ucred == nil {
		return false
	}
	return ucred.Uid == expected
}
