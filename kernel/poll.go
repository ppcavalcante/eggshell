// Copyright (c) The eggshell Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

package kernel

import (
	"encoding/binary"
	"sync/atomic"
)

// ERESTARTNOHAND restarts a system call only when no handler is invoked.
const ERESTARTNOHAND Errno = 514

const (
	EPOLLIN      = 0x001
	EPOLLOUT     = 0x004
	EPOLLERR     = 0x008
	EPOLLHUP     = 0x010
	EPOLLRDHUP   = 0x2000
	EPOLLET      = 1 << 31
	EPOLLONESHOT = 1 << 30

	EPOLL_CTL_ADD = 1
	EPOLL_CTL_DEL = 2
	EPOLL_CTL_MOD = 3
)

// tasks blocked in poll, select and epoll_wait
var pollers waitQueue

// pollSeq counts the events which might change the readiness of a file,
// edge-triggered epoll items are reported once for each one.
var pollSeq atomic.Uint64

// pollWait blocks until check returns true or the timeout (ns, negative for
// infinite) expires.
func (t *Task) pollWait(check func() bool, timeout int64) Errno {
	if check() || timeout == 0 {
		return 0
	}

	var deadline int64

	if timeout > 0 {
		deadline = nanotime() + timeout
	}

	err := t.blockOn(&pollers, check, deadline, true)

	switch err {
	case ETIMEDOUT:
		return 0
	case ERESTARTSYS:
		return ERESTARTNOHAND
	}

	return err
}

// setTempMask installs a temporary signal mask restored on return to user
// mode (ppoll, pselect6, epoll_pwait).
func (t *Task) setTempMask(addr uint64, size uint64) Errno {
	if addr == 0 {
		return 0
	}

	if size != 8 {
		return EINVAL
	}

	mask, err := t.proc.mm.readU64(addr)

	if err != 0 {
		return err
	}

	t.savedMask = t.sigmask
	t.hasSavedMask = true
	t.sigmask = mask &^ unblockable

	return 0
}

// pollfd represents the struct pollfd.
type pollfd struct {
	FD      int32
	Events  int16
	Revents int16
}

func (t *Task) ppoll(fdsAddr uint64, nfds uint64, timeout int64) (uint64, Errno) {
	if nfds > maxFDs {
		return fail(EINVAL)
	}

	mm := t.proc.mm
	fds := make([]pollfd, nfds)
	b := make([]byte, binary.Size(fds))

	if err := mm.copyIn(fdsAddr, b); err != 0 {
		return fail(err)
	}

	binary.Decode(b, binary.LittleEndian, fds)

	n := 0

	check := func() bool {
		n = 0

		for i := range fds {
			fd := fds[i].FD
			events := uint32(uint16(fds[i].Events))
			var rev uint32

			if fd >= 0 {
				if f := t.proc.files.get(int(fd)); f == nil {
					rev = POLLNVAL
				} else {
					rev = f.poll()

					if rev&POLLIN != 0 {
						rev |= POLLRDNORM
					}

					if rev&POLLOUT != 0 {
						rev |= POLLWRNORM
					}

					rev &= events | POLLERR | POLLHUP | POLLNVAL
				}
			}

			fds[i].Revents = int16(rev)

			if rev != 0 {
				n++
			}
		}

		return n > 0
	}

	if err := t.pollWait(check, timeout); err != 0 {
		return fail(err)
	}

	binary.Encode(b, binary.LittleEndian, fds)

	return uint64(int64(n)), mm.copyOut(fdsAddr, b)
}

func (t *Task) pselect(nfds int, raddr, waddr, eaddr uint64, timeout int64) (uint64, Errno) {
	if nfds < 0 || nfds > maxFDs {
		return fail(EINVAL)
	}

	mm := t.proc.mm
	words := (nfds + 63) / 64
	b := make([]byte, words*8)

	// read, write and exception sets and their poll events
	addrs := [3]uint64{raddr, waddr, eaddr}
	events := [3]uint32{POLLIN | POLLHUP | POLLERR, POLLOUT | POLLERR, POLLPRI}

	var in, out [3][]uint64

	for i, addr := range addrs {
		in[i] = make([]uint64, words)
		out[i] = make([]uint64, words)

		if addr == 0 {
			continue
		}

		if err := mm.copyIn(addr, b); err != 0 {
			return fail(err)
		}

		binary.Decode(b, binary.LittleEndian, in[i])
	}

	n := 0
	var badf bool

	check := func() bool {
		n = 0

		for fd := range nfds {
			w, bit := fd/64, uint64(1)<<(fd%64)

			if (in[0][w]|in[1][w]|in[2][w])&bit == 0 {
				continue
			}

			f := t.proc.files.get(fd)

			if f == nil {
				badf = true
				return true
			}

			ev := f.poll()

			for i := range in {
				out[i][w] &^= bit

				if in[i][w]&bit != 0 && ev&events[i] != 0 {
					out[i][w] |= bit
					n++
				}
			}
		}

		return n > 0
	}

	if err := t.pollWait(check, timeout); err != 0 {
		return fail(err)
	}

	if badf {
		return fail(EBADF)
	}

	for i, addr := range addrs {
		if addr == 0 {
			continue
		}

		binary.Encode(b, binary.LittleEndian, out[i])

		if err := mm.copyOut(addr, b); err != 0 {
			return fail(err)
		}
	}

	return ok(n)
}

// epoll

type epitem struct {
	fd       int
	file     *File
	events   uint32
	data     uint64
	disabled bool
	// pollSeq+1 when last reported (EPOLLET)
	seq uint64
}

type epollFile struct {
	items []*epitem
}

func (e *epollFile) Mode() uint32 {
	return S_IFREG | 0600
}

func (e *epollFile) Read(t *Task, f *File, buf []byte) (int, Errno) {
	return 0, EINVAL
}

func (e *epollFile) Write(t *Task, f *File, buf []byte) (int, Errno) {
	return 0, EINVAL
}

func (e *epollFile) prune() {
	items := e.items[:0]

	for _, it := range e.items {
		if it.file.refs > 0 {
			items = append(items, it)
		}
	}

	e.items = items
}

// ready returns the events (struct epoll_event) of at most max ready items,
// which are consumed (EPOLLONESHOT, EPOLLET) when reported by epoll_wait.
func (e *epollFile) ready(max int, report bool) (evs []byte) {
	e.prune()
	seq := pollSeq.Load() + 1

	for _, it := range e.items {
		if it.disabled || it.events&EPOLLET != 0 && it.seq == seq {
			continue
		}

		ev := it.file.poll() & (it.events | EPOLLERR | EPOLLHUP)

		if ev == 0 {
			continue
		}

		if report {
			it.disabled = it.events&EPOLLONESHOT != 0
			it.seq = seq
		}

		evs = binary.LittleEndian.AppendUint32(evs, ev)
		evs = binary.LittleEndian.AppendUint64(evs, it.data)

		if len(evs) == max*epollEventSize {
			break
		}
	}

	return
}

func (e *epollFile) Poll(f *File) uint32 {
	if len(e.ready(1, false)) > 0 {
		return POLLIN
	}

	return 0
}

func (e *epollFile) find(fd int, f *File) *epitem {
	for _, it := range e.items {
		if it.fd == fd && it.file == f {
			return it
		}
	}

	return nil
}

func (t *Task) epollCtl(epfd int, op int, fd int, evAddr uint64) Errno {
	ef := t.proc.files.get(epfd)

	if ef == nil {
		return EBADF
	}

	ep, ok := ef.ops.(*epollFile)

	if !ok {
		return EINVAL
	}

	f := t.proc.files.get(fd)

	if f == nil {
		return EBADF
	}

	if f == ef {
		return EINVAL
	}

	// regular files and directories do not support epoll
	switch f.ops.(type) {
	case regularFile, dirFile:
		return EPERM
	}

	var ev epollEvent

	if op != EPOLL_CTL_DEL {
		b := make([]byte, binary.Size(ev))

		if err := t.proc.mm.copyIn(evAddr, b); err != 0 {
			return err
		}

		binary.Decode(b, binary.LittleEndian, &ev)
	}

	events, data := ev.Events, ev.Data

	it := ep.find(fd, f)

	switch op {
	case EPOLL_CTL_ADD:
		if it != nil {
			return EEXIST
		}

		ep.items = append(ep.items, &epitem{fd: fd, file: f, events: events, data: data})
	case EPOLL_CTL_MOD:
		if it == nil {
			return ENOENT
		}

		it.events = events
		it.data = data
		it.disabled = false
		it.seq = 0
	case EPOLL_CTL_DEL:
		if it == nil {
			return ENOENT
		}

		for i, o := range ep.items {
			if o == it {
				ep.items = append(ep.items[:i], ep.items[i+1:]...)
				break
			}
		}
	default:
		return EINVAL
	}

	// the interest list change might make an epoll_wait ready
	pollers.wake()

	return 0
}

func (t *Task) epollWait(epfd int, evAddr uint64, max int, timeout int64) (uint64, Errno) {
	ef := t.proc.files.get(epfd)

	if ef == nil {
		return fail(EBADF)
	}

	ep, ok := ef.ops.(*epollFile)

	if !ok || max <= 0 || max > 65536 {
		return fail(EINVAL)
	}

	var evs []byte

	check := func() bool {
		evs = ep.ready(max, true)
		return len(evs) > 0
	}

	if err := t.pollWait(check, timeout); err != 0 {
		return fail(err)
	}

	return uint64(len(evs) / epollEventSize), t.proc.mm.copyOut(evAddr, evs)
}

// eventfd

type eventFD struct {
	count     uint64
	semaphore bool
}

func (e *eventFD) Mode() uint32 {
	return S_IFREG | 0600
}

func (e *eventFD) Read(t *Task, f *File, buf []byte) (int, Errno) {
	if len(buf) < 8 {
		return 0, EINVAL
	}

	ready := func() bool { return e.count > 0 }

	if !ready() {
		if f.nonblock() {
			return 0, EAGAIN
		}

		if err := t.block(ready, 0, true); err != 0 {
			return 0, err
		}
	}

	v := e.count

	if e.semaphore {
		v = 1
	}

	e.count -= v
	binary.LittleEndian.PutUint64(buf, v)
	wakeAll()

	return 8, 0
}

func (e *eventFD) Write(t *Task, f *File, buf []byte) (int, Errno) {
	if len(buf) < 8 {
		return 0, EINVAL
	}

	v := binary.LittleEndian.Uint64(buf)

	if v == ^uint64(0) {
		return 0, EINVAL
	}

	ready := func() bool {
		return e.count+v < ^uint64(0) && e.count+v >= e.count
	}

	if !ready() {
		if f.nonblock() {
			return 0, EAGAIN
		}

		if err := t.block(ready, 0, true); err != 0 {
			return 0, err
		}
	}

	e.count += v
	wakeAll()

	return 8, 0
}

func (e *eventFD) Poll(f *File) uint32 {
	ev := uint32(POLLOUT)

	if e.count > 0 {
		ev |= POLLIN
	}

	return ev
}

func registerPollSyscalls() {
	reg(SYS_PPOLL, func(t *Task, a *[6]uint64) (uint64, Errno) {
		timeout := int64(-1)

		if a[2] != 0 {
			d, err := t.readTimespec(a[2])

			if err != 0 {
				return fail(err)
			}

			timeout = d
		}

		if err := t.setTempMask(a[3], a[4]); err != 0 {
			return fail(err)
		}

		return t.ppoll(a[0], a[1], timeout)
	})

	reg(SYS_PSELECT6, func(t *Task, a *[6]uint64) (uint64, Errno) {
		timeout := int64(-1)

		if a[4] != 0 {
			d, err := t.readTimespec(a[4])

			if err != 0 {
				return fail(err)
			}

			timeout = d
		}

		if a[5] != 0 {
			ss, err := t.proc.mm.readU64(a[5])

			if err != 0 {
				return fail(err)
			}

			size, err := t.proc.mm.readU64(a[5] + 8)

			if err != 0 {
				return fail(err)
			}

			if err := t.setTempMask(ss, size); err != 0 {
				return fail(err)
			}
		}

		return t.pselect(int(a[0]), a[1], a[2], a[3], timeout)
	})

	reg(SYS_EPOLL_CREATE1, func(t *Task, a *[6]uint64) (uint64, Errno) {
		f := newFile(&epollFile{}, nil, O_RDWR, "anon_inode:[eventpoll]")
		fd := t.proc.files.install(f, 0, a[0]&O_CLOEXEC != 0)
		f.decref()

		if fd < 0 {
			return fail(Errno(-fd))
		}

		return ok(fd)
	})

	reg(SYS_EPOLL_CTL, func(t *Task, a *[6]uint64) (uint64, Errno) {
		return 0, t.epollCtl(int(int32(a[0])), int(a[1]), int(int32(a[2])), a[3])
	})

	reg(SYS_EPOLL_PWAIT, func(t *Task, a *[6]uint64) (uint64, Errno) {
		timeout := int64(int32(a[3]))

		if timeout > 0 {
			timeout *= 1e6
		}

		if err := t.setTempMask(a[4], a[5]); err != 0 {
			return fail(err)
		}

		return t.epollWait(int(int32(a[0])), a[1], int(int32(a[2])), timeout)
	})

	reg(SYS_EPOLL_PWAIT2, func(t *Task, a *[6]uint64) (uint64, Errno) {
		timeout := int64(-1)

		if a[3] != 0 {
			d, err := t.readTimespec(a[3])

			if err != 0 {
				return fail(err)
			}

			timeout = d
		}

		if err := t.setTempMask(a[4], a[5]); err != 0 {
			return fail(err)
		}

		return t.epollWait(int(int32(a[0])), a[1], int(int32(a[2])), timeout)
	})

	reg(SYS_EVENTFD2, func(t *Task, a *[6]uint64) (uint64, Errno) {
		e := &eventFD{count: uint64(uint32(a[0])), semaphore: a[1]&1 != 0}
		f := newFile(e, nil, O_RDWR|(int(a[1])&O_NONBLOCK), "anon_inode:[eventfd]")
		fd := t.proc.files.install(f, 0, a[1]&O_CLOEXEC != 0)
		f.decref()

		if fd < 0 {
			return fail(Errno(-fd))
		}

		return ok(fd)
	})
}
