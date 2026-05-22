//go:build linux

/*
 *
 * Copyright 2025 gRPC authors.
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
	"fmt"
	"math"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

func init() {
	// Set platform-specific function implementations
	unmapMemory = munmapImpl
}

// CreateSegment creates a new shared memory segment for the server
func CreateSegment(name string, ringCapA, ringCapB uint64) (*Segment, error) {
	// Calculate the layout
	totalSize, ringAOffset, ringBOffset, err := CalculateSegmentLayout(ringCapA, ringCapB)
	if err != nil {
		return nil, fmt.Errorf("layout calculation failed: %w", err)
	}

	// Try /dev/shm first, then fall back to temp dir for:
	// - Permission errors
	// - Not enough space (e.g. /dev/shm is smaller than segment size)
	// - Any creation/truncation/allocation errors
	paths := []string{
		generateSegmentPath(name),
		filepath.Join(os.TempDir(), "grpc_shm_"+name),
	}

	var file *os.File
	var path string
	var lastErr error

	for _, tryPath := range paths {
		// Try to create the file
		f, err := os.OpenFile(tryPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
		if err != nil {
			// If file already exists, don't fall back to another path - this is an error
			if os.IsExist(err) {
				return nil, fmt.Errorf("segment %q already exists: %w", name, err)
			}
			lastErr = err
			continue
		}

		// Use Fallocate to actually allocate space, not just extend the file.
		// This is crucial for tmpfs (/dev/shm) where Truncate creates sparse files
		// that fail with SIGBUS when accessed beyond available space.
		// Fallocate returns ENOSPC if there's not enough space.
		fd := int(f.Fd())
		if err := syscall.Fallocate(fd, 0, 0, int64(totalSize)); err != nil {
			f.Close()
			os.Remove(tryPath)
			lastErr = fmt.Errorf("fallocate failed: %w", err)
			continue
		}

		// Try to mmap the file
		mem, err := mmapFile(f, int(totalSize))
		if err != nil {
			f.Close()
			os.Remove(tryPath)
			lastErr = err
			continue
		}

		// Success - use this file and path
		file = f
		path = tryPath
		// We have the mem already, continue with segment creation
		segment := &Segment{
			File: file,
			Mem:  mem,
			Path: path,
			H:    &hdrView{basePtr: unsafe.Pointer(&mem[0])},
			A:    &ringView{basePtr: unsafe.Pointer(&mem[0]), offset: ringAOffset},
			B:    &ringView{basePtr: unsafe.Pointer(&mem[0]), offset: ringBOffset},
		}

		// Initialize the segment header
		magic := [8]byte{'G', 'R', 'P', 'C', 'S', 'H', 'M', 0}
		segment.H.SetMagic(magic)
		segment.H.SetVersion(SegmentVersion)
		segment.H.SetTotalSize(totalSize)
		segment.H.SetRingAOffset(ringAOffset)
		segment.H.SetRingACapacity(ringCapA)
		segment.H.SetRingBOffset(ringBOffset)
		segment.H.SetRingBCapacity(ringCapB)
		segment.H.SetServerPID(uint32(os.Getpid()))
		segment.H.SetMaxStreams(math.MaxUint32)

		// Initialize ring headers
		segment.A.SetCapacity(ringCapA)
		segment.A.SetWriteIndex(0)
		segment.A.SetReadIndex(0)
		segment.A.SetClosed(false)

		segment.B.SetCapacity(ringCapB)
		segment.B.SetWriteIndex(0)
		segment.B.SetReadIndex(0)
		segment.B.SetClosed(false)

		// If the eventfd waker is enabled and this is a non-control segment,
		// allocate a SOCK_STREAM socketpair and stash one endpoint
		// for the matching OpenSegment to claim. No-op otherwise.
		setupDataSegWakeForCreator(segment)

		// Close the backing file fd: the mmap holds an independent
		// inode reference, so the mapped region stays valid for the
		// segment's lifetime. Saves 1 FD/segment (2 FDs/conn over
		// control + data segments). Path is preserved in segment.Path
		// for path-based unlink via RemoveSegment / Segment.Close.
		//
		// Phase 2 cross-process: SCM_RIGHTS handshake must complete
		// BEFORE this close. Phase 1 is single-process / handle-by-
		// path, so the fd is no longer needed after mmap.
		if err := file.Close(); err != nil {
			munmapImpl(mem)
			os.Remove(tryPath)
			lastErr = fmt.Errorf("close fd after mmap: %w", err)
			continue
		}
		segment.File = nil

		return segment, nil
	}

	return nil, fmt.Errorf("failed to create segment in any location: %w", lastErr)
}

// OpenSegment opens an existing shared memory segment for the client
func OpenSegment(name string) (*Segment, error) {
	// Generate the segment path
	path := generateSegmentPath(name)

	// Open the existing file. If /dev/shm path exists but is not accessible,
	// fall back to temp dir.
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		if os.IsPermission(err) || os.IsNotExist(err) {
			altPath := filepath.Join(os.TempDir(), "grpc_shm_"+name)
			path = altPath
			file, err = os.OpenFile(path, os.O_RDWR, 0)
		}
		if err != nil {
			return nil, fmt.Errorf("failed to open segment file %s: %w", path, err)
		}
	}

	// Get file info to determine size
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("failed to stat segment file: %w", err)
	}

	size := info.Size()
	if size < SegmentHeaderSize {
		file.Close()
		return nil, fmt.Errorf("segment file too small: %d bytes", size)
	}

	// Memory map the file
	mem, err := mmapFile(file, int(size))
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("failed to mmap segment: %w", err)
	}

	// Create header view for validation
	hdr := &hdrView{basePtr: unsafe.Pointer(&mem[0])}

	// Validate the header
	if err := ValidateSegmentHeader((*SegmentHeader)(hdr.basePtr)); err != nil {
		munmapImpl(mem)
		file.Close()
		return nil, fmt.Errorf("invalid segment header: %w", err)
	}

	// Get ring offsets from header
	ringAOffset := hdr.RingAOffset()
	ringBOffset := hdr.RingBOffset()

	// Create segment views
	segment := &Segment{
		File: file,
		Mem:  mem,
		Path: path,
		H:    hdr,
		A:    &ringView{basePtr: unsafe.Pointer(&mem[0]), offset: ringAOffset},
		B:    &ringView{basePtr: unsafe.Pointer(&mem[0]), offset: ringBOffset},
	}

	// Set client PID. NOTE: we defer SetClientReady until AFTER
	// setupDataSegWakeForOpener has recorded OpenerWakeReady in the
	// header, so the creator's WaitForClient gate releases with a
	// stable wake-mode flag. finalizeDataSegWaker on the creator
	// side reads OpenerWakeReady to decide whether to keep its own
	// eventfd waker or release it (avoiding the asymmetric-wake
	// deadlock when SCM_RIGHTS handoff failed).
	segment.H.SetClientPID(uint32(os.Getpid()))

	// Claim the stashed per-data-segment socketpair endpoint (same-
	// process fast path) or receive it via SCM_RIGHTS (cross-process).
	// No-op for control segments / when the wake mode is off. Sets
	// OpenerWakeReady on the header as part of its deferred bookkeeping.
	setupDataSegWakeForOpener(segment)

	// Now that OpenerWakeReady is published, release the creator's
	// WaitForClient gate.
	segment.H.SetClientReady(true)

	// Close the backing file fd: the mmap holds an independent
	// inode reference, so the mapped region stays valid for the
	// segment's lifetime. Saves 1 FD/segment. See CreateSegment
	// for the full rationale.
	if err := file.Close(); err != nil {
		munmapImpl(mem)
		return nil, fmt.Errorf("close fd after mmap: %w", err)
	}
	segment.File = nil

	return segment, nil
}

// generateSegmentPath generates the file path for a shared memory segment
func generateSegmentPath(name string) string {
	// Try /dev/shm first (preferred for shared memory on Linux)
	shmPath := filepath.Join("/dev/shm", "grpc_shm_"+name)
	if isDevShmAvailable() {
		return shmPath
	}

	// Fallback to temporary directory
	return filepath.Join(os.TempDir(), "grpc_shm_"+name)
}

// isDevShmAvailable checks if /dev/shm is available and writable
func isDevShmAvailable() bool {
	info, err := os.Stat("/dev/shm")
	if err != nil {
		return false
	}
	return info.IsDir()
}

// mmapFile memory maps a file
func mmapFile(file *os.File, size int) ([]byte, error) {
	fd := int(file.Fd())

	data, err := syscall.Mmap(fd, 0, size, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("mmap failed: %w", err)
	}

	return data, nil
}

// munmapImpl unmaps a memory-mapped region
func munmapImpl(data []byte) error {
	if len(data) == 0 {
		return nil
	}

	err := syscall.Munmap(data)
	if err != nil {
		return fmt.Errorf("munmap failed: %w", err)
	}

	return nil
}
