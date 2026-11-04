// Copyright (c) The eggshell Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

package kernel

import (
	"crypto/rand"
	"path"
	"runtime/debug"
	"strings"
	"time"

	"github.com/usbarmory/eggshell/monitor"
)

// clone flags
const (
	CLONE_VM             = 0x00000100
	CLONE_FS             = 0x00000200
	CLONE_FILES          = 0x00000400
	CLONE_SIGHAND        = 0x00000800
	CLONE_PIDFD          = 0x00001000
	CLONE_VFORK          = 0x00004000
	CLONE_PARENT         = 0x00008000
	CLONE_THREAD         = 0x00010000
	CLONE_SYSVSEM        = 0x00040000
	CLONE_SETTLS         = 0x00080000
	CLONE_PARENT_SETTID  = 0x00100000
	CLONE_CHILD_CLEARTID = 0x00200000
	CLONE_DETACHED       = 0x00400000
	CLONE_CHILD_SETTID   = 0x01000000
)

// FSContext holds file system information shared by CLONE_FS.
type FSContext struct {
	cwd   *Inode
	umask uint32
}

// Proc represents a process (thread group).
type Proc struct {
	pid  int
	pgid int
	sid  int

	parent   *Proc
	children []*Proc
	threads  []*Task

	mm    *MM
	files *FDTable
	fs    *FSContext
	sigh  *SigHand

	// process directed pending signals
	pending uint64
	info    [NSIG]SigInfo

	exitSignal int

	exiting    bool
	zombie     bool
	exitStatus int
	done       chan struct{}

	stopped      bool
	stopSig      int
	stopReported bool
	contReported bool

	vforkDone bool

	comm string
	exe  string
	argv []string
	envp []string

	itimer *intervalTimer

	start int64

	utime  int64
	cutime int64

	rlimits map[int][2]uint64
}

// Task represents a thread of execution.
type Task struct {
	tid  int
	proc *Proc

	ctx monitor.ExecCtx

	sigmask uint64
	pending uint64
	info    [NSIG]SigInfo

	// saved mask restored after signal delivery (sigsuspend, ppoll)
	savedMask    uint64
	hasSavedMask bool

	altstack sigaltstack

	clearTID uint64

	dead bool

	// system call restart state
	restart       bool
	restartNoHand bool

	// system call restart state
	sleepRestart int64
	origArg      uint64

	// noReturn indicates that the current system call must not update
	// the return value register (execve, rt_sigreturn).
	noReturn bool

	utime     int64
	lastYield int64
}

var (
	procs   = map[int]*Proc{}
	tasks   = map[int]*Task{}
	nextPID = 1
)

func allocPID() int {
	for {
		pid := nextPID
		nextPID++

		if nextPID >= 32768 {
			nextPID = 2
		}

		if _, ok := tasks[pid]; ok {
			continue
		}

		if _, ok := procs[pid]; ok {
			continue
		}

		return pid
	}
}

func getRandom(b []byte) {
	rand.Read(b)
}

func invalidateTLB() {
	monitor.InvalidateTLB()
}

// spawnInit creates the first process.
func spawnInit(argv []string, envp []string) (p *Proc, err error) {
	if len(argv) == 0 {
		argv = []string{"/bin/sh"}
	}

	// stdin, stdout, stderr on the console
	var stdio [3]*File

	for i := range stdio {
		stdio[i], _ = openConsole(O_RDWR)
	}

	return spawn(argv, envp, rootInode, stdio, console)
}

// spawn creates a process, leader of a new session, with the argument
// standard streams (released by spawn) and controlling terminal (if any).
func spawn(argv []string, envp []string, cwd *Inode, stdio [3]*File, tty *TTY) (p *Proc, err error) {
	p = &Proc{
		pid:     allocPID(),
		files:   newFDTable(),
		fs:      &FSContext{cwd: cwd, umask: 022},
		sigh:    &SigHand{},
		done:    make(chan struct{}),
		start:   nanotime(),
		rlimits: defaultRlimits(),
	}

	p.pgid = p.pid
	p.sid = p.pid
	procs[p.pid] = p

	t := &Task{tid: p.pid, proc: p}
	tasks[t.tid] = t
	p.threads = []*Task{t}

	if tty != nil {
		tty.sid = p.sid
		tty.pgrp = p.pgid
	}

	for _, f := range stdio {
		p.files.install(f, 0, false)
		f.decref()
	}

	if e := t.execve(argv[0], argv, envp, 0); e != 0 {
		if tty != nil {
			tty.hangup()
		}

		p.files.release()
		delete(tasks, t.tid)
		delete(procs, p.pid)

		return nil, e
	}

	go t.run()

	return
}

// run executes the task until it exits.
func (t *Task) run() {
	klock.Lock()
	defer klock.Unlock()

	for {
		if t.dead {
			break
		}

		t.handleStop()
		t.deliverSignals()

		if t.dead {
			break
		}

		start := nanotime()
		tr := t.enterUser(t.proc.mm)
		elapsed := nanotime() - start
		t.utime += elapsed
		t.proc.utime += elapsed

		switch tr.kind {
		case trapSyscall:
			t.safeSyscall()
		case trapPreempt:
			t.lastYield = nanotime()
			yield()
		default:
			t.exception(tr)
		}

		if now := nanotime(); now-t.lastYield > int64(quantum) {
			t.lastYield = now
			yield()
		}
	}

	t.exit()
}

// safeSyscall services a system call, a kernel panic terminates the calling
// process.
func (t *Task) safeSyscall() {
	defer func() {
		if r := recover(); r != nil {
			printk("kernel: panic in %s (pid %d): %v\n%s",
				syscallName(t.sysno()),
				t.proc.pid,
				r,
				debug.Stack(),
			)
			t.proc.exitGroup(SIGSYS | 0x80)
		}
	}()

	t.syscall()
}

// trap kinds
const (
	trapSyscall = iota
	trapPreempt
	trapFault  // memory access fault, possibly resolved by demand paging
	trapSignal // synchronous fault signal
)

// trap describes the reason for returning from user mode.
type trap struct {
	kind   int
	sig    int
	code   int
	addr   uint64
	access int
}

// exception handles a synchronous exception raised by user code.
func (t *Task) exception(tr trap) {
	if tr.kind == trapFault {
		switch t.proc.mm.fault(tr.addr, tr.access) {
		case 0:
			return
		case ENOMEM:
			// no page to resolve the fault, as the Linux OOM killer
			printk("kernel: out of memory, killing pid %d (%s) on fault at %#x\n", t.proc.pid, t.proc.comm, tr.addr)
			t.forceSignal(SIGKILL, SI_KERNEL, tr.addr)
			return
		}

		code := SEGV_MAPERR

		if t.proc.mm.findVMA(tr.addr) != nil {
			code = SEGV_ACCERR
		}

		tr.sig, tr.code = SIGSEGV, code
	}

	if cfg.Strace {
		printk("[%d] fault signal %d code %d addr %#x pc %#x\n", t.tid, tr.sig, tr.code, tr.addr, t.pc())
	}

	t.forceSignal(tr.sig, tr.code, tr.addr)
}

// block waits until ready() returns true, a signal is pending
// (ERESTARTSYS), the deadline (absolute nanotime, if non zero) expires
// (ETIMEDOUT) or the task is killed.
func (t *Task) block(ready func() bool, deadline int64, interruptible bool) Errno {
	return t.blockOn(nil, ready, deadline, interruptible)
}

// blockOn is like block but waits on the argument wait queue, woken only by
// events on it (and by wakeAll), rather than on all events when nil.
func (t *Task) blockOn(q *waitQueue, ready func() bool, deadline int64, interruptible bool) Errno {
	var timer *time.Timer

	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()

	var w *sleeper

	// before checking the condition, see waitq.go
	if q != nil {
		w = t.enqueue(q)
		defer t.dequeue(q, w)
	}

	for {
		if ready() {
			return 0
		}

		if t.dead || t.proc.exiting {
			return EINTR
		}

		if interruptible && t.signalPending() {
			return ERESTARTSYS
		}

		if deadline > 0 {
			now := nanotime()

			if now >= deadline {
				return ETIMEDOUT
			}

			if timer == nil {
				timer = time.AfterFunc(time.Duration(deadline-now), func() {
					if w != nil {
						w.wake()
						return
					}

					klock.Lock()
					wakeAll()
					klock.Unlock()
				})
			}
		}

		if w != nil {
			w.sleep()
		} else {
			kcond.Wait()
		}
	}
}

func (t *Task) clone(flags uint64, newsp uint64, ptid uint64, tls uint64, ctid uint64) (int, Errno) {
	parent := t.proc

	if flags&CLONE_THREAD != 0 && flags&(CLONE_VM|CLONE_SIGHAND) != (CLONE_VM|CLONE_SIGHAND) {
		return 0, EINVAL
	}

	child := &Task{
		ctx:     t.ctx,
		sigmask: t.sigmask,
	}

	child.cloneRegs(newsp, tls, flags&CLONE_SETTLS != 0)

	child.tid = allocPID()

	var p *Proc

	if flags&CLONE_THREAD != 0 {
		p = parent
		p.threads = append(p.threads, child)
	} else {
		p = &Proc{
			pid:        child.tid,
			pgid:       parent.pgid,
			sid:        parent.sid,
			parent:     parent,
			exitSignal: int(flags & 0xff),
			done:       make(chan struct{}),
			comm:       parent.comm,
			exe:        parent.exe,
			argv:       parent.argv,
			envp:       parent.envp,
			start:      nanotime(),
			rlimits:    parent.rlimits,
			threads:    []*Task{child},
		}

		if flags&CLONE_PARENT != 0 && parent.parent != nil {
			p.parent = parent.parent
		}

		if flags&CLONE_VM != 0 {
			p.mm = parent.mm
			p.mm.users++
		} else {
			mm, err := parent.mm.fork()

			if err != 0 {
				return 0, err
			}

			p.mm = mm
		}

		if flags&CLONE_FILES != 0 {
			p.files = parent.files
			p.files.refs++
		} else {
			p.files = parent.files.dup()
		}

		if flags&CLONE_FS != 0 {
			p.fs = parent.fs
		} else {
			fs := *parent.fs
			p.fs = &fs
		}

		if flags&CLONE_SIGHAND != 0 {
			p.sigh = parent.sigh
		} else {
			sigh := *parent.sigh
			p.sigh = &sigh
		}

		p.parent.children = append(p.parent.children, p)
		procs[p.pid] = p
	}

	child.proc = p
	tasks[child.tid] = child

	if flags&CLONE_PARENT_SETTID != 0 {
		parent.mm.writeU32(ptid, uint32(child.tid))
	}

	if flags&CLONE_CHILD_SETTID != 0 {
		p.mm.writeU32(ctid, uint32(child.tid))
	}

	if flags&CLONE_CHILD_CLEARTID != 0 {
		child.clearTID = ctid
	}

	go child.run()

	if flags&CLONE_VFORK != 0 {
		t.block(func() bool { return p.vforkDone }, 0, false)
	}

	return child.tid, 0
}

// execve replaces the process image.
func (t *Task) execve(file string, argv []string, envp []string, depth int) Errno {
	p := t.proc

	ino, err := t.lookup(AT_FDCWD, file, true)

	if err != 0 {
		return err
	}

	if ino.isDir() {
		return EACCES
	}

	if !ino.isReg() || ino.mode&0111 == 0 {
		return EACCES
	}

	data := ino.data.bytes(0, PageSize)

	// interpreter scripts
	if len(data) > 2 && data[0] == '#' && data[1] == '!' {
		if depth > 4 {
			return ELOOP
		}

		line := string(data[2:])

		if i := strings.IndexByte(line, '\n'); i >= 0 {
			line = line[:i]
		}

		line = strings.TrimSpace(line)

		if line == "" {
			return ENOEXEC
		}

		var nargv []string

		if i := strings.IndexAny(line, " \t"); i >= 0 {
			nargv = []string{line[:i], strings.TrimSpace(line[i:])}
		} else {
			nargv = []string{line}
		}

		nargv = append(nargv, file)

		if len(argv) > 1 {
			nargv = append(nargv, argv[1:]...)
		}

		return t.execve(nargv[0], nargv, envp, depth+1)
	}

	mm, err := newMM()

	if err != 0 {
		return err
	}

	img, err := loadELF(mm, ino, pieBase)

	if err != 0 {
		mm.release()
		return err
	}

	mm.brkStart = img.end
	mm.brk = img.end

	entry := img.entry
	var base uint64

	// dynamically linked executables: map the program interpreter
	if img.interp != "" {
		ld, err := t.lookup(AT_FDCWD, img.interp, true)

		if err == 0 && !ld.isReg() {
			err = EACCES
		}

		if err != 0 {
			mm.release()
			return err
		}

		interp, err := loadELF(mm, ld, interpBase)

		if err != 0 || interp.interp != "" {
			mm.release()
			return ENOEXEC
		}

		entry = interp.entry
		base = interp.bias
	}

	sp, err := setupStack(mm, img, base, file, argv, envp)

	if err != 0 {
		mm.release()
		return err
	}

	// point of no return: terminate other threads
	for _, o := range p.threads {
		if o != t {
			o.dead = true
			o.clearTID = 0
		}
	}

	p.threads = []*Task{t}

	if p.mm != nil {
		p.mm.release()
	}

	p.mm = mm
	invalidateTLB()

	if !p.vforkDone {
		p.vforkDone = true
	}

	p.files.closeOnExec()

	// reset caught signals (handlers do not exist in the new image)
	sigh := *p.sigh
	for i := range sigh.act {
		if sigh.act[i].Handler != SIG_IGN {
			sigh.act[i] = SigAction{}
		}
	}
	p.sigh = &sigh

	t.altstack = sigaltstack{Flags: SS_DISABLE}

	p.comm = path.Base(file)

	if len(p.comm) > 15 {
		p.comm = p.comm[:15]
	}

	if abs, e := t.absPath(AT_FDCWD, file); e == 0 {
		p.exe = abs
	} else {
		p.exe = file
	}

	p.argv = argv
	p.envp = envp

	t.resetRegs(entry, sp)
	t.noReturn = true

	wakeAll()

	return 0
}

// exit terminates the calling thread (the task goroutine must return after
// invoking it).
func (t *Task) exit() {
	p := t.proc

	if t.clearTID != 0 && p.mm != nil && !p.exiting {
		p.mm.writeU32(t.clearTID, 0)
		futexWake(p.mm, t.clearTID, 1, ^uint32(0))
	}

	delete(tasks, t.tid)

	for i, o := range p.threads {
		if o == t {
			p.threads = append(p.threads[:i], p.threads[i+1:]...)
			break
		}
	}

	if len(p.threads) == 0 {
		p.exited()
	}

	wakeAll()
}

// exitGroup terminates all threads in the process with the argument wait
// status.
func (p *Proc) exitGroup(status int) {
	if p.exiting {
		return
	}

	p.exiting = true
	p.exitStatus = status

	for _, t := range p.threads {
		t.dead = true
	}

	wakeAll()
}

// exited releases process resources and turns it into a zombie.
func (p *Proc) exited() {
	if !p.exiting {
		p.exiting = true
	}

	p.vforkDone = true

	if p.itimer != nil {
		p.itimer.stop()
	}

	p.files.release()
	p.mm.release()
	p.mm = nil

	if tty := ctty(p); tty != nil && p.sid == p.pid {
		tty.hangup()
	}

	initProc := procs[1]

	// reparent children
	for _, c := range p.children {
		c.parent = initProc

		if initProc != nil && initProc != p {
			initProc.children = append(initProc.children, c)

			if c.zombie {
				initProc.signal(SIGCHLD, SigInfo{Signo: SIGCHLD, Code: CLD_EXITED, Pid: c.pid})
			}
		}
	}

	p.children = nil
	p.zombie = true

	close(p.done)

	if p.parent == nil {
		// init process
		delete(procs, p.pid)
		return
	}

	parent := p.parent
	act := parent.sigh.act[SIGCHLD]

	if act.Handler == SIG_IGN || act.Flags&SA_NOCLDWAIT != 0 {
		parent.reap(p)
	} else if p.exitSignal != 0 {
		code := CLD_EXITED

		if p.exitStatus&0x7f != 0 {
			code = CLD_KILLED
		}

		parent.signal(p.exitSignal, SigInfo{Signo: p.exitSignal, Code: code, Pid: p.pid, Status: p.exitStatus})
	}

	wakeAll()
}

// reap removes a zombie child.
func (p *Proc) reap(c *Proc) {
	for i, o := range p.children {
		if o == c {
			p.children = append(p.children[:i], p.children[i+1:]...)
			break
		}
	}

	p.cutime += c.utime + c.cutime

	delete(procs, c.pid)
}

const (
	WNOHANG    = 1
	WUNTRACED  = 2
	WEXITED    = 4
	WCONTINUED = 8
	WNOWAIT    = 0x01000000
)

// wait implements wait4.
func (t *Task) wait(pid int, options int) (rpid int, status int, err Errno) {
	p := t.proc

	match := func(c *Proc) bool {
		switch {
		case pid > 0:
			return c.pid == pid
		case pid == 0:
			return c.pgid == p.pgid
		case pid == -1:
			return true
		default:
			return c.pgid == -pid
		}
	}

	found := false

	ready := func() bool {
		found = false

		for _, c := range p.children {
			if !match(c) {
				continue
			}

			found = true

			switch {
			case c.zombie:
				rpid = c.pid
				status = c.exitStatus

				if options&WNOWAIT == 0 {
					p.reap(c)
				}

				return true
			case c.stopped && !c.stopReported && options&WUNTRACED != 0:
				c.stopReported = true
				rpid = c.pid
				status = c.stopSig<<8 | 0x7f
				return true
			case !c.stopped && !c.contReported && options&WCONTINUED != 0:
				c.contReported = true
				rpid = c.pid
				status = 0xffff
				return true
			}
		}

		return !found || options&WNOHANG != 0
	}

	if err = t.block(ready, 0, true); err != 0 {
		return
	}

	if !found {
		return 0, 0, ECHILD
	}

	return
}

// handleStop blocks the task while its process is stopped.
func (t *Task) handleStop() {
	p := t.proc

	if !p.stopped {
		return
	}

	t.block(func() bool {
		return !p.stopped || t.pending&sigbit(SIGKILL) != 0 || p.pending&sigbit(SIGKILL) != 0
	}, 0, false)
}
