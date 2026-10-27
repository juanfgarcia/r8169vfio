//go:build linux

package vfio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	// DefaultIOVA sits below 4 GiB so a 32-bit DMA mask (MAC version < 18) can reach it.
	DefaultIOVA = 0x10000000
	dmaBytes    = 8 << 20

	regChipCmd  = 0x37
	regTxPoll   = 0x38
	regIntrMask = 0x3c
	regIntrStat = 0x3e
	regTxConfig = 0x40
	regCfg9346  = 0x50
	regTxDescLo = 0x20
	regTxDescHi = 0x24
	regRxDescLo = 0xe4
	regRxDescHi = 0xe8
	regIntCfg0  = 0x34
	regMask8125 = 0x38
	regStat8125 = 0x3c

	cmdRxEnb    = 0x08
	cmdTxEnb    = 0x04
	cfgUnlock   = 0xc0
	cfgLock     = 0x00
	swInt       = 0x0100
	forceSw     = 0x01
	descOwn     = uint32(1) << 31
	ringEnd     = uint32(1) << 30
	rxBufSize   = 16383
	ringCount   = 256
	linkChg8125 = uint32(1) << 29
)

// Family selects the IRQ order the in-tree r8169 driver actually requests.
// Legacy (MAC version <= 17) is INTx. 8125/8126/8127 prefer MSI-X, then MSI.
type Family int

const (
	FamilyLegacy Family = iota
	Family8125
)

var (
	ErrGroupNotViable = errors.New("vfio: iommu group is not viable")
	ErrBadMMIO        = errors.New("vfio: TxConfig read 0xffffffff")
	ErrEnginesOn      = errors.New("vfio: rx/tx engines are still enabled")
)

// Device is the VFIO control plane for one Realtek function: BAR, IOVA, IRQ.
// It does not set CmdRxEnb|CmdTxEnb. That stays behind the per-revision PHY init.
type Device struct {
	sys       rawOS
	family    Family
	container int
	group     int
	dev       int
	bar       []byte
	dma       []byte
	iova      uint64
	cfgOff    int64
	irq       int
	irqFile   *os.File
	irqIndex  uint32
	automask  bool
	huge      bool
	locked    bool
	xid       uint32
	txconfig  uint32
}

// Attach opens the group, maps the first mmapable memory BAR, enables memory
// decode and bus master, checks TxConfig, maps an 8 MiB IOVA window and binds
// one IRQ eventfd. group is the IOMMU group number ("15"), bdf is "0000:03:00.0".
func Attach(sys rawOS, group, bdf string, family Family) (*Device, error) {
	d := &Device{
		sys: sys, family: family,
		container: -1, group: -1, dev: -1, irq: -1,
		iova: DefaultIOVA,
	}
	if err := d.open(group, bdf); err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

func (d *Device) open(group, bdf string) error {
	gpath, err := groupDev(group)
	if err != nil {
		return err
	}
	d.container, err = d.sys.Open("/dev/vfio/vfio", unix.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("vfio: open container: %w", err)
	}
	ver, err := d.sys.IoctlInt(d.container, reqAPI, 0)
	if err != nil {
		return fmt.Errorf("vfio: api version: %w", err)
	}
	if ver != apiVersion {
		return fmt.Errorf("vfio: api version %d", ver)
	}
	ext, err := d.sys.IoctlInt(d.container, reqExtension, type1IOMMU)
	if err != nil {
		return fmt.Errorf("vfio: check extension: %w", err)
	}
	if ext == 0 {
		return errors.New("vfio: type1 IOMMU is not available")
	}

	d.group, err = d.sys.Open(gpath, unix.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("vfio: open group: %w", err)
	}
	var gst groupStatus
	gst.Argsz = uint32(unsafe.Sizeof(gst))
	if _, err = d.sys.IoctlPtr(d.group, reqGroupStatus, unsafe.Pointer(&gst)); err != nil {
		return fmt.Errorf("vfio: group status: %w", err)
	}
	if gst.Flags&groupViable == 0 {
		return ErrGroupNotViable
	}
	cfd := int32(d.container)
	if _, err = d.sys.IoctlPtr(d.group, reqSetContainer, unsafe.Pointer(&cfd)); err != nil {
		return fmt.Errorf("vfio: set container: %w", err)
	}
	if _, err = d.sys.IoctlInt(d.container, reqSetIOMMU, type1IOMMU); err != nil {
		return fmt.Errorf("vfio: set iommu: %w", err)
	}

	name := append([]byte(bdf), 0)
	r, err := d.sys.IoctlPtr(d.group, reqGetDevice, unsafe.Pointer(&name[0]))
	if err != nil {
		return fmt.Errorf("vfio: get device fd: %w", err)
	}
	d.dev = int(r)

	var info deviceInfo
	info.Argsz = uint32(unsafe.Sizeof(info))
	if _, err = d.sys.IoctlPtr(d.dev, reqDevInfo, unsafe.Pointer(&info)); err != nil {
		return fmt.Errorf("vfio: device info: %w", err)
	}
	if _, err = d.findBAR(info.NumRegions); err != nil {
		return err
	}
	if err = d.enableMemMaster(); err != nil {
		return err
	}
	d.txconfig = d.MMIO().Read32(regTxConfig)
	if d.txconfig == 0xffffffff {
		return ErrBadMMIO
	}
	d.xid = (d.txconfig >> 20) & 0xfcf
	if err = d.mapDMA(); err != nil {
		return err
	}
	return d.bindIRQ()
}

func groupDev(group string) (string, error) {
	if group == "" {
		return "", errors.New("vfio: empty iommu group")
	}
	for _, c := range group {
		if c < '0' || c > '9' {
			return "", fmt.Errorf("vfio: group %q is not a decimal id", group)
		}
	}
	return "/dev/vfio/" + group, nil
}

func (d *Device) findBAR(nreg uint32) (regionInfo, error) {
	if nreg > 6 {
		nreg = 6
	}
	for i := uint32(0); i < nreg; i++ {
		var reg regionInfo
		reg.Argsz = uint32(unsafe.Sizeof(reg))
		reg.Index = i
		if _, err := d.sys.IoctlPtr(d.dev, reqRegionInfo, unsafe.Pointer(&reg)); err != nil {
			return regionInfo{}, fmt.Errorf("vfio: region %d: %w", i, err)
		}
		if reg.Flags&regionMmap == 0 || reg.Size < 256 {
			continue
		}
		length := pageRound(int(reg.Size))
		buf, err := d.sys.Mmap(d.dev, int64(reg.Offset), length, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
		if err != nil {
			return regionInfo{}, fmt.Errorf("vfio: mmap BAR%d: %w", i, err)
		}
		d.bar = buf
		var cfg regionInfo
		cfg.Argsz = uint32(unsafe.Sizeof(cfg))
		cfg.Index = pciConfigRegion
		if _, err = d.sys.IoctlPtr(d.dev, reqRegionInfo, unsafe.Pointer(&cfg)); err != nil {
			return regionInfo{}, fmt.Errorf("vfio: config region: %w", err)
		}
		d.cfgOff = int64(cfg.Offset)
		return reg, nil
	}
	return regionInfo{}, errors.New("vfio: no mmapable memory BAR")
}

func pageRound(n int) int {
	p := unix.Getpagesize()
	return (n + p - 1) &^ (p - 1)
}

func (d *Device) enableMemMaster() error {
	var cmd [2]byte
	off := d.cfgOff + 0x04
	n, err := d.sys.Pread(d.dev, cmd[:], off)
	if err != nil || n != 2 {
		return fmt.Errorf("vfio: pread PCI_COMMAND: %w", err)
	}
	v := binary.LittleEndian.Uint16(cmd[:]) | 0x0006 // MEMORY | BUS MASTER
	binary.LittleEndian.PutUint16(cmd[:], v)
	n, err = d.sys.Pwrite(d.dev, cmd[:], off)
	if err != nil || n != 2 {
		return fmt.Errorf("vfio: pwrite PCI_COMMAND: %w", err)
	}
	return nil
}

func (d *Device) mapDMA() error {
	buf, huge, locked, err := allocPinned(d.sys, dmaBytes)
	if err != nil {
		return err
	}
	d.dma = buf
	d.huge = huge
	d.locked = locked

	var info iommuInfo
	info.Argsz = uint32(unsafe.Sizeof(info))
	if _, err = d.sys.IoctlPtr(d.container, reqIOMMUInfo, unsafe.Pointer(&info)); err != nil {
		return fmt.Errorf("vfio: iommu info: %w", err)
	}
	if info.Flags&iommuPgSizes != 0 && info.IOVAPgSizes&0x1000 == 0 {
		return errors.New("vfio: iommu has no 4 KiB pages")
	}
	m := dmaMap{
		Argsz: uint32(unsafe.Sizeof(dmaMap{})),
		Flags: dmaRead | dmaWrite,
		Vaddr: uint64(uintptr(unsafe.Pointer(&buf[0]))),
		IOVA:  d.iova,
		Size:  uint64(len(buf)),
	}
	if _, err = d.sys.IoctlPtr(d.container, reqMapDMA, unsafe.Pointer(&m)); err != nil {
		return fmt.Errorf("vfio: map dma: %w", err)
	}
	return nil
}

func allocPinned(sys rawOS, size int) (buf []byte, huge, locked bool, err error) {
	buf, err = sys.Mmap(-1, 0, size, unix.PROT_READ|unix.PROT_WRITE,
		unix.MAP_PRIVATE|unix.MAP_ANONYMOUS|unix.MAP_HUGETLB)
	if err == nil {
		return buf, true, true, nil
	}
	buf, err = sys.Mmap(-1, 0, size, unix.PROT_READ|unix.PROT_WRITE,
		unix.MAP_PRIVATE|unix.MAP_ANONYMOUS)
	if err != nil {
		return nil, false, false, fmt.Errorf("vfio: mmap dma: %w", err)
	}
	if err = sys.Mlock(buf); err != nil {
		// MAP_DMA itself pins the pages when the process may lock memory.
		return buf, false, false, nil
	}
	return buf, false, true, nil
}

func (d *Device) bindIRQ() error {
	for _, index := range irqOrder(d.family) {
		var info irqInfo
		info.Argsz = uint32(unsafe.Sizeof(info))
		info.Index = index
		if _, err := d.sys.IoctlPtr(d.dev, reqIRQInfo, unsafe.Pointer(&info)); err != nil {
			return fmt.Errorf("vfio: irq info %d: %w", index, err)
		}
		if info.Count == 0 || info.Flags&irqEventFD == 0 {
			continue
		}
		// EFD_NONBLOCK is what makes os.NewFile register the fd with the
		// netpoller on Go 1.27. A blocking eventfd stays off the poller and
		// Read occupies an OS thread. Do not call File.Fd() afterwards:
		// that drops the fd back to blocking mode.
		efd, err := unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
		if err != nil {
			return fmt.Errorf("vfio: eventfd: %w", err)
		}
		payload := irqPayload{
			Argsz: uint32(unsafe.Sizeof(irqPayload{})),
			Flags: irqDataEventFD | irqActionTrigger,
			Index: index,
			Start: 0,
			Count: 1,
			Fd:    int32(efd),
		}
		if _, err = d.sys.IoctlPtr(d.dev, reqSetIRQs, unsafe.Pointer(&payload)); err != nil {
			unix.Close(efd)
			return fmt.Errorf("vfio: set irqs: %w", err)
		}
		d.irq = efd
		d.irqIndex = index
		d.automask = info.Flags&irqAutomasked != 0
		d.irqFile = os.NewFile(uintptr(efd), "vfio-irq")
		return nil
	}
	return errors.New("vfio: no eventfd irq")
}

func irqOrder(f Family) []uint32 {
	if f == Family8125 {
		return []uint32{irqMSIX, irqMSI, irqINTX}
	}
	return []uint32{irqINTX, irqMSI, irqMSIX}
}

func (d *Device) unmaskINTX() error {
	// vfio_irq_set with DATA_NONE stops at Count. That prefix is 20 bytes.
	var hdr struct {
		Argsz, Flags, Index, Start, Count uint32
	}
	hdr.Argsz = uint32(unsafe.Sizeof(hdr))
	hdr.Flags = irqDataNone | irqActionUnmask
	hdr.Index = irqINTX
	hdr.Count = 1
	_, err := d.sys.IoctlPtr(d.dev, reqSetIRQs, unsafe.Pointer(&hdr))
	return err
}

// Close releases mappings and fds. It does not close the IRQ file after
// TakeIRQ transferred it to Watch.
func (d *Device) Close() error {
	var first error
	remember := func(err error) {
		if err != nil && first == nil {
			first = err
		}
	}
	if d.dma != nil {
		remember(d.sys.Munmap(d.dma))
		d.dma = nil
	}
	if d.bar != nil {
		remember(d.sys.Munmap(d.bar))
		d.bar = nil
	}
	if d.irqFile != nil {
		remember(d.irqFile.Close())
		d.irqFile = nil
		d.irq = -1
	} else if d.irq >= 0 {
		remember(d.sys.Close(d.irq))
		d.irq = -1
	}
	if d.dev >= 0 {
		remember(d.sys.Close(d.dev))
		d.dev = -1
	}
	if d.group >= 0 {
		remember(d.sys.Close(d.group))
		d.group = -1
	}
	if d.container >= 0 {
		remember(d.sys.Close(d.container))
		d.container = -1
	}
	return first
}

// XID is (TxConfig >> 20) & 0xfcf, the same value rtl_init_one computes.
func (d *Device) XID() uint32 { return d.xid }

// TxConfig is the raw register at 0x40.
func (d *Device) TxConfig() uint32 { return d.txconfig }

// IOVA is the DMA address programmed into the chip, not a virtual address.
func (d *Device) IOVA() uint64 { return d.iova }

// IRQIndex is the VFIO irq index that was bound: 0 INTx, 1 MSI, 2 MSI-X.
func (d *Device) IRQIndex() uint32 { return d.irqIndex }

// Automasked reports VFIO_IRQ_INFO_AUTOMASKED. INTx needs an unmask after each event.
func (d *Device) Automasked() bool { return d.automask }

// Huge reports whether the DMA window came from a hugetlb mapping.
func (d *Device) Huge() bool { return d.huge }

// Locked reports whether mlock succeeded. VFIO_IOMMU_MAP_DMA pins on its own
// when the caller may lock memory; mlock is only a head start.
func (d *Device) Locked() bool { return d.locked }

// MMIO is the mapped BAR. Register offsets are byte offsets into this window.
func (d *Device) MMIO() MMIO { return sliceMMIO{d.bar} }

// DMABuf is the pinned window whose byte 0 is at IOVA().
func (d *Device) DMABuf() []byte { return d.dma }
