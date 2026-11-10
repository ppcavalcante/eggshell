// Copyright (c) The eggshell Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

package kernel

import (
	"encoding/binary"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// memory protection flags
const (
	PROT_NONE  = 0
	PROT_READ  = 1
	PROT_WRITE = 2
	PROT_EXEC  = 4
)

// mmap flags
const (
	MAP_SHARED          = 0x01
	MAP_PRIVATE         = 0x02
	MAP_FIXED           = 0x10
	MAP_ANONYMOUS       = 0x20
	MAP_GROWSDOWN       = 0x100
	MAP_DENYWRITE       = 0x800
	MAP_NORESERVE       = 0x4000
	MAP_POPULATE        = 0x8000
	MAP_STACK           = 0x20000
	MAP_FIXED_NOREPLACE = 0x100000
)

// User virtual address space layout (within the lower 256GB, common to Sv39
// and x86-64 4-level paging)
const (
	userMax    = 0x4000000000
	stackTop   = 0x3fffffe000
	stackSize  = 8 << 20
	mmapTop    = 0x3f00000000
	mmapBottom = 0x1000000000
)

// VMA represents a virtual memory area.
type VMA struct {
	start uint64
	end   uint64
	prot  int

	// shared pages are not copied on fork
	shared bool

	// file backed contents
	file    *Inode
	fileOff uint64
	fileLen uint64

	// shared file mapping, whose pages are written back to the file
	// when unmapped or synced (see writeBack)
	backing *Inode

	name string
}

// MM represents a process address space.
type MM struct {
	root uint64
	vmas []*VMA

	brkStart uint64
	brk      uint64

	users int
}

func newMM() (mm *MM, err Errno) {
	root, ok := allocPage()

	if !ok {
		return nil, ENOMEM
	}

	mm = &MM{root: root, users: 1}

	if err = mm.archInit(); err != 0 {
		mm.release()
		return nil, err
	}

	return
}

// walk returns the leaf PTE for the argument virtual address (0 if none),
// intermediate tables are allocated as needed if alloc is set.
func (mm *MM) walk(va uint64, alloc bool) ptep {
	table := mm.root

	for level := ptLevels - 1; level > 0; level-- {
		idx := (va >> (PageShift + 9*uint(level))) & 0x1ff
		pte := ptep(table + idx*8)

		if !pteValid(pte.load()) {
			if !alloc {
				return 0
			}

			pa, ok := allocPage()

			if !ok {
				return 0
			}

			pte.store(mkTable(pa))
		} else if !pteIsTable(pte.load()) {
			// large kernel mapping
			return 0
		}

		table = pteAddr(pte.load())
	}

	idx := (va >> PageShift) & 0x1ff

	return ptep(table + idx*8)
}

func (mm *MM) mapPage(va uint64, pa uint64, prot int) bool {
	pte := mm.walk(va, true)

	if pte == 0 {
		return false
	}

	if pteValid(pte.load()) {
		putPage(pteAddr(pte.load()))
	}

	pte.store(mkLeaf(pa, prot))

	return true
}

// forEachPTE invokes fn for each valid leaf PTE within [start, end).
func (mm *MM) forEachPTE(start, end uint64, fn func(va uint64, pte ptep)) {
	mm.forEachLevel(mm.root, ptLevels-1, 0, start, end, fn)
}

func (mm *MM) forEachLevel(table uint64, level int, base, start, end uint64, fn func(va uint64, pte ptep)) {
	span := uint64(1) << (PageShift + 9*uint(level))
	entries := physPage(table)

	for i := range uint64(512) {
		va := base + i*span

		if va+span <= start || va >= end {
			continue
		}

		pte := binary.LittleEndian.Uint64(entries[i*8:])

		switch {
		case !pteValid(pte):
		case level == 0:
			fn(va, ptep(table+i*8))
		case pteIsTable(pte):
			mm.forEachLevel(pteAddr(pte), level-1, va, start, end, fn)
		}
	}
}

func (mm *MM) unmapPages(start, end uint64) {
	mm.forEachPTE(start, end, func(va uint64, pte ptep) {
		putPage(pteAddr(pte.load()))
		pte.store(0)
	})

	invalidateTLB(mm)
}

func (mm *MM) freeTables(table uint64, level int) {
	if level > 0 {
		entries := physPage(table)

		for i := range uint64(512) {
			pte := binary.LittleEndian.Uint64(entries[i*8:])

			if pteValid(pte) && pteIsTable(pte) && !kernelTable(level, i) {
				mm.freeTables(pteAddr(pte), level-1)
			}
		}
	}

	putPage(table)
}

// page table roots of released address spaces, still in use by a processor
var zombieRoots []uint64

// release drops a reference to the address space, freeing its resources
// when unused.
func (mm *MM) release() {
	mm.users--

	if mm.users > 0 {
		return
	}

	mm.writeBack(0, userMax)
	mm.unmapPages(0, userMax)

	// page tables still loaded by a processor are freed by a later release
	zombieRoots = slices.DeleteFunc(append(zombieRoots, mm.root), func(root uint64) bool {
		if rootInUse(root) {
			return false
		}

		mm.freeTables(root, ptLevels-1)
		return true
	})

	for _, v := range mm.vmas {
		v.unrefFile()
	}

	mm.vmas = nil
	mm.root = 0
}

// findVMA returns the area containing the argument address.
func (mm *MM) findVMA(va uint64) *VMA {
	i := sort.Search(len(mm.vmas), func(i int) bool { return mm.vmas[i].end > va })

	if i < len(mm.vmas) && mm.vmas[i].start <= va {
		return mm.vmas[i]
	}

	return nil
}

func (v *VMA) unrefFile() {
	if v.file != nil {
		v.file.unref()
	}
}

func (mm *MM) insertVMA(v *VMA) {
	if v.file != nil {
		v.file.ref()
	}

	i := sort.Search(len(mm.vmas), func(i int) bool {
		return mm.vmas[i].start >= v.start
	})

	mm.vmas = append(mm.vmas, nil)
	copy(mm.vmas[i+1:], mm.vmas[i:])
	mm.vmas[i] = v
}

// split ensures that no area crosses the argument address.
func (mm *MM) split(va uint64) {
	v := mm.findVMA(va)

	if v == nil || v.start == va {
		return
	}

	n := *v
	n.start = va

	off := va - v.start

	if v.fileLen > off {
		n.fileOff = v.fileOff + off
		n.fileLen = v.fileLen - off
	} else {
		n.file = nil
		n.fileOff = 0
		n.fileLen = 0
		n.backing = nil
	}

	v.end = va

	if v.fileLen > off {
		v.fileLen = off
	}

	mm.insertVMA(&n)
}

// overlaps returns whether any area overlaps [start, end).
func (mm *MM) overlaps(start, end uint64) bool {
	for _, v := range mm.vmas {
		if v.start < end && v.end > start {
			return true
		}
	}
	return false
}

// removeRange unmaps [start, end).
func (mm *MM) removeRange(start, end uint64) {
	mm.writeBack(start, end)
	mm.split(start)
	mm.split(end)

	vmas := mm.vmas[:0]

	for _, v := range mm.vmas {
		if v.start >= start && v.end <= end {
			v.unrefFile()
			continue
		}
		vmas = append(vmas, v)
	}

	clear(mm.vmas[len(vmas):])
	mm.vmas = vmas
	mm.unmapPages(start, end)
}

// writeBack copies the resident pages of shared file mappings within
// [start, end) to their file, up to its current size.
//
// Shared file mappings are not coherent with read(2) and write(2) while
// mapped: their pages are private copies of the file contents, written back
// on munmap(2), msync(2), mprotect(2) to PROT_NONE and process exit.
func (mm *MM) writeBack(start, end uint64) {
	for _, v := range mm.vmas {
		if v.backing == nil || v.end <= start || v.start >= end {
			continue
		}

		mm.forEachPTE(max(start, v.start), min(end, v.end), func(va uint64, pte ptep) {
			off := v.fileOff + (va - v.start)

			if size := uint64(v.backing.data.size); off < size {
				page := physPage(pteAddr(pte.load()))
				v.backing.data.writeAt(page[:min(size-off, PageSize)], int64(off))
			}
		})

		v.backing.mtime = now()
	}
}

// findFree returns the highest free range of the argument size.
func (mm *MM) findFree(size uint64) (addr uint64, err Errno) {
	gapEnd := uint64(mmapTop)

	for _, v := range slices.Backward(mm.vmas) {

		if v.start >= gapEnd {
			continue
		}

		if v.end <= gapEnd && gapEnd-v.end >= size && gapEnd-size >= mmapBottom {
			return gapEnd - size, 0
		}

		if v.start < gapEnd {
			gapEnd = v.start
		}
	}

	if gapEnd-mmapBottom >= size {
		return gapEnd - size, 0
	}

	return 0, ENOMEM
}

// mmap creates a new mapping.
func (mm *MM) mmap(addr, length uint64, prot, flags int, file *Inode, backing *Inode, off uint64, name string) (uint64, Errno) {
	if length == 0 {
		return 0, EINVAL
	}

	length = pageUp(length)

	if length >= userMax {
		return 0, ENOMEM
	}

	switch {
	case flags&MAP_FIXED != 0:
		if addr&PageMask != 0 || addr+length > userMax {
			return 0, EINVAL
		}

		if archReserved(addr, addr+length) {
			return 0, ENOMEM
		}

		mm.removeRange(addr, addr+length)
	case flags&MAP_FIXED_NOREPLACE != 0:
		if addr&PageMask != 0 || addr+length > userMax || archReserved(addr, addr+length) {
			return 0, EINVAL
		}

		if mm.overlaps(addr, addr+length) {
			return 0, EEXIST
		}
	case addr != 0 && addr&PageMask == 0 && addr+length <= mmapTop && addr >= PageSize &&
		!mm.overlaps(addr, addr+length) && !archReserved(addr, addr+length):
		// honor hint
	default:
		var err Errno

		if addr, err = mm.findFree(length); err != 0 {
			return 0, err
		}
	}

	v := &VMA{
		start:  addr,
		end:    addr + length,
		prot:   prot,
		shared: flags&MAP_SHARED != 0,
		name:   name,
	}

	if file != nil {
		v.file = file
		v.fileOff = off

		if size := uint64(file.data.size); off < size {
			v.fileLen = size - off
		}

		// file mappings are populated on fault, shared ones are
		// written back to the file (see writeBack)
		v.shared = false
		v.backing = backing
	}

	mm.insertVMA(v)

	if flags&MAP_POPULATE != 0 {
		for va := v.start; va < v.end; va += PageSize {
			mm.fault(va, 0)
		}
	}

	return addr, 0
}

// munmap removes mappings within the argument range.
func (mm *MM) munmap(addr uint64, length uint64) Errno {
	if addr&PageMask != 0 || length == 0 {
		return EINVAL
	}

	end := pageUp(addr + length)

	if end > userMax || end < addr {
		return EINVAL
	}

	mm.removeRange(addr, end)

	return 0
}

// mprotect changes protection of mappings within the argument range.
func (mm *MM) mprotect(addr uint64, length uint64, prot int) Errno {
	if addr&PageMask != 0 {
		return EINVAL
	}

	end := pageUp(addr + length)

	// the whole range must be mapped
	for va := addr; va < end; {
		v := mm.findVMA(va)

		if v == nil {
			return ENOMEM
		}

		va = v.end
	}

	if prot == PROT_NONE {
		mm.writeBack(addr, end)
	}

	mm.split(addr)
	mm.split(end)

	for _, v := range mm.vmas {
		if v.start >= addr && v.end <= end {
			v.prot = prot
		}
	}

	mm.forEachPTE(addr, end, func(va uint64, pte ptep) {
		if prot == PROT_NONE {
			putPage(pteAddr(pte.load()))
			pte.store(0)
		} else {
			pte.store(mkLeaf(pteAddr(pte.load()), prot))
		}
	})

	invalidateTLB(mm)

	return 0
}

// mremap resizes (and possibly moves) a mapping.
func (mm *MM) mremap(old uint64, oldSize uint64, newSize uint64, flags int, newAddr uint64) (uint64, Errno) {
	const (
		MREMAP_MAYMOVE = 1
		MREMAP_FIXED   = 2
	)

	if old&PageMask != 0 || newSize == 0 {
		return 0, EINVAL
	}

	oldSize = pageUp(oldSize)
	newSize = pageUp(newSize)

	v := mm.findVMA(old)

	if v == nil || old+oldSize > v.end {
		return 0, EFAULT
	}

	if flags&MREMAP_FIXED == 0 {
		if newSize <= oldSize {
			if newSize < oldSize {
				mm.removeRange(old+newSize, old+oldSize)
			}
			return old, 0
		}

		// try to grow in place
		if old+oldSize == v.end &&
			!mm.overlaps(v.end, old+newSize) &&
			old+newSize <= mmapTop &&
			!archReserved(v.end, old+newSize) {
			v.end = old + newSize
			return old, 0
		}

		if flags&MREMAP_MAYMOVE == 0 {
			return 0, ENOMEM
		}

		var err Errno

		if newAddr, err = mm.findFree(newSize); err != 0 {
			return 0, err
		}
	} else {
		if flags&MREMAP_MAYMOVE == 0 || newAddr&PageMask != 0 {
			return 0, EINVAL
		}

		if newAddr < old+oldSize && old < newAddr+newSize {
			return 0, EINVAL
		}

		mm.removeRange(newAddr, newAddr+newSize)
	}

	// move pages
	mm.split(old)
	mm.split(old + oldSize)

	v = mm.findVMA(old)
	n := *v
	n.start = newAddr
	n.end = newAddr + newSize

	type move struct{ va, pte uint64 }
	var moves []move

	mm.forEachPTE(old, old+oldSize, func(va uint64, pte ptep) {
		moves = append(moves, move{va - old + newAddr, pte.load()})
		pte.store(0)
	})

	// insert first, keeping a file reference
	mm.insertVMA(&n)
	mm.removeRange(old, old+oldSize)

	for _, m := range moves {
		pte := mm.walk(m.va, true)

		if pte == 0 {
			return 0, ENOMEM
		}

		pte.store(m.pte)
	}

	invalidateTLB(mm)

	return newAddr, 0
}

// setBrk implements the brk system call.
func (mm *MM) setBrk(addr uint64) uint64 {
	if addr < mm.brkStart || addr >= mmapBottom {
		return mm.brk
	}

	oldEnd := pageUp(mm.brk)
	newEnd := pageUp(addr)

	switch {
	case newEnd > oldEnd:
		if mm.overlaps(oldEnd, newEnd) || archReserved(oldEnd, newEnd) {
			return mm.brk
		}

		if v := mm.findVMA(oldEnd - 1); v != nil && v.name == "[heap]" && v.end == oldEnd {
			v.end = newEnd
		} else {
			mm.insertVMA(&VMA{start: oldEnd, end: newEnd, prot: PROT_READ | PROT_WRITE, name: "[heap]"})
		}
	case newEnd < oldEnd:
		mm.removeRange(newEnd, oldEnd)
	}

	mm.brk = addr

	return mm.brk
}

// fault resolves a page fault for the argument address, access is a
// combination of PROT_* flags.
func (mm *MM) fault(va uint64, access int) Errno {
	v := mm.findVMA(va)

	if v == nil {
		return EFAULT
	}

	if access&^v.prot != 0 || v.prot == PROT_NONE {
		return EFAULT
	}

	page := pageDown(va)
	pte := mm.walk(page, true)

	if pte == 0 {
		return ENOMEM
	}

	if pteValid(pte.load()) {
		// present with insufficient permissions
		if access&PROT_WRITE != 0 && !pteWritable(pte.load()) {
			return EFAULT
		}

		return 0
	}

	pa, ok := allocPage()

	if !ok {
		return ENOMEM
	}

	if off := page - v.start; off < v.fileLen {
		n := min(v.fileLen-off, PageSize)

		v.file.data.readAt(physPage(pa)[:n], int64(v.fileOff+off))
	}

	pte.store(mkLeaf(pa, v.prot))

	return 0
}

// translate returns the physical address for a user virtual address,
// resolving faults as needed.
func (mm *MM) translate(va uint64, write bool) (uint64, Errno) {
	if va >= userMax {
		return 0, EFAULT
	}

	pte := mm.walk(va, false)

	if pte == 0 || !pteValid(pte.load()) || (write && !pteWritable(pte.load())) || !pteReadable(pte.load()) {
		access := PROT_READ

		if write {
			access |= PROT_WRITE
		}

		if err := mm.fault(va, access); err != 0 {
			return 0, err
		}

		if pte = mm.walk(va, false); pte == 0 || !pteValid(pte.load()) {
			return 0, EFAULT
		}

		if write && !pteWritable(pte.load()) {
			return 0, EFAULT
		}
	}

	return pteAddr(pte.load()) | (va & PageMask), 0
}

// copyIn copies user memory into the argument buffer.
func (mm *MM) copyIn(va uint64, buf []byte) Errno {
	for len(buf) > 0 {
		pa, err := mm.translate(va, false)

		if err != 0 {
			return EFAULT
		}

		n := copy(buf, physPage(pageDown(pa))[pa&PageMask:])
		buf = buf[n:]
		va += uint64(n)
	}

	return 0
}

// copyOut copies the argument buffer to user memory.
func (mm *MM) copyOut(va uint64, buf []byte) Errno {
	for len(buf) > 0 {
		pa, err := mm.translate(va, true)

		if err != 0 {
			return EFAULT
		}

		n := copy(physPage(pageDown(pa))[pa&PageMask:], buf)
		buf = buf[n:]
		va += uint64(n)
	}

	return 0
}

// zeroOut clears user memory.
func (mm *MM) zeroOut(va uint64, n uint64) Errno {
	for n > 0 {
		pa, err := mm.translate(va, true)

		if err != 0 {
			return EFAULT
		}

		p := physPage(pageDown(pa))[pa&PageMask:]

		if uint64(len(p)) > n {
			p = p[:n]
		}

		clear(p)
		n -= uint64(len(p))
		va += uint64(len(p))
	}

	return 0
}

// readString reads a NUL terminated string from user memory.
func (mm *MM) readString(va uint64, max int) (string, Errno) {
	var sb strings.Builder

	for sb.Len() < max {
		pa, err := mm.translate(va, false)

		if err != 0 {
			return "", EFAULT
		}

		p := physPage(pageDown(pa))[pa&PageMask:]

		for i, c := range p {
			if c == 0 {
				sb.Write(p[:i])
				return sb.String(), 0
			}
		}

		sb.Write(p)
		va += uint64(len(p))
	}

	return "", ENAMETOOLONG
}

func (mm *MM) readU64(va uint64) (uint64, Errno) {
	var b [8]byte
	err := mm.copyIn(va, b[:])
	return binary.LittleEndian.Uint64(b[:]), err
}

func (mm *MM) readU32(va uint64) (uint32, Errno) {
	var b [4]byte
	err := mm.copyIn(va, b[:])
	return binary.LittleEndian.Uint32(b[:]), err
}

func (mm *MM) writeU64(va uint64, val uint64) Errno {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], val)
	return mm.copyOut(va, b[:])
}

func (mm *MM) writeU32(va uint64, val uint32) Errno {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], val)
	return mm.copyOut(va, b[:])
}

// fork returns a copy of the address space.
func (mm *MM) fork() (n *MM, err Errno) {
	root, ok := allocPage()

	if !ok {
		return nil, ENOMEM
	}

	n = &MM{
		root:     root,
		brkStart: mm.brkStart,
		brk:      mm.brk,
		users:    1,
	}

	if err = n.archInitFork(); err != 0 {
		n.release()
		return nil, err
	}

	for _, v := range mm.vmas {
		c := *v
		n.insertVMA(&c)

		mm.forEachPTE(v.start, v.end, func(va uint64, pte ptep) {
			if err != 0 {
				return
			}

			pa := pteAddr(pte.load())

			if !v.shared {
				var ok bool
				src := pa

				if pa, ok = allocPage(); !ok {
					err = ENOMEM
					return
				}

				copy(physPage(pa), physPage(src))
			} else {
				getPage(pa)
			}

			npte := n.walk(va, true)

			if npte == 0 {
				putPage(pa)
				err = ENOMEM
				return
			}

			npte.store(pteWithAddr(pte.load(), pa))
		})
	}

	if err != 0 {
		n.release()
		return nil, err
	}

	return
}

// maps returns the /proc/<pid>/maps representation.
func (mm *MM) maps() string {
	var sb strings.Builder

	for _, v := range mm.vmas {
		perm := []byte("---p")

		if v.prot&PROT_READ != 0 {
			perm[0] = 'r'
		}
		if v.prot&PROT_WRITE != 0 {
			perm[1] = 'w'
		}
		if v.prot&PROT_EXEC != 0 {
			perm[2] = 'x'
		}
		if v.shared {
			perm[3] = 's'
		}

		fmt.Fprintf(&sb, "%08x-%08x %s %08x 00:00 0          %s\n", v.start, v.end, perm, v.fileOff, v.name)
	}

	return sb.String()
}

// rss returns the number of resident pages.
func (mm *MM) rss() (n uint64) {
	mm.forEachPTE(0, userMax, func(va uint64, pte ptep) {
		n++
	})

	return
}
