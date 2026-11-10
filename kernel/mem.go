// Copyright (c) The eggshell Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

package kernel

import (
	"encoding/binary"
	"fmt"

	"github.com/usbarmory/tamago/dma"
)

const (
	PageSize  = 4096
	PageShift = 12
	PageMask  = PageSize - 1
)

func pageDown(v uint64) uint64 {
	return v &^ PageMask
}

func pageUp(v uint64) uint64 {
	return (v + PageMask) &^ PageMask
}

// physical memory accessible to the kernel, outside Go runtime memory
var (
	pool    []byte
	ptables []byte
)

// physical page frame allocator
var pages struct {
	start uint64
	end   uint64
	free  []uint64
	refs  []uint16
	used  int
}

func initPages(start, end uint64) {
	start = pageUp(start)
	end = pageDown(end)

	pages.start = start
	pages.end = end

	pool = reserve(start, end-start)
	ptables = reserve(kernelTablesStart, kernelTablesSize)

	n := int((end - start) >> PageShift)
	pages.refs = make([]uint16, n)
	pages.free = make([]uint64, 0, n)

	// keep 1/16th of the pool for processes when files fill it
	fileReserve = uint64(n / 16)

	// allocate low addresses first
	for pa := end - PageSize; pa >= start && pa < end; pa -= PageSize {
		pages.free = append(pages.free, pa)
	}
}

func pageIndex(pa uint64) int {
	return int((pa - pages.start) >> PageShift)
}

// allocPage returns a zeroed physical page.
func allocPage() (pa uint64, ok bool) {
	if pa, ok = newPage(); ok {
		clear(physPage(pa))
	}

	return
}

// newPage returns a physical page with undefined contents, for callers which
// overwrite it.
func newPage() (pa uint64, ok bool) {
	n := len(pages.free)

	if n == 0 {
		return 0, false
	}

	pa = pages.free[n-1]
	pages.free = pages.free[:n-1]
	pages.refs[pageIndex(pa)] = 1
	pages.used++

	return pa, true
}

// getPage increments a page reference count.
func getPage(pa uint64) {
	pages.refs[pageIndex(pa)]++
}

// putPage decrements a page reference count, releasing it when unused.
func putPage(pa uint64) {
	i := pageIndex(pa)

	if pages.refs[i] == 0 {
		panic("putPage: free page")
	}

	pages.refs[i]--

	if pages.refs[i] == 0 {
		pages.free = append(pages.free, pa)
		pages.used--
	}
}

// reserve returns a slice mapping the argument physical memory range, which
// must be outside Go runtime memory.
func reserve(start uint64, size uint64) []byte {
	r, err := dma.NewRegion(uint(start), int(size), false)

	if err != nil {
		panic(err)
	}

	addr, buf := r.Reserve(int(size), PageSize)

	if uint64(addr) != start || uint64(len(buf)) != size {
		panic(fmt.Sprintf("physical memory reservation at %#x", start))
	}

	return buf
}

// physMem returns the slice mapping the argument physical memory range
// (the kernel uses identity mapped physical addressing).
func physMem(pa uint64, n uint64) []byte {
	if off := pa - pages.start; off < uint64(len(pool)) && n <= uint64(len(pool))-off {
		return pool[off : off+n : off+n]
	}

	return physMemTables(pa, n)
}

// physMemTables returns the slice mapping the argument physical memory range
// within the TamaGo page tables.
func physMemTables(pa uint64, n uint64) []byte {
	if off := pa - kernelTablesStart; off < kernelTablesSize && n <= kernelTablesSize-off {
		return ptables[off : off+n : off+n]
	}

	panic(fmt.Sprintf("invalid physical address %#x", pa))
}

// physPage returns the slice mapping a physical page.
func physPage(pa uint64) []byte {
	return physMem(pa, PageSize)
}

// ptep represents the physical address of a page table entry, 0 for none.
type ptep uint64

func (p ptep) load() uint64 {
	return binary.LittleEndian.Uint64(physMem(uint64(p), 8))
}

func (p ptep) store(pte uint64) {
	binary.LittleEndian.PutUint64(physMem(uint64(p), 8), pte)
}

func memTotal() uint64 {
	return pages.end - pages.start
}

func memFree() uint64 {
	return uint64(len(pages.free)) * PageSize
}
