//go:build linux

package vfio

import "unsafe"

// ProgramRings writes 256 TX/RX descriptors into the IOVA window and stores
// the ring bases in the BAR. The NIC does not fetch them until CmdRxEnb|CmdTxEnb,
// and this function refuses to run if those bits are already set.
func (d *Device) ProgramRings() error {
	mmio := d.MMIO()
	if mmio.Read8(regChipCmd)&(cmdRxEnb|cmdTxEnb) != 0 {
		return ErrEnginesOn
	}
	tx := d.dma[0x0000 : 0x0000+ringCount*16]
	rx := d.dma[0x1000 : 0x1000+ringCount*16]
	bufIOVA := d.iova + 0x100000
	for i := 0; i < ringCount; i++ {
		var txd desc
		if i == ringCount-1 {
			txd.Opts1 = ringEnd
		}
		storeDesc(tx[i*16:], txd)

		rxd := desc{
			Opts2: 0,
			Addr:  bufIOVA + uint64(i)*16384,
		}
		storeDesc(rx[i*16:], rxd)
		opts := descOwn | rxBufSize
		if i == ringCount-1 {
			opts |= ringEnd
		}
		// noinline store is the stand-in for dma_wmb before DescOwn is published.
		write32(unsafe.Pointer(&rx[i*16]), opts)
	}

	mmio.Write8(regCfg9346, cfgUnlock)
	mmio.Write32(regTxDescHi, uint32(d.iova>>32))
	mmio.Write32(regTxDescLo, uint32(d.iova))
	mmio.Write32(regRxDescHi, uint32((d.iova+0x1000)>>32))
	mmio.Write32(regRxDescLo, uint32(d.iova+0x1000))
	mmio.Write8(regCfg9346, cfgLock)
	_ = mmio.Read8(regChipCmd) // rtl_pci_commit: drain posted MMIO
	return nil
}

type desc struct {
	Opts1 uint32
	Opts2 uint32
	Addr  uint64
}

func storeDesc(dst []byte, v desc) {
	*(*desc)(unsafe.Pointer(&dst[0])) = v
}

// ArmLegacySWInt is the on-demand interrupt of the classic 8168/8169:
// IntrMask = SWInt, then TxPoll = FSWInt. On 8125, offset 0x38 is the mask
// register and bit 0 of TxPoll at 0x90 is the TX doorbell, so this must not
// be used for that family.
func (d *Device) ArmLegacySWInt() {
	mmio := d.MMIO()
	mmio.Write16(regIntrMask, 0)
	if st := mmio.Read16(regIntrStat); st != 0 {
		mmio.Write16(regIntrStat, st)
	}
	mmio.Write16(regIntrMask, swInt)
	_ = mmio.Read8(regChipCmd)
	mmio.Write8(regTxPoll, forceSw)
}

// AckLegacy reads IntrStatus, writes it back (the ISR ack) and masks the line.
func (d *Device) AckLegacy() uint16 {
	mmio := d.MMIO()
	st := mmio.Read16(regIntrStat)
	if st != 0 {
		mmio.Write16(regIntrStat, st)
	}
	mmio.Write16(regIntrMask, 0)
	_ = mmio.Read8(regChipCmd)
	return st
}

// Arm8125 opens the 8125 interrupt gate and unmasks link-change only.
// It does not write TxPoll at 0x90.
func (d *Device) Arm8125() {
	mmio := d.MMIO()
	mmio.Write8(regIntCfg0, mmio.Read8(regIntCfg0)|0x01)
	mmio.Write32(regMask8125, 0)
	if st := mmio.Read32(regStat8125); st != 0 {
		mmio.Write32(regStat8125, st)
	}
	mmio.Write32(regMask8125, linkChg8125)
	_ = mmio.Read8(regChipCmd)
}

// Ack8125 acknowledges IntrStatus_8125 and clears the mask.
func (d *Device) Ack8125() uint32 {
	mmio := d.MMIO()
	st := mmio.Read32(regStat8125)
	if st != 0 {
		mmio.Write32(regStat8125, st)
	}
	mmio.Write32(regMask8125, 0)
	_ = mmio.Read8(regChipCmd)
	return st
}
