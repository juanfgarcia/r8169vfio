//go:build linux

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"r8169vfio/vfio"
)

// This binary is the real VFIO path. It refuses to open /dev/vfio unless
// R8169VFIO_ALLOW_HARDWARE=1 is set, and even then it belongs inside a VM
// whose NIC is not the laptop's. go test never sets the variable and uses Fake.
func main() {
	swint := flag.String("swint", "", "legacy or 8125")
	rings := flag.Bool("rings", false, "program descriptor bases, leave RX/TX disabled")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: R8169VFIO_ALLOW_HARDWARE=1 %s <group> <bdf> [-swint legacy|8125] [-rings]\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "without that environment variable, /dev/vfio is not opened.\n")
	}
	flag.Parse()
	if os.Getenv("R8169VFIO_ALLOW_HARDWARE") != "1" {
		fmt.Fprintln(os.Stderr, "refused: set R8169VFIO_ALLOW_HARDWARE=1 only inside a test VM")
		os.Exit(2)
	}
	if flag.NArg() != 2 {
		flag.Usage()
		os.Exit(2)
	}
	family := vfio.FamilyLegacy
	if *swint == "8125" {
		family = vfio.Family8125
	} else if *swint != "" && *swint != "legacy" {
		fmt.Fprintln(os.Stderr, "-swint expects legacy or 8125")
		os.Exit(2)
	}
	dev, err := vfio.Attach(vfio.Linux(), flag.Arg(0), flag.Arg(1), family)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer dev.Close()
	fmt.Printf("XID %#x TxConfig %#x BAR irq %d IOVA %#x\n",
		dev.XID(), dev.TxConfig(), dev.IRQIndex(), dev.IOVA())
	if *rings {
		if err := dev.ProgramRings(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("rings programmed, RX/TX engines left disabled")
	}
	if *swint == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ch := dev.WatchIRQ(ctx)
	if family == vfio.Family8125 {
		dev.Arm8125()
	} else {
		dev.ArmLegacySWInt()
	}
	select {
	case n := <-ch:
		fmt.Printf("eventfd %d\n", n)
		if family == vfio.Family8125 {
			fmt.Printf("IntrStatus_8125 %#x\n", dev.Ack8125())
		} else {
			fmt.Printf("IntrStatus %#x\n", dev.AckLegacy())
		}
	case <-ctx.Done():
		fmt.Fprintln(os.Stderr, "timed out waiting for the eventfd")
		os.Exit(1)
	}
}
