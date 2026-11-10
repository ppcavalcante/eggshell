// Copyright (c) The eggshell Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

package kernel

import (
	"debug/elf"
	"encoding/binary"
	"fmt"
	"runtime"
	"time"

	"github.com/usbarmory/eggshell/monitor"
)

const (
	elfMachine = elf.EM_X86_64
	utsMachine = "x86_64"

	// advertised in /proc/cpuinfo
	flags = "fpu tsc msr pae cx8 apic sep cmov pat clflush mmx fxsr sse sse2 syscall nx lm"

	// load bias for position independent executables (outside the kernel
	// range, see mmu_amd64.go)
	pieBase = 0x2800000000
	// load bias for the program interpreter
	interpBase = 0x2000000000

	// CPUID.01H:EDX of a generic x86-64 processor
	hwcap = 0x178bfbff

	SA_RESTORER = 0x04000000

	FPE_INTDIV = 1
	FPE_FLTINV = 7
	ILL_ILLOPN = 2

	// user mode RFLAGS bits settable through sigreturn
	rflagsUser = 0x3f7fd5
	rflagsIF   = 0x200
)

func (t *Task) sysargs() [6]uint64 {
	c := &t.ctx
	return [6]uint64{c.RDI, c.RSI, c.RDX, c.R10, c.R8, c.R9}
}

func (t *Task) sysno() uint64 {
	return t.ctx.RAX
}

func (t *Task) setRet(v uint64) {
	t.ctx.RAX = v
}

func (t *Task) pc() uint64 {
	return t.ctx.RIP
}

// saveRestartState records the state required to restart the current system
// call.
func (t *Task) saveRestartState() {
	t.origArg = t.ctx.RAX
}

// restartSyscall rewinds the context to re-execute the interrupted syscall
// instruction.
func (t *Task) restartSyscall() {
	t.ctx.RIP -= 2
	t.ctx.RAX = t.origArg
}

func cloneArgs(a *[6]uint64) (flags, sp, ptid, ctid, tls uint64) {
	return a[0], a[1], a[2], a[3], a[4]
}

func (child *Task) cloneRegs(newsp uint64, tls uint64, settls bool) {
	// the floating point state is not shared
	fp := *child.ctx.FP
	child.ctx.FP = &fp
	child.ctx.RAX = 0

	if newsp != 0 {
		child.ctx.RSP = newsp
	}

	if settls {
		child.ctx.FSBase = tls
	}
}

func (t *Task) resetRegs(entry uint64, sp uint64) {
	t.ctx = monitor.ExecCtx{
		RIP:    entry,
		RSP:    sp,
		RFLAGS: rflagsIF,
		FP:     monitor.NewFPState(),
	}
}

// enterUser runs the task in ring 3 until a trap.
func (t *Task) enterUser() trap {
	ctx := &t.ctx

	if ctx.FP == nil {
		ctx.FP = monitor.NewFPState()
	}

	ctx.Run(uint64(quantum))

	if ctx.CS&3 != 3 {
		panic(fmt.Sprintf("kernel exception in user mode switch (vector %d)\n%s", ctx.Vector, ctx))
	}

	switch v := ctx.Vector; {
	case v == monitor.VectorSyscall:
		return trap{kind: trapSyscall}
	case v >= 32:
		// timer or device interrupt, relayed by the monitor to its handler
		return trap{kind: trapPreempt}
	case v == 14:
		// page fault, error code: P(0) W/R(1) U/S(2) RSVD(3) I/D(4)
		access := PROT_READ

		switch {
		case ctx.ErrorCode&(1<<4) != 0:
			access = PROT_EXEC
		case ctx.ErrorCode&(1<<1) != 0:
			access = PROT_WRITE
		}

		return trap{kind: trapFault, addr: ctx.CR2, access: access}
	case v == 0:
		return trap{kind: trapSignal, sig: SIGFPE, code: FPE_INTDIV, addr: ctx.RIP}
	case v == 16 || v == 19:
		return trap{kind: trapSignal, sig: SIGFPE, code: FPE_FLTINV, addr: ctx.RIP}
	case v == 1 || v == 3:
		return trap{kind: trapSignal, sig: SIGTRAP, code: TRAP_BRKPT, addr: ctx.RIP}
	case v == 6:
		return trap{kind: trapSignal, sig: SIGILL, code: ILL_ILLOPN, addr: ctx.RIP}
	case v == 13:
		return trap{kind: trapSignal, sig: SIGSEGV, code: SI_KERNEL}
	case v == 12 || v == 17:
		return trap{kind: trapSignal, sig: SIGBUS, code: BUS_ADRALN}
	}

	printk("tid %d: unhandled exception\n%s", t.tid, ctx)

	return trap{kind: trapSignal, sig: SIGSEGV, code: SI_KERNEL}
}

// hwcap2 returns the AT_HWCAP2 auxiliary vector entry.
func hwcap2() uint64 {
	const HWCAP2_FSGSBASE = 1 << 1

	if monitor.FSGSBASE() {
		return HWCAP2_FSGSBASE
	}

	return 0
}

// size of the x86_64 struct epoll_event, which is packed
const epollEventSize = 12

// sigcontext represents the x86_64 struct sigcontext.
type sigcontext struct {
	Gregs   [18]uint64 // see gregs()
	CS      uint16
	GS      uint16
	FS      uint16
	SS      uint16
	Err     uint64
	Trapno  uint64
	Oldmask uint64
	CR2     uint64
	FPState uint64
	_       [8]uint64
}

// ucontext represents the x86_64 struct ucontext.
type ucontext struct {
	Flags    uint64
	Link     uint64
	Stack    sigaltstack
	Mcontext sigcontext
	Sigmask  uint64
}

// sigframe represents the x86_64 struct rt_sigframe, which is followed by the
// FXSAVE area referenced by uc_mcontext.fpstate.
type sigframe struct {
	Pretcode uint64
	UC       ucontext
	Info     [128]byte
}

// Bytes converts the signal frame structure to byte array format.
func (f *sigframe) Bytes() []byte {
	b, _ := binary.Append(nil, binary.LittleEndian, f)
	return b
}

var (
	frameUC      = uint64(binary.Size(sigframe{}.Pretcode))
	ucontextSize = uint64(binary.Size(ucontext{}))
	frameInfo    = frameUC + ucontextSize
	frameSize    = uint64(binary.Size(sigframe{}))
)

const (
	fpStateSize = monitor.FXSaveSize
	redZone     = 128
)

// sigcontext general purpose register order
func gregs(c *monitor.ExecCtx) []*uint64 {
	return []*uint64{
		&c.R8, &c.R9, &c.R10, &c.R11, &c.R12, &c.R13, &c.R14, &c.R15,
		&c.RDI, &c.RSI, &c.RBP, &c.RBX, &c.RDX, &c.RAX, &c.RCX, &c.RSP,
		&c.RIP, &c.RFLAGS,
	}
}

func (t *Task) setupFrame(sig int, info SigInfo, act SigAction, mask uint64) Errno {
	mm := t.proc.mm
	ctx := &t.ctx

	if act.Flags&SA_RESTORER == 0 || act.Restorer == 0 {
		return EFAULT
	}

	sp := ctx.RSP - redZone

	if act.Flags&SA_ONSTACK != 0 && t.altstack.Flags&SS_DISABLE == 0 && t.altstack.Size > 0 &&
		!(ctx.RSP > t.altstack.Sp && ctx.RSP <= t.altstack.Sp+t.altstack.Size) {
		sp = t.altstack.Sp + t.altstack.Size
	}

	// floating point state
	sp -= fpStateSize
	sp &^= 63
	fpstate := sp

	monitor.NormalizeFPState(ctx.FP)

	if err := mm.copyOut(fpstate, ctx.FP[:monitor.FXSaveSize]); err != 0 {
		return err
	}

	// ABI alignment as after a call: (sp + 8) % 16 == 0
	sp -= frameSize
	sp = ((sp + 8) &^ 15) - 8

	frame := &sigframe{
		Pretcode: act.Restorer,
		UC: ucontext{
			Stack: t.altstack,
			Mcontext: sigcontext{
				CS:      0x23,
				Err:     ctx.ErrorCode,
				Trapno:  ctx.Vector,
				Oldmask: mask,
				CR2:     ctx.CR2,
				FPState: fpstate,
			},
			Sigmask: mask,
		},
	}

	for i, r := range gregs(ctx) {
		frame.UC.Mcontext.Gregs[i] = *r
	}

	copy(frame.Info[:], info.marshal())

	if err := mm.copyOut(sp, frame.Bytes()); err != 0 {
		return err
	}

	ctx.RDI = uint64(sig)
	ctx.RSI = sp + frameInfo
	ctx.RDX = sp + frameUC
	ctx.RAX = 0
	ctx.RSP = sp
	ctx.RIP = act.Handler
	// clear DF and TF
	ctx.RFLAGS &^= 1<<10 | 1<<8

	return 0
}

// sigreturn restores the context saved by setupFrame, the handler returned
// to the restorer (popping pretcode) so the ucontext is at the stack pointer.
func (t *Task) sigreturn() Errno {
	mm := t.proc.mm
	ctx := &t.ctx

	b := make([]byte, ucontextSize)

	if err := mm.copyIn(ctx.RSP, b); err != 0 {
		return err
	}

	var uc ucontext
	binary.Decode(b, binary.LittleEndian, &uc)

	flags := ctx.RFLAGS

	for i, r := range gregs(ctx) {
		*r = uc.Mcontext.Gregs[i]
	}

	ctx.RFLAGS = (flags &^ rflagsUser) | (ctx.RFLAGS & rflagsUser) | rflagsIF

	if fpstate := uc.Mcontext.FPState; fpstate != 0 {
		if err := mm.copyIn(fpstate, ctx.FP[:monitor.FXSaveSize]); err != 0 {
			return err
		}
		monitor.SanitizeFPState(ctx.FP)
	}

	t.sigmask = uc.Sigmask &^ unblockable
	t.noReturn = true

	return 0
}

// stat represents the x86_64 struct stat.
type stat struct {
	Dev       uint64
	Ino       uint64
	Nlink     uint64
	Mode      uint32
	UID       uint32
	GID       uint32
	_         uint32
	Rdev      uint64
	Size      int64
	Blksize   int64
	Blocks    int64
	Atime     int64
	AtimeNsec int64
	Mtime     int64
	MtimeNsec int64
	Ctime     int64
	CtimeNsec int64
	_         [3]int64
}

// Bytes converts the stat structure to byte array format.
func (s *stat) Bytes() []byte {
	b, _ := binary.Append(nil, binary.LittleEndian, s)
	return b
}

func statBytes(ino *Inode) []byte {
	size := ino.size()

	s := &stat{
		Dev:       ino.dev,
		Ino:       ino.ino,
		Nlink:     uint64(ino.nlink),
		Mode:      ino.mode,
		UID:       ino.uid,
		GID:       ino.gid,
		Rdev:      ino.rdev,
		Size:      size,
		Blksize:   PageSize,
		Blocks:    (size + 511) / 512,
		Atime:     ino.atime / 1e9,
		AtimeNsec: ino.atime % 1e9,
		Mtime:     ino.mtime / 1e9,
		MtimeNsec: ino.mtime % 1e9,
		Ctime:     ino.ctime / 1e9,
		CtimeNsec: ino.ctime % 1e9,
	}

	return s.Bytes()
}

// alias registers a legacy system call as a generic one with converted
// arguments.
func alias(num uint64, target uint64, conv func(a *[6]uint64) [6]uint64) {
	reg(num, func(t *Task, a *[6]uint64) (uint64, Errno) {
		b := conv(a)
		return syscallTable[target](t, &b)
	})
}

const fdcwd = uint64(0xffffffffffffff9c) // AT_FDCWD

func registerArchSyscalls() {
	const (
		ARCH_SET_GS = 0x1001
		ARCH_SET_FS = 0x1002
		ARCH_GET_FS = 0x1003
		ARCH_GET_GS = 0x1004
	)

	reg(SYS_ARCH_PRCTL, func(t *Task, a *[6]uint64) (uint64, Errno) {
		switch a[0] {
		case ARCH_SET_FS:
			// canonical lower half only
			if a[1] >= 0x800000000000 {
				return fail(EPERM)
			}
			t.ctx.FSBase = a[1]
			return ok(0)
		case ARCH_GET_FS:
			return 0, t.proc.mm.writeU64(a[1], t.ctx.FSBase)
		case ARCH_GET_GS:
			return 0, t.proc.mm.writeU64(a[1], 0)
		case ARCH_SET_GS:
			return fail(EPERM)
		}

		return fail(EINVAL)
	})

	// legacy file system calls

	alias(SYS_OPEN, SYS_OPENAT, func(a *[6]uint64) [6]uint64 {
		return [6]uint64{fdcwd, a[0], a[1], a[2]}
	})

	alias(SYS_CREAT, SYS_OPENAT, func(a *[6]uint64) [6]uint64 {
		return [6]uint64{fdcwd, a[0], O_CREAT | O_WRONLY | O_TRUNC, a[1]}
	})

	alias(SYS_STAT, SYS_NEWFSTATAT, func(a *[6]uint64) [6]uint64 {
		return [6]uint64{fdcwd, a[0], a[1], 0}
	})

	alias(SYS_LSTAT, SYS_NEWFSTATAT, func(a *[6]uint64) [6]uint64 {
		return [6]uint64{fdcwd, a[0], a[1], AT_SYMLINK_NOFOLLOW}
	})

	alias(SYS_ACCESS, SYS_FACCESSAT, func(a *[6]uint64) [6]uint64 {
		return [6]uint64{fdcwd, a[0], a[1]}
	})

	alias(SYS_READLINK, SYS_READLINKAT, func(a *[6]uint64) [6]uint64 {
		return [6]uint64{fdcwd, a[0], a[1], a[2]}
	})

	alias(SYS_UNLINK, SYS_UNLINKAT, func(a *[6]uint64) [6]uint64 {
		return [6]uint64{fdcwd, a[0], 0}
	})

	alias(SYS_RMDIR, SYS_UNLINKAT, func(a *[6]uint64) [6]uint64 {
		return [6]uint64{fdcwd, a[0], AT_REMOVEDIR}
	})

	alias(SYS_MKDIR, SYS_MKDIRAT, func(a *[6]uint64) [6]uint64 {
		return [6]uint64{fdcwd, a[0], a[1]}
	})

	alias(SYS_MKNOD, SYS_MKNODAT, func(a *[6]uint64) [6]uint64 {
		return [6]uint64{fdcwd, a[0], a[1], a[2]}
	})

	alias(SYS_RENAME, SYS_RENAMEAT2, func(a *[6]uint64) [6]uint64 {
		return [6]uint64{fdcwd, a[0], fdcwd, a[1], 0}
	})

	alias(SYS_RENAMEAT, SYS_RENAMEAT2, func(a *[6]uint64) [6]uint64 {
		return [6]uint64{a[0], a[1], a[2], a[3], 0}
	})

	alias(SYS_LINK, SYS_LINKAT, func(a *[6]uint64) [6]uint64 {
		return [6]uint64{fdcwd, a[0], fdcwd, a[1], 0}
	})

	alias(SYS_SYMLINK, SYS_SYMLINKAT, func(a *[6]uint64) [6]uint64 {
		return [6]uint64{a[0], fdcwd, a[1]}
	})

	alias(SYS_CHMOD, SYS_FCHMODAT, func(a *[6]uint64) [6]uint64 {
		return [6]uint64{fdcwd, a[0], a[1]}
	})

	alias(SYS_CHOWN, SYS_FCHOWNAT, func(a *[6]uint64) [6]uint64 {
		return [6]uint64{fdcwd, a[0], a[1], a[2], 0}
	})

	alias(SYS_LCHOWN, SYS_FCHOWNAT, func(a *[6]uint64) [6]uint64 {
		return [6]uint64{fdcwd, a[0], a[1], a[2], AT_SYMLINK_NOFOLLOW}
	})

	alias(SYS_PIPE, SYS_PIPE2, func(a *[6]uint64) [6]uint64 {
		return [6]uint64{a[0], 0}
	})

	alias(SYS_EVENTFD, SYS_EVENTFD2, func(a *[6]uint64) [6]uint64 {
		return [6]uint64{a[0], 0}
	})

	alias(SYS_EPOLL_CREATE, SYS_EPOLL_CREATE1, func(a *[6]uint64) [6]uint64 {
		return [6]uint64{0}
	})

	alias(SYS_EPOLL_WAIT, SYS_EPOLL_PWAIT, func(a *[6]uint64) [6]uint64 {
		return [6]uint64{a[0], a[1], a[2], a[3], 0, 8}
	})

	alias(SYS_GETDENTS, SYS_GETDENTS64, func(a *[6]uint64) [6]uint64 {
		return [6]uint64{a[0], a[1], a[2]}
	})

	reg(SYS_DUP2, func(t *Task, a *[6]uint64) (uint64, Errno) {
		if a[0] == a[1] {
			if _, err := t.file(a[0]); err != 0 {
				return fail(err)
			}
			return a[1], 0
		}

		b := [6]uint64{a[0], a[1], 0}
		return syscallTable[SYS_DUP3](t, &b)
	})

	reg(SYS_POLL, func(t *Task, a *[6]uint64) (uint64, Errno) {
		timeout := int64(int32(a[2]))

		if timeout > 0 {
			timeout *= int64(time.Millisecond)
		}

		return t.ppoll(a[0], a[1], timeout)
	})

	reg(SYS_SELECT, func(t *Task, a *[6]uint64) (uint64, Errno) {
		timeout := int64(-1)

		if a[4] != 0 {
			d, err := t.readTimeval(a[4])

			if err != 0 {
				return fail(err)
			}

			timeout = d
		}

		return t.pselect(int(a[0]), a[1], a[2], a[3], timeout)
	})

	// legacy process calls
	reg(SYS_FORK, func(t *Task, a *[6]uint64) (uint64, Errno) {
		tid, err := t.clone(SIGCHLD, 0, 0, 0, 0)
		return uint64(tid), err
	})

	reg(SYS_VFORK, func(t *Task, a *[6]uint64) (uint64, Errno) {
		tid, err := t.clone(CLONE_VM|CLONE_VFORK|SIGCHLD, 0, 0, 0, 0)
		return uint64(tid), err
	})

	reg(SYS_GETPGRP, func(t *Task, a *[6]uint64) (uint64, Errno) {
		return ok(t.proc.pgid)
	})

	reg(SYS_PAUSE, func(t *Task, a *[6]uint64) (uint64, Errno) {
		t.blockSelf(func() bool { return false }, 0, true)
		return fail(ERESTARTNOHAND)
	})

	reg(SYS_ALARM, func(t *Task, a *[6]uint64) (uint64, Errno) {
		p := t.proc

		if p.itimer == nil {
			p.itimer = &intervalTimer{p: p}
		}

		rem := (p.itimer.remaining() + 1e9 - 1) / 1e9
		p.itimer.arm(int64(a[0])*1e9, 0)

		return uint64(rem), 0
	})

	reg(SYS_TIME, func(t *Task, a *[6]uint64) (uint64, Errno) {
		now := uint64(time.Now().Unix())

		if a[0] != 0 {
			if err := t.proc.mm.writeU64(a[0], now); err != 0 {
				return fail(err)
			}
		}

		return now, 0
	})
}

func cpuinfo() (s string) {
	for i := range runtime.GOMAXPROCS(0) {
		s += fmt.Sprintf("processor\t: %d\nvendor_id\t: eggshell\nmodel name\t: QEMU x86-64\nflags\t\t: %s\n\n", i, flags)
	}

	return
}
