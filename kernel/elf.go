// Copyright (c) The eggshell Authors. All Rights Reserved.
//
// Use of this source code is governed by the license
// that can be found in the LICENSE file.

package kernel

import (
	"debug/elf"
	"encoding/binary"
)

const (
	AT_NULL   = 0
	AT_PHDR   = 3
	AT_PHENT  = 4
	AT_PHNUM  = 5
	AT_PAGESZ = 6
	AT_BASE   = 7
	AT_FLAGS  = 8
	AT_ENTRY  = 9
	AT_UID    = 11
	AT_EUID   = 12
	AT_GID    = 13
	AT_EGID   = 14
	AT_HWCAP  = 16
	AT_CLKTCK = 17
	AT_SECURE = 23
	AT_RANDOM = 25
	AT_HWCAP2 = 26
	AT_EXECFN = 31
)

type elfImage struct {
	entry uint64
	phdr  uint64
	phent uint64
	phnum uint64
	end   uint64
	bias  uint64

	// program interpreter (dynamic loader) path
	interp string
}

// loadELF maps an executable in the argument address space, position
// independent images are loaded at the argument bias.
func loadELF(mm *MM, file *Inode, pie uint64) (img *elfImage, err Errno) {
	var hdr elf.Header64

	size := uint64(file.data.size)
	data := file.data.bytes(0, binary.Size(hdr))

	if _, e := binary.Decode(data, binary.LittleEndian, &hdr); e != nil || string(hdr.Ident[:4]) != elf.ELFMAG {
		return nil, ENOEXEC
	}

	if elf.Class(hdr.Ident[elf.EI_CLASS]) != elf.ELFCLASS64 ||
		elf.Data(hdr.Ident[elf.EI_DATA]) != elf.ELFDATA2LSB {
		return nil, ENOEXEC
	}

	typ := elf.Type(hdr.Type)

	if elf.Machine(hdr.Machine) != elfMachine ||
		(typ != elf.ET_EXEC && typ != elf.ET_DYN) {
		return nil, ENOEXEC
	}

	var bias uint64

	if typ == elf.ET_DYN {
		bias = pie
	}

	img = &elfImage{
		entry: hdr.Entry + bias,
		bias:  bias,
		phent: uint64(hdr.Phentsize),
		phnum: uint64(hdr.Phnum),
	}

	phoff := hdr.Phoff

	if img.phent < uint64(binary.Size(elf.Prog64{})) ||
		img.phent*img.phnum > 65536 ||
		phoff+img.phent*img.phnum > size {
		return nil, ENOEXEC
	}

	phdrs := file.data.bytes(int64(phoff), int(img.phent*img.phnum))

	for i := uint64(0); i < img.phnum; i++ {
		var ph elf.Prog64

		binary.Decode(phdrs[i*img.phent:], binary.LittleEndian, &ph)

		ptype := elf.ProgType(ph.Type)
		flags := elf.ProgFlag(ph.Flags)
		off := ph.Off
		vaddr := ph.Vaddr + bias
		filesz := ph.Filesz
		memsz := ph.Memsz

		switch ptype {
		case elf.PT_INTERP:
			if off+filesz > size || filesz < 2 || filesz > 4096 {
				return nil, ENOEXEC
			}

			img.interp = string(file.data.bytes(int64(off), int(filesz-1)))
		case elf.PT_PHDR:
			img.phdr = vaddr
		case elf.PT_LOAD:
			if memsz == 0 {
				continue
			}

			if off+filesz > size || filesz > memsz {
				return nil, ENOEXEC
			}

			if img.phdr == 0 && phoff >= off && phoff < off+filesz {
				img.phdr = vaddr + (phoff - off)
			}

			var prot int

			if flags&elf.PF_R != 0 {
				prot |= PROT_READ
			}
			if flags&elf.PF_W != 0 {
				prot |= PROT_WRITE
			}
			if flags&elf.PF_X != 0 {
				prot |= PROT_EXEC
			}

			start := pageDown(vaddr)
			end := pageUp(vaddr + memsz)
			lead := vaddr - start

			if lead > off {
				return nil, ENOEXEC
			}

			mm.removeRange(start, end)

			mm.insertVMA(&VMA{
				start:   start,
				end:     end,
				prot:    prot,
				file:    file,
				fileOff: off - lead,
				fileLen: lead + filesz,
				name:    "[exe]",
			})

			if end > img.end {
				img.end = end
			}
		}
	}

	if img.end == 0 {
		return nil, ENOEXEC
	}

	return
}

// setupStack creates the process stack with arguments, environment and
// auxiliary vector, it returns the initial stack pointer.
func setupStack(mm *MM, img *elfImage, base uint64, path string, argv, envp []string) (sp uint64, err Errno) {
	mm.insertVMA(&VMA{
		start: stackTop - stackSize,
		end:   stackTop,
		prot:  PROT_READ | PROT_WRITE,
		name:  "[stack]",
	})

	pos := uint64(stackTop)

	push := func(s string) uint64 {
		pos -= uint64(len(s) + 1)
		b := append([]byte(s), 0)

		if e := mm.copyOut(pos, b); e != 0 {
			err = e
		}

		return pos
	}

	execfn := push(path)

	argvAddr := make([]uint64, len(argv))
	for i, s := range argv {
		argvAddr[i] = push(s)
	}

	envAddr := make([]uint64, len(envp))
	for i, s := range envp {
		envAddr[i] = push(s)
	}

	pos -= 16
	pos &^= 15
	random := pos

	var rnd [16]byte
	getRandom(rnd[:])
	mm.copyOut(random, rnd[:])

	auxv := []uint64{
		AT_PHDR, img.phdr,
		AT_PHENT, img.phent,
		AT_PHNUM, img.phnum,
		AT_PAGESZ, PageSize,
		AT_BASE, base,
		AT_FLAGS, 0,
		AT_ENTRY, img.entry,
		AT_UID, 0,
		AT_EUID, 0,
		AT_GID, 0,
		AT_EGID, 0,
		AT_HWCAP, hwcap,
		AT_HWCAP2, hwcap2(),
		AT_CLKTCK, 100,
		AT_SECURE, 0,
		AT_RANDOM, random,
		AT_EXECFN, execfn,
	}

	if vdso := mm.mapVDSO(); vdso != 0 {
		auxv = append(auxv, AT_SYSINFO_EHDR, vdso)
	}

	auxv = append(auxv, AT_NULL, 0)

	var vec []uint64

	vec = append(vec, uint64(len(argv)))
	vec = append(vec, argvAddr...)
	vec = append(vec, 0)
	vec = append(vec, envAddr...)
	vec = append(vec, 0)
	vec = append(vec, auxv...)

	pos -= uint64(len(vec) * 8)
	pos &^= 15

	buf, _ := binary.Append(nil, binary.LittleEndian, vec)

	if e := mm.copyOut(pos, buf); e != 0 {
		err = e
	}

	return pos, err
}
