//go:build linux

package vfio

import (
	"fmt"
	"unsafe"
)

// MMIO is one naturally aligned access of the register's real width.
// sync/atomic.StoreUint32 is an XCHG on amd64 and must not be used here.
type MMIO interface {
	Read8(off int) uint8
	Read16(off int) uint16
	Read32(off int) uint32
	Write8(off int, v uint8)
	Write16(off int, v uint16)
	Write32(off int, v uint32)
}

type sliceMMIO struct{ b []byte }

func (m sliceMMIO) Read8(off int) uint8 {
	m.access(off, 1)
	return read8(unsafe.Pointer(&m.b[off]))
}

func (m sliceMMIO) Read16(off int) uint16 {
	m.access(off, 2)
	return read16(unsafe.Pointer(&m.b[off]))
}

func (m sliceMMIO) Read32(off int) uint32 {
	m.access(off, 4)
	return read32(unsafe.Pointer(&m.b[off]))
}

func (m sliceMMIO) Write8(off int, v uint8) {
	m.access(off, 1)
	write8(unsafe.Pointer(&m.b[off]), v)
}

func (m sliceMMIO) Write16(off int, v uint16) {
	m.access(off, 2)
	write16(unsafe.Pointer(&m.b[off]), v)
}

func (m sliceMMIO) Write32(off int, v uint32) {
	m.access(off, 4)
	write32(unsafe.Pointer(&m.b[off]), v)
}

func (m sliceMMIO) access(off, n int) {
	if off < 0 || off&^(n-1) != off || off+n > len(m.b) {
		panic(fmt.Sprintf("mmio: off %#x size %d bar %d", off, n, len(m.b)))
	}
}

//go:noinline
func read8(addr unsafe.Pointer) uint8 { return *(*uint8)(addr) }

//go:noinline
func read16(addr unsafe.Pointer) uint16 { return *(*uint16)(addr) }

//go:noinline
func read32(addr unsafe.Pointer) uint32 { return *(*uint32)(addr) }

//go:noinline
func write8(addr unsafe.Pointer, v uint8) { *(*uint8)(addr) = v }

//go:noinline
func write16(addr unsafe.Pointer, v uint16) { *(*uint16)(addr) = v }

//go:noinline
func write32(addr unsafe.Pointer, v uint32) { *(*uint32)(addr) = v }
