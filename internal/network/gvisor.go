// Copyright (c) The eggshell Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

package network

import (
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"

	"github.com/usbarmory/go-net"

	"github.com/usbarmory/eggshell/kernel"
)

func initStack(dev gnet.NetworkDevice, addr string, gateway string) (iface *gnet.Interface, err error) {
	gs := gnet.NewGVisorStack(1)

	iface = &gnet.Interface{
		Stack:         gs,
		NetworkDevice: dev,
	}

	if err = iface.Init(addr, "", gateway); err != nil {
		return nil, err
	}

	if err = gs.EnableLoopback(); err != nil {
		return nil, err
	}

	gs.Stack.SetForwardingDefaultAndAllNICs(ipv4.ProtocolNumber, false)
	kernel.SetNetStack(gs.Stack)

	return iface, nil
}

func initLoopback() (err error) {
	gs := gnet.NewGVisorStack(1)

	if err = gs.EnableLoopback(); err != nil {
		return err
	}

	kernel.SetNetStack(gs.Stack)

	return nil
}
