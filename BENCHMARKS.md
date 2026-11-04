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
| cpu_xorshift_50M | ms | 132.99 | 133.56 | 1.00× faster |
| clock_gettime | ns | 10.22 | 14.22 | 1.39× faster |
| thread_create_join | us | 12.37 | 24.98 | 2.02× faster |
| condvar_pingpong | us | 2.58 | 6.90 | 2.67× faster |
| page_fault | ns/page | 788.65 | 996.73 | 1.26× faster |
| memset_bandwidth | MB/s | 41,772.25 | 41,145.39 | 1.02× faster |
| file_write | MB/s | 10,010.89 | 6,617.06 | 1.51× faster |
| unix_latency | us | 3.08 | 3.12 | 1.01× faster |
| file_write_4k | MB/s | 5,802.35 | 5,293.24 | 1.10× faster |
| file_write_64k | MB/s | 10,749.29 | 7,049.84 | 1.52× faster |
| file_write_1024k | MB/s | 8,590.69 | 6,818.58 | 1.26× faster |
| shell-loop-100k | ms | 162.90 | 171.70 | 1.05× faster |
| rm-go-src | ms | 16.80 | 21.10 | 1.26× faster |
| gzip-go-binary | ms | 947.40 | 960.80 | 1.01× faster |
| gunzip-go-binary | ms | 129.00 | 130.70 | 1.01× faster |
| boot to init | ms | 64.90 | 404.50 | 6.23× faster |
| null_syscall | ns | 292.52 | 88.23 | 3.32× slower |
| fork_exit_wait | us | 36.25 | 24.36 | 1.49× slower |
| fork_exec_wait | us | 365.50 | 123.56 | 2.96× slower |
| mmap_touch_munmap | us | 3.00 | 2.71 | 1.11× slower |
| open_close | us | 1.11 | 0.60 | 1.85× slower |
| stat | us | 1.15 | 0.39 | 2.95× slower |
| file_read | MB/s | 9,759.75 | 11,184.41 | 1.15× slower |
| pipe_latency | us | 3.34 | 1.90 | 1.76× slower |
| tcp_latency | us | 16.01 | 5.56 | 2.88× slower |
| pipe_bandwidth | MB/s | 7,644.22 | 10,121.92 | 1.32× slower |
| unix_bandwidth | MB/s | 8,317.13 | 10,695.82 | 1.29× slower |
| tcp_bandwidth | MB/s | 3,533.33 | 7,606.89 | 2.15× slower |
| file_rewrite_4k | MB/s | 5,827.02 | 11,261.23 | 1.93× slower |
| file_read_4k | MB/s | 5,955.27 | 8,690.35 | 1.46× slower |
| file_rewrite_64k | MB/s | 10,563.06 | 13,415.40 | 1.27× slower |
| file_read_64k | MB/s | 10,571.96 | 11,356.28 | 1.07× slower |
| file_rewrite_1024k | MB/s | 9,750.71 | 13,943.31 | 1.43× slower |
| file_read_1024k | MB/s | 8,055.25 | 11,270.15 | 1.40× slower |
| exec-500 | ms | 62.10 | 44.30 | 1.40× slower |
| exec-dynamic-200 | ms | 36.20 | 26.70 | 1.36× slower |
| small-files-2000 | ms | 25.00 | 22.90 | 1.09× slower |
| tar-go-src | ms | 38.60 | 34.00 | 1.14× slower |
| untar-go-src | ms | 85.50 | 66.50 | 1.29× slower |
| go-build-hello | ms | 11,899.70 | 7,387.90 | 1.61× slower |
| redis SET | req/s | 51,867.22 | 130,039.02 | 2.51× slower |
| redis GET | req/s | 52,659.29 | 128,865.98 | 2.45× slower |
| redis INCR | req/s | 51,759.83 | 128,865.98 | 2.49× slower |
| redis LPUSH | req/s | 48,971.59 | 115,606.94 | 2.36× slower |
| redis LRANGE_100 | req/s | 27,754.65 | 41,000.41 | 1.48× slower |
| redis-p16 SET | req/s | 510,725.25 | 794,281.19 | 1.56× slower |
| redis-p16 GET | req/s | 567,536.88 | 978,473.62 | 1.72× slower |
