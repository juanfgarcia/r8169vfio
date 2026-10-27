//go:build linux

package vfio

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

func TestUAPIMatchesHostHeader(t *testing.T) {
	if reqAPI != 15204 || reqMapDMA != 15217 || reqGetDevice != 15210 {
		t.Fatalf("ioctl numbers drifted: api %d map %d getdev %d", reqAPI, reqMapDMA, reqGetDevice)
	}
	if unsafe.Sizeof(deviceInfo{}) != 24 || unsafe.Sizeof(iommuInfo{}) != 24 {
		t.Fatal("struct size")
	}
}

func TestHardwareGateStaysOff(t *testing.T) {
	if os.Getenv("R8169VFIO_ALLOW_HARDWARE") == "1" {
		t.Fatal("default go test must not set R8169VFIO_ALLOW_HARDWARE")
	}
}

func TestGroupNotViable(t *testing.T) {
	f := NewFake(false, 0x509<<20)
	_, err := Attach(f, "15", "0000:03:00.0", FamilyLegacy)
	if !errors.Is(err, ErrGroupNotViable) {
		t.Fatalf("got %v", err)
	}
}

func TestBadMMIORead(t *testing.T) {
	f := NewFake(true, 0xffffffff)
	_, err := Attach(f, "15", "0000:03:00.0", FamilyLegacy)
	if !errors.Is(err, ErrBadMMIO) {
		t.Fatalf("got %v", err)
	}
}

func TestLegacyControlPlane(t *testing.T) {
	f := NewFake(true, 0x509<<20)
	dev, err := Attach(f, "15", "0000:03:00.0", FamilyLegacy)
	if err != nil {
		t.Fatal(err)
	}
	defer dev.Close()

	if dev.XID() != 0x509 {
		t.Fatalf("xid %#x", dev.XID())
	}
	if f.Command()&0x0006 != 0x0006 {
		t.Fatalf("PCI_COMMAND %#x", f.Command())
	}
	if dev.IRQIndex() != irqINTX || !dev.Automasked() {
		t.Fatalf("irq index %d automask %v", dev.IRQIndex(), dev.Automasked())
	}
	if dev.Huge() {
		t.Fatal("fake must refuse hugetlb so the fallback path is what runs")
	}
	if !dev.Locked() {
		t.Fatal("fake mlock should report success")
	}
	dmas := f.DMAs()
	if len(dmas) != 1 || dmas[0].IOVA != DefaultIOVA || dmas[0].Size != dmaBytes {
		t.Fatalf("dma %+v", dmas)
	}
	if !ordered(f.Ioctls(), reqAPI, reqExtension, reqGroupStatus, reqSetContainer, reqSetIOMMU, reqGetDevice, reqMapDMA, reqSetIRQs) {
		t.Fatalf("ioctl order %v", f.Ioctls())
	}

	if err := dev.ProgramRings(); err != nil {
		t.Fatal(err)
	}
	mmio := dev.MMIO()
	if mmio.Read8(regCfg9346) != cfgLock {
		t.Fatalf("cfg9346 %#x", mmio.Read8(regCfg9346))
	}
	if mmio.Read32(regTxDescLo) != uint32(DefaultIOVA) || mmio.Read32(regTxDescHi) != 0 {
		t.Fatalf("tx base lo %#x hi %#x", mmio.Read32(regTxDescLo), mmio.Read32(regTxDescHi))
	}
	if mmio.Read32(regRxDescLo) != uint32(DefaultIOVA+0x1000) {
		t.Fatalf("rx base %#x", mmio.Read32(regRxDescLo))
	}
	if mmio.Read8(regChipCmd)&(cmdRxEnb|cmdTxEnb) != 0 {
		t.Fatal("engines were enabled")
	}
	first := loadDesc(dev.DMABuf()[0x1000:])
	if first.Addr != DefaultIOVA+0x100000 || first.Opts1&(descOwn|rxBufSize) != descOwn|rxBufSize {
		t.Fatalf("rx[0] %+v", first)
	}
	last := loadDesc(dev.DMABuf()[0x1000+(ringCount-1)*16:])
	if last.Opts1&ringEnd == 0 {
		t.Fatal("last rx descriptor missing RingEnd")
	}

	irq := dev.irqFile
	if err := irq.SetReadDeadline(time.Now().Add(30 * time.Millisecond)); err != nil {
		t.Fatalf("eventfd is not registered with the netpoller: %v", err)
	}
	buf := make([]byte, 8)
	if _, err := irq.Read(buf); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("poller read: %v", err)
	}
	if err := irq.SetReadDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ch := dev.WatchIRQ(ctx)
	dev.ArmLegacySWInt()
	if mmio.Read8(regTxPoll) != forceSw || mmio.Read16(regIntrMask) != swInt {
		t.Fatalf("poll %#x mask %#x", mmio.Read8(regTxPoll), mmio.Read16(regIntrMask))
	}
	var one [8]byte
	binary.LittleEndian.PutUint64(one[:], 1)
	if _, err := unix.Write(f.EventFD(), one[:]); err != nil {
		t.Fatal(err)
	}
	select {
	case n := <-ch:
		if n != 1 {
			t.Fatalf("eventfd count %d", n)
		}
	case <-ctx.Done():
		t.Fatal("Watch did not observe the eventfd")
	}
	if f.Unmasks() != 1 {
		t.Fatalf("intx unmasks %d", f.Unmasks())
	}
	cancel()
}

func TestEnginesRefuseRingRewrite(t *testing.T) {
	dev := mustAttach(t, FamilyLegacy)
	defer dev.Close()
	dev.MMIO().Write8(regChipCmd, cmdRxEnb|cmdTxEnb)
	if err := dev.ProgramRings(); !errors.Is(err, ErrEnginesOn) {
		t.Fatalf("got %v", err)
	}
}

func Test8125DoesNotKickTX(t *testing.T) {
	f := NewFake(true, 0x609<<20)
	dev, err := Attach(f, "15", "0000:03:00.0", Family8125)
	if err != nil {
		t.Fatal(err)
	}
	defer dev.Close()
	if dev.XID() != 0x609 {
		t.Fatalf("xid %#x", dev.XID())
	}
	if dev.IRQIndex() != irqMSI || dev.Automasked() {
		t.Fatalf("irq %d automask %v", dev.IRQIndex(), dev.Automasked())
	}
	dev.Arm8125()
	mmio := dev.MMIO()
	if mmio.Read8(regIntCfg0)&1 == 0 {
		t.Fatal("INT_CFG0 enable not set")
	}
	if mmio.Read32(regMask8125) != linkChg8125 {
		t.Fatalf("mask %#x", mmio.Read32(regMask8125))
	}
	for off := 0x90; off < 0x94; off++ {
		if dev.DMABuf() != nil && mmio.Read8(off) != 0 {
			t.Fatalf("TxPoll_8125 byte %#x is %#x", off, mmio.Read8(off))
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ch := dev.WatchIRQ(ctx)
	var one [8]byte
	binary.LittleEndian.PutUint64(one[:], 1)
	if _, err := unix.Write(f.EventFD(), one[:]); err != nil {
		t.Fatal(err)
	}
	select {
	case n := <-ch:
		if n != 1 {
			t.Fatalf("count %d", n)
		}
	case <-ctx.Done():
		t.Fatal("msi eventfd was not delivered")
	}
	if f.Unmasks() != 0 {
		t.Fatalf("msi unmasked %d times", f.Unmasks())
	}
}

func mustAttach(t *testing.T, family Family) *Device {
	t.Helper()
	dev, err := Attach(NewFake(true, 0x509<<20), "15", "0000:03:00.0", family)
	if err != nil {
		t.Fatal(err)
	}
	return dev
}

func ordered(got []uintptr, want ...uintptr) bool {
	i := 0
	for _, req := range got {
		if req == want[i] {
			i++
			if i == len(want) {
				return true
			}
		}
	}
	return false
}

func loadDesc(b []byte) desc {
	return *(*desc)(unsafe.Pointer(&b[0]))
}
