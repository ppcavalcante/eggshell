#!/usr/bin/env python3
#
# Copyright (c) The eggshell Authors. All Rights Reserved.
#
# Use of this source code is governed by the license
# that can be found in the LICENSE file.

"""eggshell vs Linux benchmark driver.

Runs bench/guest.sh on eggshell and on a Linux kernel booted on the same QEMU
microvm machine, with the same userland (rootfs/amd64.tar, used as the Linux
initramfs), and prints a markdown comparison of the median results.

  make rootfs
  bench/run.py --linux /path/to/vmlinuz [--rounds 3]

KVM is used when /dev/kvm is accessible, as with `make qemu`.
"""

import argparse
import json
import os
import re
import shutil
import statistics
import subprocess
import sys
import tarfile
import time

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
BENCH = os.path.join(ROOT, "bench")
BUILD = os.path.join(BENCH, "build")

GUEST_FILES = ["guest.sh", "gobuild.sh", "linux-init"]
HELPERS = ["tm", "fio"]


def kvm():
    return os.access("/dev/kvm", os.R_OK | os.W_OK)


def qemu_cmd(args, linux):
    use_kvm = kvm() and not args.tcg
    # Linux calibrates its timers against the PIT without kvmclock
    pit = "off" if use_kvm or not linux else "on"
    cpu = "host,invtsc=on,kvmclock=on" if use_kvm else "max"

    cmd = [
        args.qemu,
        "-machine", f"microvm,x-option-roms=on,pit={pit},pic=off,rtc=on",
        "-smp", str(args.smp),
        "-global", "virtio-mmio.force-legacy=false",
        "-cpu", cpu, "-no-reboot",
        "-m", "2G", "-nographic", "-monitor", "none", "-serial", "stdio",
        "-device", "virtio-net-device,netdev=net0",
        "-netdev", "user,id=net0",
        "-device", "isa-debug-exit,iobase=0xf4,iosize=4",
    ]

    if use_kvm:
        cmd.insert(1, "-enable-kvm")

    return cmd


def build_helpers(args):
    for h in HELPERS:
        out = os.path.join(BUILD, h)
        subprocess.run([args.cc, "-static", "-O2", "-o", out,
                        os.path.join(BENCH, h + ".c")], check=True)


def build_rootfs(args):
    """Copies the root file system adding the suite under /root/bench."""
    out = os.path.join(BUILD, "rootfs.tar")
    shutil.copyfile(args.rootfs, out)

    with tarfile.open(out, "a", format=tarfile.GNU_FORMAT) as tar:
        names = tar.getnames()
        # follow the archive naming (with or without a leading "./")
        prefix = "./" if names and names[0].startswith("./") else ""
        d = prefix + "root/bench"

        if d not in names:
            ti = tarfile.TarInfo(d)
            ti.type = tarfile.DIRTYPE
            ti.mode = 0o755
            tar.addfile(ti)

        files = [(os.path.join(BENCH, f), f) for f in GUEST_FILES]
        files += [(os.path.join(BUILD, h), h) for h in HELPERS]

        for src, name in files:
            ti = tar.gettarinfo(src, d + "/" + name)
            ti.uid = ti.gid = 0
            ti.uname = ti.gname = "root"
            ti.mode = 0o755

            with open(src, "rb") as f:
                tar.addfile(ti, f)

    return out


def build_eggshell(args, rootfs):
    """Builds the kernel embedding the benchmark root file system."""
    overlay = os.path.join(BUILD, "overlay.json")
    kernel = os.path.join(BUILD, "kernel-amd64.elf")

    with open(overlay, "w") as f:
        json.dump({"Replace": {os.path.join(ROOT, "rootfs", "amd64.tar"): rootfs}}, f)

    env = dict(os.environ)
    env["GOFLAGS"] = (env.get("GOFLAGS", "") + " -overlay=" + overlay).strip()
    subprocess.run(["make", "-C", ROOT, "kernel", "KERNEL=" + kernel], check=True, env=env)

    return kernel


def cpio_newc(tar_path, out_path):
    """Converts a tar archive to a newc cpio archive (Linux initramfs)."""
    ino = 1

    def header(name, mode, size, nlink=1, mtime=0):
        nonlocal ino
        ino += 1
        fields = [ino, mode, 0, 0, nlink, mtime, size, 0, 0, 0, 0, len(name) + 1, 0]
        return b"070701" + b"".join(b"%08X" % v for v in fields) + name.encode() + b"\0"

    def pad(f):
        f.write(b"\0" * (-f.tell() % 4))

    with tarfile.open(tar_path) as tar, open(out_path, "wb") as f:
        seen = {}

        for m in tar.getmembers():
            name = os.path.normpath(m.name).lstrip("/")

            if name in ("", "."):
                continue

            # later members override earlier ones
            seen[name] = m

        for name in sorted(seen):
            m = seen[name]
            perm = m.mode & 0o7777
            data = b""

            if m.isdir():
                mode = 0o040000 | perm
            elif m.issym():
                mode = 0o120000 | 0o777
                data = m.linkname.encode()
            elif m.islnk():
                mode = 0o100000 | perm
                data = tar.extractfile(m).read()
            elif m.isreg():
                mode = 0o100000 | perm
                data = tar.extractfile(m).read()
            elif m.isfifo():
                mode = 0o010000 | perm
            else:
                continue

            f.write(header(name, mode, len(data), mtime=int(m.mtime)))
            pad(f)
            f.write(data)
            pad(f)

        f.write(header("TRAILER!!!", 0, 0, nlink=1))
        pad(f)


def run(cmd, log_path, timeout):
    """Runs a guest, logging its output and the time until the suite starts."""
    start = time.monotonic()
    boot = None

    with open(log_path, "w") as log:
        p = subprocess.Popen(cmd, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                             stderr=subprocess.STDOUT, text=True, errors="replace")

        try:
            for line in p.stdout:
                log.write(line)
                log.flush()

                if boot is None and "SUITE-START" in line:
                    boot = time.monotonic() - start

                if time.monotonic() - start > timeout:
                    p.kill()
                    break
        finally:
            p.wait()

        if boot is not None:
            log.write(f"HOST-BOOT {boot * 1000:.1f} ms\n")


def parse(path):
    """Returns the results of a guest log as {name: (value, unit, higher)}."""
    r = {}

    with open(path, errors="replace") as f:
        text = f.read()

    if "SUITE-END" not in text:
        print(f"warning: {path} is incomplete", file=sys.stderr)

    for line in text.splitlines():
        line = line.strip()

        if m := re.match(r"HOST-BOOT ([\d.]+) ms", line):
            r["boot to init"] = (float(m.group(1)), "ms", False)
        elif m := re.match(r"BENCH (\S+)\s+([\d.]+) (\S+)", line):
            unit = m.group(3)
            r[m.group(1)] = (float(m.group(2)), unit, unit == "MB/s")
        elif m := re.match(r"PERF (\S+) ([\d.]+) ms", line):
            r[m.group(1)] = (float(m.group(2)), "ms", False)
        elif m := re.match(r'(REDIS\S*) "([^"]+)","([\d.]+)"', line):
            name = {"REDIS": "redis", "REDISP16": "redis-p16"}.get(m.group(1), m.group(1))
            r[f"{name} {m.group(2).split()[0]}"] = (float(m.group(3)), "req/s", True)

    return r


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--linux", required=True, help="Linux kernel image (bzImage)")
    ap.add_argument("--rounds", type=int, default=3)
    ap.add_argument("--rootfs", default=os.path.join(ROOT, "rootfs", "amd64.tar"))
    ap.add_argument("--qemu", default="qemu-system-x86_64")
    ap.add_argument("--cc", default=os.environ.get("CC", "cc"))
    ap.add_argument("--tcg", action="store_true", help="do not use KVM")
    ap.add_argument("--smp", type=int, default=1, help="number of processors")
    ap.add_argument("--timeout", type=int, default=3600, help="seconds per guest run")
    ap.add_argument("--only", choices=["eggshell", "linux"])
    ap.add_argument("--no-build", action="store_true", help="reuse bench/build")
    ap.add_argument("--first", type=int, default=1, help="number of the first round")
    ap.add_argument("--report", action="store_true", help="only report bench/build/logs")
    args = ap.parse_args()

    logs = os.path.join(BUILD, "logs")
    os.makedirs(logs, exist_ok=True)

    if args.report:
        return report(logs, "KVM" if kvm() and not args.tcg else "TCG")

    kernel = os.path.join(BUILD, "kernel-amd64.elf")
    initrd = os.path.join(BUILD, "initrd.cpio")

    if not args.no_build:
        build_helpers(args)
        rootfs = build_rootfs(args)
        build_eggshell(args, rootfs)
        cpio_newc(rootfs, initrd)

    guests = {
        "eggshell": qemu_cmd(args, False) + [
            "-kernel", kernel,
            "-append", "quiet ssh=0 -- /bin/sh /root/bench/guest.sh"],
        "linux": qemu_cmd(args, True) + [
            "-kernel", args.linux, "-initrd", initrd,
            "-append", "console=ttyS0 quiet rdinit=/root/bench/linux-init"],
    }

    for i in range(args.first, args.first + args.rounds):
        for g, cmd in guests.items():
            if args.only and g != args.only:
                continue

            print(f"round {i}: {g}", file=sys.stderr)
            run(cmd, os.path.join(logs, f"{g}.{i}.log"), args.timeout)

    report(logs, "KVM" if kvm() and not args.tcg else "TCG")


def report(logs, accel):
    """Prints the median of each result across rounds as a markdown table."""
    results = {}

    for f in sorted(os.listdir(logs)):
        if m := re.match(r"(eggshell|linux)\.\d+\.log$", f):
            results.setdefault(m.group(1), []).append(parse(os.path.join(logs, f)))

    with open(os.path.join(BUILD, "results.json"), "w") as f:
        json.dump(results, f, indent=1)

    rounds = {g: len(r) for g, r in results.items()}
    print(f"rounds: {rounds}", file=sys.stderr)

    names = []

    for runs in results.values():
        for r in runs:
            names += [n for n in r if n not in names]

    def median(g, n):
        v = [r[n][0] for r in results.get(g, []) if n in r]
        return statistics.median(v) if v else None

    print(f"| benchmark ({accel}) | unit | eggshell | Linux | eggshell vs Linux |")
    print("|---|---|---:|---:|---:|")

    for n in names:
        unit, higher = next(r[n][1:] for runs in results.values() for r in runs if n in r)
        e, l = median("eggshell", n), median("linux", n)
        cmp = ""

        if e and l:
            # speedup of eggshell over Linux (>1 is faster)
            s = e / l if higher else l / e
            cmp = f"{s:.2f}× faster" if s >= 1 else f"{1 / s:.2f}× slower"

        fmt = lambda v: "–" if v is None else f"{v:,.2f}"
        print(f"| {n} | {unit} | {fmt(e)} | {fmt(l)} | {cmp} |")


if __name__ == "__main__":
    main()
