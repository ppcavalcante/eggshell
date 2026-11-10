# Copyright (c) The eggshell Authors. All Rights Reserved.
#
# Use of this source code is governed by the license
# that can be found in the LICENSE file.

ARCH ?= amd64
TAMAGO ?= $(shell go tool -n github.com/usbarmory/tamago/cmd/tamago)
GOENV := GOOS=tamago GOARCH=$(ARCH) GOOSPKG=github.com/usbarmory/tamago
TAGS := linkramsize

TEXT_START := 0x10010000 # TamaGo amd64 ramStart + 0x10000
BUILD := build
KERNEL := $(BUILD)/kernel-amd64.elf
VDSO := kernel/vdso/vdso_amd64.so

# expand $(,) to a literal comma
, := ,
KVM ?= $(shell [ -r /dev/kvm ] && [ -w /dev/kvm ] && echo y)
CPU ?= $(if $(KVM),host$(,)invtsc=on$(,)kvmclock=on,max)
TAP0 = $(shell f=$$(cat /sys/class/net/tap0/flags 2>/dev/null) && [ $$((f & 1)) -eq 1 ] && \
	ip -4 -o addr show dev tap0 2>/dev/null | grep -q inet && echo y)

NETDEV_TAP := tap,id=net0,ifname=tap0,script=no,downscript=no
NETDEV_USER := user,id=net0,net=10.0.0.0/24,host=10.0.0.2,dhcpstart=10.0.0.1
NETDEV_USER := $(NETDEV_USER),hostfwd=tcp:127.0.0.1:2222-:22,hostfwd=tcp::6379-:6379
NETDEV ?= $(if $(TAP0),$(NETDEV_TAP),$(NETDEV_USER))
SMP ?= 1

QEMU ?= qemu-system-x86_64 -machine microvm,x-option-roms=on,pit=off,pic=off,rtc=on \
        -smp $(SMP) \
        -global virtio-mmio.force-legacy=false \
        $(if $(KVM),-enable-kvm) -cpu $(CPU) -no-reboot \
        -m 2G -nographic -monitor none -serial stdio \
        -device virtio-net-device,netdev=net0 -netdev $(NETDEV) \
        -device isa-debug-exit,iobase=0xf4,iosize=4

BUILDFLAGS := -trimpath -tags $(TAGS) -ldflags "-s -w -T $(TEXT_START) -R 0x1000"

.PHONY: all kernel rootfs qemu clean distclean

all: kernel

kernel: $(KERNEL)

rootfs: rootfs/$(ARCH).tar

$(VDSO): kernel/vdso/vdso.c kernel/vdso/vdso.lds kernel/vdso/build.sh
	kernel/vdso/build.sh

.PHONY: $(KERNEL)
$(KERNEL): $(VDSO)
	@mkdir -p $(BUILD)
	$(GOENV) $(TAMAGO) build $(BUILDFLAGS) -o $(KERNEL) .

# APPEND is passed to the kernel environment
qemu: export APPEND := $(APPEND)
qemu: $(KERNEL)
	@echo "qemu: $(if $(KVM),kvm,tcg), $(if $(findstring tap,$(NETDEV)),tap0,$(firstword $(subst $(,), ,$(NETDEV))) networking)" >&2
	@$(QEMU) -kernel $(KERNEL) $(if $(APPEND),-append "$$APPEND"); \
	s=$$?; [ $$((s & 1)) -eq 1 ] && s=$$((s >> 1)); exit $$s

rootfs/$(ARCH).tar:
	nix-build nix && cp result/*.tar rootfs/

clean:
	rm -rf $(BUILD)

distclean: clean
	rm -rf rootfs/*.tar
