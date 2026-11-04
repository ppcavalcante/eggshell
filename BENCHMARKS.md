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
| cpu_xorshift_50M | ms | 131.91 | 138.08 | 1.05× faster |
| clock_gettime | ns | 10.22 | 14.28 | 1.40× faster |
| thread_create_join | us | 11.96 | 28.20 | 2.36× faster |
| condvar_pingpong | us | 2.44 | 6.69 | 2.74× faster |
| page_fault | ns/page | 760.35 | 851.92 | 1.12× faster |
| memset_bandwidth | MB/s | 42,404.41 | 39,136.53 | 1.08× faster |
| file_write | MB/s | 9,206.86 | 6,672.58 | 1.38× faster |
| unix_latency | us | 2.98 | 3.09 | 1.04× faster |
| file_write_4k | MB/s | 6,097.02 | 5,410.96 | 1.13× faster |
| file_write_64k | MB/s | 10,578.29 | 7,069.48 | 1.50× faster |
| file_write_1024k | MB/s | 8,007.62 | 6,873.30 | 1.17× faster |
| shell-loop-100k | ms | 156.20 | 165.00 | 1.06× faster |
| rm-go-src | ms | 14.30 | 20.80 | 1.45× faster |
| gzip-go-binary | ms | 920.10 | 939.50 | 1.02× faster |
| gunzip-go-binary | ms | 123.30 | 125.10 | 1.01× faster |
| boot to init | ms | 66.40 | 397.60 | 5.99× faster |
| null_syscall | ns | 281.98 | 83.64 | 3.37× slower |
| fork_exit_wait | us | 35.01 | 23.59 | 1.48× slower |
| fork_exec_wait | us | 359.19 | 121.46 | 2.96× slower |
| mmap_touch_munmap | us | 2.98 | 2.41 | 1.24× slower |
| open_close | us | 0.94 | 0.60 | 1.57× slower |
| stat | us | 1.11 | 0.38 | 2.92× slower |
| file_read | MB/s | 10,554.35 | 11,362.39 | 1.08× slower |
| pipe_latency | us | 3.42 | 1.86 | 1.84× slower |
| tcp_latency | us | 15.45 | 5.59 | 2.76× slower |
| pipe_bandwidth | MB/s | 8,184.07 | 10,496.39 | 1.28× slower |
| unix_bandwidth | MB/s | 8,884.84 | 10,790.30 | 1.21× slower |
| tcp_bandwidth | MB/s | 3,709.07 | 8,197.37 | 2.21× slower |
| file_rewrite_4k | MB/s | 6,020.27 | 11,333.20 | 1.88× slower |
| file_read_4k | MB/s | 5,999.57 | 8,639.36 | 1.44× slower |
| file_rewrite_64k | MB/s | 11,051.82 | 13,358.69 | 1.21× slower |
| file_read_64k | MB/s | 10,643.85 | 11,391.55 | 1.07× slower |
| file_rewrite_1024k | MB/s | 9,860.50 | 13,728.52 | 1.39× slower |
| file_read_1024k | MB/s | 7,887.76 | 11,252.14 | 1.43× slower |
| exec-500 | ms | 60.20 | 43.90 | 1.37× slower |
| exec-dynamic-200 | ms | 34.80 | 26.70 | 1.30× slower |
| small-files-2000 | ms | 24.40 | 23.10 | 1.06× slower |
| tar-go-src | ms | 37.80 | 34.10 | 1.11× slower |
| untar-go-src | ms | 84.60 | 66.20 | 1.28× slower |
| go-build-hello | ms | 10,384.90 | 7,324.60 | 1.42× slower |
| redis SET | req/s | 52,410.90 | 128,700.12 | 2.46× slower |
| redis GET | req/s | 53,995.68 | 129,870.13 | 2.41× slower |
| redis INCR | req/s | 53,734.55 | 129,870.13 | 2.42× slower |
| redis LPUSH | req/s | 48,030.74 | 115,874.86 | 2.41× slower |
| redis LRANGE_100 | req/s | 27,746.95 | 41,288.19 | 1.49× slower |
| redis-p16 SET | req/s | 516,262.25 | 793,021.38 | 1.54× slower |
| redis-p16 GET | req/s | 567,859.19 | 966,183.62 | 1.70× slower |
