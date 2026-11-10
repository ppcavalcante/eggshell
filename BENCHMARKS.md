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
| cpu_xorshift_50M | ms | 132.38 | 133.53 | 1.01× faster |
| clock_gettime | ns | 9.89 | 13.77 | 1.39× faster |
| thread_create_join | us | 12.09 | 27.05 | 2.24× faster |
| condvar_pingpong | us | 2.37 | 6.93 | 2.92× faster |
| page_fault | ns/page | 788.85 | 822.69 | 1.04× faster |
| memset_bandwidth | MB/s | 42,251.62 | 40,361.95 | 1.05× faster |
| file_write | MB/s | 9,385.26 | 6,806.62 | 1.38× faster |
| unix_latency | us | 2.91 | 2.97 | 1.02× faster |
| file_write_4k | MB/s | 6,086.05 | 5,492.49 | 1.11× faster |
| file_write_64k | MB/s | 10,832.49 | 7,387.22 | 1.47× faster |
| file_write_1024k | MB/s | 8,228.93 | 7,090.35 | 1.16× faster |
| shell-loop-100k | ms | 155.20 | 163.60 | 1.05× faster |
| rm-go-src | ms | 14.90 | 20.50 | 1.38× faster |
| gzip-go-binary | ms | 916.40 | 922.40 | 1.01× faster |
| boot to init | ms | 67.30 | 400.90 | 5.96× faster |
| null_syscall | ns | 283.93 | 83.66 | 3.39× slower |
| fork_exit_wait | us | 33.11 | 24.09 | 1.37× slower |
| fork_exec_wait | us | 346.35 | 122.14 | 2.84× slower |
| mmap_touch_munmap | us | 2.99 | 2.24 | 1.33× slower |
| open_close | us | 1.01 | 0.58 | 1.74× slower |
| stat | us | 1.08 | 0.38 | 2.84× slower |
| file_read | MB/s | 10,692.64 | 11,570.38 | 1.08× slower |
| pipe_latency | us | 3.43 | 1.84 | 1.86× slower |
| tcp_latency | us | 14.84 | 5.47 | 2.71× slower |
| pipe_bandwidth | MB/s | 8,266.29 | 10,547.17 | 1.28× slower |
| unix_bandwidth | MB/s | 9,026.18 | 11,069.33 | 1.23× slower |
| tcp_bandwidth | MB/s | 3,780.31 | 7,828.24 | 2.07× slower |
| file_rewrite_4k | MB/s | 5,948.86 | 11,565.48 | 1.94× slower |
| file_read_4k | MB/s | 5,977.45 | 8,694.42 | 1.45× slower |
| file_rewrite_64k | MB/s | 10,706.03 | 13,227.90 | 1.24× slower |
| file_read_64k | MB/s | 10,567.68 | 11,353.01 | 1.07× slower |
| file_rewrite_1024k | MB/s | 9,748.74 | 14,165.60 | 1.45× slower |
| file_read_1024k | MB/s | 7,978.99 | 11,184.89 | 1.40× slower |
| exec-500 | ms | 56.90 | 42.40 | 1.34× slower |
| exec-dynamic-200 | ms | 32.80 | 25.80 | 1.27× slower |
| small-files-2000 | ms | 24.30 | 22.00 | 1.10× slower |
| tar-go-src | ms | 37.10 | 33.00 | 1.12× slower |
| untar-go-src | ms | 82.40 | 66.60 | 1.24× slower |
| gunzip-go-binary | ms | 124.80 | 124.20 | 1.00× slower |
| go-build-hello | ms | 7,200.40 | 7,115.80 | 1.01× slower |
| redis SET | req/s | 51,599.59 | 130,548.30 | 2.53× slower |
| redis GET | req/s | 52,603.89 | 131,061.59 | 2.49× slower |
| redis INCR | req/s | 51,255.77 | 130,548.30 | 2.55× slower |
| redis LPUSH | req/s | 48,732.94 | 116,822.43 | 2.40× slower |
| redis LRANGE_100 | req/s | 26,932.40 | 41,305.25 | 1.53× slower |
| redis-p16 SET | req/s | 498,256.12 | 782,472.62 | 1.57× slower |
| redis-p16 GET | req/s | 561,482.31 | 987,166.81 | 1.76× slower |
