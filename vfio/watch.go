//go:build linux

package vfio

import (
	"context"
	"encoding/binary"
	"os"
)

// Watch parks a goroutine on irq.Read. On Linux, an eventfd opened with
// EFD_NONBLOCK and wrapped by os.NewFile is a pollable file, so Read is
// serviced by the runtime netpoller: the goroutine blocks, and no extra
// OS thread or pthread is created to wait.
//
// Canceling ctx closes irq, which unblocks Read. For an AUTOMASKED INTx
// line, after fires once the event has been consumed; pass Device.unmask
// there. after runs before the value is sent, so a receiver observes the
// unmask as already done.
//
// Watch takes ownership of irq and closes it.
func Watch(ctx context.Context, irq *os.File, after func()) <-chan uint64 {
	ch := make(chan uint64, 1)
	go func() {
		<-ctx.Done()
		irq.Close()
	}()
	go func() {
		defer close(ch)
		buf := make([]byte, 8)
		for {
			if _, err := irq.Read(buf); err != nil {
				return
			}
			if after != nil {
				after()
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

// WatchIRQ is Watch bound to this device's eventfd.
// INTx is unmasked after every delivery. The file is taken from the device.
func (d *Device) WatchIRQ(ctx context.Context) <-chan uint64 {
	f := d.irqFile
	d.irqFile = nil
	var after func()
	if d.automask {
		after = func() { _ = d.unmaskINTX() }
	}
	return Watch(ctx, f, after)
}
