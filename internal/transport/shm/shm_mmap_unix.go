//go:build linux && (amd64 || arm64)

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

package shm

import (
	"fmt"
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
	// Generate the segment path
	path := generateSegmentPath(name)

	// Calculate the layout
	totalSize, ringAOffset, ringBOffset, err := CalculateSegmentLayout(ringCapA, ringCapB)
	if err != nil {
		return nil, fmt.Errorf("layout calculation failed: %w", err)
	}

	// Create the file with exclusive access
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("failed to create segment file %s: %w", path, err)
	}

	// Ensure cleanup on error
	cleanup := func() {
		file.Close()
		os.Remove(path)
	}

	// Set the file size
	if err := file.Truncate(int64(totalSize)); err != nil {
		cleanup()
		return nil, fmt.Errorf("failed to resize segment file: %w", err)
	}

	// Memory map the file
	mem, err := mmapFile(file, int(totalSize))
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("failed to mmap segment: %w", err)
	}

	// Create segment views
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
	segment.H.SetServerReady(true)

	// Initialize ring headers
	segment.A.SetCapacity(ringCapA)
	segment.A.SetWriteIndex(0)
	segment.A.SetReadIndex(0)
	segment.A.SetClosed(false)

	segment.B.SetCapacity(ringCapB)
	segment.B.SetWriteIndex(0)
	segment.B.SetReadIndex(0)
	segment.B.SetClosed(false)

	return segment, nil
}

// OpenSegment opens an existing shared memory segment for the client
func OpenSegment(name string) (*Segment, error) {
	// Generate the segment path
	path := generateSegmentPath(name)

	// Open the existing file
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to open segment file %s: %w", path, err)
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

	// Set client PID and ready flag
	segment.H.SetClientPID(uint32(os.Getpid()))
	segment.H.SetClientReady(true)

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
