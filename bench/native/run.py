#!/usr/bin/env python3
"""Native benchmark runner: builds each Apus program (via `apus build`) and an
equivalent C program with the same clang, checks they print the same thing, and
reports run time (median of N runs) and binary size.

usage: python3 bench/native/run.py [--apus path/to/apus] [--runs 5] [--target triple]
"""
import argparse, os, statistics, subprocess, sys, tempfile, time

HERE = os.path.dirname(os.path.abspath(__file__))
BENCHES = [  # name, stdin, extra C args
    ("hello", "", []),
    ("fib", "", []),
    ("popcount", "400000000\n", []),
    ("collatz", "3000000\n", []),
    ("sieve", "100000\n", []),
    ("harmonic", "300000000\n", []),
    ("mandelbrot", "1200\n800\n", []),
    ("crc32", "The quick brown fox jumps over the lazy dog. " * 4 + "\n300000\n", []),
]

def run(cmd, stdin=""):
    t = time.perf_counter()
    p = subprocess.run(cmd, input=stdin.encode(), capture_output=True)
    return time.perf_counter() - t, p.stdout.decode(), p.returncode

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--apus", default=os.path.join(HERE, "..", "..", "apus"))
    ap.add_argument("--runs", type=int, default=5)
    ap.add_argument("--clang", default="clang")
    ap.add_argument("--opt", default="2")
    ap.add_argument("--only", default="")
    a = ap.parse_args()
    tmp = tempfile.mkdtemp(prefix="apus-bench-")
    rows = []
    for name, stdin, _ in BENCHES:
        if a.only and name != a.only:
            continue
        ab, cb = os.path.join(tmp, name + ".apus.bin"), os.path.join(tmp, name + ".c.bin")
        t0 = time.perf_counter()
        r = subprocess.run([a.apus, "build", "-O", a.opt, "-o", ab, os.path.join(HERE, name + ".apus")], capture_output=True)
        build_apus = time.perf_counter() - t0
        if r.returncode:
            sys.exit(f"apus build failed for {name}:\n{r.stderr.decode()}")
        subprocess.run([a.clang, "-O" + a.opt, "-o", cb, os.path.join(HERE, name + ".c")], check=True)
        cargs = [cb, "42"] if name == "fib" else [cb]
        times = {"apus": [], "c": []}
        outs = {}
        for _ in range(a.runs):
            for key, cmd in (("apus", [ab]), ("c", cargs)):
                dt, out, rc = run(cmd, stdin)
                times[key].append(dt)
                outs[key] = out
        status = "ok" if outs["apus"] == outs["c"] else "MISMATCH"
        rows.append((name, statistics.median(times["apus"]), statistics.median(times["c"]),
                     os.path.getsize(ab), os.path.getsize(cb), build_apus, status, outs["apus"].strip()))
    print(f"{'bench':10} {'apus (s)':>10} {'C (s)':>10} {'apus/C':>8} {'apus size':>10} {'C size':>8}  output")
    for n, ta, tc, sa, sc, bt, st, out in rows:
        ratio = ta / tc if tc > 0 else float('nan')
        print(f"{n:10} {ta:10.4f} {tc:10.4f} {ratio:8.2f} {sa:10d} {sc:8d}  {st}: {out[:30]}")

if __name__ == "__main__":
    main()
