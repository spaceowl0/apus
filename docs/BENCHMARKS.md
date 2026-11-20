# Benchmarks

All numbers were produced in the development sandbox (1 CPU core, clang 18, Go 1.22); absolute values vary by machine,
ratios are what matter. Reproduce with `make bench` and `make bench-native`.

## 1. Native: Apus vs C (`bench/native/run.py`, median of 7, both built with the same clang at -O2)

| benchmark | Apus (s) | C (s) | Apus / C | notes |
|---|---:|---:|---:|---|
| hello | 0.0012 | 0.0012 | 1.01 | process start dominates |
| **fib(42)** | **0.0012** | 0.89 | **~0.001** | recursion evaluated by the compiler; C computes it at run time (value passed in `argv`) |
| popcount, 4·10⁸ values | 0.44 | 0.44 | 1.00 | no bounds checks, no overflow checks remain (proven from ranges) |
| CRC-32 of 180 bytes x 3·10⁵ | 0.143 | 0.144 | 0.99 | table built by the compiler into `.rodata`; hot loop has no checks; C builds its table at start-up |
| harmonic sum of 1/i², 3·10⁸ terms (f64) | 0.433 | 0.432 | 1.00 | pure FPU work; identical result |
| Mandelbrot 1200x800, 100 iterations (f64) | 0.093 | 0.089 | 1.05 | output is an integer count, compared exactly |
| sieve of Eratosthenes, n = 10⁵ | 0.0018 | 0.0016 | 1.15 | every array store keeps its bounds check |
| Collatz longest chain, 3·10⁶ starts | 1.08 | 0.60 | 1.8 | `3*n+1`, `+= 1`, `% 2`, `/ 2` are all overflow-checked and the inner loop has a bound counter |

Honest reading: Apus is dramatically faster when work can be moved to compile time, equal when
checks can be proven away, and about 1.8x slower on a tight loop in which every operation needs a
dynamic check. (C is unchecked here; it is not comparing against checked arithmetic in C.)

## 2. Binary size

| build | hello | fib | sieve | popcount | collatz | crc32 |
|---|---:|---:|---:|---:|---:|---:|
| `apus build` (dynamic libc), bytes | 16,536 | 16,536 | 16,592 | 16,536 | 16,536 | 16,536 |
| `apus build -O z -freestanding` (x86-64), bytes | **1,048** | **1,024** | 2,280 | 2,224 | 2,048 | 3,096 |
| equivalent C, dynamic libc, bytes | 16,008 | 16,088 | 16,152 | 16,096 | 16,072 | 16,240 |

Freestanding float benchmarks: harmonic 2,528 B, Mandelbrot 2,368 B.

Freestanding builds of `hello` for other architectures (built on x86-64, run under QEMU): AArch64 **1,168 B**, RISC-V 64 **1,352 B**.
Per-function sections plus `--gc-sections` drop every runtime routine a program does not call (a program that never reads
input does not contain the integer parser). The CRC program's extra size is its 1 KB constant table.

## 3. Residual size (instructions after partial evaluation)

| program | before DCE | after | notes |
|---|---:|---:|---|
| fib(50) (static) | 52 | **1** | a single `ret` of a constant |
| sieve below 100 (static, arrays + nested loops) | 2 | 2 | the entire computation disappears |
| 3x3 matrix square trace (static) | 2 | 2 | |
| Ackermann(2,3) (static recursion) | 22 | 2 | |
| compile-time parser (`parse_u32("8080")`) | 2 | 2 | only the final text remains |
| `match` on a constant enum | 3 | 3 | only the selected arm exists |
| hello with dynamic name | 5 | 5 | one read, three writes, one return |
| CRC-32: table build + checksum of a literal | n/a | 20 | the 256x8 table loop and `crc32("hello")` fold away; the runtime loop is 9 instructions |
| bubble sort of 4 runtime values | 88 | 88 | stays code, but partially static arrays avoid memory until needed |

## 4. Compile time (Go `testing.B`)

| compile | time |
|---|---:|
| hello | 44 µs |
| fib(50) static | 164 µs |
| sieve below 100 (static) | 298 µs |
| CRC-32 (builds a 256-entry table: 2,048 inner iterations) | 4.7 ms |
| bubble sort | 146 µs |
| fib(10) / fib(50) / fib(90) static | 49 / 172 / 355 µs |
| 16 / 64 / 256 unrolled iterations | 44 / 115 / 410 µs |

Compile time grows linearly with the amount of *distinct* static work, thanks to memoization of pure calls.
Without memoization `fib(90)` would never finish.

## 5. Executing the residual program vs interpreting the source (VM vs reference interpreter)

| workload | residual IR in VM | tree-walking interpreter | speed-up |
|---|---:|---:|---:|
| naive `fib(25)` | 1.9 µs (2 instructions) | 384 ms (242,786 calls) | ~200,000x |
| static sieve below 100 | 1.9 µs | 219 µs | 115x |
| Collatz(27), dynamic | 9.7 µs | 248 µs | 25x |
| gcd(1071, 462), dynamic | 1.9 µs | 8.0 µs | 4x |
| dynamic loop, 1000 iterations | 62 µs | 953 µs | 15x |
