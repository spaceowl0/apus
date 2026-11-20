# Apus Language Reference (v0.5)

## 1. Lexical structure

* Comments: `// line` and `/* block */`.
* Identifiers: `[A-Za-z_][A-Za-z0-9_]*`. Keywords: `fn pure io bound const struct enum arena region alloc let mut if else while for in return break continue match static as true false`.
* Integer literals: decimal or `0x` hex, `_` separators allowed (`1_000`, `0xFF_FF`). There are no suffixes; the type comes from context.
* String literals: `"..."` with escapes `\n \t \r \0 \\ \"`.
* Operators by precedence (low to high): `||`, `&&`, `== !=`, `< <= > >=`, `|`, `^`, `&`, `<< >>`, `++`, `+ -`, `* / %`; then prefix `- ! static`, postfix `[i] .f`, and `as T` (binds tighter than every binary operator, so `a as u64 + b as u64` means `(a as u64) + (b as u64)`).
* Compound assignment `+= -= *= /= %= &= |= ^=` is sugar for `x = x op e`.

## 2. Types

| Type | Meaning |
|---|---|
| `u8 u16 u32 u64 usize`, `i8 i16 i32 i64 isize` | fixed-width integers (`usize`/`isize` are 64-bit) |
| `f32`, `f64` | IEEE-754 binary32 / binary64 |
| `bool` | `true` / `false` |
| `Str<N>` | byte string of at most N bytes. A literal has capacity equal to its length. `Str<a>` converts to `Str<b>` when `a <= b`. |
| `StaticStr` | only on `const`/`let`; the capacity is inferred from the initialiser |
| `[T; N]` | fixed array; `N` is a compile-time constant expression (may use `sizeof`, `bits_of`, `max_of`, constants, `-D` overrides) |
| `enum E { A, B = 5, C }` | C-like enumeration, 4 bytes, values default to previous + 1 |
| `struct S { f: T, ... }` | C-layout aggregate, declared before use, no recursion |
| `()` | unit (functions without `->`) |

**Integer literal inference.** An unsuffixed literal has no type until context assigns one:
`let x: u8 = 5`, `a + 2` (adopts the type of `a`), `clamp(5, 0, 10)` (adopts the context).
With no context it defaults to `i32`. A literal that does not fit its type is `E0301`.

**Floating point.** `1.5`, `2.5e-3`, `1_000.25` are float literals; like integer literals they take their type from context
and default to `f64`. An integer literal may stand for a float (`let x: f64 = 2;`) when the value is exactly representable. Floats support
`+ - * /`, unary `-`, the six comparisons, `min`/`max`, `sqrt`, `floor`, `ceil`, `trunc`, `abs`, `is_nan` and casts (`%`, bit operations and
shifts are not defined for floats; `clamp`/`require`/`match` are for integers).

* **Arithmetic never traps**: division by zero gives infinity or NaN, as in IEEE-754. Comparisons are ordered: every comparison with a NaN is
  false except `!=`. `min(a, b)` is `b` if `b < a`, otherwise `a`; `max(a, b)` is `b` if `b > a`, otherwise `a`.
* **`f32` is real single precision**: every operation is rounded to 32 bits (no hidden extended precision), at compile time and at run time.
* **Casts.** Integer to float rounds to nearest-even. Float to float rounds or widens exactly. **Float to integer truncates toward zero and is
  checked**: NaN or a value outside the target range is a compile-time error for constants and a run-time trap (`arithmetic overflow`) otherwise.
* **Text.** `str(x)` prints six fractional digits, computed as round-half-even(|x| * 1e6) split into integer and fractional parts: `1.5` is
  `1.500000`, `-0.0000004` is `0.000000`, NaN is `nan`, infinities are `inf`/`-inf`, and magnitudes of 9e9 or more print `big`/`-big`.
  Output is byte-for-byte identical on every backend.
* **Input.** `io::read_f64()` / `io::read_f32()` parse `-?digits[.digits]` with at most 15 digits in total (value = digits / 10^fraction digits, one
  correctly rounded division); anything else is an `invalid input` trap. There is no exponent syntax in input.
* Constants fold with the same IEEE code the VM executes, no fast-math or fused-multiply-add is ever used, and floating-point values carry no range
  information (the interval analysis is for integers).

**Layout** (used by `sizeof`): natural alignment, C struct rules. `Str<N>` occupies `4 + N` bytes.
Example: `struct Packet { id: u16, payload: [u8; 512] }` is 514 bytes.

## 3. Items

```apus
const NAME: T = const_expr;                  // evaluated at compile time
struct P { x: i32, y: i32 }
arena Frame(4096);                            // fixed-size region of memory
fn f(a: u32, b: u32 in 0..=10) -> u64 { ... }
pure fn g(x: u32) -> u32 bound(rec = 20) { ... }
fn h() io { ... }
```

* `const` initialisers may call `pure fn`s and may reference other constants; they must fold completely (`E0101`).
* **Refinements** `p: T in lo..=hi` make a promise on a parameter. At every call the compiler must prove the argument's value range lies inside it (`E0501` otherwise). Use `clamp`, `require`, or masks to establish ranges.
* `bound(rec = N)` is required on every function that can reach itself (directly or mutually) and caps compile-time unfolding depth (`E0201` if exceeded).
* `main` takes no parameters and returns `()` or an integer type (the process exit status is the value modulo 256).

### Generic functions

```apus
fn max2<T>(a: T, b: T) -> T { if a > b { return a; } return b; }
fn sum<const N: usize>(a: [u32; N]) -> u32 { ... }          // N: the array's length, usable as a value in the body
fn fill<T, const N: usize>(v: T) -> [T; N] { ... }
let a = max2(x, y);          // T inferred from the arguments
let b = fill::<u8, 4>(7);    // or given explicitly (types and constants)
```

* Parameters are inferred by matching declared parameter types against argument types, including array lengths (`[T; N]`)
  and string capacities (`Str<N>`). Untyped integer literals default `T` to `i32` when nothing else decides.
* The compiler creates **one ordinary function per distinct set of generic arguments** (`max2<i32>`, `sum<3>`), memoized, and
  type-checks it with the arguments substituted (like C++ templates). Errors name the instantiation. A generic function that is
  never called is never checked. Generic recursion works (`fact::<T>(n - 1)`), including at compile time.
* Const parameters must have an integer type and the argument must fit it. Failures are `E0313`.

## 4. Effects

```
pure fn   may call only pure fns; no I/O, no alloc
fn        may call pure and plain fns, alloc, region
fn ... io may additionally call io:: builtins and other io fns
```
Violations are `E0102`. Effects are what make compile-time evaluation sound: a `pure fn` with constant arguments is *always* executed by the compiler, and its result memoized.

## 5. Statements

```apus
let x = e;            let mut y: u32 = e;
x = e;   a[i] = e;   s.f = e;   s.arr[i] += 1;
if c { ... } else if d { ... } else { ... }
while c bound(N) { ... }          // N is mandatory
for i in lo..hi { ... }           // half-open; i is immutable
for x in array { ... }            // visits each element of an array (or each byte of a string); x is immutable.
                                  // The array is evaluated once and iterated as a snapshot: changing it inside the body
                                  // does not affect which elements are visited.
region Frame { ... }              // allocations inside are freed at the closing brace
{ ... }                           // plain block / scope
return e;
```

* Assigning to a variable that is not `let mut` is `E0310`.
* **`return` inside loops.** In an inlined function, a `return` in an unrolled loop, a runtime loop or nested loops sets a flag that ends every
  enclosing loop and skips the rest of the function.
* **Loops.** A `for` with constant bounds and at most `-unroll` (default 256) iterations is fully unrolled and constant-folded. Longer or runtime-bounded `for` loops become residual loops; their trip count must have a provable upper bound (`clamp`/`require`/constant), otherwise `E0201`. A `while` is unrolled while its condition is statically known; once it depends on runtime data it becomes a residual loop that traps (`loop bound exceeded`) if more than `N` iterations execute. A loop whose bound is statically exceeded is a compile error.
* **Inside `static` evaluation** (a `pure fn` called with constant arguments) loops always run to completion regardless of `-unroll`; the limit is `-fuel`.

## 6. Expressions and builtins

| Expression | Notes |
|---|---|
| arithmetic `+ - * / %`, bitwise `& \| ^`, shifts `<< >>` | **checked**: overflow, divide-by-zero and shift amount ≥ width are errors if provable, runtime traps otherwise. `<<` discards shifted-out bits (wrapping) but checks the shift amount. |
| `a as T` | checked conversion between integer types; `bool` and enums convert to integers (never the other way) |
| `a ++ b` | string concatenation (strings only; use `str()` for numbers and bools) |
| `str(b)` for `bool` | `"true"` or `"false"` |
| `s[i]` | byte of a string (`u8`). Folds for constant strings; runtime strings (including `++` results with numbers) are bounds-checked against their current length |
| `a == b`, `a != b` | on integers, `bool`, enums and strings (runtime vs runtime, runtime vs constant, and built `++` strings all work) |
| `match x { ... }` | see below |
| `static e` | `e` must be fully known at compile time (`E0101`) |
| `static_assert(c)`, `static_assert(c, "message")` | compile-time assertion (`E0308`) |
| `static_print(s)` | prints a compile-time string as a build note (`note: file:line:col: ...`) |
| `clamp(x, lo, hi)` | saturate; `lo`/`hi` constants. Result range is `[lo, hi]` |
| `require(x, lo, hi)` | trap (`require failed`) when outside; success path knows the range |
| `min(a, b)`, `max(a, b)` | integers or floats |
| `sqrt(x)`, `floor(x)`, `ceil(x)`, `trunc(x)`, `is_nan(x)` | floating point only (`is_nan` returns `bool`) |
| `abs(x)` | signed integer or float, same type as `x`; traps (`arithmetic overflow`) for `min_of(T)`; unsigned is `E0305` |
| `str(n)` | text of an integer, or of a float (six fractional digits, see above) |
| `len(x)` | byte length of a string, element count of an array |
| `bytes("...")` | the bytes of a compile-time string as `[u8; N]` |
| `embed("path")` | contents of a file read **at build time** as `[u8; N]` (1 B to 1 MiB; path relative to the source file, may not escape its directory) |
| `sizeof(T)`, `alignof(T)`, `bits_of(T)`, `offsetof(S, field)` | layout facts as compile-time `u64` |
| `max_of(T)`, `min_of(T)` | limits of an integer type, as a value of type `T` |
| `alloc(Arena, bytes)` | bump-allocate; size must be provably bounded (`E0412`) |
| `io::println(s)`, `io::print(s)` | write a string to stdout |
| `io::read_line(N)` | read a line (at most N bytes kept, rest of the line discarded) → `Str<N>` |
| `io::read_u32()`, `read_u64`, `read_i32`, `read_i64` | parse a decimal token; malformed/out-of-range input traps (`invalid input`) |
| `io::read_f32()`, `io::read_f64()` | parse a decimal number (see Floating point) |

### `match`

```apus
match n {                                   // statement form: arms are blocks, one statement, or break/continue/return
    min_of(i32)..=-1 => { return -1; }
    0 | 1            => count += 1,
    2..=9            => break,
    _                => {}
}
let level = match mode { Mode::Fast => 1, Mode::Safe => 2, Mode::Debug => 3 };   // expression form
```

* The scrutinee is an integer, `bool` or enum. Patterns are `_`, a constant, a range `lo..=hi` or alternatives joined by `|`.
* **Exhaustiveness is checked** (`E0309`): enums must cover every variant, integers must cover the whole type range (ranges are unioned), `bool` both values, or use `_`.
* A match on a **compile-time value selects one arm during compilation**; the others generate no code. On a runtime value it becomes a chain of comparisons, and inside each arm the compiler knows the range it matched (`0..=3 => table[x]` needs no bounds check).
* The expression form needs a scalar result when the scrutinee is a runtime value.

### `break` and `continue`

Allowed in `while` and `for` (innermost loop only). In loops that are unrolled at compile time they simply stop the unrolling; in runtime loops they cost one flag variable.

### Build-time configuration

`apus build|run|ir|... -D NAME=VALUE` overrides a typed integer or `bool` `const` before anything else is evaluated, so it can change array lengths, enum selections, and which branches exist at all:

```sh
apus run -D DEBUG=true -D BUF=32 examples/config.apus
```
Unknown names, non-integer constants and out-of-range values are `E0312`.

### Compile-time guarantees you can rely on

* A `pure fn` called with constant arguments always runs in the compiler, however long its loops (bounded by `-fuel`), and results are memoized.
* A `const` initializer may call such functions: `const TABLE: [u32; 256] = make_table();` builds the table while compiling.
* Constant arrays that are indexed at run time become **read-only data**: no copy on the stack, no initialization code.
* **Range knowledge is path-sensitive.** Inside `if x > 0 { ... }`, `x != 0 && 100 / x > 1`, a match arm, a loop body guarded by `i < 10`, or after a guard clause `if x < 0 { return; }`, the compiler uses the condition to remove checks.

### Runtime traps
`arithmetic overflow`, `division by zero`, `index out of bounds`, `loop bound exceeded`, `require failed`,
`shift amount out of range`, `invalid input`. A trap flushes stdout, prints `apus: runtime trap: <reason>` to stderr
and exits with status **101**. The compiler removes every trap it can prove impossible.

## 7. Memory model

* No heap, no GC, no dynamic allocation: storage is stack variables, fixed arrays/structs/strings, and arenas.
* `arena A(N)` reserves `N` bytes. `alloc(A, size)` bumps a cursor; `region A { ... }` restores it on exit.
* The compiler computes the worst-case live bytes along every path, using value ranges for sizes. A loop body's net allocation is multiplied by its trip bound. If the maximum exceeds `N`, compilation fails with `E0412`. `apus report` prints the proven high-water mark.

## 8. Error catalogue

| Code | Meaning |
|---|---|
| E0001 / E0002 | lexical / syntax error |
| E0003 | missing or malformed `main` |
| E0004 | unknown or duplicate name, type, field, arena |
| E0005 | wrong number of arguments |
| E0101 | `static`/`const` expression is not a compile-time value |
| E0102 | effect violation (`pure`/`io`) |
| E0103 | compile-time fuel exhausted |
| E0201 | missing or exceeded loop/recursion bound |
| E0301 | overflow or value out of range, provable at compile time |
| E0302 | `++` applied to non-strings |
| E0303 / E0304 | division by zero / index out of range (constant) |
| E0305 | type mismatch |
| E0306 | function may finish without returning |
| E0307 | invalid compile-time constant (array length, capacity) |
| E0308 | `static_assert` or `require` fails at compile time |
| E0309 | non-exhaustive `match`, or an invalid `embed` (missing/escaping/empty/too large file) |
| E0310 | assignment to immutable variable |
| E0311 | `break` / `continue` outside a loop |
| E0312 | invalid `-D` override |
| E0313 | generics: cannot infer a parameter, conflicting arguments, wrong argument count, const out of range |
| E0412 | arena budget exceeded or allocation size unbounded |
| E0501 | refinement not provable |
| E0502 | construct not supported yet (never a silent miscompile): dynamic writes into arrays of aggregates that contain arrays or strings beyond 256 leaves, assignments with two dynamic indices into memory-backed rows of non-scalar arrays, ... |

Warnings: an operation that traps on *every* execution of its path (for example `x - 100` where `x ≤ 5` in `u32`) compiles to an explicit trap on that path and is reported in `Result.Warnings`.
