//go:build linux

package vfio

import "unsafe"

// linux/vfio.h is not in the header list golang.org/x/sys uses to generate
// constants (v0.49.0 has none of these). The numbers are the stable _IO(';')
// encoding from the UAPI: type 0x3b, nr = 100+n, no size bits. Struct layouts
// match the linux/vfio.h shipped with this kernel, including the cap_offset
// fields added after the original VFIO revisions.

// _IO(';', 100+n). Checked against the host header: API == 15204.
const (
	reqAPI          uintptr = (';' << 8) | (100 + 0)
	reqExtension    uintptr = (';' << 8) | (100 + 1)
	reqSetIOMMU     uintptr = (';' << 8) | (100 + 2)
	reqGroupStatus  uintptr = (';' << 8) | (100 + 3)
	reqSetContainer uintptr = (';' << 8) | (100 + 4)
	reqGetDevice    uintptr = (';' << 8) | (100 + 6)
	reqDevInfo      uintptr = (';' << 8) | (100 + 7)
	reqRegionInfo   uintptr = (';' << 8) | (100 + 8)
	reqIRQInfo      uintptr = (';' << 8) | (100 + 9)
	reqSetIRQs      uintptr = (';' << 8) | (100 + 10)
	reqIOMMUInfo    uintptr = (';' << 8) | (100 + 12)
	reqMapDMA       uintptr = (';' << 8) | (100 + 13)
)

const (
	apiVersion = 0
	type1IOMMU = 1

	groupViable = 1 << 0

	regionRead  = 1 << 0
	regionWrite = 1 << 1
	regionMmap  = 1 << 2

	irqEventFD    = 1 << 0
	irqAutomasked = 1 << 2

	irqDataNone      = 1 << 0
	irqDataEventFD   = 1 << 2
	irqActionUnmask  = 1 << 4
	irqActionTrigger = 1 << 5

	dmaRead  = 1 << 0
	dmaWrite = 1 << 1

	iommuPgSizes = 1 << 0

	pciConfigRegion = 7
	irqINTX         = 0
	irqMSI          = 1
	irqMSIX         = 2
)

type groupStatus struct {
	Argsz uint32
	Flags uint32
}

type deviceInfo struct {
	Argsz      uint32
	Flags      uint32
	NumRegions uint32
	NumIRQs    uint32
	CapOffset  uint32
	Pad        uint32
}

type regionInfo struct {
	Argsz     uint32
	Flags     uint32
	Index     uint32
	CapOffset uint32
	Size      uint64
	Offset    uint64
}

type irqInfo struct {
	Argsz uint32
	Flags uint32
	Index uint32
	Count uint32
}

// irqPayload is vfio_irq_set plus one eventfd. The flexible array starts at
// byte 20; an int32 there stays naturally aligned.
type irqPayload struct {
	Argsz uint32
	Flags uint32
	Index uint32
	Start uint32
	Count uint32
	Fd    int32
}

type iommuInfo struct {
	Argsz       uint32
	Flags       uint32
	IOVAPgSizes uint64
	CapOffset   uint32
	Pad         uint32
}

type dmaMap struct {
	Argsz uint32
	Flags uint32
	Vaddr uint64
	IOVA  uint64
	Size  uint64
}

func init() {
	if unsafe.Sizeof(deviceInfo{}) != 24 ||
		unsafe.Sizeof(regionInfo{}) != 32 ||
		unsafe.Sizeof(irqInfo{}) != 16 ||
		unsafe.Sizeof(irqPayload{}) != 24 ||
		unsafe.Sizeof(iommuInfo{}) != 24 ||
		unsafe.Sizeof(dmaMap{}) != 32 ||
		unsafe.Offsetof(dmaMap{}.Vaddr) != 8 ||
		unsafe.Offsetof(irqPayload{}.Fd) != 20 {
		panic("vfio: Go struct layout drifted from linux/vfio.h")
	}
}
