// Copyright (c) The eggshell Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

// Package monitor provides execution of isolated user mode (ring 3) contexts
// under a TamaGo unikernel running in ring 0 on amd64.
//
// It follows the GoTEE monitor design (https://github.com/usbarmory/GoTEE):
// a single exit point (exec), which switches to the execution context, and a
// single return point (kreturn), reached from the syscall instruction entry
// point and from every interrupt/exception vector, which saves the context
// and resumes the calling goroutine as if exec returned.
//
// While a context runs, the monitor installs its own IDT (all vectors trap
// back to Go) and the user FS base; a GDT with user segments and a TSS (ring 0
// stack for interrupts from ring 3) is installed once by Init, keeping the
// TamaGo kernel selectors.
//
// Floating point state: all state components enabled in XCR0 (x87, SSE, AVX
// and, when present, AVX-512) are restored with XRSTOR when a context is
// entered and saved with XSAVE on every trap, the Go runtime then runs with its
// own MXCSR and a zeroed X15 (its ABI zero register). XSAVE, unlike XSAVEOPT,
// always writes the legacy region, which the kernel copies to and from signal
// frames.
//
// The LAPIC timer (vector TimerVector) preempts running contexts.
//
// Device interrupts received while a context runs are relayed, once the TamaGo
// IDT is restored, as software interrupts of the same vector. They reach the
// TamaGo interrupt handler (amd64.CPU.ServiceInterrupts), which acknowledges
// them once serviced, as if received while Go code runs.
//
// This package is only meant to be used with `GOOS=tamago` as
// supported by the TamaGo framework for bare metal Go, see
// https://github.com/usbarmory/tamago.
package monitor

import (
	"encoding/binary"
	"fmt"
	"strings"
	"sync"

	"github.com/usbarmory/tamago/amd64"
	"github.com/usbarmory/tamago/amd64/lapic"
)

const (
	// VectorSyscall is the pseudo vector reported for system calls.
	VectorSyscall = 256

	// TimerVector is the LAPIC timer vector used for preemption, it
	// matches TamaGo IRQ_WAKEUP which is acknowledged by TamaGo when
	// raised while the Go runtime runs.
	TimerVector = amd64.IRQ_WAKEUP

	// privilege levels (selector RPL, matching DPL)
	ring0 = 0
	ring3 = 3

	cr4FSGSBASE = 1 << 16

	// segment selectors
	kernelCS = 0x08 | ring0
	kernelDS = 0x10 | ring0
	userDS   = 0x18 | ring3
	userCS   = 0x20 | ring3
	tssSel   = 0x28 | ring0

	efer   = 0xc0000080
	star   = 0xc0000081
	lstar  = 0xc0000082
	sfmask = 0xc0000084

	kernelStackSize = 32 * 1024

	callSize         = 5
	callDivMul       = 52429
	callDivShift     = 18
	errorCodeVectors = 1<<8 | 1<<10 | 1<<11 | 1<<12 | 1<<13 | 1<<14 | 1<<17 | 1<<21 | 1<<29 | 1<<30
)

// ExecCtx represents a user mode execution context initialization or returning
// state.
type ExecCtx struct {
	RAX uint64
	RBX uint64
	RCX uint64
	RDX uint64
	RSI uint64
	RDI uint64
	RBP uint64
	RSP uint64
	R8  uint64
	R9  uint64
	R10 uint64
	R11 uint64
	R12 uint64
	R13 uint64
	R14 uint64
	R15 uint64

	RIP    uint64
	RFLAGS uint64
	CS     uint64

	FSBase uint64
	GSBase uint64
	CR3    uint64

	// trap information
	Vector    uint64
	ErrorCode uint64
	CR2       uint64

	FP *FPState

	// executing g stack pointer
	g_sp uint64
}

// descriptorTable represents the GDTR and IDTR registers.
type descriptorTable struct {
	Limit uint16
	Base  uint64
}

// taskState represents the 64-bit Task State Segment.
type taskState struct {
	_ uint32
	// ring 0-2 stacks
	RSP [3]uint64
	_   uint64
	// interrupt stacks (IST1-IST7)
	IST [7]uint64
	_   uint64
	_   uint16
	// I/O permission bitmap offset
	IOPB uint16
}

// cpu represents the monitor state of a processor.
//
// For now a single instance (BSP) exists, the trap entry points locate it
// statically (see monitor_amd64.s).
type cpu struct {
	// running context, only set by exec
	ctx uintptr
	// system call entry scratch register
	scratch uint64

	// GDT with this processor TSS, which holds its ring 0 stack
	gdt    [7]uint64
	gdtr   [16]byte
	tss    [104]byte
	kstack [kernelStackSize]byte
	// interrupt stack for NMI, #DF and #MC, which can occur in ring 0
	// with the user stack pointer (see enter)
	istack [4096]byte

	// TLB flush request before the next context execution
	forceCR3 uint8
	// timeslice deadline (TSC)
	armedTSC uint64
}

var (
	mux sync.Mutex

	// boot processor monitor state
	bsp cpu

	// monitor IDT, shared by all processors
	idt   [256 * 2]uint64
	uIDTR [16]byte

	// AVX enabled (YMM state)
	hasAVX uint8
	// XSAVEOPT supported, saves only modified state components
	hasXSAVEOPT uint8
	// FS/GS base instructions enabled (CR4.FSGSBASE), usable from ring 3
	hasFSGSBASE uint8
	// Go runtime MXCSR
	kernelMXCSR uint32 = 0x1f80

	kernelCR3 uint64
	defaultFP FPState

	// TSC frequency (Hz)
	tscHz uint64
)

// defined in monitor_amd64.s
func aligned64(fp *FPState) bool
func exec(ctx *ExecCtx)
func syscallEntryAddr() uint64
func vectorsAddr() uint64
func tableAddrs(c *cpu) (gdt uint64, tss uint64, idt uint64, kstack uint64, istack uint64)
func relay(n uint64)

// vectorAddr returns the monitor IDT entry point for the argument vector.
func vectorAddr(n int) uint64 {
	return vectorsAddr() + uint64(n)*callSize
}

func gateDescriptor(addr uint64, dpl uint64) (lo, hi uint64) {
	lo = addr&0xffff | kernelCS<<16 | (0x8e|dpl<<5)<<40 | (addr>>16&0xffff)<<48
	hi = addr >> 32
	return
}

// Init installs the user mode GDT, TSS, IDT and system call MSRs, the
// argument is the TSC frequency used for timeslice accounting.
func Init(tscFrequency uint64) error {
	if tscFrequency == 0 {
		return fmt.Errorf("invalid TSC frequency")
	}

	tscHz = tscFrequency

	// GDT: TamaGo kernel selectors plus user segments and TSS
	c := &bsp
	gdt := &c.gdt

	gdt[0] = 0
	gdt[1] = 0x00209a0000000000 // kernel code (64-bit)
	gdt[2] = 0x0000920000000000 // kernel data
	gdt[3] = 0x0000f20000000000 // user data (DPL 3)
	gdt[4] = 0x0020fa0000000000 // user code (64-bit, DPL 3)

	gdtAddr, tssAddr, idtAddr, kstackAddr, istackAddr := tableAddrs(c)

	// TSS: ring 0 stack for interrupts from ring 3, interrupt stack (IST1)
	// for NMI, #DF and #MC, no I/O permissions
	tss := taskState{IOPB: uint16(len(c.tss))}
	tss.RSP[0] = (kstackAddr + kernelStackSize - 64) &^ 15
	tss.IST[0] = (istackAddr + uint64(len(c.istack)) - 64) &^ 15
	binary.Encode(c.tss[:], binary.LittleEndian, &tss)

	limit := uint64(len(c.tss) - 1)
	gdt[5] = limit&0xffff | (tssAddr&0xffffff)<<16 | 0x89<<40 | (limit>>16&0xf)<<48 | (tssAddr>>24&0xff)<<56
	gdt[6] = tssAddr >> 32

	binary.Encode(c.gdtr[:], binary.LittleEndian, &descriptorTable{uint16(len(gdt)*8 - 1), gdtAddr})
	lgdt(&c.gdtr)
	ltr(tssSel)

	// IDT: every vector returns to Go, breakpoints allowed from ring 3
	for i := range 256 {
		dpl := uint64(0)

		if i == 3 || i == 4 {
			dpl = ring3
		}

		idt[i*2], idt[i*2+1] = gateDescriptor(vectorAddr(i), dpl)

		// NMI, #DF, #MC: IST1
		if i == 2 || i == 8 || i == 18 {
			idt[i*2] |= 1 << 32
		}
	}

	binary.Encode(uIDTR[:], binary.LittleEndian, &descriptorTable{uint16(len(idt)*8 - 1), idtAddr})

	// system call instruction: EFER.SCE, also enable NX (EFER.NXE)
	wrmsr(efer, rdmsr(efer)|1<<0|1<<11)

	// SYSRET selectors: SS = STAR[63:48]+8 (userDS), CS = STAR[63:48]+16 (userCS)
	wrmsr(star, uint64(kernelDS|ring3)<<48|uint64(kernelCS)<<32)
	wrmsr(lstar, syscallEntryAddr())

	// mask TF, IF, DF, IOPL, NT, AC on entry
	wrmsr(sfmask, 0x47700)

	cpu := amd64.CPU{}

	// floating point state: XSAVE enabled (CPUID.1:ECX.OSXSAVE), area size
	// for the components enabled in XCR0 (CPUID.(EAX=0DH,ECX=0):EBX)
	if _, _, ecx, _ := cpu.CPUID(1, 0); ecx&(1<<27) == 0 {
		return fmt.Errorf("XSAVE not enabled")
	}

	if _, ebx, _, _ := cpu.CPUID(0xd, 0); int(ebx) > len(FPState{}) {
		return fmt.Errorf("XSAVE area size %d exceeds FP state", ebx)
	}

	// AVX usable: CPUID.1:ECX.(OSXSAVE|AVX), XCR0.(SSE|AVX)
	if _, _, ecx, _ := cpu.CPUID(1, 0); ecx&(1<<27|1<<28) == 1<<27|1<<28 && xgetbv(0)&6 == 6 {
		hasAVX = 1
	}

	// XSAVEOPT: CPUID.(EAX=0DH,ECX=1):EAX[0]
	if eax, _, _, _ := cpu.CPUID(0xd, 1); eax&1 != 0 {
		hasXSAVEOPT = 1
	}

	// RDFSBASE/WRFSBASE: CPUID.(EAX=07H,ECX=0):EBX[0], enabled through
	// CR4.FSGSBASE, cheaper than the FS base MSR on each context switch
	if _, ebx, _, _ := cpu.CPUID(7, 0); ebx&1 != 0 {
		writeCR4(readCR4() | cr4FSGSBASE)
		hasFSGSBASE = 1
	}

	defaultFP = FPState{}
	binary.LittleEndian.PutUint16(defaultFP[0:], 0x037f)      // FCW
	binary.LittleEndian.PutUint32(defaultFP[24:], 0x1f80)     // MXCSR
	binary.LittleEndian.PutUint32(defaultFP[28:], 0x0000ffff) // MXCSR_MASK

	kernelCR3 = readCR3()

	// LAPIC timer: one-shot, divide by 1
	write32(amd64.LAPIC_BASE+lapic.LAPIC_LVT_TIMER_DIV, 0b1011)
	write32(amd64.LAPIC_BASE+lapic.LAPIC_LVT_TIMER, TimerVector)

	return nil
}

// InvalidateTLB requests a TLB flush before the next context execution.
func InvalidateTLB() {
	bsp.forceCR3 = 1
}

// ReleaseRoot switches to the kernel page tables if the argument page table
// root is in use, so that it can be freed.
func ReleaseRoot(root uint64) {
	if readCR3() == root {
		writeCR3(kernelCR3)
	}
}

// EOI acknowledges the current LAPIC interrupt.
func EOI() {
	write32(amd64.LAPIC_EOI, 0)
}

func armTimer(ns uint64) {
	ns = min(ns, 0xffffffff)
	write32(amd64.LAPIC_BASE+lapic.LAPIC_LVT_TIMER_INIT, uint32(ns))
}

// Run starts the execution context in ring 3, the execution yields back to the
// invoking Go runtime only when exceptions are caught or the argument timer
// expires.
func (ctx *ExecCtx) Run(quantum uint64) {
	mux.Lock()
	defer mux.Unlock()

	if ctx.FP == nil || !aligned64(ctx.FP) {
		panic("misaligned or missing FP state")
	}

	if quantum > 0 {
		if now := rdtsc(); now >= bsp.armedTSC {
			armTimer(quantum)
			bsp.armedTSC = now + quantum*tscHz/1e9
		}
	}

	exec(ctx)

	if v := ctx.Vector; v >= 32 && v < TimerVector {
		relay(v)
	}

	bsp.ctx = 0
}

// Idle halts the processor until the argument number of nanoseconds elapse,
// or any other interrupt is received.
func Idle(ns uint64) {
	bsp.armedTSC = 0
	ns = min(ns, 0xffffffff)
	idleHalt(uint32(ns))
}

// String returns the string form of the execution context registers.
func (ctx *ExecCtx) String() string {
	var sb strings.Builder

	fmt.Fprintf(&sb, "  rax:%016x rbx:%016x rcx:%016x rdx:%016x\n", ctx.RAX, ctx.RBX, ctx.RCX, ctx.RDX)
	fmt.Fprintf(&sb, "  rsi:%016x rdi:%016x rbp:%016x rsp:%016x\n", ctx.RSI, ctx.RDI, ctx.RBP, ctx.RSP)
	fmt.Fprintf(&sb, "   r8:%016x  r9:%016x r10:%016x r11:%016x\n", ctx.R8, ctx.R9, ctx.R10, ctx.R11)
	fmt.Fprintf(&sb, "  r12:%016x r13:%016x r14:%016x r15:%016x\n", ctx.R12, ctx.R13, ctx.R14, ctx.R15)
	fmt.Fprintf(&sb, "  rip:%016x rflags:%x cs:%x vector:%d err:%x cr2:%x\n", ctx.RIP, ctx.RFLAGS, ctx.CS, ctx.Vector, ctx.ErrorCode, ctx.CR2)

	return sb.String()
}
