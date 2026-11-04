// Copyright (c) The eggshell Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

#include "go_asm.h"
#include "textflag.h"

#define MSR_FS_BASE 0xc0000100

// The Ring 0 (Go runtime) <> ring 3 (POSIX process) context switch follows the
// GoTEE exit point (exec) / return point (kreturn) design.
//
// The Go runtime state is saved in the exec frame on the calling goroutine
// stack, enter records the stack pointer in the context (ExecCtx_g_sp) for
// kreturn to return into exec, which restores it.

#define FRAME_RFLAGS 0
#define FRAME_R12 8
#define FRAME_R13 16
#define FRAME_R14 24
#define FRAME_R15 32
#define FRAME_FS 40
#define FRAME_IDTR 48

// SAVE_GPRS saves general purpose registers, but RAX and RSP, to the context
// pointed by AX.
#define SAVE_GPRS \
	MOVQ	BX, ExecCtx_RBX(AX); \
	MOVQ	CX, ExecCtx_RCX(AX); \
	MOVQ	DX, ExecCtx_RDX(AX); \
	MOVQ	SI, ExecCtx_RSI(AX); \
	MOVQ	DI, ExecCtx_RDI(AX); \
	MOVQ	BP, ExecCtx_RBP(AX); \
	MOVQ	R8, ExecCtx_R8(AX); \
	MOVQ	R9, ExecCtx_R9(AX); \
	MOVQ	R10, ExecCtx_R10(AX); \
	MOVQ	R11, ExecCtx_R11(AX); \
	MOVQ	R12, ExecCtx_R12(AX); \
	MOVQ	R13, ExecCtx_R13(AX); \
	MOVQ	R14, ExecCtx_R14(AX); \
	MOVQ	R15, ExecCtx_R15(AX)

// RESTORE_GPRS restores general purpose registers except RBX (the context
// pointer), RCX and R11 (set by SYSRET) and RSP, from the context pointed by
// BX.
#define RESTORE_GPRS \
	MOVQ	ExecCtx_RAX(BX), AX; \
	MOVQ	ExecCtx_RDX(BX), DX; \
	MOVQ	ExecCtx_RSI(BX), SI; \
	MOVQ	ExecCtx_RDI(BX), DI; \
	MOVQ	ExecCtx_RBP(BX), BP; \
	MOVQ	ExecCtx_R8(BX), R8; \
	MOVQ	ExecCtx_R9(BX), R9; \
	MOVQ	ExecCtx_R10(BX), R10; \
	MOVQ	ExecCtx_R12(BX), R12; \
	MOVQ	ExecCtx_R13(BX), R13; \
	MOVQ	ExecCtx_R14(BX), R14; \
	MOVQ	ExecCtx_R15(BX), R15

// func aligned64(fp *FPState) bool
TEXT ·aligned64(SB),NOSPLIT,$0-9
	MOVQ	fp+0(FP), AX
	TESTQ	$63, AX
	SETEQ	ret+8(FP)
	RET

// func exec(ctx *ExecCtx)
TEXT ·exec(SB),NOSPLIT,$64-8
	MOVQ	ctx+0(FP), BX
	MOVQ	$·bsp(SB), AX
	MOVQ	BX, cpu_ctx(AX)

	PUSHFQ
	POPQ	AX
	MOVQ	AX, FRAME_RFLAGS(SP)
	CLI

	MOVQ	R12, FRAME_R12(SP)
	MOVQ	R13, FRAME_R13(SP)
	MOVQ	R14, FRAME_R14(SP)
	MOVQ	R15, FRAME_R15(SP)

	// kernel FS base (Go TLS)
	CMPB	·hasFSGSBASE(SB), $0
	JEQ	rdmsr
	RDFSBASEQ	AX
	MOVQ	AX, FRAME_FS(SP)
	JMP	idt
rdmsr:
	MOVL	$MSR_FS_BASE, CX
	RDMSR
	MOVL	AX, FRAME_FS(SP)
	MOVL	DX, FRAME_FS+4(SP)
idt:

	// TamaGo IDT
	SIDT	FRAME_IDTR(SP)

	// returns through kreturn
	CALL	·enter(SB)

	CMPB	·hasFSGSBASE(SB), $0
	JEQ	wrmsr
	MOVQ	FRAME_FS(SP), AX
	WRFSBASEQ	AX
	JMP	kidt
wrmsr:
	MOVL	$MSR_FS_BASE, CX
	MOVL	FRAME_FS(SP), AX
	MOVL	FRAME_FS+4(SP), DX
	WRMSR
kidt:
	LIDT	FRAME_IDTR(SP)

	MOVQ	FRAME_R12(SP), R12
	MOVQ	FRAME_R13(SP), R13
	MOVQ	FRAME_R14(SP), R14
	MOVQ	FRAME_R15(SP), R15

	// frame pointer (saved above the frame by the prologue), restored
	// before interrupts are enabled
	MOVQ	64(SP), BP

	MOVQ	FRAME_RFLAGS(SP), AX
	PUSHQ	AX
	POPFQ
	RET

TEXT ·enter(SB),NOSPLIT|NOFRAME,$0
	MOVQ	SP, ExecCtx_g_sp(BX)

	// user FS and GS base (a null selector keeps the base on privilege
	// change), GS is only settable by user mode through WRGSBASE
	XORL	AX, AX
	MOVW	AX, FS
	MOVQ	ExecCtx_FSBase(BX), AX
	CMPB	·hasFSGSBASE(SB), $0
	JEQ	wrmsr
	WRFSBASEQ	AX
	MOVQ	ExecCtx_GSBase(BX), AX
	WRGSBASEQ	AX
	JMP	uidt
wrmsr:
	MOVQ	AX, DX
	SHRQ	$32, DX
	MOVL	$MSR_FS_BASE, CX
	WRMSR
uidt:

	// monitor IDT
	LIDT	·uIDTR(SB)

	// address space
	MOVQ	$·bsp(SB), CX
	MOVQ	ExecCtx_CR3(BX), AX
	MOVQ	CR3, DX
	CMPQ	AX, DX
	JNE	loadcr3

	CMPB	cpu_forceCR3(CX), $0
	JEQ	fp
loadcr3:
	MOVQ	AX, CR3
	MOVB	$0, cpu_forceCR3(CX)
fp:
	// user floating point state (all enabled components)
	MOVQ	ExecCtx_FP(BX), CX
	MOVL	$-1, AX
	MOVL	$-1, DX
	XRSTOR64	(CX)

	// System call returns with RCX and RIP, R11 and RFLAGS unchanged (as
	// left by the syscall instruction), in such cases we use SYSRET as it
	// is cheaper than IRETQ.

	CMPQ	ExecCtx_Vector(BX), $const_VectorSyscall
	JNE	iret

	MOVQ	ExecCtx_RIP(BX), CX
	CMPQ	CX, ExecCtx_RCX(BX)
	JNE	iret

	MOVQ	ExecCtx_RFLAGS(BX), R11
	CMPQ	R11, ExecCtx_R11(BX)
	JNE	iret

	MOVQ	CX, AX
	SHRQ	$47, AX
	JNE	iret

	ANDQ	$0x250dd5, R11	// user settable flags (no IOPL)
	ORQ	$0x202, R11	// IF

	RESTORE_GPRS
	MOVQ	ExecCtx_RSP(BX), SP
	MOVQ	ExecCtx_RBX(BX), BX

	// SYSRETQ (the assembler only encodes the 32-bit form)
	BYTE	$0x48; BYTE $0x0f; BYTE $0x07

iret:
	// interrupt return frame: SS, RSP, RFLAGS, CS, RIP
	PUSHQ	$const_userDS
	PUSHQ	ExecCtx_RSP(BX)
	MOVQ	ExecCtx_RFLAGS(BX), AX
	ANDQ	$0x250dd5, AX	// user settable flags (no IOPL)
	ORQ	$0x202, AX	// IF
	PUSHQ	AX
	PUSHQ	$const_userCS
	PUSHQ	ExecCtx_RIP(BX)

	RESTORE_GPRS
	MOVQ	ExecCtx_RCX(BX), CX
	MOVQ	ExecCtx_R11(BX), R11
	MOVQ	ExecCtx_RBX(BX), BX

	IRETQ

TEXT ·kreturn(SB),NOSPLIT|NOFRAME,$0
	MOVQ	$·bsp(SB), BX
	MOVQ	cpu_ctx(BX), BX

	// user FS and GS base, which user mode can change with FSGSBASE
	CMPB	·hasFSGSBASE(SB), $0
	JEQ	fp
	RDFSBASEQ	CX
	MOVQ	CX, ExecCtx_FSBase(BX)
	RDGSBASEQ	CX
	MOVQ	CX, ExecCtx_GSBase(BX)
fp:
	// user floating point state (all enabled components), XSAVEOPT skips
	// unmodified ones and those in their initial configuration
	MOVQ	ExecCtx_FP(BX), CX
	MOVL	$-1, AX
	MOVL	$-1, DX
	CMPB	·hasXSAVEOPT(SB), $0
	JEQ	xsave
	XSAVEOPT64	(CX)
	JMP	avx
xsave:
	XSAVE64	(CX)
avx:

	CMPB	·hasAVX(SB), $0
	JEQ	kmxcsr
	VZEROUPPER
kmxcsr:
	// Go runtime floating point environment (X15 is the ABI zero register)
	LDMXCSR	·kernelMXCSR(SB)
	XORPS	X15, X15

	MOVQ	ExecCtx_g_sp(BX), SP
	RET

// the user one, hence the static references.
TEXT ·syscallEntry(SB),NOSPLIT|NOFRAME,$0
	MOVQ	AX, ·bsp+cpu_scratch(SB)
	MOVQ	·bsp+cpu_ctx(SB), AX

	SAVE_GPRS
	MOVQ	CX, ExecCtx_RIP(AX)
	MOVQ	R11, ExecCtx_RFLAGS(AX)
	MOVQ	SP, ExecCtx_RSP(AX)

	MOVQ	·bsp+cpu_scratch(SB), BX
	MOVQ	BX, ExecCtx_RAX(AX)

	MOVQ	$const_VectorSyscall, ExecCtx_Vector(AX)
	MOVQ	$const_userCS, ExecCtx_CS(AX)

	JMP	·kreturn(SB)

// Common interrupt/exception entry, the stack holds: vector, error code,
// RIP, CS, RFLAGS, RSP, SS.
TEXT ·intrCommon(SB),NOSPLIT|NOFRAME,$0
	PUSHQ	AX
	MOVQ	$·bsp(SB), AX
	MOVQ	cpu_ctx(AX), AX

	SAVE_GPRS

	POPQ	ExecCtx_RAX(AX)
	POPQ	ExecCtx_Vector(AX)
	POPQ	ExecCtx_ErrorCode(AX)
	POPQ	ExecCtx_RIP(AX)
	POPQ	ExecCtx_CS(AX)
	POPQ	ExecCtx_RFLAGS(AX)
	POPQ	ExecCtx_RSP(AX)
	ADDQ	$8, SP

	MOVQ	CR2, BX
	MOVQ	BX, ExecCtx_CR2(AX)
	JMP	·kreturn(SB)

// Interrupt and exception vectors: a table of CALLs (callSize bytes each) to
// intrEntry, whose return address identifies the vector.

#define V1 CALL ·intrEntry(SB)
#define V4 V1; V1; V1; V1
#define V16 V4; V4; V4; V4
#define V64 V16; V16; V16; V16

TEXT ·vectors(SB),NOSPLIT|NOFRAME,$0
	V64; V64; V64; V64

TEXT ·intrEntry(SB),NOSPLIT|NOFRAME,$0
	PUSHQ	BX
	PUSHQ	CX

	// vector = (return address - ·vectors - callSize) / callSize
	MOVQ	16(SP), BX
	MOVQ	$·vectors(SB), CX
	SUBQ	CX, BX
	SUBQ	$const_callSize, BX
	IMULQ	$const_callDivMul, BX
	SHRQ	$const_callDivShift, BX

	CMPQ	BX, $32
	JAE	noerror
	MOVQ	$const_errorCodeVectors, CX
	BTQ	BX, CX
	JCC	noerror

	// the vector replaces the return address
	MOVQ	BX, 16(SP)
	POPQ	CX
	POPQ	BX
	JMP	·intrCommon(SB)

noerror:
	// a zero error code replaces the return address, the vector goes above
	MOVQ	$0, 16(SP)
	XCHGQ	BX, 8(SP)
	POPQ	CX
	JMP	·intrCommon(SB)

// func vectorsAddr() uint64
TEXT ·vectorsAddr(SB),NOSPLIT,$0-8
	MOVQ	$·vectors(SB), AX
	MOVQ	AX, ret+0(FP)
	RET

// func tableAddrs(c *cpu) (gdt uint64, tss uint64, idt uint64, kstack uint64, istack uint64)
TEXT ·tableAddrs(SB),NOSPLIT,$0-48
	MOVQ	c+0(FP), BX
	LEAQ	cpu_gdt(BX), AX
	MOVQ	AX, gdt+8(FP)
	LEAQ	cpu_tss(BX), AX
	MOVQ	AX, tss+16(FP)
	MOVQ	$·idt(SB), AX
	MOVQ	AX, idt+24(FP)
	LEAQ	cpu_kstack(BX), AX
	MOVQ	AX, kstack+32(FP)
	LEAQ	cpu_istack(BX), AX
	MOVQ	AX, istack+40(FP)
	RET

// func syscallEntryAddr() uint64
TEXT ·syscallEntryAddr(SB),NOSPLIT,$0-8
	MOVQ	$·syscallEntry(SB), AX
	MOVQ	AX, ret+0(FP)
	RET

// func relay(n uint64)
//
// Raises a vector through the TamaGo IDT (in use) as a software interrupt
// would, by calling its gate with an interrupt return frame.
TEXT ·relay(SB),NOSPLIT,$16-8
	SIDT	0(SP)
	MOVQ	n+0(FP), AX
	SHLQ	$4, AX
	ADDQ	2(SP), AX

	// gate offset: bits 15:0, 31:16 (+6) and 63:32 (+8)
	MOVWQZX	0(AX), BX
	MOVWQZX	6(AX), CX
	SHLQ	$16, CX
	ORQ	CX, BX
	MOVLQZX	8(AX), CX
	SHLQ	$32, CX
	ORQ	CX, BX

	// interrupt return frame: SS, RSP, RFLAGS, CS, RIP (pushed by CALL),
	// on a 16 byte aligned stack as the processor does, IRETQ restores SP
	PUSHFQ
	POPQ	DX
	MOVQ	SP, CX
	ANDQ	$~15, SP
	LEAQ	-32(SP), SP
	MOVQ	$const_kernelDS, 24(SP)
	MOVQ	CX, 16(SP)
	MOVQ	DX, 8(SP)
	MOVQ	$const_kernelCS, 0(SP)
	CLI
	CALL	BX
	RET
