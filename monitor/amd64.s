// Copyright (c) The eggshell Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

#include "go_asm.h"
#include "textflag.h"

// func alarm(count uint32, poll uint64, gap uint64) bool
TEXT ·alarm(SB),NOSPLIT,$0-25
	MOVL	count+0(FP), BX
	PUSHFQ
	CLI

	CALL	·current(SB)

	// a call past gap cycles from the last one starts an idle period
	MOVQ	AX, CX
	SUBQ	cpu_idleLast(DI), CX
	MOVQ	AX, cpu_idleLast(DI)
	CMPQ	CX, gap+16(FP)
	JBE	period
	MOVQ	AX, cpu_idleStart(DI)
period:
	SUBQ	cpu_idleStart(DI), AX
	CMPQ	AX, poll+8(FP)
	SETCC	ret+24(FP)
	JCS	done

	// the timeslice timer is reused (see exec)
	MOVQ	$0, cpu_armedTSC(DI)

	MOVQ	$const_lapicTimerInit, AX
	MOVL	BX, (AX)
done:
	POPFQ
	RET

// func xgetbv(index uint32) uint64
TEXT ·xgetbv(SB),NOSPLIT,$0-16
	MOVL	index+0(FP), CX
	XGETBV
	SHLQ	$32, DX
	MOVL	AX, AX
	ORQ	DX, AX
	MOVQ	AX, ret+8(FP)
	RET
