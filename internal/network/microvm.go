// Copyright (c) The eggshell Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

// Package network configures the network device and the TCP/IP stack
// serving AF_INET sockets through gVisor.
package network

import (
	"fmt"

	"github.com/usbarmory/tamago/amd64"
	"github.com/usbarmory/tamago/soc/intel/ioapic"

	"github.com/usbarmory/go-net/virtio"

	"github.com/usbarmory/eggshell/kernel"
)

func Init(addr string, gateway string, dev *vnet.Net, cpu *amd64.CPU, ioapic *ioapic.IOAPIC) (info string, err error) {
	if err = dev.Init(); err != nil {
		if err := initLoopback(); err != nil {
			return "", err
		}

		return "", fmt.Errorf("could not initialize VirtIO device, %v (loopback only)", err)
	}

	iface, err := initStack(dev, addr, gateway)

	if err != nil {
		return "", fmt.Errorf("could not start network stack, %v", err)
	}

	dev.Start()

	go startInterruptHandler(dev, iface, cpu, ioapic)

	mac, _ := iface.Stack.HardwareAddress()

	return fmt.Sprintf("virtio-net %s %s gw:%s %s", mac, addr, gateway, kernel.NetBackend), nil
}
