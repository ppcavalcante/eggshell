// Copyright (c) The eggshell Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

package kernel

import (
	"bytes"
	"encoding/binary"
	"time"
)

// address families and socket types
const (
	AF_UNIX  = 1
	AF_INET  = 2
	AF_INET6 = 10

	SOCK_STREAM    = 1
	SOCK_DGRAM     = 2
	SOCK_RAW       = 3
	SOCK_SEQPACKET = 5
	SOCK_NONBLOCK  = O_NONBLOCK
	SOCK_CLOEXEC   = O_CLOEXEC

	IPPROTO_IP   = 0
	IPPROTO_TCP  = 6
	IPPROTO_UDP  = 17
	IPPROTO_IPV6 = 41

	SOL_SOCKET = 1

	SO_DEBUG      = 1
	SO_REUSEADDR  = 2
	SO_TYPE       = 3
	SO_ERROR      = 4
	SO_DONTROUTE  = 5
	SO_BROADCAST  = 6
	SO_SNDBUF     = 7
	SO_RCVBUF     = 8
	SO_KEEPALIVE  = 9
	SO_LINGER     = 13
	SO_REUSEPORT  = 15
	SO_RCVTIMEO   = 20
	SO_SNDTIMEO   = 21
	SO_ACCEPTCONN = 30
	SO_PROTOCOL   = 38
	SO_DOMAIN     = 39

	TCP_NODELAY = 1

	MSG_OOB      = 0x1
	MSG_PEEK     = 0x2
	MSG_TRUNC    = 0x20
	MSG_DONTWAIT = 0x40
	MSG_WAITALL  = 0x100
	MSG_NOSIGNAL = 0x4000

	SHUT_RD   = 0
	SHUT_WR   = 1
	SHUT_RDWR = 2
)

// socket represents the operations common to all socket types.
type socket interface {
	FileOps
	poller
	closer
	statter

	bind(t *Task, addr []byte) Errno
	listen(t *Task, backlog int) Errno
	accept(t *Task, f *File) (*File, []byte, Errno)
	connect(t *Task, f *File, addr []byte) Errno
	sockname() []byte
	peername() ([]byte, Errno)
	sendto(t *Task, f *File, buf []byte, flags int, addr []byte) (int, Errno)
	recvfrom(t *Task, f *File, buf []byte, flags int) (int, []byte, Errno)
	setsockopt(level int, opt int, val []byte) Errno
	getsockopt(level int, opt int) ([]byte, Errno)
	shutdown(how int) Errno
}

func u32bytes(v uint32) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, v)
	return b
}

func boolOpt(val []byte) bool {
	return len(val) >= 4 && binary.LittleEndian.Uint32(val) != 0 || len(val) > 0 && len(val) < 4 && val[0] != 0
}

// common socket options
func sockTimeout(val []byte) time.Duration {
	if len(val) < 16 {
		return 0
	}

	sec := int64(binary.LittleEndian.Uint64(val[0:]))
	usec := int64(binary.LittleEndian.Uint64(val[8:]))

	return time.Duration(sec)*time.Second + time.Duration(usec)*time.Microsecond
}

// sockOpts holds options not handled by the protocol implementation.
type sockOpts struct {
	rcvtimeo time.Duration
	sndtimeo time.Duration
}

func (o *sockOpts) deadline(recv bool) int64 {
	d := o.sndtimeo

	if recv {
		d = o.rcvtimeo
	}

	if d == 0 {
		return 0
	}

	return nanotime() + int64(d)
}

type unixListener struct {
	sock    *unixSocket
	backlog []*unixSocket
	max     int
}

type unixSocket struct {
	sockOpts

	stype int

	rx   *Pipe
	peer *unixSocket

	path            string
	listener        *unixListener
	pendingListener *unixListener
	connected       bool
	shutWr          bool
	shutRd          bool
}

func newUnixPair(stype int) (*unixSocket, *unixSocket) {
	a := &unixSocket{stype: stype, rx: &Pipe{readers: 1, writers: 1}, connected: true}
	b := &unixSocket{stype: stype, rx: &Pipe{readers: 1, writers: 1}, connected: true}
	a.peer, b.peer = b, a

	return a, b
}

func marshalUnixAddr(path string) []byte {
	b := make([]byte, 2+len(path)+1)
	binary.LittleEndian.PutUint16(b, AF_UNIX)
	copy(b[2:], path)

	if path == "" {
		return b[:2]
	}

	return b
}

func parseUnixAddr(addr []byte) (string, Errno) {
	if len(addr) < 3 || binary.LittleEndian.Uint16(addr) != AF_UNIX {
		return "", EINVAL
	}

	p := addr[2:]

	if p[0] == 0 {
		// abstract namespace
		return "@" + string(bytes.TrimRight(p[1:], "\x00")), 0
	}

	if i := bytes.IndexByte(p, 0); i >= 0 {
		p = p[:i]
	}

	return string(p), 0
}

var abstractSockets = map[string]*unixListener{}

func (s *unixSocket) Mode() uint32 {
	return S_IFSOCK | 0777
}

func (s *unixSocket) Poll(f *File) uint32 {
	var ev uint32

	if s.listener != nil {
		if len(s.listener.backlog) > 0 {
			ev |= POLLIN
		}
		return ev
	}

	if !s.connected {
		return POLLOUT
	}

	if s.rx.n > 0 || s.rx.writers == 0 || s.shutRd {
		ev |= POLLIN
	}

	if s.rx.writers == 0 {
		ev |= POLLRDHUP
	}

	if s.peer == nil || s.peer.rx.readers == 0 {
		ev |= POLLHUP | POLLERR
	} else if pipeSize-s.peer.rx.n >= pipeBuf && !s.shutWr {
		ev |= POLLOUT
	}

	if s.peer == nil && s.rx.writers == 0 {
		ev |= POLLHUP
	}

	return ev
}

func (s *unixSocket) Close(f *File) {
	if s.listener != nil {
		for _, c := range s.listener.backlog {
			c.close()
		}

		s.listener.backlog = nil

		if s.path != "" && s.path[0] == '@' {
			delete(abstractSockets, s.path)
		}
	}

	s.close()
}

func (s *unixSocket) close() {
	if s.rx != nil {
		s.rx.readers = 0
	}

	if s.peer != nil && !s.shutWr {
		s.peer.rx.writers = 0
	}

	wakeAll()
}

func (s *unixSocket) Read(t *Task, f *File, buf []byte) (int, Errno) {
	n, _, err := s.recvfrom(t, f, buf, 0)
	return n, err
}

func (s *unixSocket) Write(t *Task, f *File, buf []byte) (int, Errno) {
	return s.sendto(t, f, buf, 0, nil)
}

func (s *unixSocket) bind(t *Task, addr []byte) Errno {
	p, err := parseUnixAddr(addr)

	if err != 0 {
		return err
	}

	if s.path != "" {
		return EINVAL
	}

	l := &unixListener{sock: s}

	if p[0] == '@' {
		if abstractSockets[p] != nil {
			return EADDRINUSE
		}

		abstractSockets[p] = l
	} else {
		dir, name, err := t.lookupParent(AT_FDCWD, p)

		if err != 0 {
			return err
		}

		if dir.child(name) != nil {
			return EADDRINUSE
		}

		ino := newInode(S_IFSOCK | (0777 &^ t.proc.fs.umask))
		ino.unix = l
		ino.nlink = 0
		dir.link(name, ino)
	}

	s.path = p
	s.listener = nil
	s.pendingListener = l

	return 0
}

func (s *unixSocket) listen(t *Task, backlog int) Errno {
	if s.pendingListener == nil {
		return EINVAL
	}

	if backlog <= 0 || backlog > 4096 {
		backlog = 4096
	}

	s.listener = s.pendingListener
	s.listener.max = backlog

	return 0
}

func (s *unixSocket) accept(t *Task, f *File) (*File, []byte, Errno) {
	l := s.listener

	if l == nil {
		return nil, nil, EINVAL
	}

	if len(l.backlog) == 0 {
		if f.nonblock() {
			return nil, nil, EAGAIN
		}

		if err := t.block(func() bool { return len(l.backlog) > 0 }, s.deadline(true), true); err != 0 {
			if err == ETIMEDOUT {
				err = EAGAIN
			}
			return nil, nil, err
		}
	}

	c := l.backlog[0]
	l.backlog = l.backlog[1:]
	c.path = s.path

	return newFile(c, nil, O_RDWR, "socket:"), marshalUnixAddr(""), 0
}

func (s *unixSocket) connect(t *Task, f *File, addr []byte) Errno {
	p, err := parseUnixAddr(addr)

	if err != 0 {
		return err
	}

	if s.connected {
		return EISCONN
	}

	var l *unixListener

	if p[0] == '@' {
		l = abstractSockets[p]
	} else {
		ino, err := t.lookup(AT_FDCWD, p, true)

		if err != 0 {
			return err
		}

		if ino.mode&S_IFMT != S_IFSOCK {
			return ECONNREFUSED
		}

		l = ino.unix
	}

	if l == nil || l.sock.listener != l {
		return ECONNREFUSED
	}

	if len(l.backlog) >= l.max {
		return EAGAIN
	}

	server, client := newUnixPair(s.stype)

	s.rx = client.rx
	s.peer = server
	server.peer = s
	s.connected = true

	l.backlog = append(l.backlog, server)
	wakeAll()

	return 0
}

func (s *unixSocket) sockname() []byte {
	return marshalUnixAddr(s.path)
}

func (s *unixSocket) peername() ([]byte, Errno) {
	if s.peer == nil {
		return nil, ENOTCONN
	}

	return marshalUnixAddr(s.peer.path), 0
}

func (s *unixSocket) sendto(t *Task, f *File, buf []byte, flags int, addr []byte) (int, Errno) {
	if !s.connected || s.peer == nil {
		return 0, ENOTCONN
	}

	if s.shutWr || s.peer.rx.readers == 0 {
		if flags&MSG_NOSIGNAL == 0 {
			t.signal(SIGPIPE, SigInfo{Signo: SIGPIPE})
		}
		return 0, EPIPE
	}

	end := &pipeEnd{pipe: s.peer.rx, write: true}

	if flags&MSG_DONTWAIT != 0 && !f.nonblock() {
		tmp := *f
		tmp.flags |= O_NONBLOCK
		f = &tmp
	}

	if flags&MSG_NOSIGNAL != 0 && s.peer.rx.readers == 0 {
		return 0, EPIPE
	}

	return end.Write(t, f, buf)
}

func (s *unixSocket) recvfrom(t *Task, f *File, buf []byte, flags int) (int, []byte, Errno) {
	if !s.connected {
		return 0, nil, ENOTCONN
	}

	if s.shutRd {
		return 0, nil, 0
	}

	p := s.rx

	if flags&MSG_PEEK != 0 {
		ready := func() bool { return p.n > 0 || p.writers == 0 }

		if !ready() {
			if f.nonblock() || flags&MSG_DONTWAIT != 0 {
				return 0, nil, EAGAIN
			}

			if err := t.block(ready, s.deadline(true), true); err != 0 {
				return 0, nil, err
			}
		}

		return p.peek(buf), nil, 0
	}

	if flags&MSG_DONTWAIT != 0 && !f.nonblock() {
		tmp := *f
		tmp.flags |= O_NONBLOCK
		f = &tmp
	}

	n, err := (&pipeEnd{pipe: p}).Read(t, f, buf)

	return n, nil, err
}

func (s *unixSocket) setsockopt(level int, opt int, val []byte) Errno {
	if level == SOL_SOCKET {
		switch opt {
		case SO_RCVTIMEO:
			s.rcvtimeo = sockTimeout(val)
		case SO_SNDTIMEO:
			s.sndtimeo = sockTimeout(val)
		}
	}

	return 0
}

func (s *unixSocket) getsockopt(level int, opt int) ([]byte, Errno) {
	if level == SOL_SOCKET {
		switch opt {
		case SO_TYPE:
			return u32bytes(uint32(s.stype)), 0
		case SO_DOMAIN:
			return u32bytes(AF_UNIX), 0
		case SO_ACCEPTCONN:
			if s.listener != nil {
				return u32bytes(1), 0
			}
		case SO_SNDBUF, SO_RCVBUF:
			return u32bytes(pipeSize), 0
		}
	}

	return u32bytes(0), 0
}

func (s *unixSocket) shutdown(how int) Errno {
	if !s.connected {
		return ENOTCONN
	}

	if how == SHUT_RD || how == SHUT_RDWR {
		s.shutRd = true
	}

	if (how == SHUT_WR || how == SHUT_RDWR) && !s.shutWr {
		s.shutWr = true

		if s.peer != nil {
			s.peer.rx.writers = 0
		}
	}

	wakeAll()

	return 0
}

// system calls

func (t *Task) socket(f *File) (socket, Errno) {
	s, ok := f.ops.(socket)

	if !ok {
		return nil, ENOTSOCK
	}

	return s, 0
}

func (t *Task) sockFD(fd uint64) (*File, socket, Errno) {
	f, err := t.file(fd)

	if err != 0 {
		return nil, nil, err
	}

	s, err := t.socket(f)

	return f, s, err
}

func (t *Task) readSockaddr(addr uint64, length uint64) ([]byte, Errno) {
	if addr == 0 {
		return nil, 0
	}

	if length > 128 {
		return nil, EINVAL
	}

	b := make([]byte, length)

	return b, t.proc.mm.copyIn(addr, b)
}

// writeSockaddr stores a socket address honoring the user buffer length.
func (t *Task) writeSockaddr(addr uint64, lenAddr uint64, sa []byte) Errno {
	if addr == 0 || lenAddr == 0 {
		return 0
	}

	mm := t.proc.mm

	l, err := mm.readU32(lenAddr)

	if err != 0 {
		return err
	}

	n := uint32(len(sa))

	if n > l {
		sa = sa[:l]
	}

	if err = mm.copyOut(addr, sa); err != 0 {
		return err
	}

	return mm.writeU32(lenAddr, n)
}

func (t *Task) installSocket(s socket, flags int) (uint64, Errno) {
	f := newFile(s, nil, O_RDWR|(flags&O_NONBLOCK), "socket:")
	fd := t.proc.files.install(f, 0, flags&O_CLOEXEC != 0)
	f.decref()

	if fd < 0 {
		return fail(Errno(-fd))
	}

	return ok(fd)
}

// msghdr represents the struct msghdr.
type msghdr struct {
	Name       uint64
	Namelen    uint32
	_          uint32
	Iov        uint64
	Iovlen     uint64
	Control    uint64
	Controllen uint64
	Flags      int32
	_          int32
}

// Bytes converts the message header structure to byte array format.
func (h *msghdr) Bytes() []byte {
	b, _ := binary.Append(nil, binary.LittleEndian, h)
	return b
}

func (t *Task) readMsghdr(addr uint64, h *msghdr) Errno {
	b := make([]byte, binary.Size(h))

	if err := t.proc.mm.copyIn(addr, b); err != 0 {
		return err
	}

	binary.Decode(b, binary.LittleEndian, h)

	return 0
}

// send sends the contents of an I/O vector on a socket.
func (t *Task) send(fd uint64, iov []iovec, flags int, addr []byte) (uint64, Errno) {
	f, s, err := t.sockFD(fd)

	if err != 0 {
		return fail(err)
	}

	buf, p, err := t.gather(iov)

	if err != 0 {
		return fail(err)
	}

	defer putIOBuf(p)

	n, err := s.sendto(t, f, buf, flags, addr)

	return uint64(int64(n)), err
}

// recv receives data from a socket to an I/O vector, it returns the length
// reported by the socket and the sender address.
func (t *Task) recv(fd uint64, iov []iovec, flags int) (int, []byte, Errno) {
	f, s, err := t.sockFD(fd)

	if err != 0 {
		return 0, nil, err
	}

	buf, p := getIOBuf(min(iovLen(iov), ioChunk))
	defer putIOBuf(p)

	n, from, err := s.recvfrom(t, f, buf, flags)

	if err != 0 {
		return 0, nil, err
	}

	if err := t.scatter(iov, buf[:min(n, len(buf))]); err != 0 {
		return 0, nil, err
	}

	return n, from, 0
}

func registerNetSyscalls() {
	reg(SYS_SOCKET, func(t *Task, a *[6]uint64) (uint64, Errno) {
		family := int(a[0])
		stype := int(a[1]) & 0xf
		flags := int(a[1]) &^ 0xf

		switch family {
		case AF_UNIX:
			if stype != SOCK_STREAM && stype != SOCK_SEQPACKET {
				return fail(EPROTONOSUPPORT)
			}

			return t.installSocket(&unixSocket{stype: stype}, flags)
		case AF_INET:
			s, err := inetSocketCreate(stype, int(a[2]))

			if err != 0 {
				return fail(err)
			}

			return t.installSocket(s, flags)
		}

		return fail(EAFNOSUPPORT)
	})

	reg(SYS_SOCKETPAIR, func(t *Task, a *[6]uint64) (uint64, Errno) {
		stype := int(a[1]) & 0xf
		flags := int(a[1]) &^ 0xf

		if a[0] != AF_UNIX {
			return fail(EAFNOSUPPORT)
		}

		if stype != SOCK_STREAM && stype != SOCK_SEQPACKET && stype != SOCK_DGRAM {
			return fail(EPROTONOSUPPORT)
		}

		x, y := newUnixPair(stype)

		fd0, err := t.installSocket(x, flags)

		if err != 0 {
			return fail(err)
		}

		fd1, err := t.installSocket(y, flags)

		if err != 0 {
			t.proc.files.close(int(fd0))
			return fail(err)
		}

		b, _ := binary.Append(nil, binary.LittleEndian, []int32{int32(fd0), int32(fd1)})

		return 0, t.proc.mm.copyOut(a[3], b)
	})

	reg(SYS_BIND, func(t *Task, a *[6]uint64) (uint64, Errno) {
		_, s, err := t.sockFD(a[0])

		if err != 0 {
			return fail(err)
		}

		addr, err := t.readSockaddr(a[1], a[2])

		if err != 0 {
			return fail(err)
		}

		return 0, s.bind(t, addr)
	})

	reg(SYS_LISTEN, func(t *Task, a *[6]uint64) (uint64, Errno) {
		_, s, err := t.sockFD(a[0])

		if err != 0 {
			return fail(err)
		}

		return 0, s.listen(t, int(int32(a[1])))
	})

	accept := func(t *Task, a *[6]uint64, flags int) (uint64, Errno) {
		f, s, err := t.sockFD(a[0])

		if err != 0 {
			return fail(err)
		}

		nf, peer, err := s.accept(t, f)

		if err != 0 {
			return fail(err)
		}

		nf.flags |= flags & O_NONBLOCK
		fd := t.proc.files.install(nf, 0, flags&O_CLOEXEC != 0)
		nf.decref()

		if fd < 0 {
			return fail(Errno(-fd))
		}

		if err := t.writeSockaddr(a[1], a[2], peer); err != 0 {
			return fail(err)
		}

		return ok(fd)
	}

	reg(SYS_ACCEPT, func(t *Task, a *[6]uint64) (uint64, Errno) {
		return accept(t, a, 0)
	})

	reg(SYS_ACCEPT4, func(t *Task, a *[6]uint64) (uint64, Errno) {
		return accept(t, a, int(a[3]))
	})

	reg(SYS_CONNECT, func(t *Task, a *[6]uint64) (uint64, Errno) {
		f, s, err := t.sockFD(a[0])

		if err != 0 {
			return fail(err)
		}

		addr, err := t.readSockaddr(a[1], a[2])

		if err != 0 {
			return fail(err)
		}

		return 0, s.connect(t, f, addr)
	})

	reg(SYS_GETSOCKNAME, func(t *Task, a *[6]uint64) (uint64, Errno) {
		_, s, err := t.sockFD(a[0])

		if err != 0 {
			return fail(err)
		}

		return 0, t.writeSockaddr(a[1], a[2], s.sockname())
	})

	reg(SYS_GETPEERNAME, func(t *Task, a *[6]uint64) (uint64, Errno) {
		_, s, err := t.sockFD(a[0])

		if err != 0 {
			return fail(err)
		}

		sa, err := s.peername()

		if err != 0 {
			return fail(err)
		}

		return 0, t.writeSockaddr(a[1], a[2], sa)
	})

	reg(SYS_SENDTO, func(t *Task, a *[6]uint64) (uint64, Errno) {
		addr, err := t.readSockaddr(a[4], a[5])

		if err != 0 {
			return fail(err)
		}

		return t.send(a[0], []iovec{{Base: a[1], Len: a[2]}}, int(a[3]), addr)
	})

	reg(SYS_RECVFROM, func(t *Task, a *[6]uint64) (uint64, Errno) {
		n, from, err := t.recv(a[0], []iovec{{Base: a[1], Len: a[2]}}, int(a[3]))

		if err != 0 {
			return fail(err)
		}

		if from != nil {
			if err := t.writeSockaddr(a[4], a[5], from); err != 0 {
				return fail(err)
			}
		} else if a[5] != 0 {
			t.proc.mm.writeU32(a[5], 0)
		}

		return ok(n)
	})

	reg(SYS_SENDMSG, func(t *Task, a *[6]uint64) (uint64, Errno) {
		var hdr msghdr

		if err := t.readMsghdr(a[1], &hdr); err != 0 {
			return fail(err)
		}

		addr, err := t.readSockaddr(hdr.Name, uint64(hdr.Namelen))

		if err != 0 {
			return fail(err)
		}

		iov, err := t.iovecs(hdr.Iov, hdr.Iovlen)

		if err != 0 {
			return fail(err)
		}

		return t.send(a[0], iov, int(a[2]), addr)
	})

	reg(SYS_RECVMSG, func(t *Task, a *[6]uint64) (uint64, Errno) {
		mm := t.proc.mm

		var hdr msghdr

		if err := t.readMsghdr(a[1], &hdr); err != 0 {
			return fail(err)
		}

		iov, err := t.iovecs(hdr.Iov, hdr.Iovlen)

		if err != 0 {
			return fail(err)
		}

		n, from, err := t.recv(a[0], iov, int(a[2]))

		if err != 0 {
			return fail(err)
		}

		if hdr.Name != 0 && from != nil {
			mm.copyOut(hdr.Name, from[:min(len(from), int(hdr.Namelen))])
			hdr.Namelen = uint32(len(from))
		} else {
			hdr.Namelen = 0
		}

		// no ancillary data
		hdr.Controllen = 0
		hdr.Flags = 0

		mm.copyOut(a[1], hdr.Bytes())

		return ok(n)
	})

	reg(SYS_SETSOCKOPT, func(t *Task, a *[6]uint64) (uint64, Errno) {
		_, s, err := t.sockFD(a[0])

		if err != 0 {
			return fail(err)
		}

		if a[4] > 256 {
			return fail(EINVAL)
		}

		val := make([]byte, a[4])

		if err := t.proc.mm.copyIn(a[3], val); err != 0 {
			return fail(err)
		}

		return 0, s.setsockopt(int(a[1]), int(a[2]), val)
	})

	reg(SYS_GETSOCKOPT, func(t *Task, a *[6]uint64) (uint64, Errno) {
		_, s, err := t.sockFD(a[0])

		if err != 0 {
			return fail(err)
		}

		val, err := s.getsockopt(int(a[1]), int(a[2]))

		if err != 0 {
			return fail(err)
		}

		return 0, t.writeSockaddr(a[3], a[4], val)
	})

	reg(SYS_SHUTDOWN, func(t *Task, a *[6]uint64) (uint64, Errno) {
		_, s, err := t.sockFD(a[0])

		if err != 0 {
			return fail(err)
		}

		return 0, s.shutdown(int(a[1]))
	})
}
