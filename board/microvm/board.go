// Copyright (c) The eggshell Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

package microvm

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"runtime/goos"
	"time"

	"github.com/usbarmory/tamago/amd64"

	"github.com/usbarmory/eggshell/kernel"
	"github.com/usbarmory/eggshell/monitor"
)

const (
	// QEMU firmware configuration
	FW_CFG_PORT_SEL     = 0x510
	FW_CFG_PORT_DATA    = 0x511
	FW_CFG_CMDLINE_SIZE = 0x14
	FW_CFG_CMDLINE_DATA = 0x15

	// isa-debug-exit device (-device isa-debug-exit,iobase=0xf4)
	DEBUG_EXIT_PORT = 0xf4

	maxIdle = int64(10 * time.Millisecond)
)

var boardName = "microvm"

// defined in io_amd64.s
func Out8(port uint16, val uint8)

func Name() string {
	return boardName
}

func PowerOff(code int) {
	goos.Exit(int32(code))
}

// idle halts the processor until the next runtime deadline or IRQ_WAKEUP,
// processors without deadline (0) halt for at most maxIdle, as they are not
// woken up when goroutines become runnable on other processors (M's never
// stop, see runtime.beforeIdle).
func idle(until int64) {
	now := time.Now().UnixNano()

	if until == 0 || until-now > maxIdle {
		until = now + maxIdle
	}

	if until > now {
		monitor.Alarm(uint64(until - now))
		CPU.WaitInterrupt()
	}
}

// HeapSize returns the memory available to the Go heap (the Go runtime
// memory past the kernel image, which embeds the root file system).
func HeapSize() uint64 {
	_, dataEnd := runtime.DataRegion()
	return RamStart + RamSize - dataEnd
}

func Init(cfg *kernel.Config) error {
	freq := uint64(CPU.Freq())

	if err := monitor.Init(freq); err != nil {
		return err
	}

	boardName += fmt.Sprintf(" (%d MHz)", freq/1e6)

	cfg.Console = UART0
	cfg.PoolStart = UserStart
	cfg.PoolEnd = UserStart + UserSize

	goos.Idle = idle
	goos.Exit = exit

	if n := runtime.NumCPU(); n > 1 {
		preallocateMs(n)
	}

	// inform the Go runtime about its memory size
	debug.SetMemoryLimit(int64(HeapSize()) * 7 / 8)

	// runtime time at the same counter value
	tsc0 := CPU.Counter()
	now := int64(float64(tsc0)*CPU.TimerMultiplier) + CPU.TimerOffset
	kernel.SetClock(CPU.Counter, tsc0, 1e9<<32/freq, now)

	return nil
}

// exit powers off the machine through the isa-debug-exit device, falling back
// to a triple fault (-no-reboot).
func exit(code int32) {
	Out8(DEBUG_EXIT_PORT, uint8(code))
	time.Sleep(10 * time.Millisecond)
	amd64.Fault()
}
