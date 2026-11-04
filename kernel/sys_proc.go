// Copyright (c) The eggshell Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

package kernel

import (
	"encoding/binary"
	"maps"
	"runtime"
	"time"
)

var (
	hostname   = "none"
	domainname = "(none)"
)

const (
	RLIMIT_CPU    = 0
	RLIMIT_FSIZE  = 1
	RLIMIT_DATA   = 2
	RLIMIT_STACK  = 3
	RLIMIT_CORE   = 4
	RLIMIT_NOFILE = 7
	RLIMIT_AS     = 9
	RLIM_NLIMITS  = 16
	RLIM_INFINITY = ^uint64(0)
)

func defaultRlimits() map[int][2]uint64 {
	return map[int][2]uint64{
		RLIMIT_NOFILE: {1024, maxFDs},
		RLIMIT_STACK:  {stackSize, RLIM_INFINITY},
		RLIMIT_CORE:   {0, RLIM_INFINITY},
	}
}

// sysinfo represents the struct sysinfo.
type sysinfo struct {
	Uptime    int64
	Loads     [3]uint64
	Totalram  uint64
	Freeram   uint64
	Sharedram uint64
	Bufferram uint64
	Totalswap uint64
	Freeswap  uint64
	Procs     uint16
	_         uint16
	_         uint32
	Totalhigh uint64
	Freehigh  uint64
	MemUnit   uint32
	_         uint32
}

// Bytes converts the sysinfo structure to byte array format.
func (s *sysinfo) Bytes() []byte {
	b, _ := binary.Append(nil, binary.LittleEndian, s)
	return b
}

// rusage represents the struct rusage.
type rusage struct {
	Utime  [2]int64 // struct timeval
	Stime  [2]int64
	Maxrss int64
	_      [13]int64
}

// tms represents the struct tms.
type tms struct {
	Utime  int64
	Stime  int64
	Cutime int64
	Cstime int64
}

// capData represents the struct __user_cap_data_struct.
type capData struct {
	Effective   uint32
	Permitted   uint32
	Inheritable uint32
}

func timespec(ns int64) []byte {
	b := make([]byte, 16)
	binary.LittleEndian.PutUint64(b[0:], uint64(ns/1e9))
	binary.LittleEndian.PutUint64(b[8:], uint64(ns%1e9))
	return b
}

func timeval(ns int64) []byte {
	b := make([]byte, 16)
	binary.LittleEndian.PutUint64(b[0:], uint64(ns/1e9))
	binary.LittleEndian.PutUint64(b[8:], uint64((ns%1e9)/1e3))
	return b
}

func (t *Task) readTimespec(addr uint64) (int64, Errno) {
	b := make([]byte, 16)

	if err := t.proc.mm.copyIn(addr, b); err != 0 {
		return 0, err
	}

	sec := int64(binary.LittleEndian.Uint64(b[0:]))
	nsec := int64(binary.LittleEndian.Uint64(b[8:]))

	if nsec < 0 || nsec >= 1e9 || sec < 0 {
		return 0, EINVAL
	}

	if sec > 1<<32 {
		sec = 1 << 32
	}

	return sec*1e9 + nsec, 0
}

func (t *Task) readTimeval(addr uint64) (int64, Errno) {
	b := make([]byte, 16)

	if err := t.proc.mm.copyIn(addr, b); err != 0 {
		return 0, err
	}

	sec := int64(binary.LittleEndian.Uint64(b[0:]))
	usec := int64(binary.LittleEndian.Uint64(b[8:]))

	if usec < 0 || usec >= 1e6 || sec < 0 {
		return 0, EINVAL
	}

	return sec*1e9 + usec*1e3, 0
}

// clock identifiers
const (
	CLOCK_REALTIME           = 0
	CLOCK_MONOTONIC          = 1
	CLOCK_PROCESS_CPUTIME_ID = 2
	CLOCK_THREAD_CPUTIME_ID  = 3
	CLOCK_MONOTONIC_RAW      = 4
	CLOCK_REALTIME_COARSE    = 5
	CLOCK_MONOTONIC_COARSE   = 6
	CLOCK_BOOTTIME           = 7
)

func (t *Task) clock(id uint64) (int64, Errno) {
	switch int32(id) {
	case CLOCK_REALTIME, CLOCK_REALTIME_COARSE:
		return realtime(), 0
	case CLOCK_MONOTONIC, CLOCK_MONOTONIC_RAW, CLOCK_MONOTONIC_COARSE, CLOCK_BOOTTIME:
		return nanotime(), 0
	case CLOCK_PROCESS_CPUTIME_ID:
		return t.proc.utime, 0
	case CLOCK_THREAD_CPUTIME_ID:
		return t.utime, 0
	}

	return 0, EINVAL
}

// intervalTimer implements ITIMER_REAL.
type intervalTimer struct {
	p        *Proc
	timer    *time.Timer
	interval int64
	deadline int64
}

func (it *intervalTimer) stop() {
	if it.timer != nil {
		it.timer.Stop()
		it.timer = nil
	}
	it.deadline = 0
}

func (it *intervalTimer) arm(value int64, interval int64) {
	it.stop()
	it.interval = interval

	if value == 0 {
		return
	}

	it.deadline = nanotime() + value

	var timer *time.Timer

	timer = time.AfterFunc(time.Duration(value), func() {
		klock.Lock()
		defer klock.Unlock()

		if it.timer != timer {
			return
		}

		it.timer = nil
		it.deadline = 0
		it.p.signal(SIGALRM, SigInfo{Signo: SIGALRM, Code: SI_KERNEL})

		if it.interval > 0 && !it.p.zombie {
			it.arm(it.interval, it.interval)
		}
	})

	it.timer = timer
}

func (it *intervalTimer) remaining() int64 {
	if it.deadline == 0 {
		return 0
	}

	if r := it.deadline - nanotime(); r > 0 {
		return r
	}

	return 1
}

func registerProcSyscalls() {
	reg(SYS_EXIT, func(t *Task, a *[6]uint64) (uint64, Errno) {
		p := t.proc

		if len(p.threads) == 1 && !p.exiting {
			p.exitStatus = int(a[0]&0xff) << 8
		}

		t.dead = true
		t.noReturn = true

		return 0, 0
	})

	reg(SYS_EXIT_GROUP, func(t *Task, a *[6]uint64) (uint64, Errno) {
		t.proc.exitGroup(int(a[0]&0xff) << 8)
		t.noReturn = true
		return 0, 0
	})

	reg(SYS_GETPID, func(t *Task, a *[6]uint64) (uint64, Errno) {
		return ok(t.proc.pid)
	})

	reg(SYS_GETTID, func(t *Task, a *[6]uint64) (uint64, Errno) {
		return ok(t.tid)
	})

	reg(SYS_GETPPID, func(t *Task, a *[6]uint64) (uint64, Errno) {
		if t.proc.parent == nil {
			return ok(0)
		}
		return ok(t.proc.parent.pid)
	})

	for _, n := range []uint64{SYS_GETUID, SYS_GETEUID, SYS_GETGID, SYS_GETEGID} {
		reg(n, func(t *Task, a *[6]uint64) (uint64, Errno) { return ok(0) })
	}

	for _, n := range []uint64{SYS_SETUID, SYS_SETGID, SYS_SETREUID, SYS_SETREGID,
		SYS_SETRESUID, SYS_SETRESGID, SYS_SETFSUID, SYS_SETFSGID, SYS_SETGROUPS} {
		reg(n, func(t *Task, a *[6]uint64) (uint64, Errno) { return ok(0) })
	}

	reg(SYS_GETRESUID, func(t *Task, a *[6]uint64) (uint64, Errno) {
		mm := t.proc.mm
		for i := range 3 {
			if err := mm.writeU32(a[i], 0); err != 0 {
				return fail(err)
			}
		}
		return ok(0)
	})

	syscallTable[SYS_GETRESGID] = syscallTable[SYS_GETRESUID]

	reg(SYS_GETGROUPS, func(t *Task, a *[6]uint64) (uint64, Errno) {
		if a[0] == 0 {
			return ok(1)
		}
		return uint64(int64(1)), t.proc.mm.writeU32(a[1], 0)
	})

	reg(SYS_SET_TID_ADDRESS, func(t *Task, a *[6]uint64) (uint64, Errno) {
		t.clearTID = a[0]
		return ok(t.tid)
	})

	reg(SYS_CLONE, func(t *Task, a *[6]uint64) (uint64, Errno) {
		flags, sp, ptid, ctid, tls := cloneArgs(a)
		tid, err := t.clone(flags, sp, ptid, tls, ctid)
		return uint64(int64(tid)), err
	})

	reg(SYS_EXECVE, func(t *Task, a *[6]uint64) (uint64, Errno) {
		file, err := t.str(a[0])

		if err != 0 {
			return fail(err)
		}

		argv, err := t.strArray(a[1])

		if err != 0 {
			return fail(err)
		}

		envp, err := t.strArray(a[2])

		if err != 0 {
			return fail(err)
		}

		if len(argv) == 0 {
			argv = []string{file}
		}

		return 0, t.execve(file, argv, envp, 0)
	})

	reg(SYS_EXECVEAT, func(t *Task, a *[6]uint64) (uint64, Errno) {
		file, err := t.str(a[1])

		if err != 0 {
			return fail(err)
		}

		if int32(a[0]) != AT_FDCWD && len(file) > 0 && file[0] != '/' {
			if file, err = t.absPathAt(int(int32(a[0])), file); err != 0 {
				return fail(err)
			}
		}

		argv, err := t.strArray(a[2])

		if err != 0 {
			return fail(err)
		}

		envp, err := t.strArray(a[3])

		if err != 0 {
			return fail(err)
		}

		return 0, t.execve(file, argv, envp, 0)
	})

	reg(SYS_WAIT4, func(t *Task, a *[6]uint64) (uint64, Errno) {
		pid, status, err := t.wait(int(int32(a[0])), int(a[2]))

		if err != 0 {
			return fail(err)
		}

		if pid > 0 && a[1] != 0 {
			if err = t.proc.mm.writeU32(a[1], uint32(status)); err != 0 {
				return fail(err)
			}
		}

		if pid > 0 && a[3] != 0 {
			t.proc.mm.zeroOut(a[3], 144)
		}

		return ok(pid)
	})

	reg(SYS_WAITID, func(t *Task, a *[6]uint64) (uint64, Errno) {
		const (
			P_ALL  = 0
			P_PID  = 1
			P_PGID = 2
		)

		var pid int

		switch a[0] {
		case P_ALL:
			pid = -1
		case P_PID:
			pid = int(a[1])
		case P_PGID:
			pid = -int(a[1])
		default:
			return fail(EINVAL)
		}

		rpid, status, err := t.wait(pid, int(a[3])|WEXITED)

		if err != 0 {
			return fail(err)
		}

		if a[2] != 0 {
			info := SigInfo{Signo: SIGCHLD, Pid: rpid}

			switch {
			case rpid == 0:
				info.Signo = 0
			case status&0x7f == 0:
				info.Code = CLD_EXITED
				info.Status = (status >> 8) & 0xff
			case status&0xff == 0x7f:
				info.Code = CLD_STOPPED
				info.Status = (status >> 8) & 0xff
			default:
				info.Code = CLD_KILLED
				info.Status = status & 0x7f
			}

			if err = t.proc.mm.copyOut(a[2], info.marshal()); err != 0 {
				return fail(err)
			}
		}

		return ok(0)
	})

	reg(SYS_KILL, func(t *Task, a *[6]uint64) (uint64, Errno) {
		return 0, t.kill(int(int32(a[0])), int(a[1]))
	})

	tkill := func(t *Task, tid int, sig int) (uint64, Errno) {
		target, ok := tasks[tid]

		if !ok {
			return fail(ESRCH)
		}

		if sig < 0 || sig >= NSIG {
			return fail(EINVAL)
		}

		if sig != 0 {
			target.signal(sig, SigInfo{Signo: sig, Code: SI_TKILL, Pid: t.proc.pid})
		}

		return 0, 0
	}

	reg(SYS_TKILL, func(t *Task, a *[6]uint64) (uint64, Errno) {
		return tkill(t, int(int32(a[0])), int(a[1]))
	})

	reg(SYS_TGKILL, func(t *Task, a *[6]uint64) (uint64, Errno) {
		return tkill(t, int(int32(a[1])), int(a[2]))
	})

	reg(SYS_RT_SIGACTION, func(t *Task, a *[6]uint64) (uint64, Errno) {
		sig := int(a[0])
		mm := t.proc.mm

		if sig <= 0 || sig >= NSIG || a[3] != 8 {
			return fail(EINVAL)
		}

		act := &t.proc.sigh.act[sig]

		if a[2] != 0 {
			if err := mm.copyOut(a[2], act.Bytes()); err != 0 {
				return fail(err)
			}
		}

		if a[1] != 0 {
			if sig == SIGKILL || sig == SIGSTOP {
				return fail(EINVAL)
			}

			b := make([]byte, binary.Size(SigAction{}))

			if err := mm.copyIn(a[1], b); err != 0 {
				return fail(err)
			}

			*act = SigAction{}
			binary.Decode(b, binary.LittleEndian, act)
			act.Mask &^= unblockable

			// discard pending ignored signals
			if t.proc.ignored(sig) {
				t.proc.pending &^= sigbit(sig)

				for _, o := range t.proc.threads {
					o.pending &^= sigbit(sig)
				}
			}
		}

		return ok(0)
	})

	reg(SYS_RT_SIGPROCMASK, func(t *Task, a *[6]uint64) (uint64, Errno) {
		mm := t.proc.mm
		old := t.sigmask

		if a[3] != 8 {
			return fail(EINVAL)
		}

		if a[1] != 0 {
			set, err := mm.readU64(a[1])

			if err != 0 {
				return fail(err)
			}

			switch a[0] {
			case SIG_BLOCK:
				t.sigmask |= set
			case SIG_UNBLOCK:
				t.sigmask &^= set
			case SIG_SETMASK:
				t.sigmask = set
			default:
				return fail(EINVAL)
			}

			t.sigmask &^= unblockable
		}

		if a[2] != 0 {
			if err := mm.writeU64(a[2], old); err != 0 {
				return fail(err)
			}
		}

		return ok(0)
	})

	reg(SYS_RT_SIGPENDING, func(t *Task, a *[6]uint64) (uint64, Errno) {
		return uint64(int64(0)), t.proc.mm.writeU64(a[0], (t.pending|t.proc.pending)&t.sigmask)
	})

	reg(SYS_RT_SIGRETURN, func(t *Task, a *[6]uint64) (uint64, Errno) {
		if err := t.sigreturn(); err != 0 {
			t.proc.exitGroup(SIGSEGV)
		}
		return 0, 0
	})

	reg(SYS_RT_SIGSUSPEND, func(t *Task, a *[6]uint64) (uint64, Errno) {
		mask, err := t.proc.mm.readU64(a[0])

		if err != 0 {
			return fail(err)
		}

		t.savedMask = t.sigmask
		t.hasSavedMask = true
		t.sigmask = mask &^ unblockable

		t.blockSelf(func() bool { return false }, 0, true)

		return fail(EINTR)
	})

	reg(SYS_RT_SIGTIMEDWAIT, func(t *Task, a *[6]uint64) (uint64, Errno) {
		mm := t.proc.mm

		set, err := mm.readU64(a[0])

		if err != 0 {
			return fail(err)
		}

		var deadline int64

		if a[2] != 0 {
			d, err := t.readTimespec(a[2])

			if err != 0 {
				return fail(err)
			}

			deadline = nanotime() + d

			if d == 0 {
				deadline = nanotime()
			}
		}

		pending := func() uint64 { return (t.pending | t.proc.pending) & set }

		// temporarily unblock the waited signals so that they wake us
		old := t.sigmask
		t.sigmask &^= set
		err = t.blockSelf(func() bool { return pending() != 0 }, deadline, true)
		t.sigmask = old

		if p := pending(); p != 0 {
			sig := lowestSignal(p)
			var info SigInfo

			if t.pending&sigbit(sig) != 0 {
				t.pending &^= sigbit(sig)
				info = t.info[sig]
			} else {
				t.proc.pending &^= sigbit(sig)
				info = t.proc.info[sig]
			}

			if a[1] != 0 {
				info.Signo = sig
				mm.copyOut(a[1], info.marshal())
			}

			return ok(sig)
		}

		switch err {
		case ETIMEDOUT:
			return fail(EAGAIN)
		case ERESTARTSYS:
			return fail(EINTR)
		}

		return fail(err)
	})

	reg(SYS_SIGALTSTACK, func(t *Task, a *[6]uint64) (uint64, Errno) {
		mm := t.proc.mm

		if a[1] != 0 {
			old := t.altstack

			if old.Size == 0 {
				old.Flags = SS_DISABLE
			}

			if err := mm.copyOut(a[1], old.Bytes()); err != 0 {
				return fail(err)
			}
		}

		if a[0] != 0 {
			b := make([]byte, binary.Size(sigaltstack{}))

			if err := mm.copyIn(a[0], b); err != 0 {
				return fail(err)
			}

			t.altstack = sigaltstack{}
			binary.Decode(b, binary.LittleEndian, &t.altstack)
		}

		return ok(0)
	})

	reg(SYS_UNAME, func(t *Task, a *[6]uint64) (uint64, Errno) {
		b := make([]byte, 65*6)

		for i, s := range []string{
			"eggshell",        // sysname
			hostname,          // nodename
			runtime.GOOS,      // release
			runtime.Version(), // version
			utsMachine,        // machine
			domainname,
		} {
			copy(b[i*65:i*65+64], s)
		}

		return uint64(int64(0)), t.proc.mm.copyOut(a[0], b)
	})

	reg(SYS_SETHOSTNAME, func(t *Task, a *[6]uint64) (uint64, Errno) {
		b := make([]byte, a[1])

		if err := t.proc.mm.copyIn(a[0], b); err != 0 {
			return fail(err)
		}

		hostname = string(b)

		return ok(0)
	})

	reg(SYS_SETDOMAINNAME, func(t *Task, a *[6]uint64) (uint64, Errno) {
		b := make([]byte, a[1])

		if err := t.proc.mm.copyIn(a[0], b); err != 0 {
			return fail(err)
		}

		domainname = string(b)

		return ok(0)
	})

	reg(SYS_SETPGID, func(t *Task, a *[6]uint64) (uint64, Errno) {
		pid := int(int32(a[0]))
		pgid := int(int32(a[1]))

		p := t.proc

		if pid != 0 && pid != p.pid {
			c, ok := procs[pid]

			if !ok || (c.parent != p) {
				return fail(ESRCH)
			}

			p = c
		}

		if pgid == 0 {
			pgid = p.pid
		}

		if pgid < 0 {
			return fail(EINVAL)
		}

		if p.sid == p.pid {
			return fail(EPERM)
		}

		p.pgid = pgid

		return ok(0)
	})

	reg(SYS_GETPGID, func(t *Task, a *[6]uint64) (uint64, Errno) {
		pid := int(int32(a[0]))

		if pid == 0 {
			return ok(t.proc.pgid)
		}

		p, ok := procs[pid]

		if !ok {
			return fail(ESRCH)
		}

		return uint64(p.pgid), 0
	})

	reg(SYS_GETSID, func(t *Task, a *[6]uint64) (uint64, Errno) {
		pid := int(int32(a[0]))

		if pid == 0 {
			return ok(t.proc.sid)
		}

		p, ok := procs[pid]

		if !ok {
			return fail(ESRCH)
		}

		return uint64(p.sid), 0
	})

	reg(SYS_SETSID, func(t *Task, a *[6]uint64) (uint64, Errno) {
		p := t.proc

		if p.pgid == p.pid {
			return fail(EPERM)
		}

		p.sid = p.pid
		p.pgid = p.pid

		return ok(p.sid)
	})

	reg(SYS_PRLIMIT64, func(t *Task, a *[6]uint64) (uint64, Errno) {
		return t.rlimit(int(int32(a[0])), int(a[1]), a[2], a[3])
	})

	reg(SYS_GETRLIMIT, func(t *Task, a *[6]uint64) (uint64, Errno) {
		return t.rlimit(0, int(a[0]), 0, a[1])
	})

	reg(SYS_SETRLIMIT, func(t *Task, a *[6]uint64) (uint64, Errno) {
		return t.rlimit(0, int(a[0]), a[1], 0)
	})

	reg(SYS_GETRUSAGE, func(t *Task, a *[6]uint64) (uint64, Errno) {
		var ut int64

		switch int32(a[0]) {
		case 0: // RUSAGE_SELF
			ut = t.proc.utime
		case -1: // RUSAGE_CHILDREN
			ut = t.proc.cutime
		case 1: // RUSAGE_THREAD
			ut = t.utime
		}

		ru := rusage{
			Utime:  [2]int64{ut / 1e9, ut % 1e9 / 1e3},
			Maxrss: int64(t.proc.mm.rss() * 4),
		}

		b, _ := binary.Append(nil, binary.LittleEndian, &ru)

		return uint64(int64(0)), t.proc.mm.copyOut(a[1], b)
	})

	reg(SYS_TIMES, func(t *Task, a *[6]uint64) (uint64, Errno) {
		if a[0] != 0 {
			b, _ := binary.Append(nil, binary.LittleEndian, &tms{
				Utime:  t.proc.utime / 1e7,
				Cutime: t.proc.cutime / 1e7,
			})

			if err := t.proc.mm.copyOut(a[0], b); err != 0 {
				return fail(err)
			}
		}

		return uint64(nanotime() / 1e7), 0
	})

	reg(SYS_UMASK, func(t *Task, a *[6]uint64) (uint64, Errno) {
		old := t.proc.fs.umask
		t.proc.fs.umask = uint32(a[0]) & 0777
		return uint64(old), 0
	})

	reg(SYS_PRCTL, func(t *Task, a *[6]uint64) (uint64, Errno) {
		const (
			PR_SET_PDEATHSIG = 1
			PR_GET_DUMPABLE  = 3
			PR_SET_DUMPABLE  = 4
			PR_SET_NAME      = 15
			PR_GET_NAME      = 16
		)

		switch a[0] {
		case PR_SET_NAME:
			s, err := t.proc.mm.readString(a[1], 16)

			if err != 0 && err != ENAMETOOLONG {
				return fail(err)
			}

			if len(s) > 15 {
				s = s[:15]
			}

			t.proc.comm = s

			return ok(0)
		case PR_GET_NAME:
			b := make([]byte, 16)
			copy(b, t.proc.comm)
			return uint64(int64(0)), t.proc.mm.copyOut(a[1], b)
		case PR_SET_PDEATHSIG:
			return ok(0)
		case PR_GET_DUMPABLE:
			// processes never dump core
			return ok(0)
		case PR_SET_DUMPABLE:
			if a[1] > 1 {
				return fail(EINVAL)
			}

			return ok(0)
		}

		return fail(EINVAL)
	})

	reg(SYS_SCHED_YIELD, func(t *Task, a *[6]uint64) (uint64, Errno) {
		yield()
		return ok(0)
	})

	reg(SYS_SCHED_GETAFFINITY, func(t *Task, a *[6]uint64) (uint64, Errno) {
		if a[1] < 8 {
			return fail(EINVAL)
		}
		return uint64(int64(8)), t.proc.mm.writeU64(a[2], 1)
	})

	reg(SYS_SCHED_GETPARAM, func(t *Task, a *[6]uint64) (uint64, Errno) {
		return uint64(int64(0)), t.proc.mm.writeU32(a[1], 0)
	})

	reg(SYS_GETCPU, func(t *Task, a *[6]uint64) (uint64, Errno) {
		if a[0] != 0 {
			t.proc.mm.writeU32(a[0], 0)
		}
		if a[1] != 0 {
			t.proc.mm.writeU32(a[1], 0)
		}
		return ok(0)
	})

	reg(SYS_FUTEX, func(t *Task, a *[6]uint64) (uint64, Errno) {
		op := a[1] &^ (FUTEX_PRIVATE_FLAG | FUTEX_CLOCK_REALTIME)
		val := uint32(a[2])

		switch op {
		case FUTEX_WAIT, FUTEX_WAIT_BITSET:
			var deadline int64
			bitset := uint32(FUTEX_BITSET_MATCH_ANY)

			if op == FUTEX_WAIT_BITSET {
				bitset = uint32(a[5])
			}

			if a[3] != 0 {
				d, err := t.readTimespec(a[3])

				if err != 0 {
					return fail(err)
				}

				if op == FUTEX_WAIT {
					deadline = nanotime() + d
				} else if a[1]&FUTEX_CLOCK_REALTIME != 0 {
					deadline = nanotime() + (d - time.Now().UnixNano())
				} else {
					deadline = d
				}

				if deadline <= 0 {
					deadline = 1
				}
			}

			return 0, t.futexWait(a[0], val, deadline, bitset)
		case FUTEX_WAKE, FUTEX_WAKE_BITSET:
			bitset := uint32(FUTEX_BITSET_MATCH_ANY)

			if op == FUTEX_WAKE_BITSET {
				bitset = uint32(a[5])
			}

			n, err := futexWake(t.proc.mm, a[0], int(val), bitset)

			return uint64(int64(n)), err
		case FUTEX_REQUEUE, FUTEX_CMP_REQUEUE:
			n, err := t.futexRequeue(a[0], int(val), a[4], int(a[3]), op == FUTEX_CMP_REQUEUE, uint32(a[5]))
			return uint64(int64(n)), err
		case FUTEX_WAKE_OP:
			n, err := t.futexWakeOp(a[0], int(val), a[4], int(a[3]), uint32(a[5]))
			return uint64(int64(n)), err
		}

		return fail(ENOSYS)
	})

	reg(SYS_NANOSLEEP, func(t *Task, a *[6]uint64) (uint64, Errno) {
		d, err := t.readTimespec(a[0])

		if err != 0 {
			return fail(err)
		}

		deadline := t.restartDeadline(nanotime() + d)

		return t.sleepUntil(deadline, a[1])
	})

	reg(SYS_CLOCK_NANOSLEEP, func(t *Task, a *[6]uint64) (uint64, Errno) {
		d, err := t.readTimespec(a[2])

		if err != 0 {
			return fail(err)
		}

		var deadline int64

		if a[1]&1 != 0 {
			// TIMER_ABSTIME
			now, err := t.clock(a[0])

			if err != 0 {
				return fail(err)
			}

			deadline = nanotime() + (d - now)
		} else {
			deadline = nanotime() + d
		}

		rem := a[3]

		if a[1]&1 != 0 {
			rem = 0
		} else {
			deadline = t.restartDeadline(deadline)
		}

		return t.sleepUntil(deadline, rem)
	})

	reg(SYS_CLOCK_GETTIME, func(t *Task, a *[6]uint64) (uint64, Errno) {
		ns, err := t.clock(a[0])

		if err != 0 {
			return fail(err)
		}

		return uint64(int64(0)), t.proc.mm.copyOut(a[1], timespec(ns))
	})

	reg(SYS_CLOCK_GETRES, func(t *Task, a *[6]uint64) (uint64, Errno) {
		if _, err := t.clock(a[0]); err != 0 {
			return fail(err)
		}

		if a[1] == 0 {
			return ok(0)
		}

		return uint64(int64(0)), t.proc.mm.copyOut(a[1], timespec(100))
	})

	reg(SYS_GETTIMEOFDAY, func(t *Task, a *[6]uint64) (uint64, Errno) {
		if a[0] != 0 {
			if err := t.proc.mm.copyOut(a[0], timeval(time.Now().UnixNano())); err != 0 {
				return fail(err)
			}
		}

		if a[1] != 0 {
			t.proc.mm.zeroOut(a[1], 8)
		}

		return ok(0)
	})

	reg(SYS_SETITIMER, func(t *Task, a *[6]uint64) (uint64, Errno) {
		p := t.proc

		if a[0] != 0 {
			// only ITIMER_REAL is supported
			return fail(EINVAL)
		}

		if p.itimer == nil {
			p.itimer = &intervalTimer{p: p}
		}

		it := p.itimer

		if a[2] != 0 {
			b := make([]byte, 32)
			copy(b[0:], timeval(it.interval))
			copy(b[16:], timeval(it.remaining()))

			if err := p.mm.copyOut(a[2], b); err != 0 {
				return fail(err)
			}
		}

		if a[1] != 0 {
			interval, err := t.readTimeval(a[1])

			if err != 0 {
				return fail(err)
			}

			value, err := t.readTimeval(a[1] + 16)

			if err != 0 {
				return fail(err)
			}

			it.arm(value, interval)
		}

		return ok(0)
	})

	reg(SYS_GETITIMER, func(t *Task, a *[6]uint64) (uint64, Errno) {
		p := t.proc
		b := make([]byte, 32)

		if a[0] == 0 && p.itimer != nil {
			copy(b[0:], timeval(p.itimer.interval))
			copy(b[16:], timeval(p.itimer.remaining()))
		}

		return uint64(int64(0)), p.mm.copyOut(a[1], b)
	})

	reg(SYS_SYSINFO, func(t *Task, a *[6]uint64) (uint64, Errno) {
		info := &sysinfo{
			Uptime:   nanotime() / 1e9,
			Totalram: memTotal(),
			Freeram:  memFree(),
			Procs:    uint16(len(procs)),
			MemUnit:  1,
		}

		return uint64(int64(0)), t.proc.mm.copyOut(a[0], info.Bytes())
	})

	reg(SYS_GETRANDOM, func(t *Task, a *[6]uint64) (uint64, Errno) {
		n := min(a[1], 1<<20)

		b := make([]byte, n)
		getRandom(b)

		return uint64(int64(int(n))), t.proc.mm.copyOut(a[0], b)
	})

	reg(SYS_CAPGET, func(t *Task, a *[6]uint64) (uint64, Errno) {
		if a[1] != 0 {
			all := capData{Effective: 0xffffffff, Permitted: 0xffffffff}
			b, _ := binary.Append(nil, binary.LittleEndian, [2]capData{all, all})

			return uint64(int64(0)), t.proc.mm.copyOut(a[1], b)
		}
		return ok(0)
	})

	reg(SYS_REBOOT, func(t *Task, a *[6]uint64) (uint64, Errno) {
		const (
			LINUX_REBOOT_CMD_POWER_OFF = 0x4321fedc
			LINUX_REBOOT_CMD_RESTART   = 0x01234567
			LINUX_REBOOT_CMD_HALT      = 0xcdef0123
		)

		switch uint32(a[2]) {
		case LINUX_REBOOT_CMD_POWER_OFF, LINUX_REBOOT_CMD_HALT, LINUX_REBOOT_CMD_RESTART:
			printk("reboot: power down\n")
			cfg.Exit(0)
		}

		return ok(0)
	})
}

func (t *Task) rlimit(pid int, res int, newAddr uint64, oldAddr uint64) (uint64, Errno) {
	p := t.proc

	if pid != 0 {
		var found bool

		if p, found = procs[pid]; !found {
			return fail(ESRCH)
		}
	}

	if res < 0 || res >= RLIM_NLIMITS {
		return fail(EINVAL)
	}

	lim, found := p.rlimits[res]

	if !found {
		lim = [2]uint64{RLIM_INFINITY, RLIM_INFINITY}
	}

	// struct rlimit: soft and hard limits
	if oldAddr != 0 {
		b, _ := binary.Append(nil, binary.LittleEndian, lim)

		if err := t.proc.mm.copyOut(oldAddr, b); err != 0 {
			return fail(err)
		}
	}

	if newAddr != 0 {
		b := make([]byte, binary.Size(lim))

		if err := t.proc.mm.copyIn(newAddr, b); err != 0 {
			return fail(err)
		}

		binary.Decode(b, binary.LittleEndian, &lim)

		if lim[0] > lim[1] {
			return fail(EINVAL)
		}

		if res == RLIMIT_NOFILE && lim[1] > maxFDs {
			return fail(EPERM)
		}

		// copy on write the limits table
		n := map[int][2]uint64{}

		maps.Copy(n, p.rlimits)

		n[res] = lim
		p.rlimits = n
	}

	return ok(0)
}

// restartDeadline returns the deadline of an interrupted sleep being
// transparently restarted, or the argument one.
func (t *Task) restartDeadline(deadline int64) int64 {
	if t.sleepRestart != 0 {
		deadline = t.sleepRestart
	}

	t.sleepRestart = 0

	return deadline
}

// sleepUntil implements nanosleep semantics: interruption by a signal
// handler returns EINTR (and the remaining time), otherwise the sleep is
// resumed after signal processing (e.g. stop/continue).
func (t *Task) sleepUntil(deadline int64, remAddr uint64) (uint64, Errno) {
	err := t.blockSelf(func() bool { return nanotime() >= deadline }, deadline, true)

	if err == ETIMEDOUT || err == 0 {
		return ok(0)
	}

	if remAddr != 0 {
		rem := max(deadline-nanotime(), 0)

		t.proc.mm.copyOut(remAddr, timespec(rem))
	}

	if err == ERESTARTSYS {
		t.sleepRestart = deadline
		return fail(ERESTARTNOHAND)
	}

	return fail(err)
}
