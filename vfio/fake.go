//go:build linux

package vfio

import (
	"encoding/binary"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Fake is an in-process VFIO UAPI. Ioctl numbers and struct layouts are the
// real ones. mmap of the BAR and of the DMA window is anonymous RAM, so the
// test never opens /dev/vfio and never touches a PCI device. A hugetlb request
// fails with ENOMEM, which is what a laptop without reserved hugepages does,
// and the caller falls back to private anonymous pages.
//
// MMIO stores are ordinary stores into that RAM. They do not trap, so a write
// of FSWInt does not signal the eventfd by itself. The test writes the eventfd
// that SET_IRQS recorded, which is the same split a hardware IRQ would cross
// from the device to the vfio-pci handler.
type Fake struct {
	mu       sync.Mutex
	viable   bool
	txconfig uint32
	cfg      []byte
	bar      []byte

	container int
	group     int
	dev       int
	next      int

	ioctls   []uintptr
	dmas     []dmaMap
	eventFD  int
	irqIndex uint32
	unmasks  int
}

// NewFake builds a group that is viable or not, with TxConfig planted at BAR offset 0x40.
func NewFake(viable bool, txconfig uint32) *Fake {
	return &Fake{
		viable:    viable,
		txconfig:  txconfig,
		cfg:       make([]byte, 256),
		container: -1,
		group:     -1,
		dev:       -1,
		eventFD:   -1,
	}
}

func (f *Fake) Open(path string, _ int, _ uint32) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	fd := 1000 + f.next
	switch path {
	case "/dev/vfio/vfio":
		f.container = fd
	default:
		if len(path) < len("/dev/vfio/") || path[:len("/dev/vfio/")] != "/dev/vfio/" {
			return -1, unix.ENOENT
		}
		f.group = fd
	}
	return fd, nil
}

func (f *Fake) Close(fd int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch fd {
	case f.container:
		f.container = -1
	case f.group:
		f.group = -1
	case f.dev:
		f.dev = -1
	default:
		return unix.EBADF
	}
	return nil
}

func (f *Fake) IoctlInt(fd int, req uintptr, val uintptr) (uintptr, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ioctls = append(f.ioctls, req)
	if fd != f.container {
		return 0, unix.EBADF
	}
	switch req {
	case reqAPI:
		return apiVersion, nil
	case reqExtension:
		if val == type1IOMMU {
			return 1, nil
		}
		return 0, nil
	case reqSetIOMMU:
		if val != type1IOMMU {
			return 0, unix.EINVAL
		}
		return 0, nil
	default:
		return 0, unix.ENOTTY
	}
}

func (f *Fake) IoctlPtr(fd int, req uintptr, arg unsafe.Pointer) (uintptr, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ioctls = append(f.ioctls, req)
	switch req {
	case reqGroupStatus:
		if fd != f.group {
			return 0, unix.EBADF
		}
		st := (*groupStatus)(arg)
		st.Flags = 0
		if f.viable {
			st.Flags = groupViable
		}
		return 0, nil
	case reqSetContainer:
		if fd != f.group {
			return 0, unix.EBADF
		}
		if *(*int32)(arg) != int32(f.container) {
			return 0, unix.EINVAL
		}
		return 0, nil
	case reqGetDevice:
		if fd != f.group {
			return 0, unix.EBADF
		}
		f.next++
		f.dev = 1000 + f.next
		return uintptr(f.dev), nil
	case reqDevInfo:
		if fd != f.dev {
			return 0, unix.EBADF
		}
		info := (*deviceInfo)(arg)
		info.Flags = 1 << 1 // PCI
		info.NumRegions = 9
		info.NumIRQs = 5
		return 0, nil
	case reqRegionInfo:
		if fd != f.dev {
			return 0, unix.EBADF
		}
		reg := (*regionInfo)(arg)
		switch reg.Index {
		case 0:
			reg.Size, reg.Flags, reg.Offset = 256, regionRead|regionWrite, 0
		case 2:
			reg.Size = 4096
			reg.Flags = regionRead | regionWrite | regionMmap
			reg.Offset = 2 << 40
		case pciConfigRegion:
			reg.Size, reg.Flags, reg.Offset = 256, regionRead|regionWrite, 7<<40
		default:
			reg.Size, reg.Flags, reg.Offset = 0, 0, 0
		}
		return 0, nil
	case reqIRQInfo:
		if fd != f.dev {
			return 0, unix.EBADF
		}
		info := (*irqInfo)(arg)
		switch info.Index {
		case irqINTX:
			info.Count = 1
			info.Flags = irqEventFD | irqAutomasked
		case irqMSI:
			info.Count = 1
			info.Flags = irqEventFD
		default:
			info.Count = 0
			info.Flags = 0
		}
		return 0, nil
	case reqSetIRQs:
		if fd != f.dev {
			return 0, unix.EBADF
		}
		hdr := (*struct {
			Argsz, Flags, Index, Start, Count uint32
		})(arg)
		if hdr.Flags&irqActionUnmask != 0 {
			f.unmasks++
			return 0, nil
		}
		payload := (*irqPayload)(arg)
		f.eventFD = int(payload.Fd)
		f.irqIndex = payload.Index
		return 0, nil
	case reqIOMMUInfo:
		if fd != f.container {
			return 0, unix.EBADF
		}
		info := (*iommuInfo)(arg)
		info.Flags = iommuPgSizes
		info.IOVAPgSizes = 0x1000 | (2 << 20)
		return 0, nil
	case reqMapDMA:
		if fd != f.container {
			return 0, unix.EBADF
		}
		m := (*dmaMap)(arg)
		page := uint64(unix.Getpagesize())
		if m.Size == 0 || m.Vaddr%page != 0 || m.IOVA%page != 0 || m.Size%page != 0 {
			return 0, unix.EINVAL
		}
		if m.Flags&(dmaRead|dmaWrite) != dmaRead|dmaWrite {
			return 0, unix.EINVAL
		}
		f.dmas = append(f.dmas, *m)
		return 0, nil
	default:
		return 0, unix.ENOTTY
	}
}

func (f *Fake) Mmap(fd int, offset int64, length, prot, flags int) ([]byte, error) {
	if flags&unix.MAP_HUGETLB != 0 {
		return nil, unix.ENOMEM
	}
	b, err := unix.Mmap(-1, 0, length, prot, unix.MAP_PRIVATE|unix.MAP_ANONYMOUS)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if fd == f.dev && offset == 2<<40 {
		binary.LittleEndian.PutUint32(b[0x40:], f.txconfig)
		f.bar = b
	}
	return b, nil
}

func (f *Fake) Munmap(b []byte) error { return unix.Munmap(b) }

func (f *Fake) Pread(fd int, p []byte, off int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rel, err := f.cfgRel(fd, off, len(p))
	if err != nil {
		return 0, err
	}
	return copy(p, f.cfg[rel:rel+len(p)]), nil
}

func (f *Fake) Pwrite(fd int, p []byte, off int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rel, err := f.cfgRel(fd, off, len(p))
	if err != nil {
		return 0, err
	}
	return copy(f.cfg[rel:rel+len(p)], p), nil
}

func (f *Fake) cfgRel(fd int, off int64, n int) (int, error) {
	if fd != f.dev {
		return 0, unix.EBADF
	}
	rel := off - (7 << 40)
	if rel < 0 || rel+int64(n) > int64(len(f.cfg)) {
		return 0, unix.EINVAL
	}
	return int(rel), nil
}

func (f *Fake) Mlock([]byte) error { return nil }

// Ioctls is the request stream Attach issued, in order.
func (f *Fake) Ioctls() []uintptr {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]uintptr, len(f.ioctls))
	copy(out, f.ioctls)
	return out
}

// DMAs are the windows registered with VFIO_IOMMU_MAP_DMA.
func (f *Fake) DMAs() []dmaMap {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]dmaMap, len(f.dmas))
	copy(out, f.dmas)
	return out
}

// EventFD is the descriptor SET_IRQS received. It is a real eventfd.
func (f *Fake) EventFD() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.eventFD
}

// Unmasks counts VFIO_IRQ_SET_ACTION_UNMASK calls.
func (f *Fake) Unmasks() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.unmasks
}

// Command is the emulated PCI_COMMAND register.
func (f *Fake) Command() uint16 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return binary.LittleEndian.Uint16(f.cfg[4:6])
}
