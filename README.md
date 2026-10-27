# r8169vfio

Go library that opens an RTL8168/8169/8125 PCI device through VFIO type1, maps its memory BAR, registers a DMA window, and delivers an interrupt on an `*os.File`.

`Attach` opens `/dev/vfio/vfio` and `/dev/vfio/<group>`, checks the API version and the type1 extension, and requires a viable IOMMU group. It attaches the group to the container, selects `VFIO_TYPE1_IOMMU`, and opens the device by BDF. Among regions 0–5 it picks the first with `VFIO_REGION_INFO_FLAG_MMAP` and at least 256 bytes, and publishes it with `unix.Mmap`. It sets the PCI command memory-space and bus-master bits. It reads `TxConfig` at offset `0x40`. The XID is `(TxConfig >> 20) & 0xfcf`. A read of `0xffffffff` aborts attach.

The DMA window is 8 MiB. When hugepages are available the mapping uses `MAP_HUGETLB`; otherwise it uses anonymous pages and `mlock`. `VFIO_IOMMU_MAP_DMA` registers it at IOVA `0x10000000`, below 4 GiB. Rings and buffers live in that range. `ProgramRings` writes 256 TX descriptors at offset 0 and 256 RX descriptors at `0x1000`, with 16384-byte buffers starting at `IOVA+0x100000`. The last descriptor carries `RingEnd`. RX descriptors are published with `DescOwn` and length 16383. Ring bases are written to `TxDescStartAddr*` (`0x24`, then `0x20`) and `RxDescAddr*` (`0xe8`, then `0xe4`) with `Cfg9346` unlocked. If `ChipCmd` already has RX or TX enabled, `ProgramRings` returns an error.

The interrupt is an `eventfd` created with `EFD_NONBLOCK` and wrapped in `os.NewFile`. `WatchIRQ` reads 8 bytes from a goroutine. `FamilyLegacy` tries INTx, MSI, and MSI-X, in that order. `Family8125` tries MSI-X, MSI, and INTx. After each INTx event the code sends `VFIO_IRQ_SET_ACTION_UNMASK`. `ArmLegacySWInt` sets the `SWInt` mask (`0x0100`) and writes `0x01` to `TxPoll` (`0x38`). `Arm8125` sets bit 0 of `INT_CFG0` (`0x34`) and the link-change mask (bit 29) at `0x38`.

MMIO accesses are loads and stores of the register width (`8`, `16`, or `32` bits) on the slice returned by `mmap`.

`NewFake` implements the same syscall interface in memory. Package tests use that backend. `Linux()` uses `golang.org/x/sys/unix`.

## Tests

```bash
go test -race ./...
```

## Example

`cmd/r8169vfio` calls `Attach(Linux(), group, bdf, family)`. Without `R8169VFIO_ALLOW_HARDWARE=1` it exits with status 2.

Replace the BDF with the PCI function. The IOMMU group must contain only that device.

```bash
export BDF=0000:03:00.0
ls -l /sys/bus/pci/devices/$BDF/iommu_group/devices/
export GROUP=$(basename "$(readlink -f /sys/bus/pci/devices/$BDF/iommu_group)")

sudo modprobe vfio-pci
echo "$BDF" | sudo tee /sys/bus/pci/devices/$BDF/driver/unbind
echo vfio-pci | sudo tee /sys/bus/pci/devices/$BDF/driver_override
echo "$BDF" | sudo tee /sys/bus/pci/drivers_probe

cd cmd/r8169vfio && go build
sudo -E env R8169VFIO_ALLOW_HARDWARE=1 ./r8169vfio "$GROUP" "$BDF" -swint legacy
```

On an 8125 the flag is `-swint 8125`. `-rings` calls `ProgramRings`. The program prints the XID, `TxConfig`, IRQ index, and IOVA. With `-swint` it waits up to 2 seconds for the `eventfd`, then reads the status register.

To hand the device back to the previous driver:

```bash
echo "$BDF" | sudo tee /sys/bus/pci/drivers/vfio-pci/unbind
printf '\n' | sudo tee /sys/bus/pci/devices/$BDF/driver_override
echo "$BDF" | sudo tee /sys/bus/pci/drivers_probe
```

## API

```go
dev, err := vfio.Attach(vfio.Linux(), group, bdf, vfio.FamilyLegacy)
defer dev.Close()

xid := dev.XID()
mmio := dev.MMIO()
buf := dev.DMABuf() // byte 0 == dev.IOVA()

ch := dev.WatchIRQ(ctx)
dev.ArmLegacySWInt()
```

`Family8125` uses `Arm8125` and `Ack8125`. `AckLegacy` pairs with `ArmLegacySWInt`.
