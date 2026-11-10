// Copyright (c) The eggshell Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

package kernel

// AF_INET sockets over the gVisor TCP/IP stack

import (
	"bytes"
	"encoding/binary"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// NetBackend names the network stack serving AF_INET sockets.
const NetBackend = "gvisor"

var (
	netStack *stack.Stack
)

// SetNetStack registers the gVisor network stack serving AF_INET sockets.
func SetNetStack(s *stack.Stack) {
	// Requires merge of https://github.com/google/gvisor/pull/16953
	//da := tcpip.TCPDelayedACKOption(true)
	//s.SetTransportProtocolOption(tcp.ProtocolNumber, &da)

	netStack = s
}

// AF_INET sockets (gVisor)

type inetSocket struct {
	sockOpts

	stype int
	proto int

	ep    tcpip.Endpoint
	wq    *waiter.Queue
	entry waiter.Entry

	// tasks waiting for endpoint events
	q waitQueue

	listening bool
}

func newInetSocket(stype int, proto int, ep tcpip.Endpoint, wq *waiter.Queue) *inetSocket {
	s := &inetSocket{
		stype: stype,
		proto: proto,
		ep:    ep,
		wq:    wq,
	}

	// Requires merge of https://github.com/google/gvisor/pull/17082
	//ep.SocketOptions().SetQuickAck(true)

	s.entry = waiter.NewFunctionEntry(
		waiter.EventIn|
			waiter.EventOut|
			waiter.EventErr|
			waiter.EventHUp|
			waiter.EventRdHUp,
		func(waiter.EventMask) {
			// raised by network stack goroutines, without klock
			s.q.wake()
			pollSeq.Add(1)
			pollers.wake()
		},
	)

	wq.EventRegister(&s.entry)

	return s
}

func inetSocketCreate(stype int, proto int) (*inetSocket, Errno) {
	if netStack == nil {
		return nil, EAFNOSUPPORT
	}

	var tp tcpip.TransportProtocolNumber

	switch stype {
	case SOCK_STREAM:
		if proto != 0 && proto != IPPROTO_TCP {
			return nil, EPROTONOSUPPORT
		}
		tp = tcp.ProtocolNumber
		proto = IPPROTO_TCP
	case SOCK_DGRAM:
		if proto != 0 && proto != IPPROTO_UDP {
			return nil, EPROTONOSUPPORT
		}
		tp = udp.ProtocolNumber
		proto = IPPROTO_UDP
	default:
		return nil, ESOCKTNOSUPPORT
	}

	wq := &waiter.Queue{}
	ep, err := netStack.NewEndpoint(tp, ipv4.ProtocolNumber, wq)

	if err != nil {
		return nil, tcpipErrno(err)
	}

	return newInetSocket(stype, proto, ep, wq), 0
}

func tcpipErrno(err tcpip.Error) Errno {
	switch err.(type) {
	case nil:
		return 0
	case *tcpip.ErrWouldBlock:
		return EAGAIN
	case *tcpip.ErrConnectionRefused:
		return ECONNREFUSED
	case *tcpip.ErrConnectionReset:
		return ECONNRESET
	case *tcpip.ErrConnectionAborted:
		return ECONNABORTED
	case *tcpip.ErrTimeout:
		return ETIMEDOUT
	case *tcpip.ErrPortInUse, *tcpip.ErrDuplicateAddress:
		return EADDRINUSE
	case *tcpip.ErrBadLocalAddress:
		return EADDRNOTAVAIL
	case *tcpip.ErrHostUnreachable:
		return EHOSTUNREACH
	case *tcpip.ErrNetworkUnreachable:
		return ENETUNREACH
	case *tcpip.ErrAlreadyBound:
		return EINVAL
	case *tcpip.ErrInvalidEndpointState:
		return EINVAL
	case *tcpip.ErrAlreadyConnecting:
		return EALREADY
	case *tcpip.ErrAlreadyConnected:
		return EISCONN
	case *tcpip.ErrNotConnected:
		return ENOTCONN
	case *tcpip.ErrConnectStarted:
		return EINPROGRESS
	case *tcpip.ErrClosedForSend:
		return EPIPE
	case *tcpip.ErrClosedForReceive:
		return 0
	case *tcpip.ErrMessageTooLong:
		return EMSGSIZE
	case *tcpip.ErrNoBufferSpace:
		return ENOBUFS
	case *tcpip.ErrAddressFamilyNotSupported:
		return EAFNOSUPPORT
	case *tcpip.ErrDestinationRequired:
		return EDESTADDRREQ
	case *tcpip.ErrUnknownProtocolOption, *tcpip.ErrNotSupported:
		return ENOPROTOOPT
	case *tcpip.ErrInvalidOptionValue, *tcpip.ErrBadAddress:
		return EINVAL
	case *tcpip.ErrBroadcastDisabled, *tcpip.ErrNotPermitted:
		return EACCES
	case *tcpip.ErrAborted:
		return EPIPE
	}

	return EIO
}

func parseInetAddr(addr []byte) (fa tcpip.FullAddress, err Errno) {
	if len(addr) < 16 {
		return fa, EINVAL
	}

	family := binary.LittleEndian.Uint16(addr[0:])

	switch family {
	case AF_INET:
	case 0: // AF_UNSPEC
		return fa, EAFNOSUPPORT
	default:
		return fa, EAFNOSUPPORT
	}

	fa.Port = binary.BigEndian.Uint16(addr[2:])

	ip := addr[4:8]

	if !bytes.Equal(ip, []byte{0, 0, 0, 0}) {
		fa.Addr = tcpip.AddrFrom4Slice(ip)
	}

	return fa, 0
}

func marshalInetAddr(fa tcpip.FullAddress) []byte {
	b := make([]byte, 16)
	binary.LittleEndian.PutUint16(b[0:], AF_INET)
	binary.BigEndian.PutUint16(b[2:], fa.Port)

	if fa.Addr.Len() == 4 {
		copy(b[4:], fa.Addr.AsSlice())
	}

	return b
}

func (s *inetSocket) Mode() uint32 { return S_IFSOCK | 0777 }

func (s *inetSocket) Poll(f *File) uint32 {
	mask := s.ep.Readiness(
		waiter.EventIn |
			waiter.EventOut |
			waiter.EventErr |
			waiter.EventHUp |
			waiter.EventRdHUp,
	)

	return uint32(mask)
}

func (s *inetSocket) Close(f *File) {
	s.wq.EventUnregister(&s.entry)
	s.ep.Close()
}

func (s *inetSocket) Read(t *Task, f *File, buf []byte) (int, Errno) {
	n, _, err := s.recvfrom(t, f, buf, 0)
	return n, err
}

func (s *inetSocket) Write(t *Task, f *File, buf []byte) (int, Errno) {
	return s.sendto(t, f, buf, 0, nil)
}

func (s *inetSocket) bind(t *Task, addr []byte) Errno {
	fa, err := parseInetAddr(addr)

	if err != 0 {
		return err
	}

	return tcpipErrno(s.ep.Bind(fa))
}

func (s *inetSocket) listen(t *Task, backlog int) Errno {
	if s.stype != SOCK_STREAM {
		return EOPNOTSUPP
	}

	if backlog <= 0 || backlog > 4096 {
		backlog = 4096
	}

	if err := s.ep.Listen(backlog); err != nil {
		return tcpipErrno(err)
	}

	s.listening = true

	return 0
}

func (s *inetSocket) accept(t *Task, f *File) (*File, []byte, Errno) {
	if !s.listening {
		return nil, nil, EINVAL
	}

	for {
		var peer tcpip.FullAddress

		ep, wq, err := s.ep.Accept(&peer)

		if err == nil {
			ns := newInetSocket(s.stype, s.proto, ep, wq)
			return newFile(ns, nil, O_RDWR, "socket:"), marshalInetAddr(peer), 0
		}

		if _, ok := err.(*tcpip.ErrWouldBlock); !ok {
			return nil, nil, tcpipErrno(err)
		}

		if f.nonblock() {
			return nil, nil, EAGAIN
		}

		if e := t.blockOn(&s.q, func() bool { return s.Poll(f)&POLLIN != 0 }, s.deadline(true), true); e != 0 {
			if e == ETIMEDOUT {
				e = EAGAIN
			}
			return nil, nil, e
		}
	}
}

func (s *inetSocket) connect(t *Task, f *File, addr []byte) Errno {
	if len(addr) >= 2 && binary.LittleEndian.Uint16(addr) == 0 && s.stype == SOCK_DGRAM {
		// AF_UNSPEC: dissolve association
		s.ep.Disconnect()
		return 0
	}

	fa, err := parseInetAddr(addr)

	if err != 0 {
		return err
	}

	if fa.Addr.Len() == 0 {
		fa.Addr = tcpip.AddrFrom4([4]byte{127, 0, 0, 1})
	}

	terr := s.ep.Connect(fa)

	if terr == nil {
		return 0
	}

	if _, ok := terr.(*tcpip.ErrConnectStarted); !ok {
		return tcpipErrno(terr)
	}

	if f.nonblock() {
		return EINPROGRESS
	}

	e := t.blockOn(&s.q, func() bool {
		return s.Poll(f)&(POLLOUT|POLLERR|POLLHUP) != 0
	}, s.deadline(false), true)

	if e != 0 {
		if e == ETIMEDOUT {
			return EINPROGRESS
		}
		return e
	}

	if lerr := s.ep.LastError(); lerr != nil {
		return tcpipErrno(lerr)
	}

	if _, rerr := s.ep.GetRemoteAddress(); rerr != nil {
		return ECONNREFUSED
	}

	return 0
}

func (s *inetSocket) sockname() []byte {
	fa, _ := s.ep.GetLocalAddress()
	return marshalInetAddr(fa)
}

func (s *inetSocket) peername() ([]byte, Errno) {
	fa, err := s.ep.GetRemoteAddress()

	if err != nil {
		return nil, ENOTCONN
	}

	return marshalInetAddr(fa), 0
}

func (s *inetSocket) sendto(t *Task, f *File, buf []byte, flags int, addr []byte) (int, Errno) {
	var opts tcpip.WriteOptions

	if len(addr) > 0 && s.stype == SOCK_DGRAM {
		fa, err := parseInetAddr(addr)

		if err != 0 {
			return 0, err
		}

		opts.To = &fa
	}

	nonblock := f.nonblock() || flags&MSG_DONTWAIT != 0
	written := 0

	for {
		var r bytes.Reader
		r.Reset(buf[written:])

		n, err := s.ep.Write(&r, opts)
		written += int(n)

		if err == nil {
			if s.stype != SOCK_STREAM || written == len(buf) {
				return written, 0
			}

			continue
		}

		if _, ok := err.(*tcpip.ErrWouldBlock); !ok {
			if written > 0 {
				return written, 0
			}

			e := tcpipErrno(err)

			if e == EPIPE && flags&MSG_NOSIGNAL == 0 {
				t.signal(SIGPIPE, SigInfo{Signo: SIGPIPE})
			}

			return 0, e
		}

		if nonblock {
			if written > 0 {
				return written, 0
			}

			return 0, EAGAIN
		}

		e := t.blockOn(&s.q, func() bool {
			return s.Poll(f)&(POLLOUT|POLLERR|POLLHUP) != 0
		}, s.deadline(false), true)

		if e != 0 {
			if written > 0 {
				return written, 0
			}

			if e == ETIMEDOUT {
				e = EAGAIN
			}

			return 0, e
		}
	}
}

func (s *inetSocket) recvfrom(t *Task, f *File, buf []byte, flags int) (int, []byte, Errno) {
	nonblock := f.nonblock() || flags&MSG_DONTWAIT != 0

	for {
		w := tcpip.SliceWriter(buf)

		res, err := s.ep.Read(&w, tcpip.ReadOptions{
			Peek:           flags&MSG_PEEK != 0,
			NeedRemoteAddr: s.stype == SOCK_DGRAM,
		})

		if err == nil {
			var from []byte

			if s.stype == SOCK_DGRAM {
				from = marshalInetAddr(res.RemoteAddr)
			}

			n := res.Count

			if flags&MSG_TRUNC != 0 && s.stype == SOCK_DGRAM {
				n = res.Total
			}

			return n, from, 0
		}

		switch err.(type) {
		case *tcpip.ErrWouldBlock:
		case *tcpip.ErrClosedForReceive:
			return 0, nil, 0
		case *tcpip.ErrNotConnected:
			if s.stype == SOCK_STREAM {
				return 0, nil, ENOTCONN
			}
		default:
			return 0, nil, tcpipErrno(err)
		}

		if len(buf) == 0 {
			return 0, nil, 0
		}

		if nonblock {
			return 0, nil, EAGAIN
		}

		e := t.blockOn(&s.q, func() bool {
			return s.Poll(f)&(POLLIN|POLLERR|POLLHUP|POLLRDHUP) != 0
		}, s.deadline(true), true)

		if e != 0 {
			if e == ETIMEDOUT {
				e = EAGAIN
			}
			return 0, nil, e
		}
	}
}

func (s *inetSocket) setsockopt(level int, opt int, val []byte) Errno {
	so := s.ep.SocketOptions()

	switch level {
	case SOL_SOCKET:
		switch opt {
		case SO_REUSEADDR:
			so.SetReuseAddress(boolOpt(val))
		case SO_REUSEPORT:
			so.SetReusePort(boolOpt(val))
		case SO_KEEPALIVE:
			so.SetKeepAlive(boolOpt(val))
		case SO_BROADCAST:
			so.SetBroadcast(boolOpt(val))
		case SO_RCVTIMEO:
			s.rcvtimeo = sockTimeout(val)
		case SO_SNDTIMEO:
			s.sndtimeo = sockTimeout(val)
		case SO_LINGER:
			// struct linger
			var l struct{ Onoff, Linger int32 }

			if _, err := binary.Decode(val, binary.LittleEndian, &l); err == nil {
				so.SetLinger(tcpip.LingerOption{
					Enabled: l.Onoff != 0,
					Timeout: time.Duration(l.Linger) * time.Second,
				})
			}
		}
	case IPPROTO_TCP:
		switch opt {
		case TCP_NODELAY:
			so.SetDelayOption(!boolOpt(val))
		}
	}

	// unsupported options are accepted silently
	return 0
}

func (s *inetSocket) getsockopt(level int, opt int) ([]byte, Errno) {
	so := s.ep.SocketOptions()

	b2u := func(v bool) []byte {
		if v {
			return u32bytes(1)
		}
		return u32bytes(0)
	}

	switch level {
	case SOL_SOCKET:
		switch opt {
		case SO_TYPE:
			return u32bytes(uint32(s.stype)), 0
		case SO_DOMAIN:
			return u32bytes(AF_INET), 0
		case SO_PROTOCOL:
			return u32bytes(uint32(s.proto)), 0
		case SO_ERROR:
			return u32bytes(uint32(tcpipErrno(s.ep.LastError()))), 0
		case SO_REUSEADDR:
			return b2u(so.GetReuseAddress()), 0
		case SO_KEEPALIVE:
			return b2u(so.GetKeepAlive()), 0
		case SO_ACCEPTCONN:
			return b2u(s.listening), 0
		case SO_SNDBUF:
			return u32bytes(uint32(so.GetSendBufferSize())), 0
		case SO_RCVBUF:
			return u32bytes(uint32(so.GetReceiveBufferSize())), 0
		}
	case IPPROTO_TCP:
		switch opt {
		case TCP_NODELAY:
			return b2u(!so.GetDelayOption()), 0
		}
	}

	return u32bytes(0), 0
}

func (s *inetSocket) shutdown(how int) Errno {
	var flags tcpip.ShutdownFlags

	switch how {
	case SHUT_RD:
		flags = tcpip.ShutdownRead
	case SHUT_WR:
		flags = tcpip.ShutdownWrite
	case SHUT_RDWR:
		flags = tcpip.ShutdownRead | tcpip.ShutdownWrite
	default:
		return EINVAL
	}

	if err := s.ep.Shutdown(flags); err != nil {
		return tcpipErrno(err)
	}

	return 0
}
