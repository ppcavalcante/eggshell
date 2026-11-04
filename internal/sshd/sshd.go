// Copyright (c) The eggshell Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

// Package sshd implements an SSH server meant to run within a Linux compatible
// POSIX process environment implemented as a TamaGo unikernel.
//
// Sessions run shells and commands as processes, on a pseudo terminal when
// requested, within the kernel userspace.
//
// The host key is /etc/ssh/ssh_host_ed25519_key if present, otherwise one is
// generated at each boot.
//
// Clients authenticate with a key listed in /root/.ssh/authorized_keys, if
// present, otherwise no authentication is required.
package sshd

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/usbarmory/eggshell/kernel"
)

const (
	hostKeyPath        = "/etc/ssh/ssh_host_ed25519_key"
	authorizedKeysPath = "/root/.ssh/authorized_keys"

	motd  = "/etc/motd"
	shell = "/bin/bash"
	home  = "/root"
	sftp  = "/usr/libexec/sftp-server"

	timeout = 30 * time.Second
)

var env = []string{
	"HOME=" + home,
	"PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
	"SHELL=" + shell,
	"USER=root",
	"LOGNAME=root",
}

// Start starts the SSH server on the argument TCP port.
func Start(port uint16) (err error) {
	signer, err := hostKey()

	if err != nil {
		return
	}

	l, err := kernel.Listen(port)

	if err != nil {
		return
	}

	log.Printf("sshd: listening on port %d, host key %s", port, ssh.FingerprintSHA256(signer.PublicKey()))

	go func() {
		for {
			conn, err := l.Accept()

			if err != nil {
				log.Printf("sshd: %v", err)
				return
			}

			go serve(conn, signer)
		}
	}()

	return
}

func hostKey() (ssh.Signer, error) {
	if pem, err := kernel.ReadFile(hostKeyPath); err == nil {
		return ssh.ParsePrivateKey(pem)
	}

	_, key, err := ed25519.GenerateKey(rand.Reader)

	if err != nil {
		return nil, err
	}

	return ssh.NewSignerFromKey(key)
}

func config(signer ssh.Signer) *ssh.ServerConfig {
	cfg := &ssh.ServerConfig{}
	cfg.AddHostKey(signer)

	var keys [][]byte
	buf, _ := kernel.ReadFile(authorizedKeysPath)

	for len(buf) > 0 {
		key, _, _, rest, err := ssh.ParseAuthorizedKey(buf)

		if err != nil {
			break
		}

		keys = append(keys, key.Marshal())
		buf = rest
	}

	if len(keys) == 0 {
		cfg.NoClientAuth = true
		return cfg
	}

	cfg.PublicKeyCallback = func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		for _, k := range keys {
			if bytes.Equal(k, key.Marshal()) {
				return nil, nil
			}
		}

		return nil, fmt.Errorf("unknown public key")
	}

	return cfg
}

func serve(conn net.Conn, signer ssh.Signer) {
	t := time.AfterFunc(timeout, func() { conn.Close() })
	sc, chans, reqs, err := ssh.NewServerConn(conn, config(signer))
	t.Stop()

	if err != nil {
		log.Printf("sshd: %s: %v", conn.RemoteAddr(), err)
		conn.Close()
		return
	}

	log.Printf("sshd: %s: connected (%s)", sc.RemoteAddr(), sc.ClientVersion())

	go ssh.DiscardRequests(reqs)

	for nc := range chans {
		if nc.ChannelType() != "session" {
			nc.Reject(ssh.UnknownChannelType, "unsupported channel type")
			continue
		}

		ch, reqs, err := nc.Accept()

		if err != nil {
			continue
		}

		go (&session{ch: ch, env: env}).serve(reqs)
	}
}

type session struct {
	ch  ssh.Channel
	env []string
	pty *kernel.PTY

	started bool
}

func (s *session) serve(reqs <-chan *ssh.Request) {
	defer func() {
		if s.pty != nil {
			s.pty.Close()
		}

		if !s.started {
			s.ch.Close()
		}
	}()

	for req := range reqs {
		ok := s.handle(req)

		if req.WantReply {
			req.Reply(ok, nil)
		}
	}
}

func (s *session) handle(req *ssh.Request) bool {
	payload := req.Payload

	switch req.Type {
	case "pty-req":
		var r struct {
			Term          string
			Cols, Rows    uint32
			Width, Height uint32
			Modes         string
		}

		if ssh.Unmarshal(payload, &r) != nil || s.pty != nil || s.started {
			return false
		}

		pty, err := kernel.OpenPTY()

		if err != nil {
			return false
		}

		pty.SetSize(int(r.Cols), int(r.Rows))
		s.pty = pty
		s.env = append(s.env, "TERM="+r.Term)
	case "window-change":
		var r struct {
			Cols, Rows    uint32
			Width, Height uint32
		}

		if ssh.Unmarshal(payload, &r) != nil || s.pty == nil {
			return false
		}

		s.pty.SetSize(int(r.Cols), int(r.Rows))
	case "env":
		var r struct{ Name, Value string }

		if ssh.Unmarshal(payload, &r) != nil {
			return false
		}

		s.env = append(s.env, r.Name+"="+r.Value)
	case "shell":
		if s.pty != nil {
			if msg, err := kernel.ReadFile(motd); err == nil {
				s.ch.Write(bytes.ReplaceAll(msg, []byte("\n"), []byte("\r\n")))
			}
		}

		return s.start(shell, "--login")
	case "exec":
		var r struct{ Command string }

		if ssh.Unmarshal(payload, &r) != nil {
			return false
		}

		return s.start(shell, "-c", r.Command)
	case "subsystem":
		var r struct{ Name string }

		if ssh.Unmarshal(payload, &r) != nil || r.Name != "sftp" {
			return false
		}

		return s.start(sftp)
	default:
		return false
	}

	return true
}

// start starts the session process, one per session.
func (s *session) start(args ...string) bool {
	if s.started {
		return false
	}

	proc, err := kernel.Exec(&kernel.Cmd{
		Args: args,
		Env:  s.env,
		Dir:  home,
		PTY:  s.pty,
	})

	if err != nil {
		log.Printf("sshd: %s: %v", args[0], err)
		return false
	}

	s.started = true

	go s.run(proc)

	return true
}

// run relays the process streams and exit status.
func (s *session) run(proc *kernel.Process) {
	var wg sync.WaitGroup

	if pty := s.pty; pty != nil {
		go io.Copy(pty, s.ch)

		wg.Go(func() {
			io.Copy(s.ch, pty)
		})
	} else {
		// copy and close, so that the process is not left blocked
		relay := func(w io.Writer, r io.ReadCloser) {
			io.Copy(w, r)
			r.Close()
			wg.Done()
		}

		go func() {
			io.Copy(proc.Stdin, s.ch)
			proc.Stdin.Close()
		}()

		wg.Add(2)
		go relay(s.ch, proc.Stdout)
		go relay(s.ch.Stderr(), proc.Stderr)
	}

	code, sig := proc.Wait()

	if s.pty != nil {
		// hang up, its output is still consumed
		s.pty.Close()
	}

	wg.Wait()

	if sig != 0 {
		code = 128 + sig
	}

	status := make([]byte, 4)
	binary.BigEndian.PutUint32(status, uint32(code))

	s.ch.SendRequest("exit-status", false, status)
	s.ch.Close()
}
