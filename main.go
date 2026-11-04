// Copyright (c) The eggshell Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

// The eggshell project is a TamaGo unikernel that implements the Linux ABI to
// run unmodified POSIX compliant C applications compiled (statically or
// dynamically linked) against the musl library.
package main

import (
	"fmt"
	"log"
	"os"
	"runtime"

	board "github.com/usbarmory/eggshell/board/microvm"
	"github.com/usbarmory/eggshell/internal/boot"
	"github.com/usbarmory/eggshell/internal/network"
	"github.com/usbarmory/eggshell/internal/sshd"
	"github.com/usbarmory/eggshell/kernel"
	"github.com/usbarmory/eggshell/rootfs"
)

var opts = boot.Options{
	Init:     []string{"/sbin/init"},
	Hostname: "yolk",
	Address:  "10.0.0.1/24",
	Gateway:  "10.0.0.2",
	SSH:      22,
}

var initEnv = []string{
	"HOME=/root",
	"PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
	"TERM=xterm",
	"SHELL=/bin/bash",
	"USER=root",
	"LOGNAME=root",
	"PWD=/",
}

func main() {
	log.SetFlags(0)
	log.SetOutput(os.Stdout)

	opts.Parse(board.BootArgs())

	cfg := &kernel.Config{
		Rootfs:   rootfs.Archive,
		Init:     opts.Init,
		Env:      append(initEnv, opts.Env...),
		Hostname: opts.Hostname,
		Strace:   opts.Trace,
	}

	if err := board.Init(cfg); err != nil {
		fmt.Printf("board initialization error, %v\n", err)
		board.PowerOff(1)
	}

	if !opts.Quiet {
		fmt.Printf("\neggshell %s • %s %s (%s) • %d MiB available\n",
			board.Name(),
			runtime.GOOS,
			runtime.GOARCH,
			runtime.Version(),
			(cfg.PoolEnd-cfg.PoolStart)>>20,
		)
	}

	info, err := network.Init(
		opts.Address, opts.Gateway,
		board.VIRTIO_NET0, board.CPU, board.VIRTIO_NET0_IOAPIC,
	)

	if !opts.Quiet {
		if err != nil {
			fmt.Printf("network initialization error, %v\n", err)
		} else {
			fmt.Printf("%s\n", info)
		}
	}

	kernel.Log = os.Stdout
	cfg.Exit = board.PowerOff

	if opts.SSH != 0 {
		cfg.Ready = func() {
			if err := sshd.Start(uint16(opts.SSH)); err != nil {
				fmt.Printf("SSH server initialization error, %v\n", err)
			}
		}
	}

	status, err := kernel.Start(cfg)

	if err != nil {
		fmt.Printf("kernel initialization error, %v\n", err)
		board.PowerOff(1)
	}

	code := (status >> 8) & 0xff

	if sig := status & 0x7f; sig != 0 {
		code = 128 + sig
	}

	if !opts.Quiet {
		fmt.Printf("init exited with status %d, powering off\n", code)
	}

	board.PowerOff(code)
}
