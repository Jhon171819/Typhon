from __future__ import annotations

import argparse
import csv
import importlib.util
import os
import py_compile
import shutil
import statistics
import subprocess
import sys
import time
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
RESULTS_DIR = ROOT / "benchmarks" / "results"
BIN_DIR = ROOT / "benchmarks" / "bin"
TYPHON_EXE = BIN_DIR / ("typhon.exe" if os.name == "nt" else "typhon")
REQUESTS_TY = ROOT / "benchmarks" / "cases" / "python_requests.ty"


def main() -> int:
    parser = argparse.ArgumentParser(description="Precompile Python libs to .pyc and benchmark Typhon bridge impact.")
    parser.add_argument("modules", nargs="*", default=["requests"], help="Python modules/packages to precompile.")
    parser.add_argument("--runs", type=int, default=7)
    parser.add_argument("--warmups", type=int, default=1)
    parser.add_argument("--clean-first", action="store_true", help="Remove __pycache__ directories for selected packages before measuring.")
    args = parser.parse_args()

    RESULTS_DIR.mkdir(parents=True, exist_ok=True)
    build_typhon()

    module_paths = [resolve_module_path(module) for module in args.modules]
    if args.clean_first:
        for path in module_paths:
            remove_pycache(path)

    before = measure_typhon_requests(args.runs, args.warmups)
    compiled = 0
    for path in module_paths:
        compiled += precompile_path(path)
    after = measure_typhon_requests(args.runs, args.warmups)

    write_results(args.modules, compiled, before, after)
    print(f"Compiled {compiled} Python files.")
    print(f"Typhon requests bridge before .pyc: {before:.3f} ms")
    print(f"Typhon requests bridge after  .pyc: {after:.3f} ms")
    print(f"Wrote {RESULTS_DIR / 'python_pyc_bridge.csv'}")
    print(f"Wrote {RESULTS_DIR / 'python_pyc_bridge.md'}")
    return 0


def build_typhon() -> None:
    BIN_DIR.mkdir(parents=True, exist_ok=True)
    subprocess.run(
        ["go", "build", "-o", str(TYPHON_EXE), "./cmd/typhon"],
        cwd=ROOT,
        check=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )


def resolve_module_path(module: str) -> Path:
    spec = importlib.util.find_spec(module)
    if spec is None:
        raise RuntimeError(f"cannot find module {module!r}")
    if spec.submodule_search_locations:
        return Path(next(iter(spec.submodule_search_locations)))
    if spec.origin is None:
        raise RuntimeError(f"module {module!r} has no filesystem origin")
    return Path(spec.origin)


def remove_pycache(path: Path) -> None:
    roots = [path] if path.is_dir() else [path.parent]
    for root in roots:
        for cache in root.rglob("__pycache__"):
            shutil.rmtree(cache, ignore_errors=True)


def precompile_path(path: Path) -> int:
    files = list(path.rglob("*.py")) if path.is_dir() else [path]
    count = 0
    for file in files:
        try:
            py_compile.compile(str(file), doraise=True, optimize=0)
            count += 1
        except py_compile.PyCompileError:
            continue
    return count


def measure_typhon_requests(runs: int, warmups: int) -> float:
    cmd = [str(TYPHON_EXE), "run", str(REQUESTS_TY)]
    expected = run_once(cmd)
    if not expected.strip():
        raise RuntimeError("requests bridge benchmark produced empty output")
    for _ in range(warmups):
        run_once(cmd)
    samples = [time_command(cmd) for _ in range(runs)]
    return statistics.median(samples)


def run_once(cmd: list[str]) -> str:
    completed = subprocess.run(
        cmd,
        cwd=ROOT,
        check=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )
    return completed.stdout.replace("\r\n", "\n")


def time_command(cmd: list[str]) -> float:
    started = time.perf_counter()
    run_once(cmd)
    return (time.perf_counter() - started) * 1000.0


def write_results(modules: list[str], compiled: int, before: float, after: float) -> None:
    with (RESULTS_DIR / "python_pyc_bridge.csv").open("w", newline="", encoding="utf-8") as handle:
        writer = csv.writer(handle)
        writer.writerow(["modules", "compiled_files", "before_ms", "after_ms", "after_div_before"])
        writer.writerow([" ".join(modules), compiled, f"{before:.3f}", f"{after:.3f}", f"{after / before:.3f}"])

    delta = after - before
    pct = (delta / before) * 100 if before else 0
    lines = [
        "# Python `.pyc` Bridge Benchmark",
        "",
        f"- Modules: `{' '.join(modules)}`",
        f"- Compiled files: `{compiled}`",
        "",
        "| Scenario | Median ms |",
        "|---|---:|",
        f"| Before `.pyc` precompile | {before:.3f} |",
        f"| After `.pyc` precompile | {after:.3f} |",
        "",
        f"Delta: `{delta:+.3f} ms` (`{pct:+.2f}%`).",
        "",
        "Notes:",
        "- `.pyc` can reduce Python module parse/compile time.",
        "- It does not remove CPython startup, subprocess IPC, import execution, or Typhon/Python value marshalling.",
        "- Results are noisy for short CLI runs; rerun with more samples for higher confidence.",
        "",
    ]
    (RESULTS_DIR / "python_pyc_bridge.md").write_text("\n".join(lines), encoding="utf-8")


if __name__ == "__main__":
    raise SystemExit(main())
