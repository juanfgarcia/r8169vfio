//go:build linux

package vfio

import (
	"context"
	"encoding/binary"
)

// WatchIRQ parks a goroutine on the device eventfd. On Linux, an eventfd
// opened with EFD_NONBLOCK and wrapped by os.NewFile is a pollable file, so
// Read is serviced by the runtime netpoller: the goroutine blocks, and no
// extra OS thread or pthread is created to wait.
//
// Canceling ctx closes the file, which unblocks Read. INTx is unmasked after
// every delivery, before the value is sent. The read buffer and the unmask
// header are fields of Device, so a delivery does not allocate.
//
// WatchIRQ takes ownership of the IRQ file and closes it.
func (d *Device) WatchIRQ(ctx context.Context) <-chan uint64 {
	f := d.irqFile
	d.irqFile = nil
	ch := make(chan uint64, 1)
	go func() {
		<-ctx.Done()
		f.Close()
	}()
	go func() {
		defer close(ch)
		buf := d.irqBuf[:]
		for {
			if _, err := f.Read(buf); err != nil {
				return
			}
			if d.automask {
				_ = d.unmaskINTX()
			}
			n := binary.LittleEndian.Uint64(buf)
			select {
			case ch <- n:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch
}
