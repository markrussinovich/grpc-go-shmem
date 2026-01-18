//go:build windows

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
	"unsafe"

	"golang.org/x/sys/windows"
)

func init() {
	// Set platform-specific function implementations
	unmapMemory = munmapImpl
}

// CreateSegment creates a new shared memory segment for the server (Windows).
func CreateSegment(name string, ringCapA, ringCapB uint64) (*Segment, error) {
	totalSize, ringAOffset, ringBOffset, err := CalculateSegmentLayout(ringCapA, ringCapB)
	if err != nil {
		return nil, fmt.Errorf("layout calculation failed: %w", err)
	}

	// Windows uses a regular file in %TEMP% as the backing object.
	path := generateSegmentPath(name)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		if os.IsExist(err) {
			return nil, fmt.Errorf("segment %q already exists: %w", name, err)
		}
		return nil, fmt.Errorf("create segment file: %w", err)
	}

	// Ensure the file is physically sized; Truncate on Windows reserves space.
	if err := file.Truncate(int64(totalSize)); err != nil {
		file.Close()
		os.Remove(path)
		return nil, fmt.Errorf("truncate segment: %w", err)
	}

	mem, err := mmapFile(file, int(totalSize))
	if err != nil {
		file.Close()
		os.Remove(path)
		return nil, err
	}

	segment := &Segment{
		File: file,
		Mem:  mem,
		Path: path,
		H:    &hdrView{basePtr: unsafe.Pointer(&mem[0])},
		A:    &ringView{basePtr: unsafe.Pointer(&mem[0]), offset: ringAOffset},
		B:    &ringView{basePtr: unsafe.Pointer(&mem[0]), offset: ringBOffset},
	}

	// Initialize header and rings.
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

// OpenSegment opens an existing shared memory segment for the client (Windows).
func OpenSegment(name string) (*Segment, error) {
	path := generateSegmentPath(name)

	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open segment file %s: %w", path, err)
	}

	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("stat segment file: %w", err)
	}
	size := info.Size()
	if size < SegmentHeaderSize {
		file.Close()
		return nil, fmt.Errorf("segment file too small: %d bytes", size)
	}

	mem, err := mmapFile(file, int(size))
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("mmap segment: %w", err)
	}

	hdr := &hdrView{basePtr: unsafe.Pointer(&mem[0])}
	if err := ValidateSegmentHeader((*SegmentHeader)(hdr.basePtr)); err != nil {
		munmapImpl(mem)
		file.Close()
		return nil, fmt.Errorf("invalid segment header: %w", err)
	}

	ringAOffset := hdr.RingAOffset()
	ringBOffset := hdr.RingBOffset()

	segment := &Segment{
		File: file,
		Mem:  mem,
		Path: path,
		H:    hdr,
		A:    &ringView{basePtr: unsafe.Pointer(&mem[0]), offset: ringAOffset},
		B:    &ringView{basePtr: unsafe.Pointer(&mem[0]), offset: ringBOffset},
	}

	segment.H.SetClientPID(uint32(os.Getpid()))
	// Note: We set the clientReady flag here but DON'T signal the event.
	// The caller (DialShm) will open handshake events and call
	// SetClientReadyAndSignal() after WaitForServer completes.
	// This ensures the event exists before we try to signal it.
	segment.H.SetClientReady(true)

	return segment, nil
}

// generateSegmentPath builds the backing file path in the temp directory.
func generateSegmentPath(name string) string {
	return filepath.Join(os.TempDir(), "grpc_shm_"+name)
}

// mmapFile maps the given file into memory.
func mmapFile(file *os.File, size int) ([]byte, error) {
	hFile := windows.Handle(file.Fd())

	protect := uint32(windows.PAGE_READWRITE)
	maxHigh := uint32(uint64(size) >> 32)
	maxLow := uint32(uint64(size) & 0xffffffff)

	mapping, err := windows.CreateFileMapping(hFile, nil, protect, maxHigh, maxLow, nil)
	if err != nil {
		return nil, fmt.Errorf("CreateFileMapping: %w", err)
	}
	defer windows.CloseHandle(mapping)

	addr, err := windows.MapViewOfFile(mapping, windows.FILE_MAP_WRITE, 0, 0, uintptr(size))
	if err != nil {
		return nil, fmt.Errorf("MapViewOfFile: %w", err)
	}

	return unsafe.Slice((*byte)(unsafe.Pointer(addr)), size), nil
}

// munmapImpl unmaps a memory-mapped region.
func munmapImpl(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	addr := uintptr(unsafe.Pointer(&data[0]))
	if err := windows.UnmapViewOfFile(addr); err != nil {
		return fmt.Errorf("UnmapViewOfFile: %w", err)
	}
	return nil
}
