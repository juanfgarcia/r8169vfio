# r8169vfio

Biblioteca Go para abrir un dispositivo PCI RTL8168/8169/8125 por VFIO type1, mapear su BAR de memoria, registrar una ventana DMA y recibir una interrupción en un `*os.File`.

`Attach` abre `/dev/vfio/vfio` y `/dev/vfio/<grupo>`, comprueba la versión de la API y la extensión type1, y exige un grupo IOMMU viable. Ata el grupo al contenedor, selecciona `VFIO_TYPE1_IOMMU` y abre el dispositivo por su BDF. De las regiones 0–5 elige la primera con `VFIO_REGION_INFO_FLAG_MMAP` y al menos 256 bytes, y la publica con `unix.Mmap`. En el registro de comando PCI activa memory space y bus master. Lee `TxConfig` en el offset `0x40`. El XID es `(TxConfig >> 20) & 0xfcf`. Una lectura `0xffffffff` aborta el attach.

La ventana DMA son 8 MiB. Si el sistema tiene hugepages, el mapeo usa `MAP_HUGETLB`; si no, páginas anónimas y `mlock`. `VFIO_IOMMU_MAP_DMA` la registra en el IOVA `0x10000000`, por debajo de 4 GiB. Los anillos y los buffers viven en ese rango. `ProgramRings` escribe 256 descriptores TX en el offset 0 y 256 RX en `0x1000`, con buffers de 16384 bytes a partir de `IOVA+0x100000`. El último descriptor lleva `RingEnd`. Los descriptores RX se publican con `DescOwn` y longitud 16383. Las bases se escriben en `TxDescStartAddr*` (`0x24` y luego `0x20`) y `RxDescAddr*` (`0xe8` y luego `0xe4`) con `Cfg9346` desbloqueado. Si `ChipCmd` ya tiene RX o TX habilitados, `ProgramRings` devuelve error.

La interrupción es un `eventfd` con `EFD_NONBLOCK`, envuelto en `os.NewFile`. `WatchIRQ` lee 8 bytes desde una goroutine. `FamilyLegacy` prueba INTx, MSI y MSI-X, en ese orden. `Family8125` prueba MSI-X, MSI e INTx. Tras cada INTx el código envía `VFIO_IRQ_SET_ACTION_UNMASK`. `ArmLegacySWInt` pone la máscara `SWInt` (`0x0100`) y escribe `0x01` en `TxPoll` (`0x38`). `Arm8125` pone el bit 0 de `INT_CFG0` (`0x34`) y la máscara de link-change (bit 29) en `0x38`.

Los accesos MMIO son cargas y almacenes del ancho del registro (`8`, `16` o `32` bits) sobre el slice devuelto por `mmap`.

`NewFake` implementa la misma interfaz de syscalls en memoria. Los tests del paquete usan ese backend. `Linux()` usa `golang.org/x/sys/unix`.

## Tests

```bash
go test -race ./...
```

## Ejemplo

`cmd/r8169vfio` llama a `Attach(Linux(), grupo, bdf, familia)`. Sin `R8169VFIO_ALLOW_HARDWARE=1` sale con código 2.

Sustituye el BDF por el de la función PCI. El grupo tiene que contener solo ese dispositivo.

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

En un 8125 el flag es `-swint 8125`. `-rings` llama a `ProgramRings`. El programa imprime XID, `TxConfig`, índice de IRQ e IOVA. Con `-swint` espera el `eventfd` hasta 2 segundos y luego lee el registro de estado.

Para devolver el dispositivo al driver anterior:

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

`Family8125` usa `Arm8125` y `Ack8125`. `AckLegacy` corresponde a `ArmLegacySWInt`.
