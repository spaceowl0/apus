# Architecture

```
source ──lex/parse──▶ AST ──sema──▶ typed AST ──peval──▶ residual IR ──opt──▶ IR
                                                                         ├──▶ VM         (apus run, tests)
                                                                         └──▶ LLVM IR ──clang──▶ executable
                                          typed AST ──▶ reference interpreter (oracle for tests only)
```

## sema
Resolves names and types, infers integer-literal types bidirectionally (`Untyped` is resolved by the
other operand or the expected type), enforces `mut`, effects (`pure`/`io`), return paths, and finds every
function on a call-graph cycle (each needs `bound(rec = N)`). Array lengths and `Str<N>` capacities are
evaluated by a small constant evaluator. All errors carry source positions.

## peval: online partial evaluation with abstract values
The evaluator executes `main` symbolically. Each expression yields an *abstract value* `V`:

| kind | meaning |
|---|---|
| Const | exact value known at compile time (`*big.Int` / string) |
| Dyn | runtime scalar in IR variable `v`, with a proven interval `[lo, hi]` |
| Dyn string | runtime buffer + length variable (capacity from its type) |
| Rope | string made of static runs and dynamic holes; lowers to one write per run/hole |
| Arr / Struct | aggregate whose elements are tracked individually (scalar replacement) |
| Mem | array that lives in IR memory (after dynamic indexing/writes) |

Key mechanisms:

* **Folding**: operations on Const values run in the compiler with big-integer arithmetic; overflow is `E0301`.
* **Interval analysis**: every Dyn carries a range. `+ - * / % & | ^ << >> min max clamp require` and casts propagate
  ranges. A check is emitted only if the range cannot prove the operation safe, so `clamp(x,0,1000) * clamp(y,0,1000)`
  is unchecked while `x * y` on raw `u32` is checked. Comparisons whose outcome the ranges decide fold to constants,
  which removes whole branches.
* **Pure-call memoization**: a `pure fn` with all-constant arguments is evaluated once per distinct argument tuple.
  Naive recursive `fib(90)` costs about 90 evaluations instead of ~10¹⁸.
* **Static mode**: while evaluating such a call, loops always run to completion (bounded by `-fuel`);
  the unroll limit only governs residual code size.
* **Branches**: a constant condition evaluates one side. A dynamic `if` first *materializes* every variable assigned in
  either branch into an IR slot (aggregates field by field; arrays move to memory), evaluates both branches into separate
  instruction lists, and joins ranges with an interval hull.
* **Loops**: unroll while the trip count is static and small; otherwise materialize assigned variables, *widen* their ranges
  to the full type range (sound for any iteration), evaluate the body once, and emit a residual `loop`. For `for` loops over a
  bounded range the trip bound is derived from the range, so no runtime bound check exists.
* **Inlining and early return**: every call is inlined. A `return` on a dynamic path inside an inlined function writes a
  result slot and a "returned" flag; later statements are guarded by it.
* **Arena proof**: allocation sizes must have a finite upper bound; live bytes are tracked along the evaluated paths,
  `region` restores them, and loop bodies multiply their net allocation by the trip bound.
* **Path-sensitive refinement** (`refine.go`): entering a branch, a `while` body, a match arm or the right side of `&&`/`||`, the
  condition is mined for facts (`x < c`, `x == y`, `!(...)`, conjunctions) and the ranges of the variables involved are narrowed;
  `x != c` for a value strictly inside the interval records an *excluded value*, enough to drop a division-by-zero check.
  Knowledge is undone when the branch ends. When exactly one branch exits (a guard clause) the continuing path keeps the other
  condition's facts.
* **Constant tables**: a fully constant scalar array indexed at run time is interned (deduplicated by content) as a read-only
  `ir.Table`; the backend emits it as a private constant global and loads from it. A `bytes("...")`, `embed(...)` or `const` table
  therefore costs data, not code, and the table's value hull feeds range analysis of the loaded value.
* **`match`** lowers to a chain of synthetic `if` statements over a (possibly hidden) scrutinee variable, so a constant scrutinee
  folds to one arm and a runtime scrutinee inherits materialization, range joins, narrowing and guard clauses for free.
* **`break` / `continue`** use flag variables rather than new IR instructions: a dynamic `break` sets a flag, the rest of the
  enclosing statement lists run under a guard (a dynamic `if` with the usual variable merging), unrolled loops guard their
  remaining iterations, and residual loops fold the flag into their condition. Static `break`s just end the unrolling.
* **Generics** are resolved in the checker: arguments are inferred by unifying parameter type expressions with argument types
  (`[T; N]`, `Str<N>`), the function AST is deep-cloned (`syntax.CloneFn`), registered under a mangled name such as `sum<3>`
  *before* its body is checked (so recursion finds it), and checked with type/const substitutions active. After that an instance is an
  ordinary function, so the evaluator, reference interpreter and backends need no knowledge of generics.
* **`return` inside loops** reuse the flag mechanism: a dynamic `return` sets the result slot and the "returned" flag; unrolled loops
  guard their remaining iterations on it, residual loops fold it into their condition, and the statement-list guard skips the rest.
* **Runtime strings**: a `++` rope with runtime parts is materialized on demand into a buffer by one `StrBuild` instruction (static runs,
  copied buffers, decimal numbers); the buffer is cached for the emission region in which it was built, so a string used five times is
  built once. This enables indexing, `len` with numeric holes and `==` between two runtime strings (`BufEq`).
* **Floating point** values live in the same 64-bit slots as IEEE bit patterns (`f32` zero-extended in the low 32 bits), with `FBin`/`FUn`/`FCvt`
  instructions. **All float semantics are defined once**, in `ir/float.go` (`EvalFBin`, `EvalFUn`, `FloatToInt`, `IntToFloat`, `FormatFloat`,
  `ReadFloatFrom`): the VM executes them and the evaluator folds constants with the very same functions. The LLVM backend emits plain
  `fadd`/`fcmp`/... with **no fast-math or contraction flags**, and the C runtime reimplements formatting and parsing with identical algorithms
  (rounding via the add-2^52 trick, which equals round-half-even). `floor`/`ceil`/`trunc` are provided by the runtime itself (marked `optnone` so the
  optimizer cannot turn `truncf` into a call to itself). The reference interpreter implements float arithmetic independently with native Go
  operators and `big.Float`, so it is a real cross-check. Floats carry a vacuous `[0,0]` range so the shared interval bookkeeping needs no special cases.
* **Build-time facts**: `-D` overrides constants in both the checker (array lengths) and the evaluator; `embed` is resolved
  and sandboxed by the checker; `static_print` and warnings are collected in the result.
* **Determinism**: map iteration is always sorted, so the same source always produces the identical IR (tested).
* **Soundness valves**: an operation that always traps becomes an explicit `trap` + warning instead of producing an empty range.

## IR
A structured (no gotos) register IR over 64-bit canonical slots: signed values sign-extended, unsigned zero-extended.
Instructions: `mov bin neg not cast clamp load tload bufload streq store arrinit arrfill arrcopy read_line read_int write* alloc mark reset if loop ret trap`.
`Bin`/`Neg`/`Load` carry a `Check` flag; unchecked forms are the ones the evaluator proved safe.
`Optimize` removes dead pure instructions (never checked ones, never I/O) and renumbers variables densely, which also gives
the exact stack-frame size reported by `apus report`.

The VM (`ir.Run`) implements the same checked semantics bit-for-bit (using `math/bits`), always checking even when the IR
claims safety. Any compiler unsoundness therefore shows up as a VM trap that the reference interpreter does not have.

## LLVM backend
Each variable is an `alloca i64` (mem2reg promotes them), arrays use their true element width, checked ops lower to
`llvm.{s,u}{add,sub,mul}.with.overflow.i64` (64-bit) or exact i64 arithmetic plus a range test (narrower types), and traps call
`apus_trap(code)`. The C runtime (`internal/llvm/rt_c.txt`) is compiled together with the module. With `-freestanding` it uses
raw syscalls (`syscall`/`svc`/`ecall`) and a naked `_start`, so no libc, no sysroot and 2-3 KB static executables.

## Testing strategy
1. **Reference interpreter** (`internal/ref`): ~470 lines, deliberately naive, same checked semantics, no optimisation.
2. **Differential fuzzing** (`internal/e2e/fuzz_test.go`): random typed programs → compiler → compare stdout and trap against the
   reference interpreter over several random inputs including boundary values; a subset is also built natively.
3. **Golden corpus** (`testdata/programs/*.apus`): `// run: "<stdin>" => "<stdout>" exit=N trap=NAME` directives, plus
   `insts<=N` (residual size) and `arena:` (proven high-water) assertions. Each program runs on VM, reference interpreter,
   native (hosted libc) and freestanding x86-64/AArch64/RISC-V (QEMU).
4. **Negative tests**: 81 programs that must be rejected with an exact code; 22 that must be accepted; embed sandbox and `-D` tests.
5. **Unit tests** for the primitives (checked arithmetic table, cast fit, parsing of input tokens, optimizer, layout, lexer, parser, sema typing, LLVM shape).
