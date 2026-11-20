# Changelog

## 0.8.0 - gaps found by probing ordinary programs

* **Fixed (silent miscompile):** a function returning different strings on different run-time paths (`if n > 5 { return "big"; }
  return "small";`) produced an empty string. Dynamic string results now live in a slot-backed buffer like struct/array results.
* **Fixed:** a struct field, array element or parameter declared `Str<N>` lost its capacity when initialised with a shorter literal,
  so later assignments in branches were wrongly rejected. The declared capacity is now recorded for fields, elements and parameters.
* **Fixed:** a string array literal takes the capacity it is expected to have (`let a: [Str<4>; 3] = ["a", "b", "c"]` and
  `[""; 3]` used to be `E0305`).
* **New: `for x in array { .. }`** iterates the elements of an array (and the bytes of a string). Implemented as a parser desugaring
  to a hidden snapshot plus a counted loop, so every backend gets it for free; nested loops, arrays of structs/arrays/strings and
  loops inside functions work. The array is evaluated once, so mutation inside the body does not change the iteration.
* **New: `str(bool)`** gives `"true"`/`"false"` (constants fold; a run-time bool lowers to a small branch).
* Fuzzer: dynamic string returns, `str(bool)`, and for-each over struct and 2D arrays. Corpus: p57 to p59.
* Verified: 7,500 random programs (VM vs reference) and 400 native builds with no divergence.

## 0.7.0 - struct arrays live in memory

* **Arrays of flat structs (all fields scalar) are held as one memory array per field** (struct-of-arrays) as soon as they are
  indexed or written through a run-time index. Dynamic reads and writes are a single load or store at any array length, so the
  per-write gather/scatter and its 256-leaf limit no longer apply to them. Measured with `go test ./bench -run DynCost -v`:
  at N=256 the old lowering produced 1562 IR lines and ran 56x slower than hand-written parallel arrays; it now produces the same
  26 lines and runs at the same speed at every N.
* Conversion happens before a branch or loop body is captured (for roots written through a non-literal index), so the memory is
  initialised on every path. Constant-index code keeps its scalar slots and constant folding.
* Whole-array assignment, snapshots (`let q = ps`), restore (`ps = q`), parameters, results and arrays nested in structs all work.
* Arrays whose elements contain arrays or strings still use the gather/scatter lowering (limit 256 leaves).
* New: `bench/dyncost_test.go` (cost comparison), `p55_soa_structs`, and a fuzz statement for snapshot/restore.
* New dev helper: `APUS_REF_FILE=prog.apus APUS_REF_STDIN="3 4;0 1" go test ./internal/e2e -run RefPrint -v` prints the reference
  interpreter's output, for writing goldens that do not depend on the partial evaluator.
* **`abs` now works on signed integers** (previously floats only): `abs(x)` has the type of `x`, folds constants, tracks value ranges
  and traps with `arithmetic overflow` for `min_of(T)`. An integer literal stays an integer (`abs(-7)` is `7`, not `7.000000`).
  Unsigned arguments are rejected (`E0305`). The old compile-error test that expected `abs(i32)` to fail now uses `sqrt`, and new
  cases cover the unsigned and `f64 = abs(i32)` errors. The fuzzer generates `abs`; new corpus program `p56_int_abs`.
* Verified: 7,500 random programs (VM vs reference) and 500 native builds with no divergence.

## 0.6.1 - fuzzer coverage and two fixes it found

* **Fuzzer** now generates struct arrays, 2D arrays, reassigned strings and dynamically returned structs, with occasional
  out-of-range indices so trap behaviour is compared as well. The summary reports a histogram of compile-time rejections, and
  `APUS_FUZZ_NATIVE=N` sets how many programs are also built and run natively (default 60).
* **Fixed:** dynamic indexing through memory-backed rows (a 2D array whose rows were written with a dynamic column index could no
  longer be indexed by a dynamic row). Reads load from the row's memory; writes load the cell, update it and store it back.
* **Fixed:** a string variable lost its declared `Str<N>` capacity when it was reassigned in straight-line code, so a later
  assignment inside a branch could be wrongly rejected.
* **Fixed:** `apus` printed `0.5.0` as its version.
* Verified: 6,000 random programs (VM vs reference interpreter) and 500 native builds with no divergence.

## 0.6.0 - fewer E0502 limits

* **Strings can be reassigned inside dynamic control flow** (`if`, `while`, `match` on run-time values). A string variable becomes a
  fixed-capacity buffer (its declared `Str<N>` size) plus a length slot; assignments rebuild it in place, staging through a temporary
  when the new value reads the same buffer. `let t = s` snapshots the value. Assigning something that may exceed the capacity is a
  compile error, never a truncation.
* **Dynamic return of structs and arrays** from inlined functions (`return P { .. }` on a run-time path). Previously this silently
  produced an empty value and crashed the compiler; results now live in slot-backed storage with widened ranges.
* **Dynamic indexing of arrays of aggregates**: `ps[i]` reads a whole struct, `ps[i].x` reads a field, `g[i][j]` reads 2D arrays.
  Each scalar leaf becomes a load from a per-leaf table.
* **Dynamic-index writes into arrays of aggregates**: `ps[i].x = v`, `ps[i] = P { .. }`, `g[i][j] = v` (both indices dynamic works: each
  index is bounds-checked against its own length, so `j == M` can never alias the next row). Writes gather, store with the normal
  bounds check and scatter back, O(N) per scalar leaf, limited to 256 leaves.
* Still unsupported (E0502, never a miscompile): dynamic indexing through memory-backed rows, strings inside aggregates that are
  returned dynamically, and arrays of aggregates larger than 256 leaves for writes.
* New corpus programs p49 to p53 (struct reads/writes, string assignment, aggregate returns, 2D/struct-element access with traps).

## 0.5.0 - floating point

* `f32` and `f64` with IEEE-754 semantics: literals (`1.5`, `2.5e-3`), `+ - * /`, comparisons, `min`/`max`, `sqrt`, `floor`, `ceil`, `trunc`,
  `abs`, `is_nan`, `io::read_f32/f64`, checked float-to-int casts, `str(float)`.
* Floats fold at compile time (constants, loops, tables) using the same code the VM runs; float arrays and tables are read-only data.
* Native floats are bit-identical: no fast-math/contraction flags, libm-free runtime (`floor`/`ceil`/`trunc` included), identical formatting and
  parsing algorithms in Go and C. Verified on x86-64, AArch64 and RISC-V (QEMU), hosted and freestanding.
* Generic inference understands float literals; an all-literal `match` expression now resolves its arms' types.
* Benchmarks: harmonic sum and Mandelbrot run at C speed (1.00x and 1.05x). The fuzzer generates float programs.
* Fixed: the changelog itself had been truncated in the 0.4.0 package; history restored.

## 0.4.0 - generics and fewer limitations

* **Generic functions** with type and const parameters (`fn sum<const N: usize>(a: [u32; N])`, `fn max2<T>`), inferred from arguments
  (including array lengths and string capacities) or given explicitly (`fill::<u8, 4>(7)`); one memoized, folded instance per distinct
  argument set; generic recursion and compile-time evaluation work.
* `return` inside loops of inlined functions (unrolled, runtime and nested) via flags.
* Runtime strings are first class: `++` with numbers builds a runtime buffer (`StrBuild`), buffers are cached per emission region,
  `s[i]`, `len`, and `==` work on built strings and between two runtime strings (`BufEq`).
* Tests: 45-program corpus, generics inference/memoization/error tests, fuzzer generates runtime strings and returns inside loops.

## 0.3.0 - more of the program runs in the compiler

**Compile-time evaluation**
* `enum` types and exhaustive `match` (statement and expression forms; ranges, `|` alternatives, wildcard). A match on a
  compile-time value keeps only the selected arm.
* Strings as compile-time data: `s[i]`, `bytes("...")`, string `==`/`!=` (constant vs runtime), `embed("file")` reads a file
  at build time into a `[u8; N]`.
* Reflection: `alignof`, `bits_of`, `offsetof`, `max_of`, `min_of` (usable in array lengths and match patterns).
* Build-time configuration: `-D NAME=VALUE`, `static_assert(cond, "message")`, `static_print`.
* `const` tables built by `pure fn`s (for example a CRC-32 table) and constant arrays indexed at run time now live in read-only data:
  no stack copy, no initialization code, and their value range feeds the range analysis.
* Static evaluation always runs loops to completion (the unroll limit now only governs runtime code size).

**Proofs and optimization**
* Path-sensitive range refinement: facts from `if`, `while`, match arms, `&&`/`||` and guard clauses remove overflow, bounds
  and division checks. `x != 0` is tracked as an excluded value.
* Branch joins keep an interval hull; always-trapping operations become explicit traps with a warning.

**Language**
* `break` and `continue`, implemented with flags (no new IR instructions), correct in unrolled and residual loops, nested loops
  and inside match arms. Assignments are allowed as match-arm statements.

**Backends**
* LLVM: read-only constant tables, runtime string indexing/equality, `apus_memeq`.
* Freestanding runtime: function sections + `--gc-sections`; `hello` is 1,048 bytes on x86-64.

**Testing**
* 41-program golden corpus on VM, reference interpreter, native, and freestanding x86-64/AArch64/RISC-V.
* The differential fuzzer now also generates `break`/`continue`/`match` and exercises narrowing and tables.
* 81 must-reject and 22 must-accept programs; unit tests for the new IR instructions, LLVM lowering and sema.

## 0.2.0
Go rewrite: type checker, partial evaluator with interval analysis, residual IR, VM, LLVM backend, freestanding runtime,
reference interpreter, differential fuzzer, benchmarks.
