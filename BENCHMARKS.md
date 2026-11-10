Benchmarks
==========

eggshell compared with Linux running the same unmodified binaries, on the same
QEMU `microvm` machine, with the same userland.

Setup
-----

|                  | eggshell                                                | Linux                           |
|------------------|---------------------------------------------------------|---------------------------------|
| kernel           | eggshell (this tree, `tamago` go1.27.3)                 | Ubuntu 7.0.0-38-generic         |
| root file system | `rootfs/amd64.tar` embedded in the kernel               | `rootfs/amd64.tar` as initramfs |
| host             | 8 vCPU Intel i7-1165G7 @ 2.80GHz                        | same                            |
| machine          | QEMU 11.1.3 `microvm`, 1 vCPU, 2 GiB, `-cpu host` (KVM) | same                            |
| networking       | virtio-net, gVisor TCP/IP                               | virtio-net, Linux TCP/IP        |
| boot arguments   | `quiet ssh=0`                                           | `quiet`                         |

```sh
# Linux kernel taken from Ubuntu 26.04.1 LTS
./bench/run.py --linux vmlinuz-7.0.0-38-generic --rounds 3 | tee results.md
```

Results
-------

> [!WARNING]
> This project is at its early stages and therefore experimental.

| benchmark (KVM) | unit | eggshell | Linux | eggshell vs Linux |
|---|---|---:|---:|---:|
| cpu_xorshift_50M | ms | 132.37 | 137.31 | 1.04× faster |
| clock_gettime | ns | 10.49 | 14.37 | 1.37× faster |
| thread_create_join | us | 12.81 | 25.72 | 2.01× faster |
| condvar_pingpong | us | 2.60 | 7.00 | 2.69× faster |
| page_fault | ns/page | 829.38 | 925.35 | 1.12× faster |
| memset_bandwidth | MB/s | 41,492.21 | 39,688.93 | 1.05× faster |
| file_write | MB/s | 9,973.47 | 6,500.90 | 1.53× faster |
| unix_latency | us | 2.97 | 3.34 | 1.12× faster |
| file_write_4k | MB/s | 6,065.41 | 5,214.39 | 1.16× faster |
| file_write_64k | MB/s | 10,684.74 | 6,781.92 | 1.58× faster |
| file_write_1024k | MB/s | 8,217.90 | 6,821.78 | 1.20× faster |
| shell-loop-100k | ms | 156.30 | 166.50 | 1.07× faster |
| rm-go-src | ms | 15.60 | 21.60 | 1.38× faster |
| gzip-go-binary | ms | 914.30 | 960.80 | 1.05× faster |
| gunzip-go-binary | ms | 126.00 | 126.80 | 1.01× faster |
| go-build-hello | ms | 7,434.20 | 7,418.80 | 1.00× faster |
| boot to init | ms | 68.70 | 393.00 | 5.72× faster |
| null_syscall | ns | 287.23 | 86.26 | 3.33× slower |
| fork_exit_wait | us | 37.74 | 25.00 | 1.51× slower |
| fork_exec_wait | us | 380.03 | 126.70 | 3.00× slower |
| mmap_touch_munmap | us | 3.08 | 2.97 | 1.04× slower |
| open_close | us | 1.01 | 0.62 | 1.63× slower |
| stat | us | 1.07 | 0.39 | 2.74× slower |
| file_read | MB/s | 10,573.67 | 11,587.12 | 1.10× slower |
| pipe_latency | us | 3.59 | 1.95 | 1.84× slower |
| tcp_latency | us | 15.02 | 5.69 | 2.64× slower |
| pipe_bandwidth | MB/s | 8,055.68 | 10,109.13 | 1.25× slower |
| unix_bandwidth | MB/s | 8,704.38 | 10,410.55 | 1.20× slower |
| tcp_bandwidth | MB/s | 3,787.23 | 7,918.19 | 2.09× slower |
| file_rewrite_4k | MB/s | 5,988.10 | 11,310.93 | 1.89× slower |
| file_read_4k | MB/s | 5,946.56 | 8,628.37 | 1.45× slower |
| file_rewrite_64k | MB/s | 10,713.54 | 13,149.50 | 1.23× slower |
| file_read_64k | MB/s | 10,697.17 | 11,362.82 | 1.06× slower |
| file_rewrite_1024k | MB/s | 9,396.95 | 14,183.20 | 1.51× slower |
| file_read_1024k | MB/s | 8,108.29 | 11,353.14 | 1.40× slower |
| exec-500 | ms | 57.30 | 44.80 | 1.28× slower |
| exec-dynamic-200 | ms | 33.80 | 27.50 | 1.23× slower |
| small-files-2000 | ms | 24.20 | 23.40 | 1.03× slower |
| tar-go-src | ms | 38.70 | 34.10 | 1.13× slower |
| untar-go-src | ms | 82.70 | 67.20 | 1.23× slower |
| redis SET | req/s | 52,966.10 | 131,406.05 | 2.48× slower |
| redis GET | req/s | 54,318.30 | 131,926.12 | 2.43× slower |
| redis INCR | req/s | 53,705.69 | 131,752.31 | 2.45× slower |
| redis LPUSH | req/s | 48,030.74 | 117,508.81 | 2.45× slower |
| redis LRANGE_100 | req/s | 27,831.90 | 41,000.41 | 1.47× slower |
| redis-p16 SET | req/s | 500,500.50 | 793,021.38 | 1.58× slower |
| redis-p16 GET | req/s | 564,652.75 | 973,709.88 | 1.72× slower |
