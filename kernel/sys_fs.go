// Copyright (c) The eggshell Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

package kernel

import (
	"encoding/binary"
)

const (
	SEEK_SET  = 0
	SEEK_CUR  = 1
	SEEK_END  = 2
	SEEK_DATA = 3
	SEEK_HOLE = 4

	F_DUPFD         = 0
	F_GETFD         = 1
	F_SETFD         = 2
	F_GETFL         = 3
	F_SETFL         = 4
	F_GETLK         = 5
	F_SETLK         = 6
	F_SETLKW        = 7
	F_SETOWN        = 8
	F_GETOWN        = 9
	F_DUPFD_CLOEXEC = 1030
	F_SETPIPE_SZ    = 1031
	F_GETPIPE_SZ    = 1032
	FD_CLOEXEC      = 1

	DT_UNKNOWN = 0
	DT_FIFO    = 1
	DT_CHR     = 2
	DT_DIR     = 4
	DT_BLK     = 6
	DT_REG     = 8
	DT_LNK     = 10
	DT_SOCK    = 12

	UTIME_NOW  = (1 << 30) - 1
	UTIME_OMIT = (1 << 30) - 2

	// maximum transfer size for a single read/write chunk
	ioChunk = 1 << 20
)

func (t *Task) absPathAt(dirfd int, p string) (string, Errno) {
	return t.absPath(dirfd, p)
}

// statxTimestamp represents the struct statx_timestamp.
type statxTimestamp struct {
	Sec  int64
	Nsec uint32
	_    int32
}

func newStatxTimestamp(ns int64) statxTimestamp {
	return statxTimestamp{Sec: ns / 1e9, Nsec: uint32(ns % 1e9)}
}

// statx represents the struct statx.
type statx struct {
	Mask           uint32
	Blksize        uint32
	Attributes     uint64
	Nlink          uint32
	UID            uint32
	GID            uint32
	Mode           uint16
	_              uint16
	Ino            uint64
	Size           uint64
	Blocks         uint64
	AttributesMask uint64
	Atime          statxTimestamp
	Btime          statxTimestamp
	Ctime          statxTimestamp
	Mtime          statxTimestamp
	RdevMajor      uint32
	RdevMinor      uint32
	DevMajor       uint32
	DevMinor       uint32
	_              [112]byte
}

// Bytes converts the statx structure to byte array format.
func (s *statx) Bytes() []byte {
	b, _ := binary.Append(nil, binary.LittleEndian, s)
	return b
}

func statxBytes(ino *Inode) []byte {
	size := ino.size()

	s := &statx{
		Mask:      0x7ff, // STATX_BASIC_STATS
		Blksize:   PageSize,
		Nlink:     ino.nlink,
		UID:       ino.uid,
		GID:       ino.gid,
		Mode:      uint16(ino.mode),
		Ino:       ino.ino,
		Size:      uint64(size),
		Blocks:    uint64((size + 511) / 512),
		Atime:     newStatxTimestamp(ino.atime),
		Btime:     newStatxTimestamp(ino.ctime),
		Ctime:     newStatxTimestamp(ino.ctime),
		Mtime:     newStatxTimestamp(ino.mtime),
		RdevMajor: uint32(ino.rdev >> 8),
		RdevMinor: uint32(ino.rdev & 0xff),
		DevMajor:  uint32(ino.dev >> 8),
		DevMinor:  uint32(ino.dev & 0xff),
	}

	return s.Bytes()
}

func dtype(ino *Inode) byte {
	switch ino.mode & S_IFMT {
	case S_IFDIR:
		return DT_DIR
	case S_IFREG:
		return DT_REG
	case S_IFLNK:
		return DT_LNK
	case S_IFCHR:
		return DT_CHR
	case S_IFIFO:
		return DT_FIFO
	case S_IFSOCK:
		return DT_SOCK
	}
	return DT_UNKNOWN
}

// inodeOf returns the inode describing an open file.
func (f *File) stat() *Inode {
	if f.inode != nil {
		return f.inode
	}
	return f.anonInode()
}

// iovec represents the struct iovec.
type iovec struct {
	Base uint64
	Len  uint64
}

func (t *Task) iovecs(addr uint64, cnt uint64) ([]iovec, Errno) {
	if cnt > 1024 {
		return nil, EINVAL
	}

	iov := make([]iovec, cnt)
	b := make([]byte, binary.Size(iov))

	if err := t.proc.mm.copyIn(addr, b); err != 0 {
		return nil, err
	}

	binary.Decode(b, binary.LittleEndian, iov)

	return iov, 0
}

// gather reads the contents of an I/O vector, up to ioChunk bytes, in a
// buffer to be released with putIOBuf.
func (t *Task) gather(iov []iovec) ([]byte, *[]byte, Errno) {
	buf, p := getIOBuf(min(iovLen(iov), ioChunk))
	off := uint64(0)

	for _, v := range iov {
		n := min(v.Len, uint64(len(buf))-off)

		if err := t.proc.mm.copyIn(v.Base, buf[off:off+n]); err != 0 {
			putIOBuf(p)
			return nil, nil, err
		}

		off += n
	}

	return buf, p, 0
}

// scatter writes data to an I/O vector.
func (t *Task) scatter(iov []iovec, data []byte) Errno {
	for _, v := range iov {
		if len(data) == 0 {
			break
		}

		n := min(v.Len, uint64(len(data)))

		if err := t.proc.mm.copyOut(v.Base, data[:n]); err != 0 {
			return err
		}

		data = data[n:]
	}

	return 0
}

func iovLen(iov []iovec) (n uint64) {
	for _, v := range iov {
		n += v.Len
	}
	return
}

func (t *Task) readv(f *File, iov []iovec, off int64, pos bool) (uint64, Errno) {
	if !f.readable() {
		return fail(EBADF)
	}

	buf, p := getIOBuf(min(iovLen(iov), ioChunk))
	defer putIOBuf(p)

	var r int
	var err Errno

	if pos {
		p, ok := f.ops.(positional)

		if !ok {
			return fail(ESPIPE)
		}

		r, err = p.ReadAt(t, f, buf, off)
	} else {
		r, err = f.ops.Read(t, f, buf)
	}

	if err != 0 {
		return fail(err)
	}

	if err = t.scatter(iov, buf[:r]); err != 0 {
		return fail(err)
	}

	return ok(r)
}

func (t *Task) writev(f *File, iov []iovec, off int64, pos bool) (uint64, Errno) {
	if !f.writable() {
		return fail(EBADF)
	}

	pf, positional_ := f.ops.(positional)

	if pos && !positional_ {
		return fail(ESPIPE)
	}

	buf, p := getIOBuf(min(iovLen(iov), ioChunk))
	defer putIOBuf(p)

	total := 0

	// write the vector in chunks of up to ioChunk bytes
	for len(iov) > 0 {
		n := 0

		for len(iov) > 0 && n < len(buf) {
			c := min(iov[0].Len, uint64(len(buf)-n))

			if err := t.proc.mm.copyIn(iov[0].Base, buf[n:n+int(c)]); err != 0 {
				if total > 0 {
					return ok(total)
				}
				return fail(err)
			}

			n += int(c)
			iov[0].Base += c
			iov[0].Len -= c

			if iov[0].Len == 0 {
				iov = iov[1:]
			}
		}

		if n == 0 {
			break
		}

		var w int
		var err Errno

		if pos {
			w, err = pf.WriteAt(t, f, buf[:n], off+int64(total))
		} else {
			w, err = f.ops.Write(t, f, buf[:n])
		}

		if err != 0 {
			if total > 0 {
				break
			}
			return fail(err)
		}

		total += w

		if w < n {
			break
		}
	}

	return ok(total)
}

func (t *Task) getdents(f *File, addr uint64, count uint64) (uint64, Errno) {
	ino := f.inode

	if ino == nil || !ino.isDir() {
		return fail(ENOTDIR)
	}

	if f.pos == 0 || f.dirents == nil {
		f.dirents = append([]string{".", ".."}, ino.names()...)
	}

	var out []byte

	for f.pos < int64(len(f.dirents)) {
		name := f.dirents[f.pos]
		child := ino.child(name)

		if child == nil {
			f.pos++
			continue
		}

		reclen := (19 + len(name) + 1 + 7) &^ 7

		if uint64(len(out)+reclen) > count {
			if len(out) == 0 {
				return fail(EINVAL)
			}
			break
		}

		rec := make([]byte, reclen)
		binary.LittleEndian.PutUint64(rec[0:], child.ino)
		binary.LittleEndian.PutUint64(rec[8:], uint64(f.pos+1))
		binary.LittleEndian.PutUint16(rec[16:], uint16(reclen))
		rec[18] = dtype(child)
		copy(rec[19:], name)

		out = append(out, rec...)
		f.pos++
	}

	ino.atime = now()

	return uint64(int64(len(out))), t.proc.mm.copyOut(addr, out)
}

func (t *Task) fcntl(fd int, cmd int, arg uint64) (uint64, Errno) {
	files := t.proc.files
	f := files.get(fd)

	if f == nil {
		return fail(EBADF)
	}

	switch cmd {
	case F_DUPFD, F_DUPFD_CLOEXEC:
		if arg >= maxFDs {
			return fail(EINVAL)
		}

		nfd := files.install(f, int(arg), cmd == F_DUPFD_CLOEXEC)

		if nfd < 0 {
			return fail(Errno(-nfd))
		}

		return ok(nfd)
	case F_GETFD:
		if files.fds[fd].cloexec {
			return ok(FD_CLOEXEC)
		}
		return ok(0)
	case F_SETFD:
		files.fds[fd].cloexec = arg&FD_CLOEXEC != 0
		return ok(0)
	case F_GETFL:
		return ok(f.flags)
	case F_SETFL:
		const mask = O_APPEND | O_NONBLOCK | O_ASYNC | O_DIRECT | O_NOATIME
		f.flags = (f.flags &^ mask) | (int(arg) & mask)
		return ok(0)
	case F_GETLK:
		// struct flock: l_type (short) is never locked
		return uint64(int64(0)), t.proc.mm.copyOut(arg, []byte{2, 0})
	case F_SETLK, F_SETLKW:
		return ok(0)
	case F_SETOWN, F_GETOWN:
		return ok(0)
	case F_GETPIPE_SZ, F_SETPIPE_SZ:
		return ok(pipeSize)
	case 1033, 1034: // F_ADD_SEALS, F_GET_SEALS
		return ok(0)
	}

	return fail(EINVAL)
}

func (t *Task) ioctl(f *File, cmd uint64, arg uint64) (uint64, Errno) {
	switch cmd {
	case FIOCLEX, FIONCLEX:
		return ok(0)
	case FIONBIO:
		v, err := t.proc.mm.readU32(arg)

		if err != 0 {
			return fail(err)
		}

		if v != 0 {
			f.flags |= O_NONBLOCK
		} else {
			f.flags &^= O_NONBLOCK
		}

		return ok(0)
	}

	if i, ok := f.ops.(ioctler); ok {
		return i.Ioctl(t, f, cmd, arg)
	}

	if cmd == FIONREAD && f.inode != nil && f.inode.isReg() {
		n := max(f.inode.data.size-f.pos, 0)

		return 0, t.proc.mm.writeU32(arg, uint32(n))
	}

	return fail(ENOTTY)
}

func (t *Task) unlink(dirfd int, p string, flags int) Errno {
	dir, name, err := t.lookupParent(dirfd, p)

	if err != 0 {
		return err
	}

	if name == "." || name == ".." {
		return EINVAL
	}

	if dir.lookupFn != nil {
		return EPERM
	}

	ino := dir.children[name]

	if ino == nil {
		return ENOENT
	}

	if flags&AT_REMOVEDIR != 0 {
		if !ino.isDir() {
			return ENOTDIR
		}

		if len(ino.children) > 0 {
			return ENOTEMPTY
		}

		if ino == t.proc.fs.cwd && ino == rootInode {
			return EBUSY
		}
	} else if ino.isDir() {
		return EISDIR
	}

	dir.unlink(name)

	return 0
}

func (t *Task) rename(olddirfd int, oldp string, newdirfd int, newp string, flags uint64) Errno {
	const (
		RENAME_NOREPLACE = 1
		RENAME_EXCHANGE  = 2
	)

	odir, oname, err := t.lookupParent(olddirfd, oldp)

	if err != 0 {
		return err
	}

	ndir, nname, err := t.lookupParent(newdirfd, newp)

	if err != 0 {
		return err
	}

	if odir.lookupFn != nil || ndir.lookupFn != nil {
		return EXDEV
	}

	if oname == "." || oname == ".." || nname == "." || nname == ".." {
		return EBUSY
	}

	src := odir.children[oname]

	if src == nil {
		return ENOENT
	}

	dst := ndir.children[nname]

	if src == dst {
		return 0
	}

	// a directory cannot be moved into its own subtree
	if src.isDir() {
		for d := ndir; d != nil; d = d.parent {
			if d == src {
				return EINVAL
			}
		}
	}

	if flags&RENAME_EXCHANGE != 0 {
		if dst == nil {
			return ENOENT
		}

		odir.children[oname] = dst
		ndir.children[nname] = src

		if src.isDir() {
			src.parent, src.name = ndir, nname
		}

		if dst.isDir() {
			dst.parent, dst.name = odir, oname
		}

		return 0
	}

	if dst != nil {
		if flags&RENAME_NOREPLACE != 0 {
			return EEXIST
		}

		switch {
		case src.isDir() && !dst.isDir():
			return ENOTDIR
		case !src.isDir() && dst.isDir():
			return EISDIR
		case dst.isDir() && len(dst.children) > 0:
			return ENOTEMPTY
		}

		ndir.unlink(nname)
	}

	delete(odir.children, oname)

	if src.isDir() {
		odir.nlink--
		src.nlink--
	} else {
		src.nlink--
	}

	ndir.link(nname, src)
	src.ctime = now()

	return 0
}

func (t *Task) mknod(dirfd int, p string, mode uint32, rdev uint64) Errno {
	dir, name, err := t.lookupParent(dirfd, p)

	if err != 0 {
		return err
	}

	if dir.child(name) != nil {
		return EEXIST
	}

	if dir.lookupFn != nil {
		return EPERM
	}

	typ := mode & S_IFMT

	switch typ {
	case 0:
		typ = S_IFREG
	case S_IFREG, S_IFIFO, S_IFSOCK:
	case S_IFDIR:
		return EINVAL
	default:
		return EPERM
	}

	ino := newInode(typ | (mode &^ t.proc.fs.umask & 07777))
	ino.nlink = 0
	dir.link(name, ino)

	return 0
}

// statfs represents the struct statfs.
type statfs struct {
	Type    int64
	Bsize   int64
	Blocks  uint64
	Bfree   uint64
	Bavail  uint64
	Files   uint64
	Ffree   uint64
	Fsid    [2]int32
	Namelen int64
	Frsize  int64
	Flags   int64
	_       [4]int64
}

// Bytes converts the statfs structure to byte array format.
func (s *statfs) Bytes() []byte {
	b, _ := binary.Append(nil, binary.LittleEndian, s)
	return b
}

func (t *Task) statfs(addr uint64, ino *Inode) Errno {
	magic := int64(0x01021994) // TMPFS_MAGIC

	if ino.dev == 2 {
		magic = 0x9fa0 // PROC_SUPER_MAGIC
	}

	// file data shares the page pool with processes: the file system
	// size is its used pages plus the pages still available to it
	free := uint64(len(pages.free))

	if free > fileReserve {
		free -= fileReserve
	} else {
		free = 0
	}

	s := &statfs{
		Type:    magic,
		Bsize:   PageSize,
		Blocks:  filePages + free,
		Bfree:   free,
		Bavail:  free,
		Files:   1 << 20,
		Ffree:   1<<20 - nextIno,
		Namelen: 255,
		Frsize:  PageSize,
	}

	return t.proc.mm.copyOut(addr, s.Bytes())
}

func registerFSSyscalls() {
	reg(SYS_OPENAT, func(t *Task, a *[6]uint64) (uint64, Errno) {
		p, err := t.str(a[1])

		if err != 0 {
			return fail(err)
		}

		fd, err := t.open(int(int32(a[0])), p, int(a[2]), uint32(a[3]))

		return uint64(int64(fd)), err
	})

	reg(SYS_CLOSE, func(t *Task, a *[6]uint64) (uint64, Errno) {
		return 0, t.proc.files.close(int(int32(a[0])))
	})

	reg(SYS_CLOSE_RANGE, func(t *Task, a *[6]uint64) (uint64, Errno) {
		files := t.proc.files
		last := a[1]

		if last >= uint64(len(files.fds)) {
			last = uint64(len(files.fds)) - 1
		}

		for fd := a[0]; fd <= last && fd < uint64(len(files.fds)); fd++ {
			if files.fds[fd].f == nil {
				continue
			}

			if a[2]&4 != 0 { // CLOSE_RANGE_CLOEXEC
				files.fds[fd].cloexec = true
			} else {
				files.close(int(fd))
			}
		}

		return ok(0)
	})

	// read(2), write(2) and their positional and vector variants
	rw := func(write bool, vec bool, pos bool) func(t *Task, a *[6]uint64) (uint64, Errno) {
		return func(t *Task, a *[6]uint64) (uint64, Errno) {
			f, err := t.file(a[0])

			if err != 0 {
				return fail(err)
			}

			iov := []iovec{{Base: a[1], Len: a[2]}}

			if vec {
				if iov, err = t.iovecs(a[1], a[2]); err != 0 {
					return fail(err)
				}
			}

			off := int64(a[3])

			if pos && off < 0 {
				return fail(EINVAL)
			}

			if write {
				return t.writev(f, iov, off, pos)
			}

			return t.readv(f, iov, off, pos)
		}
	}

	reg(SYS_READ, rw(false, false, false))
	reg(SYS_WRITE, rw(true, false, false))
	reg(SYS_PREAD64, rw(false, false, true))
	reg(SYS_PWRITE64, rw(true, false, true))
	reg(SYS_READV, rw(false, true, false))
	reg(SYS_WRITEV, rw(true, true, false))
	reg(SYS_PREADV, rw(false, true, true))
	reg(SYS_PWRITEV, rw(true, true, true))

	reg(SYS_LSEEK, func(t *Task, a *[6]uint64) (uint64, Errno) {
		f, err := t.file(a[0])

		if err != 0 {
			return fail(err)
		}

		off := int64(a[1])

		if f.inode == nil ||
			f.inode.mode&S_IFMT == S_IFIFO ||
			f.inode.mode&S_IFMT == S_IFCHR && f.inode.device != nil {
			if f.inode != nil && f.inode.mode&S_IFMT == S_IFCHR {
				return ok(0)
			}
			return fail(ESPIPE)
		}

		var size int64

		if f.content != nil {
			size = int64(len(f.content))
		} else {
			size = f.inode.size()
		}

		var pos int64

		switch a[2] {
		case SEEK_SET:
			pos = off
		case SEEK_CUR:
			pos = f.pos + off
		case SEEK_END:
			pos = size + off
		case SEEK_DATA:
			if off >= size {
				return fail(ENXIO)
			}
			pos = off
		case SEEK_HOLE:
			if off >= size {
				return fail(ENXIO)
			}
			pos = size
		default:
			return fail(EINVAL)
		}

		if pos < 0 {
			return fail(EINVAL)
		}

		if f.inode.isDir() && pos == 0 {
			f.dirents = nil
		}

		f.pos = pos

		return uint64(pos), 0
	})

	reg(SYS_FSTAT, func(t *Task, a *[6]uint64) (uint64, Errno) {
		f, err := t.file(a[0])

		if err != 0 {
			return fail(err)
		}

		return uint64(int64(0)), t.proc.mm.copyOut(a[1], statBytes(f.stat()))
	})

	reg(SYS_NEWFSTATAT, func(t *Task, a *[6]uint64) (uint64, Errno) {
		p, err := t.str(a[1])

		if err != 0 {
			return fail(err)
		}

		ino, err := t.lookupAt(int(int32(a[0])), p, int(a[3]))

		if err != 0 {
			return fail(err)
		}

		return uint64(int64(0)), t.proc.mm.copyOut(a[2], statBytes(ino))
	})

	reg(SYS_STATX, func(t *Task, a *[6]uint64) (uint64, Errno) {
		p, err := t.str(a[1])

		if err != 0 {
			return fail(err)
		}

		ino, err := t.lookupAt(int(int32(a[0])), p, int(a[2]))

		if err != 0 {
			return fail(err)
		}

		return uint64(int64(0)), t.proc.mm.copyOut(a[4], statxBytes(ino))
	})

	reg(SYS_GETDENTS64, func(t *Task, a *[6]uint64) (uint64, Errno) {
		f, err := t.file(a[0])

		if err != 0 {
			return fail(err)
		}

		return t.getdents(f, a[1], a[2])
	})

	reg(SYS_MKDIRAT, func(t *Task, a *[6]uint64) (uint64, Errno) {
		p, err := t.str(a[1])

		if err != 0 {
			return fail(err)
		}

		dir, name, err := t.lookupParent(int(int32(a[0])), p)

		if err != 0 {
			return fail(err)
		}

		if dir.child(name) != nil || name == "." || name == ".." {
			return fail(EEXIST)
		}

		if dir.lookupFn != nil {
			return fail(EPERM)
		}

		ino := newInode(S_IFDIR | (uint32(a[2]) &^ t.proc.fs.umask & 07777))
		ino.nlink = 1
		dir.link(name, ino)

		return ok(0)
	})

	reg(SYS_MKNODAT, func(t *Task, a *[6]uint64) (uint64, Errno) {
		p, err := t.str(a[1])

		if err != 0 {
			return fail(err)
		}

		return 0, t.mknod(int(int32(a[0])), p, uint32(a[2]), a[3])
	})

	reg(SYS_UNLINKAT, func(t *Task, a *[6]uint64) (uint64, Errno) {
		p, err := t.str(a[1])

		if err != 0 {
			return fail(err)
		}

		return 0, t.unlink(int(int32(a[0])), p, int(a[2]))
	})

	reg(SYS_RENAMEAT2, func(t *Task, a *[6]uint64) (uint64, Errno) {
		oldp, err := t.str(a[1])

		if err != 0 {
			return fail(err)
		}

		newp, err := t.str(a[3])

		if err != 0 {
			return fail(err)
		}

		return 0, t.rename(int(int32(a[0])), oldp, int(int32(a[2])), newp, a[4])
	})

	reg(SYS_SYMLINKAT, func(t *Task, a *[6]uint64) (uint64, Errno) {
		target, err := t.str(a[0])

		if err != 0 {
			return fail(err)
		}

		p, err := t.str(a[2])

		if err != 0 {
			return fail(err)
		}

		dir, name, err := t.lookupParent(int(int32(a[1])), p)

		if err != 0 {
			return fail(err)
		}

		if dir.child(name) != nil {
			return fail(EEXIST)
		}

		ino := newInode(S_IFLNK | 0777)
		ino.target = target
		ino.nlink = 0
		dir.link(name, ino)

		return ok(0)
	})

	reg(SYS_LINKAT, func(t *Task, a *[6]uint64) (uint64, Errno) {
		oldp, err := t.str(a[1])

		if err != 0 {
			return fail(err)
		}

		newp, err := t.str(a[3])

		if err != 0 {
			return fail(err)
		}

		lflags := int(a[4] & AT_EMPTY_PATH)

		if a[4]&AT_SYMLINK_FOLLOW == 0 {
			lflags |= AT_SYMLINK_NOFOLLOW
		}

		src, err := t.lookupAt(int(int32(a[0])), oldp, lflags)

		if err != 0 {
			return fail(err)
		}

		if src.isDir() {
			return fail(EPERM)
		}

		dir, name, err := t.lookupParent(int(int32(a[2])), newp)

		if err != 0 {
			return fail(err)
		}

		if dir.child(name) != nil {
			return fail(EEXIST)
		}

		dir.link(name, src)

		return ok(0)
	})

	reg(SYS_READLINKAT, func(t *Task, a *[6]uint64) (uint64, Errno) {
		p, err := t.str(a[1])

		if err != 0 {
			return fail(err)
		}

		ino, err := t.lookup(int(int32(a[0])), p, false)

		if err != 0 {
			return fail(err)
		}

		if !ino.isLink() {
			return fail(EINVAL)
		}

		target := []byte(ino.linkTarget())

		if uint64(len(target)) > a[3] {
			target = target[:a[3]]
		}

		return uint64(int64(len(target))), t.proc.mm.copyOut(a[2], target)
	})

	access := func(t *Task, dirfd int, pathAddr uint64, mode uint64, flags uint64) (uint64, Errno) {
		p, err := t.str(pathAddr)

		if err != 0 {
			return fail(err)
		}

		ino, err := t.lookupAt(dirfd, p, int(flags))

		if err != 0 {
			return fail(err)
		}

		// X_OK
		if mode&1 != 0 && ino.mode&0111 == 0 && !ino.isDir() {
			return fail(EACCES)
		}

		return ok(0)
	}

	reg(SYS_FACCESSAT, func(t *Task, a *[6]uint64) (uint64, Errno) {
		return access(t, int(int32(a[0])), a[1], a[2], 0)
	})

	reg(SYS_FACCESSAT2, func(t *Task, a *[6]uint64) (uint64, Errno) {
		return access(t, int(int32(a[0])), a[1], a[2], a[3])
	})

	chmod := func(ino *Inode, mode uint64) {
		ino.mode = (ino.mode & S_IFMT) | uint32(mode&07777)
		ino.ctime = now()
	}

	reg(SYS_FCHMOD, func(t *Task, a *[6]uint64) (uint64, Errno) {
		f, err := t.file(a[0])

		if err != 0 {
			return fail(err)
		}

		chmod(f.stat(), a[1])

		return ok(0)
	})

	reg(SYS_FCHMODAT, func(t *Task, a *[6]uint64) (uint64, Errno) {
		p, err := t.str(a[1])

		if err != 0 {
			return fail(err)
		}

		ino, err := t.lookup(int(int32(a[0])), p, true)

		if err != 0 {
			return fail(err)
		}

		chmod(ino, a[2])

		return ok(0)
	})

	syscallTable[SYS_FCHMODAT2] = func(t *Task, a *[6]uint64) (uint64, Errno) {
		p, err := t.str(a[1])

		if err != 0 {
			return fail(err)
		}

		ino, err := t.lookupAt(int(int32(a[0])), p, int(a[3]))

		if err != 0 {
			return fail(err)
		}

		chmod(ino, a[2])

		return ok(0)
	}

	chown := func(ino *Inode, uid, gid uint64) {
		if uint32(uid) != 0xffffffff {
			ino.uid = uint32(uid)
		}

		if uint32(gid) != 0xffffffff {
			ino.gid = uint32(gid)
		}

		ino.ctime = now()
	}

	reg(SYS_FCHOWN, func(t *Task, a *[6]uint64) (uint64, Errno) {
		f, err := t.file(a[0])

		if err != 0 {
			return fail(err)
		}

		chown(f.stat(), a[1], a[2])

		return ok(0)
	})

	reg(SYS_FCHOWNAT, func(t *Task, a *[6]uint64) (uint64, Errno) {
		p, err := t.str(a[1])

		if err != 0 {
			return fail(err)
		}

		ino, err := t.lookupAt(int(int32(a[0])), p, int(a[4]))

		if err != 0 {
			return fail(err)
		}

		chown(ino, a[2], a[3])

		return ok(0)
	})

	reg(SYS_UTIMENSAT, func(t *Task, a *[6]uint64) (uint64, Errno) {
		var ino *Inode
		var err Errno

		if a[1] == 0 {
			f, err := t.file(a[0])

			if err != 0 {
				return fail(err)
			}

			ino = f.stat()
		} else {
			p, err := t.str(a[1])

			if err != 0 {
				return fail(err)
			}

			if ino, err = t.lookupAt(int(int32(a[0])), p, int(a[3])); err != 0 {
				return fail(err)
			}
		}

		tnow := now()
		times := [2]int64{tnow, tnow}

		if a[2] != 0 {
			b := make([]byte, 32)

			if err = t.proc.mm.copyIn(a[2], b); err != 0 {
				return fail(err)
			}

			for i := range 2 {
				sec := int64(binary.LittleEndian.Uint64(b[i*16:]))
				nsec := int64(binary.LittleEndian.Uint64(b[i*16+8:]))

				switch nsec {
				case UTIME_NOW:
				case UTIME_OMIT:
					times[i] = -1
				default:
					times[i] = sec*1e9 + nsec
				}
			}
		}

		if times[0] >= 0 {
			ino.atime = times[0]
		}

		if times[1] >= 0 {
			ino.mtime = times[1]
		}

		ino.ctime = tnow

		return ok(0)
	})

	reg(SYS_FTRUNCATE, func(t *Task, a *[6]uint64) (uint64, Errno) {
		f, err := t.file(a[0])

		if err != 0 {
			return fail(err)
		}

		if f.inode == nil || !f.writable() {
			return fail(EINVAL)
		}

		return 0, truncate(f.inode, int64(a[1]))
	})

	reg(SYS_TRUNCATE, func(t *Task, a *[6]uint64) (uint64, Errno) {
		p, err := t.str(a[0])

		if err != 0 {
			return fail(err)
		}

		ino, err := t.lookup(AT_FDCWD, p, true)

		if err != 0 {
			return fail(err)
		}

		return 0, truncate(ino, int64(a[1]))
	})

	reg(SYS_FALLOCATE, func(t *Task, a *[6]uint64) (uint64, Errno) {
		f, err := t.file(a[0])

		if err != 0 {
			return fail(err)
		}

		if f.inode == nil || !f.inode.isReg() {
			return fail(ENODEV)
		}

		end := int64(a[2] + a[3])

		if a[1] == 0 && end > f.inode.data.size {
			return 0, truncate(f.inode, end)
		}

		return ok(0)
	})

	for _, n := range []uint64{SYS_FSYNC, SYS_FDATASYNC, SYS_SYNC_FILE_RANGE, SYS_SYNCFS} {
		reg(n, func(t *Task, a *[6]uint64) (uint64, Errno) {
			if _, err := t.file(a[0]); err != 0 {
				return fail(err)
			}
			return ok(0)
		})
	}

	reg(SYS_FCNTL, func(t *Task, a *[6]uint64) (uint64, Errno) {
		return t.fcntl(int(int32(a[0])), int(a[1]), a[2])
	})

	reg(SYS_IOCTL, func(t *Task, a *[6]uint64) (uint64, Errno) {
		f, err := t.file(a[0])

		if err != 0 {
			return fail(err)
		}

		return t.ioctl(f, uint64(uint32(a[1])), a[2])
	})

	reg(SYS_DUP, func(t *Task, a *[6]uint64) (uint64, Errno) {
		f, err := t.file(a[0])

		if err != 0 {
			return fail(err)
		}

		fd := t.proc.files.install(f, 0, false)

		if fd < 0 {
			return fail(Errno(-fd))
		}

		return ok(fd)
	})

	reg(SYS_DUP3, func(t *Task, a *[6]uint64) (uint64, Errno) {
		f, err := t.file(a[0])

		if err != 0 {
			return fail(err)
		}

		nfd := int(int32(a[1]))

		if nfd < 0 || nfd >= maxFDs {
			return fail(EBADF)
		}

		if int32(a[0]) == int32(a[1]) {
			return fail(EINVAL)
		}

		t.proc.files.installAt(f, nfd, a[2]&O_CLOEXEC != 0)

		return ok(nfd)
	})

	reg(SYS_PIPE2, func(t *Task, a *[6]uint64) (uint64, Errno) {
		r, w := newPipe()

		r.flags |= int(a[1]) & O_NONBLOCK
		w.flags |= int(a[1]) & O_NONBLOCK

		cloexec := a[1]&O_CLOEXEC != 0
		files := t.proc.files

		rfd := files.install(r, 0, cloexec)
		r.decref()

		if rfd < 0 {
			w.decref()
			return fail(Errno(-rfd))
		}

		wfd := files.install(w, 0, cloexec)
		w.decref()

		if wfd < 0 {
			files.close(rfd)
			return fail(Errno(-wfd))
		}

		b, _ := binary.Append(nil, binary.LittleEndian, []int32{int32(rfd), int32(wfd)})

		if err := t.proc.mm.copyOut(a[0], b); err != 0 {
			files.close(rfd)
			files.close(wfd)
			return fail(err)
		}

		return ok(0)
	})

	reg(SYS_GETCWD, func(t *Task, a *[6]uint64) (uint64, Errno) {
		cwd := t.proc.fs.cwd

		if cwd.nlink == 0 && cwd != rootInode {
			return fail(ENOENT)
		}

		p := []byte(inodePath(cwd) + "\x00")

		if uint64(len(p)) > a[1] {
			return fail(ERANGE)
		}

		return uint64(int64(len(p))), t.proc.mm.copyOut(a[0], p)
	})

	reg(SYS_CHDIR, func(t *Task, a *[6]uint64) (uint64, Errno) {
		p, err := t.str(a[0])

		if err != 0 {
			return fail(err)
		}

		ino, err := t.lookup(AT_FDCWD, p, true)

		if err != 0 {
			return fail(err)
		}

		if !ino.isDir() {
			return fail(ENOTDIR)
		}

		t.proc.fs.cwd = ino

		return ok(0)
	})

	reg(SYS_FCHDIR, func(t *Task, a *[6]uint64) (uint64, Errno) {
		f, err := t.file(a[0])

		if err != 0 {
			return fail(err)
		}

		if f.inode == nil || !f.inode.isDir() {
			return fail(ENOTDIR)
		}

		t.proc.fs.cwd = f.inode

		return ok(0)
	})

	reg(SYS_STATFS, func(t *Task, a *[6]uint64) (uint64, Errno) {
		p, err := t.str(a[0])

		if err != 0 {
			return fail(err)
		}

		ino, err := t.lookup(AT_FDCWD, p, true)

		if err != 0 {
			return fail(err)
		}

		return uint64(int64(0)), t.statfs(a[1], ino)
	})

	reg(SYS_FSTATFS, func(t *Task, a *[6]uint64) (uint64, Errno) {
		f, err := t.file(a[0])

		if err != 0 {
			return fail(err)
		}

		return uint64(int64(0)), t.statfs(a[1], f.stat())
	})

	// unsupported for any file, callers (e.g. Go io.Copy) fall back to read
	// and write
	reg(SYS_SPLICE, func(t *Task, a *[6]uint64) (uint64, Errno) {
		return fail(EINVAL)
	})

	reg(SYS_SENDFILE, func(t *Task, a *[6]uint64) (uint64, Errno) {
		out, err := t.file(a[0])

		if err != 0 {
			return fail(err)
		}

		in, err := t.file(a[1])

		if err != 0 {
			return fail(err)
		}

		count := min(a[3], ioChunk)
		pos := in.pos

		if a[2] != 0 {
			off, err := t.proc.mm.readU64(a[2])

			if err != 0 {
				return fail(err)
			}

			pos = int64(off)
		}

		// avoid allocating more than the data left in a regular file
		if _, ok := in.ops.(regularFile); ok && in.content == nil {
			count = min(count, uint64(max(in.inode.size()-pos, 0)))
		}

		buf, p := getIOBuf(count)
		defer putIOBuf(p)

		var n int

		if a[2] != 0 {
			p, ok := in.ops.(positional)

			if !ok {
				return fail(ESPIPE)
			}

			if n, err = p.ReadAt(t, in, buf, pos); err != 0 {
				return fail(err)
			}

			t.proc.mm.writeU64(a[2], uint64(pos)+uint64(n))
		} else if n, err = in.ops.Read(t, in, buf); err != 0 {
			return fail(err)
		}

		if n == 0 {
			return ok(0)
		}

		w, err := out.ops.Write(t, out, buf[:n])

		return uint64(int64(w)), err
	})

	reg(SYS_MEMFD_CREATE, func(t *Task, a *[6]uint64) (uint64, Errno) {
		name, err := t.str(a[0])

		if err != 0 {
			return fail(err)
		}

		ino := newInode(S_IFREG | 0777)
		ino.nlink = 0

		f := newFile(regularFile{}, ino, O_RDWR, "/memfd:"+name)
		fd := t.proc.files.install(f, 0, a[1]&1 != 0)
		f.decref()

		if fd < 0 {
			return fail(Errno(-fd))
		}

		return ok(fd)
	})
}
