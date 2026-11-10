// Copyright (c) The eggshell Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

#include "go_asm.h"
#include "textflag.h"

// func alarm(count uint32)
TEXT ·alarm(SB),NOSPLIT,$0-4
	MOVL	count+0(FP), BX
	PUSHFQ
	CLI

	// the timeslice timer is reused (see exec)
	CALL	·current(SB)
	MOVQ	$0, cpu_armedTSC(DI)

	MOVQ	$const_lapicTimerInit, AX
	MOVL	BX, (AX)

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
