// Copyright (c) The eggshell Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

// Package kernel implements a Linux compatible POSIX process environment meant
// to run within a TamaGo unikernel.
//
// Executables linked against a POSIX C library such as musl run in user mode.
// Their system calls trap to the Go runtime through the monitor package and
// are serviced here through the Linux ABI, each user thread is backed by a
// goroutine so that blocking system calls map naturally to the Go scheduler.
package kernel

import (
	"fmt"
	"io"
	"runtime"
	"sync"
	"time"
)

// Console represents the serial console used as system TTY.
type Console interface {
	Tx(c byte)
	Rx(block bool) (c byte, valid bool)
}

// Config represents the kernel configuration.
type Config struct {
	// Console is the system console.
	Console Console

	// PoolStart and PoolEnd define the physical memory available for
	// processes (page frames and page tables).
	PoolStart uint64
	PoolEnd   uint64

	// TimerHz is the frequency of the timer used by the monitor for
	// preemption (see monitor.ExecCtx.Run).
	TimerHz uint64

	// Exit powers off the machine.
	Exit func(code int)

	// Rootfs is an uncompressed (ustar) tar archive unpacked as the root
	// file system.
	Rootfs []byte

	// Init is the first process command line, Env its environment.
	Init []string
	Env  []string

	// Hostname is the initial host name.
	Hostname string

	// Strace enables system call tracing on the console.
	Strace bool

	// Ready, if not nil, is called once the init process is started, to
	// start kernel services (see Listen, OpenPTY, Exec).
	Ready func()
}

var (
	klock sync.Mutex
	kcond = sync.NewCond(&klock)

	cfg      *Config
	bootTime time.Time

	// asynchronous wake up requests from goroutines not holding klock
	notifyCh = make(chan struct{}, 1)

	// Log is the kernel log destination.
	Log io.Writer
)

// timeslice for user mode preemption
const quantum = 10 * time.Millisecond

// Notify requests a wake up of all blocked tasks, it is safe to call
// without holding the kernel lock (e.g. from network stack callbacks).
func Notify() {
	select {
	case notifyCh <- struct{}{}:
	default:
	}
}

func waker() {
	for range notifyCh {
		klock.Lock()
		wakeAll()
		klock.Unlock()
	}
}

// wakeAll wakes up all blocked tasks, klock must be held.
func wakeAll() {
	pollSeq.Add(1)
	kcond.Broadcast()
	wakeQueued()
}

func printk(format string, args ...any) {
	if Log != nil {
		fmt.Fprintf(Log, format, args...)
	}
}

// Start initializes the kernel and runs the init process, it returns when the
// init process exits.
func Start(c *Config) (status int, err error) {
	cfg = c
	bootTime = time.Now()
	bootNano = realtime()

	initPages(c.PoolStart, c.PoolEnd)
	vdsoInit()

	if c.Hostname != "" {
		hostname = c.Hostname
	}

	initVFS()

	if len(c.Rootfs) > 0 {
		if err = unpackTar(c.Rootfs); err != nil {
			return
		}
	}

	initDevices()
	initProc()

	go waker()
	go console.poll()

	klock.Lock()
	initProcess, err := spawnInit(c.Init, c.Env)
	klock.Unlock()

	if err != nil {
		return
	}

	if c.Ready != nil {
		go c.Ready()
	}

	<-initProcess.done

	return initProcess.exitStatus, nil
}

func yield() {
	klock.Unlock()
	defer klock.Lock()

	runtime.Gosched()
}
