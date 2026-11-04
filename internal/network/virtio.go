// Copyright (c) The eggshell Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

package network

import (
	"log"

	"github.com/usbarmory/tamago/amd64"
	"github.com/usbarmory/tamago/soc/intel/ioapic"

	"github.com/usbarmory/go-net"
	"github.com/usbarmory/go-net/virtio"
)

// redirection vector for IOAPIC IRQ to CPU IRQ
const vector = 32

func startInterruptHandler(dev *vnet.Net, iface *gnet.Interface, cpu *amd64.CPU, ioapic *ioapic.IOAPIC) {
	if cpu.LAPIC != nil {
		cpu.LAPIC.Enable()
	}

	ioapic.EnableInterrupt(dev.IRQ, vector)

	size := dev.HeaderLength + gnet.EthernetMaximumSize + gnet.MTU
	buf := make([]byte, size)

	// VirtIO devices keep their interrupt asserted until acknowledged (see
	// virtio.MMIO.ClearInterrupt), this is not required here as the IOAPIC
	// pin is edge-triggered and supported VMMs signal each event, while
	// virtio-pci targets use MSI-X.

	isr := func(irq int) {
		switch irq {
		case vector:
			for {
				n, err := dev.ReceiveWithHeader(buf)

				if err != nil || n == 0 {
					return
				}

				iface.Stack.RecvInboundPacket(buf[dev.HeaderLength:n])
			}
		default:
			log.Printf("internal error, unexpected IRQ %d", irq)
		}
	}

	cpu.ServiceInterrupts(isr)
}
