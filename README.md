# Apus

**A systems language whose compiler is a partial evaluator.**
Anything knowable at build time is computed at build time, every byte of memory is
accounted for before the program runs, and what is left is the minimum residual
machine code. The whole toolchain is one Go program; native code and cross-compilation
go through LLVM.

```apus
pure fn make_table() -> [u32; 256] {              // runs inside the compiler
    let mut t: [u32; 256] = [0; 256];
    for i in 0..256 {
        let mut c: u32 = i as u32;
        for k in 0..8 { if c & 1 == 1 { c = 3988292384 ^ (c >> 1); } else { c = c >> 1; } }
        t[i] = c;
    }
    return t;
}
const TABLE: [u32; 256] = make_table();            // 1 KB of read-only data, zero init code

pure fn crc32(s: Str<64>) -> u32 {
    let mut crc: u32 = 0xFFFFFFFF;
    for i in 0..len(s) { crc = TABLE[((crc ^ (s[i as u64] as u32)) & 255) as u64] ^ (crc >> 8); }
    return crc ^ 0xFFFFFFFF;
}

fn main() io {
    io::println("crc32(hello) = " ++ str(static crc32("hello")));   // computed while compiling
    let line = io::read_line(32);
    io::println("crc32(input) = " ++ str(crc32(line)));             // a 9-instruction loop, no checks
}
```
```text
$ apus ir examples/crc32.apus
; vars=12 arrays=0 bufs=1 frame=128B rodata=1024B
; table 0: 256 x u32
main:
  write "crc32(hello) = 907060870\n"  ; 25B
  b0, v0 = read_line cap=32
  v1 = 0
  v2 = v0
  v3 = 4294967295
  loop (bound -1) {
    v4 = lt.u32 v1, v2
    break unless v4
    v5 = bufload b0[v1] len v0
    v6 = xor.u32 v3, v5
    v7 = and.u32 v6, 255
    v8 = tload t0[v7]                 ; table lookup: no bounds check
    v9 = shr.u32 v3, 8
    v10 = xor.u32 v8, v9
    v3 = v10
    v1 = add.u32 v1, 1                ; no overflow check
  }
  v11 = xor.u32 v3, 4294967295
  write "crc32(input) = "  ; 15B
  write_dec.u32 v11
  write "\n"  ; 1B
  ret
```
The table build, the 256x8 inner loops and the checksum of `"hello"` happened during compilation.
No overflow or bounds check survives in the hot loop, because the compiler proved none can fail.

## Quick start

Requirements: Go 1.22+ to build the compiler; `clang` (LLVM 15+) to produce native binaries
(`lld` and `qemu-user` only for cross-targets and their tests).

```sh
make build                                      # ./apus
./apus run    examples/hello.apus               # built-in VM
./apus ir     examples/primes.apus              # residual IR after partial evaluation
./apus report examples/arena.apus               # statistics + proven arena high-water marks
./apus llvm   examples/hello.apus               # textual LLVM IR
./apus build -o hello examples/hello.apus       # native executable through clang
./apus run -D DEBUG=true -D BUF=32 examples/config.apus     # build-time configuration
```

### Tiny, libc-free, cross-compiled binaries

```sh
./apus build -O z -freestanding -o hello bench/native/hello.apus                                      # 1,048 B x86-64 ELF
./apus build -O z -freestanding -lld -target aarch64-linux-gnu -o hello.a64 bench/native/hello.apus   # 1,168 B
./apus build -O z -freestanding -lld -target riscv64-linux-gnu -o hello.rv64 bench/native/hello.apus  # 1,352 B
```
`-freestanding` swaps libc for a small runtime using raw Linux syscalls and a custom `_start`.
No sysroot is needed, which is what makes cross-compiling a one-liner. Unused runtime routines are
garbage-collected by the linker, so a program that never reads input contains no integer parser.

## What moves to compile time

| You write | The compiler does |
|---|---|
| `const`, `pure fn` with constant arguments | evaluates it (memoized; loops always run to completion) |
| `const T: [u32; 256] = make_table();` | builds the table; indexed at run time it becomes `.rodata` |
| `static expr`, `static_assert(c, "msg")`, `static_print(s)` | demands / checks / reports at build time |
| `if` / `match` on a known value | keeps only the selected branch or arm |
| `for` over a constant range (up to `-unroll`) | unrolls and folds each iteration; `break`/`continue` included |
| `"a" ++ x ++ "b" ++ str(1 + 2)` | merges every static run; only the runtime parts remain |
| `s[i]`, `s == "lit"`, `len(s)`, `bytes("..")` | fold on constant strings (parsers and hashes of literals vanish) |
| `embed("data.bin")` | reads the file while compiling into a `[u8; N]` table |
| `sizeof alignof bits_of offsetof max_of min_of` | layout and limits as constants |
| `-D NAME=VALUE` | overrides a typed constant: array sizes, branches, matches |
| every `+ - * / % << >> as []` | removes the overflow/zero/bounds check when ranges prove it safe |
| `f64`/`f32` expressions, loops, tables | folds them with the exact IEEE arithmetic the hardware uses |
| `if x > 0 { 100 / x }`, `x != 0 && 100 / x`, guard clauses, match arms | uses the condition itself as proof |
| `fn f<T, const N: usize>`, `f::<u8, 4>(x)` | instantiates one specialized, folded function per distinct type/const argument |
| `arena A(N)`, `alloc`, `region` | proves the worst-case memory use fits, or refuses to compile |

## The language in one page

| Feature | Syntax |
|---|---|
| Scalars | `u8 u16 u32 u64 usize i8 i16 i32 i64 isize bool f32 f64` |
| Strings | `Str<N>` (runtime, bounded), `StaticStr`, `++`, `s[i]`, `len`, `==`, `str(n)` |
| Aggregates | `[T; N]`, `struct S { a: T }`, `enum E { A, B = 5 }` |
| Generics | `fn f<T, const N: usize>(a: [T; N])` inferred from arguments or `f::<u8, 4>(x)`; one instance per distinct argument |
| Control | `if else`, `while c bound(N)`, `for i in a..b`, `break`, `continue`, `match` (exhaustive), `return` |
| Compile-time | `const`, `pure fn`, `static`, `static_assert`, `static_print`, `embed`, `bytes` |
| Effects | `pure fn` ⊂ `fn` ⊂ `fn ... io` |
| Bounds | `bound(N)`, `bound(rec = N)`, `n: u32 in 0..=10` refinements, `clamp`, `require` |
| Memory | `arena Name(bytes);` `region Name { ... }` `alloc(Name, size)` |
| Arithmetic | always checked: overflow, division by zero, shifts, casts, indexing |

Full reference: [`docs/LANGUAGE.md`](docs/LANGUAGE.md). Design and algorithms:
[`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md). What changed: [`CHANGELOG.md`](CHANGELOG.md).

### The compiler tells you what it cannot prove

```text
error[E0412]: arena `Frame` budget exceeded: worst case 0 live + 4112 requested > 4096 bytes
  --> examples/errors/arena_overflow.apus:9:19
   |     let packets = alloc(Frame, n as u64 * sizeof(Packet));   // up to 8 * 514 = 4112 > 4096
   = help: shrink the allocation, wrap earlier ones in a `region`, or enlarge the arena

error[E0309]: non-exhaustive match: variant `Color::Blue` is not covered
```

## Results (measured on the development sandbox, 1 core; details in [`docs/BENCHMARKS.md`](docs/BENCHMARKS.md))

| benchmark | Apus | C (clang -O2) | note |
|---|---|---|---|
| `fib(42)` | **0.0012 s** | 0.89 s | folded by the compiler |
| popcount of 4·10⁸ integers | 0.44 s | 0.44 s | all bounds/overflow checks proven away |
| CRC-32, 54 MB of input | 0.143 s | 0.144 s | table built at compile time into `.rodata` |
| sieve to 10⁵ | 0.0018 s | 0.0016 s | bounds-checked stores |
| Collatz, 3·10⁶ starts | 1.08 s | 0.60 s | every `+ * %` overflow-checked (1.8x) |
| harmonic sum, 3·10⁸ terms (f64) | 0.433 s | 0.432 s | float arithmetic, bit-identical to C |
| Mandelbrot 1200x800 (f64) | 0.093 s | 0.089 s | 1.05x, integer output compared exactly |
| `hello`, libc build | 16.5 KB | 16.0 KB | |
| `hello`, `-freestanding` | **1,048 B** | n/a | x86-64; 1,168 B AArch64, 1,352 B RISC-V |

Compiling `fib(90)` by naive recursion takes 0.35 ms (memoized pure calls); building a CRC table
takes 4.7 ms.

## Correctness story

* **Differential fuzzing**: a grammar-based generator produces random well-typed programs (nested
  control flow, arrays, helper functions with returns inside loops, runtime strings, floats, `break`/`continue`, `match`). Each runs
  through a tree-walking *reference interpreter* (no optimization), the partially-evaluated IR in a VM,
  and native LLVM code. Any disagreement in output or trap is a compiler bug. Thousands of programs and
  tens of thousands of runs pass, and the fuzzer has found and fixed real bugs along the way.
* 48-program corpus with golden stdin/stdout/exit/trap expectations, run on VM + reference + native +
  libc-free x86-64/AArch64/RISC-V (QEMU).
* 81 programs that must be *rejected* with a specific error code, 22 that must be accepted, sandbox
  and `-D` tests.
* Unit tests for every package, including exhaustive tables for checked arithmetic. 458 checks in total.

```sh
make test          # everything (needs clang; lld+qemu enable the cross-target tests)
make test-short    # no native builds
make fuzz          # 20,000-program campaign
make bench         # Go benchmarks      make bench-native   # vs C
```

## Layout

```
cmd/apus            CLI driver
internal/syntax     lexer, AST, parser
internal/types      type system (ranges, layout)
internal/sema       type checker, effects, exhaustiveness, embed sandbox
internal/peval      the partial evaluator (the heart of the compiler)
internal/ir         residual IR, optimizer, bit-exact VM
internal/llvm       LLVM IR emission + C runtime (hosted and freestanding)
internal/ref        reference interpreter (test oracle)
internal/e2e        corpus, fuzzing, error and cross-target tests
bench/              Go benchmarks and native-vs-C suite
examples/           showcase programs, examples/errors = intentional failures
testdata/programs   golden-tested corpus
docs/               language reference, architecture, benchmarks
```

## Status and honest limitations (v0.4)

* Everything is inlined into `main` (no separate functions or code sharing); dynamic recursion is
  unrolled to its `bound(rec)`.
* These report `E0502` (a clear error, never a miscompile): strings reassigned under dynamic control flow,
  dynamically indexed arrays of aggregates, a `match` expression on a runtime value that produces a non-scalar.
* `while` loops are runtime-checked against their `bound(N)`; `for` loops over bounded ranges are proven.
* Generic *functions* only (no generic structs/enums); generic bodies are checked per instantiation, like C++ templates,
  so an unused generic is never type-checked. Enums have no payloads and no int-to-enum cast.
* I/O is stdin/stdout only. The freestanding runtime targets Linux (x86-64, AArch64, RISC-V 64).
* Floats: no exponent syntax in `io::read_f*` input, `str(float)` has a fixed six-decimal format, no `%`/`pow`/trig library functions (write them in Apus; they fold).
* One thread. No traits yet.
