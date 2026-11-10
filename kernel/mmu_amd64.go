// Copyright (c) The eggshell Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

package kernel

import (
	"github.com/usbarmory/eggshell/monitor"
)

// x86-64 4-level paging
//
// The Go runtime (ring 0) and user processes (ring 3) share each process
// address space: every process page table carries supervisor-only copies of
// the TamaGo identity mappings for the kernel range [kernelStart, kernelEnd),
// so that system calls and interrupts do not require a CR3 switch. User
// mappings are not allowed to overlap this range.
const ptLevels = 4

// page table entry bits
const (
	pteP  = 1 << 0
	pteRW = 1 << 1
	pteUS = 1 << 2
	pteA  = 1 << 5
	pteD  = 1 << 6
	ptePS = 1 << 7
	pteNX = 1 << 63

	pteAddrMask = 0x000ffffffffff000
)

const (
	// kernel range (Go runtime, DMA, process page pool, devices)
	kernelStart = 0x10000000
	kernelEnd   = 0x180000000

	// TamaGo page tables (see tamago/amd64/amd64.h)
	kernelPDPT = 0xa000
	kernelPDT  = 0xb000

	// physical memory range holding the TamaGo page tables read above
	kernelTablesStart = kernelPDPT
	kernelTablesSize  = 2 * PageSize
)

func pteValid(pte uint64) bool {
	return pte&pteP != 0
}

func pteIsTable(pte uint64) bool {
	return pte&ptePS == 0
}

func pteAddr(pte uint64) uint64 {
	return pte & pteAddrMask
}

func pteWritable(pte uint64) bool {
	return pte&pteRW != 0
}

func pteReadable(pte uint64) bool {
	return pte&pteP != 0
}

func mkTable(pa uint64) uint64 {
	return pa | pteP | pteRW | pteUS
}

func pteWithAddr(pte uint64, pa uint64) uint64 {
	return pa | pte&^pteAddrMask
}

func mkLeaf(pa uint64, prot int) uint64 {
	pte := pa | pteP | pteUS | pteA | pteD

	if prot&PROT_WRITE != 0 {
		pte |= pteRW
	}

	if prot&PROT_EXEC == 0 {
		pte |= pteNX
	}

	return pte
}

func kernelTable(level int, idx uint64) bool {
	return false
}

// archReserved returns whether a user range overlaps the kernel range.
func archReserved(start, end uint64) bool {
	return start < kernelEnd && end > kernelStart
}

// rootReg returns the CR3 register value for the address space.
func (mm *MM) rootReg() uint64 {
	return mm.root
}

// kernel mapping entries, captured from the TamaGo page tables before any
// process page table is in use (afterwards low memory is not mapped)
var (
	kernelPDPTEntries [kernelEnd >> 30]uint64
	kernelPDTEntries  [512]uint64
	kernelSnapshot    bool
)

func snapshotKernelTables() {
	for i := range kernelPDPTEntries {
		kernelPDPTEntries[i] = ptep(kernelPDPT + uint64(i)*8).load()
	}

	for i := range kernelPDTEntries {
		kernelPDTEntries[i] = ptep(kernelPDT + uint64(i)*8).load()
	}

	kernelSnapshot = true
}

// archInit installs the kernel mappings in a new address space.
func (mm *MM) archInit() Errno {
	if !kernelSnapshot {
		snapshotKernelTables()
	}

	pdpt, ok := allocPage()

	if !ok {
		return ENOMEM
	}

	pdt, ok := allocPage()

	if !ok {
		putPage(pdpt)
		return ENOMEM
	}

	// PML4[0] -> process PDPT
	ptep(mm.root).store(mkTable(pdpt))

	// PDPT[0] -> process PDT, PDPT[1:6] kernel 1GB pages
	ptep(pdpt).store(mkTable(pdt))

	for i := uint64(1); i < kernelEnd>>30; i++ {
		ptep(pdpt + i*8).store(kernelPDPTEntries[i])
	}

	// PDT[128:512] kernel 2MB pages (0x10000000 - 0x40000000)
	for i := uint64(kernelStart >> 21); i < 512; i++ {
		ptep(pdt + i*8).store(kernelPDTEntries[i])
	}

	return 0
}

func (mm *MM) archInitFork() Errno {
	return mm.archInit()
}

// rootInUse returns whether page tables are in use by any processor, which
// prevents them from being freed.
func rootInUse(root uint64) bool {
	return monitor.RootInUse(root)
}
