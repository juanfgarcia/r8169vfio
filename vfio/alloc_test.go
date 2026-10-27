//go:build linux && !race

package vfio

import (
	"context"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// eventOne is static so the write in the delivery test does not escape a stack buffer.
var eventOne = [8]byte{1}

// quietOS is the unmask backend for the alloc test. Fake records every ioctl
// and that log grows; this one does not.
type quietOS struct{}

func (quietOS) Open(string, int, uint32) (int, error) { return -1, unix.ENOSYS }
func (quietOS) Close(int) error                       { return nil }
func (quietOS) IoctlInt(int, uintptr, uintptr) (uintptr, error) {
	return 0, nil
}
func (quietOS) IoctlPtr(int, uintptr, unsafe.Pointer) (uintptr, error) {
	return 0, nil
}
func (quietOS) Mmap(int, int64, int, int, int) ([]byte, error) {
	return nil, unix.ENOSYS
}
func (quietOS) Munmap([]byte) error                   { return nil }
func (quietOS) Pread(int, []byte, int64) (int, error) { return 0, unix.ENOSYS }
func (quietOS) Pwrite(int, []byte, int64) (int, error) {
	return 0, unix.ENOSYS
}
func (quietOS) Mlock([]byte) error { return nil }

func TestSteadyStateDoesNotAllocate(t *testing.T) {
	f := NewFake(true, 0x509<<20)
	dev, err := Attach(f, "15", "0000:03:00.0", FamilyLegacy)
	if err != nil {
		t.Fatal(err)
	}
	defer dev.Close()
	efd := f.EventFD()

	mmio := dev.MMIO()
	if n := testing.AllocsPerRun(100, func() {
		mmio.Write16(regIntrMask, swInt)
		mmio.Write8(regTxPoll, forceSw)
		_ = mmio.Read16(regIntrStat)
		_ = mmio.Read32(regTxConfig)
	}); n != 0 {
		t.Fatalf("mmio allocs %v", n)
	}
	if n := testing.AllocsPerRun(100, func() {
		dev.ArmLegacySWInt()
		_ = dev.AckLegacy()
		dev.Arm8125()
		_ = dev.Ack8125()
	}); n != 0 {
		t.Fatalf("arm/ack allocs %v", n)
	}
	if n := testing.AllocsPerRun(20, func() {
		if err := dev.ProgramRings(); err != nil {
			t.Fatal(err)
		}
	}); n != 0 {
		t.Fatalf("rings allocs %v", n)
	}

	saved := dev.sys
	dev.sys = quietOS{}
	defer func() { dev.sys = saved }()

	if n := testing.AllocsPerRun(100, func() {
		if err := dev.unmaskINTX(); err != nil {
			t.Fatal(err)
		}
	}); n != 0 {
		t.Fatalf("unmask allocs %v", n)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := dev.WatchIRQ(ctx)
	if _, err := unix.Write(efd, eventOne[:]); err != nil {
		t.Fatal(err)
	}
	<-ch
	if n := testing.AllocsPerRun(50, func() {
		if _, err := unix.Write(efd, eventOne[:]); err != nil {
			t.Fatal(err)
		}
		<-ch
	}); n != 0 {
		t.Fatalf("irq delivery allocs %v", n)
	}
}
