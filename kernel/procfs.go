// Copyright (c) The eggshell Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

package kernel

import (
	"fmt"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// current is the task servicing a system call (valid under klock).
var current *Task

func synthFile(fn func() []byte) *Inode {
	ino := newInode(S_IFREG | 0444)
	ino.dev = 2
	ino.contentFn = fn
	return ino
}

func synthText(fn func() string) *Inode {
	return synthFile(func() []byte { return []byte(fn()) })
}

func synthLink(fn func() string) *Inode {
	ino := newInode(S_IFLNK | 0777)
	ino.dev = 2
	ino.readlinkFn = fn
	return ino
}

func synthDir(parent *Inode, name string, entries func() map[string]func() *Inode) *Inode {
	ino := newInode(S_IFDIR | 0555)
	ino.dev = 2
	ino.parent = parent
	ino.name = name

	ino.listFn = func() []string {
		var names []string

		for n := range entries() {
			names = append(names, n)
		}

		sort.Strings(names)

		return names
	}

	ino.lookupFn = func(n string) *Inode {
		if fn, ok := entries()[n]; ok {
			child := fn()

			if child.isDir() {
				child.parent = ino
				child.name = n
			}

			return child
		}

		return nil
	}

	return ino
}

func initProc() {
	proc := synthDir(rootInode, "proc", procRootEntries)
	rootInode.children["proc"] = proc
	rootInode.nlink++

	sys := mkdirAll("/sys/kernel/mm/transparent_hugepage", 0755)
	enabled := newInode(S_IFREG | 0644)
	enabled.data.setBase([]byte("always madvise [never]\n"))
	sys.link("enabled", enabled)

	mkdirAll("/etc", 0755)

	if rootInode.child("etc").children["mtab"] == nil {
		l := newInode(S_IFLNK | 0777)
		l.target = "/proc/mounts"
		l.nlink = 0
		rootInode.child("etc").link("mtab", l)
	}
}

func procRootEntries() map[string]func() *Inode {
	m := map[string]func() *Inode{
		"self": func() *Inode {
			return synthLink(func() string {
				if current == nil {
					return "1"
				}
				return strconv.Itoa(current.proc.pid)
			})
		},
		"thread-self": func() *Inode {
			return synthLink(func() string {
				if current == nil {
					return "1/task/1"
				}
				return fmt.Sprintf("%d/task/%d", current.proc.pid, current.tid)
			})
		},
		"meminfo": func() *Inode { return synthText(meminfo) },
		"cpuinfo": func() *Inode { return synthText(cpuinfo) },
		"uptime": func() *Inode {
			return synthText(func() string {
				up := float64(nanotime()) / 1e9
				return fmt.Sprintf("%.2f %.2f\n", up, up)
			})
		},
		"loadavg": func() *Inode {
			return synthText(func() string {
				return fmt.Sprintf("0.00 0.00 0.00 1/%d %d\n", len(tasks), nextPID-1)
			})
		},
		"version": func() *Inode {
			return synthText(func() string {
				return fmt.Sprintf("eggshell (%s)\n", runtime.Version())
			})
		},
		"mounts": func() *Inode {
			return synthText(func() string {
				return "tmpfs / tmpfs rw 0 0\nproc /proc proc rw 0 0\n"
			})
		},
		"filesystems": func() *Inode {
			return synthText(func() string { return "nodev\ttmpfs\nnodev\tproc\n" })
		},
		"stat": func() *Inode {
			return synthText(func() string {
				s := "cpu  0 0 0 0 0 0 0 0 0 0\n"

				for i := range runtime.GOMAXPROCS(0) {
					s += fmt.Sprintf("cpu%d 0 0 0 0 0 0 0 0 0 0\n", i)
				}

				return s + fmt.Sprintf("btime %d\nprocesses %d\nprocs_running 1\nprocs_blocked 0\n", bootTime.Unix(), nextPID-1)
			})
		},
		"sys": func() *Inode {
			return synthDir(nil, "sys", procSysEntries)
		},
	}

	for pid := range procs {
		p := procs[pid]
		m[strconv.Itoa(pid)] = func() *Inode { return procPidDir(p, pid, false) }
	}

	return m
}

func procSysEntries() map[string]func() *Inode {
	file := func(fn func() string) func() *Inode {
		return func() *Inode {
			ino := synthText(fn)
			ino.mode = S_IFREG | 0644
			return ino
		}
	}

	dir := func(entries map[string]func() *Inode) func() *Inode {
		return func() *Inode {
			return synthDir(nil, "", func() map[string]func() *Inode { return entries })
		}
	}

	return map[string]func() *Inode{
		"kernel": dir(map[string]func() *Inode{
			"hostname":   file(func() string { return hostname + "\n" }),
			"domainname": file(func() string { return domainname + "\n" }),
			"ostype":     file(func() string { return "Linux\n" }),
			"osrelease":  file(func() string { return runtime.Version() + "\n" }),
			"pid_max":    file(func() string { return "32768\n" }),
			"random": dir(map[string]func() *Inode{
				"uuid": file(func() string {
					b := make([]byte, 16)
					getRandom(b)
					b[6] = b[6]&0x0f | 0x40
					b[8] = b[8]&0x3f | 0x80
					return fmt.Sprintf("%x-%x-%x-%x-%x\n", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
				}),
				"boot_id": file(func() string { return "00000000-0000-4000-8000-000000000000\n" }),
			}),
		}),
		"vm": dir(map[string]func() *Inode{
			"overcommit_memory": file(func() string { return "1\n" }),
		}),
		"net": dir(map[string]func() *Inode{
			"core": dir(map[string]func() *Inode{
				"somaxconn": file(func() string { return "4096\n" }),
			}),
			"ipv4": dir(map[string]func() *Inode{
				"tcp_max_syn_backlog": file(func() string { return "4096\n" }),
			}),
		}),
	}
}

func procState(p *Proc) string {
	switch {
	case p.zombie:
		return "Z"
	case p.stopped:
		return "T"
	case current != nil && current.proc == p:
		return "R"
	}
	return "S"
}

// procPidDir returns the /proc/<pid> directory, or /proc/<pid>/task/<tid> for
// a thread (which has no task subdirectory).
func procPidDir(p *Proc, id int, thread bool) *Inode {
	return synthDir(nil, strconv.Itoa(id), func() map[string]func() *Inode {
		m := map[string]func() *Inode{
			"cmdline": func() *Inode {
				return synthText(func() string {
					if len(p.argv) == 0 {
						return ""
					}
					return strings.Join(p.argv, "\x00") + "\x00"
				})
			},
			"environ": func() *Inode {
				return synthText(func() string {
					if len(p.envp) == 0 {
						return ""
					}
					return strings.Join(p.envp, "\x00") + "\x00"
				})
			},
			"comm": func() *Inode { return synthText(func() string { return p.comm + "\n" }) },
			"exe":  func() *Inode { return synthLink(func() string { return p.exe }) },
			"cwd": func() *Inode {
				return synthLink(func() string {
					if p.fs == nil {
						return "/"
					}
					return inodePath(p.fs.cwd)
				})
			},
			"root": func() *Inode { return synthLink(func() string { return "/" }) },
			"stat": func() *Inode { return synthText(func() string { return procStat(p) }) },
			"statm": func() *Inode {
				return synthText(func() string {
					var rss, size uint64

					if p.mm != nil {
						rss = p.mm.rss()

						for _, v := range p.mm.vmas {
							size += (v.end - v.start) / PageSize
						}
					}

					return fmt.Sprintf("%d %d 0 0 0 0 0\n", size, rss)
				})
			},
			"status": func() *Inode { return synthText(func() string { return procStatus(p) }) },
			"maps": func() *Inode {
				return synthText(func() string {
					if p.mm == nil {
						return ""
					}
					return p.mm.maps()
				})
			},
			"fd": func() *Inode {
				return synthDir(nil, "fd", func() map[string]func() *Inode {
					m := map[string]func() *Inode{}

					if p.files == nil {
						return m
					}

					for fd, e := range p.files.fds {
						if e.f == nil {
							continue
						}

						f := e.f
						m[strconv.Itoa(fd)] = func() *Inode {
							return synthLink(func() string { return fdLinkName(f) })
						}
					}

					return m
				})
			},
		}

		if thread {
			return m
		}

		m["task"] = func() *Inode {
			return synthDir(nil, "task", func() map[string]func() *Inode {
				m := map[string]func() *Inode{}

				for _, t := range p.threads {
					m[strconv.Itoa(t.tid)] = func() *Inode { return procPidDir(p, t.tid, true) }
				}

				return m
			})
		}

		return m
	})
}

func fdLinkName(f *File) string {
	switch f.ops.(type) {
	case *pipeEnd:
		return fmt.Sprintf("pipe:[%d]", f.stat().ino)
	case *epollFile:
		return "anon_inode:[eventpoll]"
	case *eventFD:
		return "anon_inode:[eventfd]"
	}

	if f.inode == nil {
		if _, ok := f.ops.(statter); ok && f.stat().mode&S_IFMT == S_IFSOCK {
			return fmt.Sprintf("socket:[%d]", f.stat().ino)
		}
	}

	return f.path
}

func procStat(p *Proc) string {
	ppid := 0

	if p.parent != nil {
		ppid = p.parent.pid
	}

	var vsize, rss uint64

	if p.mm != nil {
		rss = p.mm.rss()

		for _, v := range p.mm.vmas {
			vsize += v.end - v.start
		}
	}

	ticks := func(ns int64) int64 { return ns / 1e7 }

	return fmt.Sprintf("%d (%s) %s %d %d %d %d %d 0 0 0 0 0 %d 0 %d 0 20 0 %d 0 %d %d %d %s\n",
		p.pid, p.comm, procState(p), ppid, p.pgid, p.sid, 0x400, console.pgrp,
		ticks(p.utime), ticks(p.cutime), len(p.threads), ticks(p.start), vsize, rss,
		strings.TrimSpace(strings.Repeat("0 ", 27)))
}

func procStatus(p *Proc) string {
	ppid := 0

	if p.parent != nil {
		ppid = p.parent.pid
	}

	var rss uint64

	if p.mm != nil {
		rss = p.mm.rss() * 4
	}

	state := map[string]string{"R": "R (running)", "S": "S (sleeping)", "Z": "Z (zombie)", "T": "T (stopped)"}[procState(p)]

	return fmt.Sprintf("Name:\t%s\nUmask:\t%04o\nState:\t%s\nTgid:\t%d\nPid:\t%d\nPPid:\t%d\nUid:\t0\t0\t0\t0\nGid:\t0\t0\t0\t0\nVmRSS:\t%d kB\nThreads:\t%d\nSigPnd:\t%016x\n",
		p.comm, p.fs.umask, state, p.pid, p.pid, ppid, rss, len(p.threads), p.pending)
}

func meminfo() string {
	total := memTotal() / 1024
	free := memFree() / 1024
	files := filePages * PageSize / 1024

	return fmt.Sprintf("MemTotal:       %8d kB\nMemFree:        %8d kB\nMemAvailable:   %8d kB\nBuffers:               0 kB\nCached:         %8d kB\nSwapTotal:             0 kB\nSwapFree:              0 kB\nShmem:          %8d kB\n",
		total, free, free, files, files)
}
