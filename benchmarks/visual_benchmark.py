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
CASES_DIR = ROOT / "benchmarks" / "cases"
RESULTS_DIR = ROOT / "benchmarks" / "results"
BIN_DIR = ROOT / "benchmarks" / "bin"
TYPHON_EXE = BIN_DIR / ("typhon.exe" if os.name == "nt" else "typhon")


@dataclass(frozen=True)
class Case:
    name: str
    label: str

    @property
    def python_path(self) -> Path:
        return CASES_DIR / f"{self.name}.py"

    @property
    def typhon_path(self) -> Path:
        return CASES_DIR / f"{self.name}.ty"


@dataclass
class Result:
    case: Case
    python_ms: float
    typhon_ms: float
    ratio: float
    python_output: str
    typhon_output: str


CASES = [
    Case("hello_print", "print"),
    Case("function_string", "function + string"),
    Case("list_loop", "list loop"),
    Case("class_fields", "class fields"),
    Case("python_json", "json native shim"),
    Case("python_math", "Python math bridge"),
    Case("python_requests", "requests bridge"),
]


def main() -> int:
    parser = argparse.ArgumentParser(description="Generate a visual Python vs Typhon benchmark.")
    parser.add_argument("--runs", type=int, default=9, help="Timed runs per case.")
    parser.add_argument("--warmups", type=int, default=2, help="Untimed warmup runs per case.")
    args = parser.parse_args()

    RESULTS_DIR.mkdir(parents=True, exist_ok=True)
    build_typhon()
    python = find_python()

    results: list[Result] = []
    for case in CASES:
        result = benchmark_case(case, python, args.runs, args.warmups)
        results.append(result)
        print(
            f"{case.label:<20} Python {result.python_ms:8.3f} ms | "
            f"Typhon {result.typhon_ms:8.3f} ms | ratio {result.ratio:6.2f}x"
        )

    write_csv(results)
    write_svg(results)
    write_report(results, args.runs, args.warmups)
    print(f"\nWrote {RESULTS_DIR / 'python_vs_typhon.svg'}")
    print(f"Wrote {RESULTS_DIR / 'python_vs_typhon.csv'}")
    print(f"Wrote {RESULTS_DIR / 'python_vs_typhon.md'}")
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


def benchmark_case(case: Case, python: str, runs: int, warmups: int) -> Result:
    python_cmd = [python, str(case.python_path)]
    typhon_cmd = [str(TYPHON_EXE), "run", str(case.typhon_path)]

    python_output = run_once(python_cmd)
    typhon_output = run_once(typhon_cmd)
    if python_output != typhon_output:
        raise RuntimeError(
            f"output mismatch for {case.name}\n"
            f"Python: {python_output!r}\n"
            f"Typhon: {typhon_output!r}"
        )

    for _ in range(warmups):
        run_once(python_cmd)
        run_once(typhon_cmd)

    python_times = [time_command(python_cmd) for _ in range(runs)]
    typhon_times = [time_command(typhon_cmd) for _ in range(runs)]
    python_ms = statistics.median(python_times)
    typhon_ms = statistics.median(typhon_times)
    ratio = typhon_ms / python_ms if python_ms else 0.0
    return Result(case, python_ms, typhon_ms, ratio, python_output, typhon_output)


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
    start = time.perf_counter()
    run_once(cmd)
    return (time.perf_counter() - start) * 1000.0


def write_csv(results: list[Result]) -> None:
    with (RESULTS_DIR / "python_vs_typhon.csv").open("w", newline="", encoding="utf-8") as handle:
        writer = csv.writer(handle)
        writer.writerow(["case", "label", "python_ms_median", "typhon_ms_median", "typhon_div_python"])
        for result in results:
            writer.writerow(
                [
                    result.case.name,
                    result.case.label,
                    f"{result.python_ms:.3f}",
                    f"{result.typhon_ms:.3f}",
                    f"{result.ratio:.3f}",
                ]
            )


def write_report(results: list[Result], runs: int, warmups: int) -> None:
    lines = [
        "# Python vs Typhon Visual Benchmark",
        "",
        "Median cold-process runtime in milliseconds. Lower is better.",
        "",
        f"- Timed runs per case: `{runs}`",
        f"- Warmups per case: `{warmups}`",
        f"- Typhon binary: `{TYPHON_EXE}`",
        "",
        "![Python vs Typhon benchmark](python_vs_typhon.svg)",
        "",
        "| Case | Python ms | Typhon ms | Typhon / Python |",
        "|---|---:|---:|---:|",
    ]
    for result in results:
        lines.append(
            f"| {result.case.label} | {result.python_ms:.3f} | "
            f"{result.typhon_ms:.3f} | {result.ratio:.2f}x |"
        )
    lines.extend(
        [
            "",
            "Notes:",
            "- These are CLI process benchmarks, so startup cost is included.",
            "- `json native shim` is served by the Go VM without starting Python.",
            "- `Python math bridge` intentionally starts the Python subprocess bridge from Typhon.",
            "- `requests bridge` imports a real third-party Python package through the bridge.",
            "- Native Typhon code and Python bridge calls have different performance profiles.",
            "",
        ]
    )
    (RESULTS_DIR / "python_vs_typhon.md").write_text("\n".join(lines), encoding="utf-8")


def write_svg(results: list[Result]) -> None:
    width = 1080
    height = 640
    left = 220
    right = 70
    top = 90
    bottom = 95
    chart_width = width - left - right
    chart_height = height - top - bottom
    row_height = chart_height / len(results)
    max_value = max(max(result.python_ms, result.typhon_ms) for result in results)
    max_value = nice_max(max_value)

    parts = [
        f'<svg xmlns="http://www.w3.org/2000/svg" width="{width}" height="{height}" viewBox="0 0 {width} {height}">',
        "<style>",
        "text{font-family:Segoe UI,Arial,sans-serif;fill:#1f2937}",
        ".title{font-size:28px;font-weight:700}",
        ".subtitle{font-size:14px;fill:#64748b}",
        ".label{font-size:14px}",
        ".value{font-size:13px;fill:#334155}",
        ".axis{stroke:#cbd5e1;stroke-width:1}",
        ".grid{stroke:#e2e8f0;stroke-width:1}",
        "</style>",
        '<rect width="100%" height="100%" fill="#f8fafc"/>',
        '<text class="title" x="40" y="44">Python vs Typhon Simple Usage Benchmark</text>',
        '<text class="subtitle" x="40" y="68">Median cold-process runtime, milliseconds. Lower is better.</text>',
        f'<line class="axis" x1="{left}" y1="{height-bottom}" x2="{width-right}" y2="{height-bottom}"/>',
    ]

    ticks = 5
    for i in range(ticks + 1):
        value = max_value * i / ticks
        x = left + chart_width * i / ticks
        parts.append(f'<line class="grid" x1="{x:.1f}" y1="{top}" x2="{x:.1f}" y2="{height-bottom}"/>')
        parts.append(f'<text class="subtitle" x="{x:.1f}" y="{height-bottom+25}" text-anchor="middle">{value:.0f}</text>')

    for index, result in enumerate(results):
        y = top + index * row_height + 12
        label_y = y + row_height * 0.43
        py_y = y + 10
        ty_y = y + 34
        bar_height = 18
        python_width = chart_width * result.python_ms / max_value
        typhon_width = chart_width * result.typhon_ms / max_value
        label = html.escape(result.case.label)
        parts.append(f'<text class="label" x="{left-15}" y="{label_y:.1f}" text-anchor="end">{label}</text>')
        parts.append(f'<rect x="{left}" y="{py_y:.1f}" width="{python_width:.1f}" height="{bar_height}" rx="4" fill="#3776ab"/>')
        parts.append(f'<rect x="{left}" y="{ty_y:.1f}" width="{typhon_width:.1f}" height="{bar_height}" rx="4" fill="#00add8"/>')
        parts.append(f'<text class="value" x="{left+python_width+8:.1f}" y="{py_y+14:.1f}">{result.python_ms:.2f} ms</text>')
        parts.append(f'<text class="value" x="{left+typhon_width+8:.1f}" y="{ty_y+14:.1f}">{result.typhon_ms:.2f} ms</text>')

    legend_y = height - 28
    parts.append(f'<rect x="{left}" y="{legend_y-14}" width="16" height="16" rx="3" fill="#3776ab"/>')
    parts.append(f'<text class="subtitle" x="{left+24}" y="{legend_y}">Python</text>')
    parts.append(f'<rect x="{left+115}" y="{legend_y-14}" width="16" height="16" rx="3" fill="#00add8"/>')
    parts.append(f'<text class="subtitle" x="{left+139}" y="{legend_y}">Typhon Go VM</text>')
    parts.append(f'<text class="subtitle" x="{width-right}" y="{legend_y}" text-anchor="end">Scale max: {max_value:.0f} ms</text>')
    parts.append("</svg>")
    (RESULTS_DIR / "python_vs_typhon.svg").write_text("\n".join(parts), encoding="utf-8")


def nice_max(value: float) -> float:
    if value <= 10:
        return 10
    if value <= 25:
        return 25
    if value <= 50:
        return 50
    if value <= 100:
        return 100
    return ((int(value) // 100) + 1) * 100


if __name__ == "__main__":
    raise SystemExit(main())
