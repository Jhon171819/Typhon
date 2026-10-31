from __future__ import annotations

import argparse
import csv
import html
import os
import statistics
import subprocess
import sys
import time
from dataclasses import dataclass
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
RESULTS_DIR = ROOT / "benchmarks" / "results"
BIN_DIR = ROOT / "benchmarks" / "bin"
TYPHON_EXE = BIN_DIR / ("typhon.exe" if os.name == "nt" else "typhon")
PY_CASE = ROOT / "benchmarks" / "cases" / "python_math.py"
TY_CASE = ROOT / "benchmarks" / "cases" / "python_math.ty"


@dataclass
class Result:
    label: str
    median_ms: float
    output: str


def main() -> int:
    parser = argparse.ArgumentParser(description="Compare cold vs warm Typhon Python bridge startup.")
    parser.add_argument("--runs", type=int, default=9)
    parser.add_argument("--warmups", type=int, default=1)
    args = parser.parse_args()

    RESULTS_DIR.mkdir(parents=True, exist_ok=True)
    build_typhon()
    python = find_python()

    cases = [
        ("Python direct", [python, str(PY_CASE)]),
        ("Typhon cold bridge", [str(TYPHON_EXE), "run", str(TY_CASE)]),
        ("Typhon warm bridge", [str(TYPHON_EXE), "--warm-python", "run", str(TY_CASE)]),
    ]
    results = [measure(label, cmd, args.runs, args.warmups) for label, cmd in cases]
    expected = results[0].output
    for result in results[1:]:
        if result.output != expected:
            raise RuntimeError(f"output mismatch for {result.label}: {result.output!r} != {expected!r}")

    for result in results:
        print(f"{result.label:<20} {result.median_ms:8.3f} ms")

    write_csv(results)
    write_svg(results)
    write_report(results, args.runs, args.warmups)
    print(f"\nWrote {RESULTS_DIR / 'python_bridge_warmup.svg'}")
    print(f"Wrote {RESULTS_DIR / 'python_bridge_warmup.csv'}")
    print(f"Wrote {RESULTS_DIR / 'python_bridge_warmup.md'}")
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


def find_python() -> str:
    candidates = [
        ROOT / ".venv" / "Scripts" / "python.exe",
        ROOT / ".venv" / "bin" / "python",
        Path(sys.executable),
    ]
    for candidate in candidates:
        if candidate.exists():
            return str(candidate)
    return sys.executable


def measure(label: str, cmd: list[str], runs: int, warmups: int) -> Result:
    output = run_once(cmd)
    for _ in range(warmups):
        run_once(cmd)
    samples = [time_command(cmd) for _ in range(runs)]
    return Result(label=label, median_ms=statistics.median(samples), output=output)


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


def write_csv(results: list[Result]) -> None:
    with (RESULTS_DIR / "python_bridge_warmup.csv").open("w", newline="", encoding="utf-8") as handle:
        writer = csv.writer(handle)
        writer.writerow(["case", "median_ms"])
        for result in results:
            writer.writerow([result.label, f"{result.median_ms:.3f}"])


def write_report(results: list[Result], runs: int, warmups: int) -> None:
    lines = [
        "# Python Bridge Warmup Benchmark",
        "",
        "Median cold-process runtime for a real Python bridge case.",
        "",
        f"- Timed runs per case: `{runs}`",
        f"- Warmups per case: `{warmups}`",
        "",
        "![Python bridge warmup benchmark](python_bridge_warmup.svg)",
        "",
        "| Case | Median ms |",
        "|---|---:|",
    ]
    for result in results:
        lines.append(f"| {result.label} | {result.median_ms:.3f} |")
    lines.extend(
        [
            "",
            "Notes:",
            "- `Typhon cold bridge` starts Python when the first Python import executes.",
            "- `Typhon warm bridge` starts Python asynchronously during VM setup using `--warm-python`.",
            "- For tiny programs, warmup can only hide the part of Python startup that overlaps parse/check/compile.",
            "",
        ]
    )
    (RESULTS_DIR / "python_bridge_warmup.md").write_text("\n".join(lines), encoding="utf-8")


def write_svg(results: list[Result]) -> None:
    width = 860
    height = 330
    left = 190
    right = 60
    top = 88
    row = 54
    chart_width = width - left - right
    max_value = nice_max(max(result.median_ms for result in results))
    parts = [
        f'<svg xmlns="http://www.w3.org/2000/svg" width="{width}" height="{height}" viewBox="0 0 {width} {height}">',
        "<style>",
        "text{font-family:Segoe UI,Arial,sans-serif;fill:#1f2937}",
        ".title{font-size:26px;font-weight:700}",
        ".subtitle{font-size:14px;fill:#64748b}",
        ".label{font-size:14px}",
        ".value{font-size:13px;fill:#334155}",
        ".grid{stroke:#e2e8f0;stroke-width:1}",
        "</style>",
        '<rect width="100%" height="100%" fill="#f8fafc"/>',
        '<text class="title" x="34" y="42">Python Bridge Warmup</text>',
        '<text class="subtitle" x="34" y="66">Median cold-process runtime, milliseconds. Lower is better.</text>',
    ]
    for i in range(6):
        value = max_value * i / 5
        x = left + chart_width * i / 5
        parts.append(f'<line class="grid" x1="{x:.1f}" y1="{top-20}" x2="{x:.1f}" y2="{top+row*len(results)}"/>')
        parts.append(f'<text class="subtitle" x="{x:.1f}" y="{top+row*len(results)+26}" text-anchor="middle">{value:.0f}</text>')
    colors = ["#3776ab", "#00add8", "#14b8a6"]
    for index, result in enumerate(results):
        y = top + index * row
        width_value = chart_width * result.median_ms / max_value
        parts.append(f'<text class="label" x="{left-14}" y="{y+24}" text-anchor="end">{html.escape(result.label)}</text>')
        parts.append(f'<rect x="{left}" y="{y+6}" width="{width_value:.1f}" height="24" rx="5" fill="{colors[index]}"/>')
        parts.append(f'<text class="value" x="{left+width_value+8:.1f}" y="{y+23}">{result.median_ms:.2f} ms</text>')
    parts.append("</svg>")
    (RESULTS_DIR / "python_bridge_warmup.svg").write_text("\n".join(parts), encoding="utf-8")


def nice_max(value: float) -> float:
    if value <= 100:
        return 100
    return ((int(value) // 100) + 1) * 100


if __name__ == "__main__":
    raise SystemExit(main())
