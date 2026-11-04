Introduction
============

The [eggshell](https://github.com/usbarmory/eggshell) project is a
[TamaGo](https://github.com/usbarmory/tamago) unikernel that implements the
Linux ABI to run unmodified Linux executables: statically linked ones, such as C
programs or Go programs (including the Go toolchain), and programs dynamically
linked against the [musl](https://musl.libc.org) C library.

While TamaGo remains committed to promote pure Go unikernels, this project is
an experiment to address uses cases where the need for a full operating
environment is unavoidable, such as running the Go compiler.

> [!WARNING]
> This project is at its early stages and therefore experimental.

```
$ make qemu
eggshell microvm (2802 MHz) • tamago amd64 (go1.27.3) • 1264 MiB available
virtio-net 52:50:9a:89:c2:39 10.0.0.1/24 gw:10.0.0.2 gvisor
sshd: listening on port 22

  ___           ___ _        _ _
 | __|__ _ __ _/ __| |_  ___| | |
 | _|/ _` / _` \__ \ ' \/ -_) | |
 |___\__, \__, |___/_||_\___|_|_|
     |___/|___/

    Linux ABI on bare-metal Go

root@yolk:~# uname -a
eggshell yolk tamago go1.27.3 x86_64 BusyBox/TamaGo
root@yolk:~# ps x
PID   USER     TIME  COMMAND
    1 root      0:00 /bin/bash --login
    5 root      0:00 ps x
root@yolk:~# halt
init exited with status 138, powering off
```

Authors
=======

Andrea Barisani  
andrea@inversepath.com

Architecture
============

The applications run unprivileged while the Go runtime, acting as operating
system kernel, runs at the highest privilege level.

System calls trap into the Go runtime (in a manner similar to the
[GoTEE](https://github.com/usbarmory/GoTEE) execution model) and are serviced
following the **Linux system call ABI**.

Each user thread is a goroutine, blocking system calls map directly onto the Go
scheduler.

```
 ┌────────────────────────────────────────────── User mode ─┐
 │  bash   redis-server   busybox   u-root    go   ...      │ Linux ELF binaries
 └──────────────┬──────────────────────▲────────────────────┘
                │                      │
        syscall │   page fault, timer  │ iretq
                │                      │
 ┌──────────────▼──────────────────────┴────── Kernel mode ─┐
 │ monitor   GoTEE-style exec/return switch                 │
 │                                                          │
 │ kernel    Linux ABI syscalls, processes, signals, VFS,   │
 │           TTY, pipes, epoll, futex, sockets              │ TamaGo / Go runtime
 │                                                          │
 │ board     QEMU microvm                                   │
 │                                                          │
 │ network   virtio-net (go-net) + gVisor                   │
 └──────────────────────────────────────────────────────────┘
```

Security
========

The Go kernel does not use `unsafe` operations, so system call handling, which
processes untrusted data from user processes, is free of memory corruption bug
classes such as buffer overflows, use-after-free and type confusion.

A kernel panic while servicing a system call terminates the calling process
rather than the kernel. Processes are isolated from the kernel by hardware,
they run in ring 3 and the kernel is mapped supervisor-only in their address
space.

The trusted base beyond the kernel code is limited to the monitor assembly
(privilege switch), page table setup and the Go runtime, along with the TamaGo
package and network stack, with just 4 direct dependencies (see
[go.mod](https://github.com/usbarmory/eggshell/blob/main/go.mod)).

Finally, a Go kernel provides the opportunity to play with layers in an
unconventional way. The SSH server is pure Go code running in the kernel,
serving a pty to user mode processes, trading the privilege separation of a
conventional user mode daemon for a memory-safe network-facing parser.

Building the compiler
=====================

The [TamaGo compiler](https://github.com/usbarmory/tamago-go) is automatically
downloaded and compiled as a `go tool` by the `Makefile`.

Alternatively the `TAMAGO` environment variable can be overridden to use the
[latest binary release](https://github.com/usbarmory/tamago-go/releases/latest):

```sh
wget https://github.com/usbarmory/tamago-go/archive/refs/tags/latest.zip
unzip latest.zip
cd tamago-go-latest/src && ./all.bash
cd ../bin && export TAMAGO=`pwd`/go
```

Building and executing
======================

The example uses
[nix-build](https://nix.dev/manual/nix/2.18/command-ref/nix-build) for root
filesystem creation, and `qemu` for microvm execution.

```sh
go mod tidy
make rootfs
make kernel
make qemu
```

All [TamaGo amd64 targets](https://github.com/usbarmory/tamago#supported-amd64-targets) can be supported with minimal integration.

Boot arguments
--------------

The kernel command line can be passed through the `APPEND` variable:

```
make qemu APPEND="ip=10.0.0.1/24 gw=10.0.0.2"
```

| option           | description                                   |
|------------------|-----------------------------------------------|
| `-- cmd args`    | init process (default `/sbin/init`)           |
| `init=/path`     | init process without arguments                |
| `strace`         | trace system calls on the console             |
| `quiet`          | no kernel banner                              |
| `ip=10.0.0.1/24` | IPv4 address of the virtio-net interface      |
| `gw=10.0.0.2`    | default gateway                               |
| `hostname=name`  | host name                                     |
| `env=KEY=VALUE`  | additional init environment variable          |
| `ssh=22`         | SSH server port, `0` disables it              |

Networking
----------

The qemu target attaches the network device to the host `tap0` interface if
available, otherwise user networking is used with a predefined list of
forwarded ports (see `NETDEV_USER` within the `Makefile`).

On Linux systems tap0 can be configured as follows:

```sh
ip tuntap add dev tap0 mode tap group <your user group>
ip addr add 10.0.0.2/24 dev tap0
ip link set tap0 up
```

Running your own application
----------------------------

Any C program that builds with musl should run.

Dynamically linked executables work as well: the kernel maps the `PT_INTERP`
program interpreter, the rootfs ships musl's `/lib/ld-musl-<arch>.so.1` (add
any other shared library under `/lib` or `/usr/lib`).

The userland is built using
[nix-build](https://nix.dev/manual/nix/2.18/command-ref/nix-build), see
[nix/README.md](https://github.com/usbarmory/eggshell/blob/main/nix/README.md)
for instructions and options.

Current limitations
===================

* While TamaGo supports SMP operation, for now supervision of userspace code
  remains a single-core only affair.

* Only single user support, no isolation among users and any identity operation
  is a no-op.

* IPv4 only, no raw sockets, no `SCM_RIGHTS`, no `AF_UNIX` datagrams.

* `EPOLLET` is emulated as level triggered.

* Not implemented: `ptrace`, SysV IPC, `inotify`, `timerfd`, `signalfd`,
  POSIX timers (`timer_create`), mounts, namespaces.

* The filesystem is in-memory and therefore all modifications are volatile
  across reboots.

License
=======

eggshell | https://github.com/usbarmory/eggshell  
Copyright (c) The eggshell Authors. All Rights Reserved.

These source files are distributed under the BSD-style license found in the
[LICENSE](https://github.com/usbarmory/eggshell/blob/main/LICENSE) file.
