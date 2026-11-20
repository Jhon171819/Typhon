from __future__ import annotations

import argparse
import csv
import ctypes
import html
import os
import statistics
import subprocess
import sys
import time
from ctypes import wintypes
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
class Sample:
    wall_ms: float
    cpu_ms: float
    peak_working_set_mb: float
    peak_private_mb: float
    max_processes: int
    output: str


@dataclass
class Result:
    case: Case
    python: Sample
    typhon: Sample
    typhon_warm: Sample | None = None


CASES = [
    Case("hello_print", "print"),
    Case("function_string", "function + string"),
    Case("list_loop", "list loop"),
    Case("class_fields", "class fields"),
    Case("python_json", "json native shim"),
    Case("python_math", "Python math bridge"),
]


def main() -> int:
    parser = argparse.ArgumentParser(description="Generate a Python vs Typhon resource usage benchmark.")
    parser.add_argument("--runs", type=int, default=7, help="Measured runs per case.")
    parser.add_argument("--warmups", type=int, default=1, help="Untimed warmup runs per case.")
    parser.add_argument("--poll-ms", type=float, default=1.0, help="Process-tree polling interval in milliseconds.")
    parser.add_argument("--include-warm-bridge", action="store_true", help="Also measure --warm-python for bridge cases.")
    args = parser.parse_args()

    if os.name != "nt":
        raise SystemExit("resource_benchmark.py currently uses Windows process APIs")

    RESULTS_DIR.mkdir(parents=True, exist_ok=True)
    build_typhon()
    python = find_python()

    results: list[Result] = []
    for case in CASES:
        result = benchmark_case(case, python, args.runs, args.warmups, args.poll_ms / 1000.0, args.include_warm_bridge)
        results.append(result)
        warm_text = ""
        if result.typhon_warm is not None:
            warm_text = f" | Ty warm mem {result.typhon_warm.peak_private_mb:7.2f} MB cpu {result.typhon_warm.cpu_ms:7.2f} ms"
        print(
            f"{case.label:<20} "
            f"Py mem {result.python.peak_private_mb:7.2f} MB cpu {result.python.cpu_ms:7.2f} ms | "
            f"Ty mem {result.typhon.peak_private_mb:7.2f} MB cpu {result.typhon.cpu_ms:7.2f} ms"
            f"{warm_text}"
        )

    write_csv(results)
    write_svg(results)
    write_report(results, args.runs, args.warmups, args.poll_ms)
    print(f"\nWrote {RESULTS_DIR / 'python_vs_typhon_resources.svg'}")
    print(f"Wrote {RESULTS_DIR / 'python_vs_typhon_resources.csv'}")
    print(f"Wrote {RESULTS_DIR / 'python_vs_typhon_resources.md'}")
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


def benchmark_case(case: Case, python: str, runs: int, warmups: int, poll_interval: float, include_warm_bridge: bool) -> Result:
    python_cmd = [python, str(case.python_path)]
    typhon_cmd = [str(TYPHON_EXE), "run", str(case.typhon_path)]
    warm_cmd = [str(TYPHON_EXE), "--warm-python", "run", str(case.typhon_path)]

    python_output = run_once(python_cmd)
    typhon_output = run_once(typhon_cmd)
    if python_output != typhon_output:
        raise RuntimeError(
            f"output mismatch for {case.name}\n"
            f"Python: {python_output!r}\n"
            f"Typhon: {typhon_output!r}"
        )

    for _ in range(warmups):
        measure_command(python_cmd, poll_interval)
        measure_command(typhon_cmd, poll_interval)

    python_samples = [measure_command(python_cmd, poll_interval) for _ in range(runs)]
    typhon_samples = [measure_command(typhon_cmd, poll_interval) for _ in range(runs)]
    typhon_warm = None
    if include_warm_bridge and case.name == "python_math":
        warm_output = run_once(warm_cmd)
        if warm_output != python_output:
            raise RuntimeError(f"warm bridge output mismatch for {case.name}")
        for _ in range(warmups):
            measure_command(warm_cmd, poll_interval)
        typhon_warm = median_sample([measure_command(warm_cmd, poll_interval) for _ in range(runs)])
    return Result(case, median_sample(python_samples), median_sample(typhon_samples), typhon_warm)


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


def measure_command(cmd: list[str], poll_interval: float) -> Sample:
    started = time.perf_counter()
    process = subprocess.Popen(
        cmd,
        cwd=ROOT,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )
    peak_working_set = 0
    peak_private = 0
    peak_processes = 0
    peak_cpu_ms = 0.0

    while process.poll() is None:
        snapshot = sample_process_tree(process.pid)
        peak_working_set = max(peak_working_set, snapshot.working_set)
        peak_private = max(peak_private, snapshot.private_bytes)
        peak_processes = max(peak_processes, snapshot.process_count)
        peak_cpu_ms = max(peak_cpu_ms, snapshot.cpu_ms)
        time.sleep(poll_interval)

    final_snapshot = sample_process_handle(process)
    peak_working_set = max(peak_working_set, final_snapshot.working_set)
    peak_private = max(peak_private, final_snapshot.private_bytes)
    peak_processes = max(peak_processes, final_snapshot.process_count)
    peak_cpu_ms = max(peak_cpu_ms, final_snapshot.cpu_ms)

    stdout, stderr = process.communicate()
    wall_ms = (time.perf_counter() - started) * 1000.0
    if process.returncode != 0:
        raise RuntimeError(f"command failed: {' '.join(cmd)}\n{stderr}")

    return Sample(
        wall_ms=wall_ms,
        cpu_ms=peak_cpu_ms,
        peak_working_set_mb=peak_working_set / (1024 * 1024),
        peak_private_mb=peak_private / (1024 * 1024),
        max_processes=peak_processes,
        output=stdout.replace("\r\n", "\n"),
    )


def median_sample(samples: list[Sample]) -> Sample:
    return Sample(
        wall_ms=statistics.median(sample.wall_ms for sample in samples),
        cpu_ms=statistics.median(sample.cpu_ms for sample in samples),
        peak_working_set_mb=statistics.median(sample.peak_working_set_mb for sample in samples),
        peak_private_mb=statistics.median(sample.peak_private_mb for sample in samples),
        max_processes=int(statistics.median(sample.max_processes for sample in samples)),
        output=samples[0].output,
    )


@dataclass
class ProcessSnapshot:
    working_set: int = 0
    private_bytes: int = 0
    cpu_ms: float = 0.0
    process_count: int = 0


def sample_process_tree(root_pid: int) -> ProcessSnapshot:
    pids = descendant_pids(root_pid)
    snapshot = ProcessSnapshot(process_count=len(pids))
    for pid in pids:
        stats = sample_pid(pid)
        snapshot.working_set += stats.working_set
        snapshot.private_bytes += stats.private_bytes
        snapshot.cpu_ms += stats.cpu_ms
    return snapshot


def sample_process_handle(process: subprocess.Popen[str]) -> ProcessSnapshot:
    stats = ProcessSnapshot(process_count=1)
    handle = getattr(process, "_handle", None)
    if handle:
        memory = process_memory_from_handle(handle)
        cpu_ms = process_cpu_from_handle(handle)
        stats.working_set = memory[0]
        stats.private_bytes = memory[1]
        stats.cpu_ms = cpu_ms
    return stats


TH32CS_SNAPPROCESS = 0x00000002
INVALID_HANDLE_VALUE = ctypes.c_void_p(-1).value
PROCESS_QUERY_INFORMATION = 0x0400
PROCESS_VM_READ = 0x0010
MAX_PATH = 260

kernel32 = ctypes.WinDLL("kernel32", use_last_error=True)
psapi = ctypes.WinDLL("psapi", use_last_error=True)


class PROCESSENTRY32W(ctypes.Structure):
    _fields_ = [
        ("dwSize", wintypes.DWORD),
        ("cntUsage", wintypes.DWORD),
        ("th32ProcessID", wintypes.DWORD),
        ("th32DefaultHeapID", ctypes.c_size_t),
        ("th32ModuleID", wintypes.DWORD),
        ("cntThreads", wintypes.DWORD),
        ("th32ParentProcessID", wintypes.DWORD),
        ("pcPriClassBase", wintypes.LONG),
        ("dwFlags", wintypes.DWORD),
        ("szExeFile", wintypes.WCHAR * MAX_PATH),
    ]


class PROCESS_MEMORY_COUNTERS_EX(ctypes.Structure):
    _fields_ = [
        ("cb", wintypes.DWORD),
        ("PageFaultCount", wintypes.DWORD),
        ("PeakWorkingSetSize", ctypes.c_size_t),
        ("WorkingSetSize", ctypes.c_size_t),
        ("QuotaPeakPagedPoolUsage", ctypes.c_size_t),
        ("QuotaPagedPoolUsage", ctypes.c_size_t),
        ("QuotaPeakNonPagedPoolUsage", ctypes.c_size_t),
        ("QuotaNonPagedPoolUsage", ctypes.c_size_t),
        ("PagefileUsage", ctypes.c_size_t),
        ("PeakPagefileUsage", ctypes.c_size_t),
        ("PrivateUsage", ctypes.c_size_t),
    ]


kernel32.CreateToolhelp32Snapshot.argtypes = [wintypes.DWORD, wintypes.DWORD]
kernel32.CreateToolhelp32Snapshot.restype = wintypes.HANDLE
kernel32.Process32FirstW.argtypes = [wintypes.HANDLE, ctypes.POINTER(PROCESSENTRY32W)]
kernel32.Process32FirstW.restype = wintypes.BOOL
kernel32.Process32NextW.argtypes = [wintypes.HANDLE, ctypes.POINTER(PROCESSENTRY32W)]
kernel32.Process32NextW.restype = wintypes.BOOL
kernel32.OpenProcess.argtypes = [wintypes.DWORD, wintypes.BOOL, wintypes.DWORD]
kernel32.OpenProcess.restype = wintypes.HANDLE
kernel32.CloseHandle.argtypes = [wintypes.HANDLE]
kernel32.CloseHandle.restype = wintypes.BOOL
kernel32.GetProcessTimes.argtypes = [
    wintypes.HANDLE,
    ctypes.POINTER(wintypes.FILETIME),
    ctypes.POINTER(wintypes.FILETIME),
    ctypes.POINTER(wintypes.FILETIME),
    ctypes.POINTER(wintypes.FILETIME),
]
kernel32.GetProcessTimes.restype = wintypes.BOOL
psapi.GetProcessMemoryInfo.argtypes = [
    wintypes.HANDLE,
    ctypes.POINTER(PROCESS_MEMORY_COUNTERS_EX),
    wintypes.DWORD,
]
psapi.GetProcessMemoryInfo.restype = wintypes.BOOL


def descendant_pids(root_pid: int) -> set[int]:
    parent_by_pid: dict[int, int] = {}
    handle = kernel32.CreateToolhelp32Snapshot(TH32CS_SNAPPROCESS, 0)
    if handle == INVALID_HANDLE_VALUE:
        return {root_pid}
    try:
        entry = PROCESSENTRY32W()
        entry.dwSize = ctypes.sizeof(PROCESSENTRY32W)
        ok = kernel32.Process32FirstW(handle, ctypes.byref(entry))
        while ok:
            parent_by_pid[int(entry.th32ProcessID)] = int(entry.th32ParentProcessID)
            ok = kernel32.Process32NextW(handle, ctypes.byref(entry))
    finally:
        kernel32.CloseHandle(handle)

    pids = {root_pid}
    changed = True
    while changed:
        changed = False
        for pid, parent in parent_by_pid.items():
            if parent in pids and pid not in pids:
                pids.add(pid)
                changed = True
    return pids


def sample_pid(pid: int) -> ProcessSnapshot:
    handle = kernel32.OpenProcess(PROCESS_QUERY_INFORMATION | PROCESS_VM_READ, False, pid)
    if not handle:
        return ProcessSnapshot()
    try:
        working_set, private_bytes = process_memory_from_handle(handle)
        cpu_ms = process_cpu_from_handle(handle)
        return ProcessSnapshot(working_set=working_set, private_bytes=private_bytes, cpu_ms=cpu_ms, process_count=1)
    finally:
        kernel32.CloseHandle(handle)


def process_memory_from_handle(handle: int) -> tuple[int, int]:
    counters = PROCESS_MEMORY_COUNTERS_EX()
    counters.cb = ctypes.sizeof(PROCESS_MEMORY_COUNTERS_EX)
    ok = psapi.GetProcessMemoryInfo(handle, ctypes.byref(counters), counters.cb)
    if not ok:
        return 0, 0
    return int(counters.WorkingSetSize), int(counters.PrivateUsage)


def process_cpu_from_handle(handle: int) -> float:
    creation = wintypes.FILETIME()
    exit_time = wintypes.FILETIME()
    kernel = wintypes.FILETIME()
    user = wintypes.FILETIME()
    ok = kernel32.GetProcessTimes(
        handle,
        ctypes.byref(creation),
        ctypes.byref(exit_time),
        ctypes.byref(kernel),
        ctypes.byref(user),
    )
    if not ok:
        return 0.0
    return (filetime_to_100ns(kernel) + filetime_to_100ns(user)) / 10_000.0


def filetime_to_100ns(value: wintypes.FILETIME) -> int:
    return (int(value.dwHighDateTime) << 32) + int(value.dwLowDateTime)


def write_csv(results: list[Result]) -> None:
    with (RESULTS_DIR / "python_vs_typhon_resources.csv").open("w", newline="", encoding="utf-8") as handle:
        writer = csv.writer(handle)
        writer.writerow(
            [
                "case",
                "label",
                "python_wall_ms",
                "typhon_wall_ms",
                "python_cpu_ms",
                "typhon_cpu_ms",
                "python_peak_working_set_mb",
                "typhon_peak_working_set_mb",
                "python_peak_private_mb",
                "typhon_peak_private_mb",
                "python_max_processes",
                "typhon_max_processes",
                "typhon_warm_wall_ms",
                "typhon_warm_cpu_ms",
                "typhon_warm_peak_working_set_mb",
                "typhon_warm_peak_private_mb",
                "typhon_warm_max_processes",
            ]
        )
        for result in results:
            writer.writerow(
                [
                    result.case.name,
                    result.case.label,
                    f"{result.python.wall_ms:.3f}",
                    f"{result.typhon.wall_ms:.3f}",
                    f"{result.python.cpu_ms:.3f}",
                    f"{result.typhon.cpu_ms:.3f}",
                    f"{result.python.peak_working_set_mb:.3f}",
                    f"{result.typhon.peak_working_set_mb:.3f}",
                    f"{result.python.peak_private_mb:.3f}",
                    f"{result.typhon.peak_private_mb:.3f}",
                    result.python.max_processes,
                    result.typhon.max_processes,
                    f"{result.typhon_warm.wall_ms:.3f}" if result.typhon_warm else "",
                    f"{result.typhon_warm.cpu_ms:.3f}" if result.typhon_warm else "",
                    f"{result.typhon_warm.peak_working_set_mb:.3f}" if result.typhon_warm else "",
                    f"{result.typhon_warm.peak_private_mb:.3f}" if result.typhon_warm else "",
                    result.typhon_warm.max_processes if result.typhon_warm else "",
                ]
            )


def write_report(results: list[Result], runs: int, warmups: int, poll_ms: float) -> None:
    lines = [
        "# Python vs Typhon Resource Usage Benchmark",
        "",
        "Median cold-process resource usage. Lower is better for wall time, CPU, and memory.",
        "",
        f"- Timed runs per case: `{runs}`",
        f"- Warmups per case: `{warmups}`",
        f"- Process polling interval: `{poll_ms:.2f} ms`",
        f"- Typhon binary: `{TYPHON_EXE}`",
        "",
        "![Python vs Typhon resource benchmark](python_vs_typhon_resources.svg)",
        "",
        "| Case | Py private MB | Ty private MB | Py CPU ms | Ty CPU ms | Py procs | Ty procs |",
        "|---|---:|---:|---:|---:|---:|---:|",
    ]
    for result in results:
        lines.append(
            f"| {result.case.label} | "
            f"{result.python.peak_private_mb:.2f} | {result.typhon.peak_private_mb:.2f} | "
            f"{result.python.cpu_ms:.2f} | {result.typhon.cpu_ms:.2f} | "
            f"{result.python.max_processes} | {result.typhon.max_processes} |"
        )
    warm_results = [result for result in results if result.typhon_warm is not None]
    if warm_results:
        lines.extend(
            [
                "",
                "Warm Python bridge comparison:",
                "",
                "| Case | Cold Ty private MB | Warm Ty private MB | Cold Ty CPU ms | Warm Ty CPU ms |",
                "|---|---:|---:|---:|---:|",
            ]
        )
        for result in warm_results:
            assert result.typhon_warm is not None
            lines.append(
                f"| {result.case.label} | "
                f"{result.typhon.peak_private_mb:.2f} | {result.typhon_warm.peak_private_mb:.2f} | "
                f"{result.typhon.cpu_ms:.2f} | {result.typhon_warm.cpu_ms:.2f} |"
            )
    lines.extend(
        [
            "",
            "Notes:",
            "- Memory is sampled from the process tree and reported as median peak private bytes.",
            "- CPU time is sampled from the process tree and can undercount very short-lived child processes.",
            "- The Python bridge case includes the Typhon process and its Python worker process.",
            "- The json native shim is served by the Go VM without starting Python.",
            "- When present, warm bridge columns use `typhon --warm-python run`.",
            "",
        ]
    )
    (RESULTS_DIR / "python_vs_typhon_resources.md").write_text("\n".join(lines), encoding="utf-8")


def write_svg(results: list[Result]) -> None:
    width = 1160
    height = 760
    left = 220
    right = 70
    top = 96
    panel_gap = 64
    panel_height = 250
    bar_height = 16
    chart_width = width - left - right

    max_mem = nice_max(max(max(result.python.peak_private_mb, result.typhon.peak_private_mb) for result in results))
    max_cpu = nice_max(max(max(result.python.cpu_ms, result.typhon.cpu_ms) for result in results))

    parts = [
        f'<svg xmlns="http://www.w3.org/2000/svg" width="{width}" height="{height}" viewBox="0 0 {width} {height}">',
        "<style>",
        "text{font-family:Segoe UI,Arial,sans-serif;fill:#1f2937}",
        ".title{font-size:28px;font-weight:700}",
        ".subtitle{font-size:14px;fill:#64748b}",
        ".panel{font-size:18px;font-weight:650}",
        ".label{font-size:14px}",
        ".value{font-size:13px;fill:#334155}",
        ".axis{stroke:#cbd5e1;stroke-width:1}",
        ".grid{stroke:#e2e8f0;stroke-width:1}",
        "</style>",
        '<rect width="100%" height="100%" fill="#f8fafc"/>',
        '<text class="title" x="40" y="44">Python vs Typhon Resource Usage</text>',
        '<text class="subtitle" x="40" y="68">Median cold-process usage. Lower bars are better.</text>',
    ]

    draw_panel(
        parts,
        results,
        title="Peak private memory (MB)",
        top=top,
        max_value=max_mem,
        chart_width=chart_width,
        left=left,
        value_getter=lambda result, runtime: getattr(result, runtime).peak_private_mb,
        suffix=" MB",
    )
    draw_panel(
        parts,
        results,
        title="Estimated CPU time (ms)",
        top=top + panel_height + panel_gap,
        max_value=max_cpu,
        chart_width=chart_width,
        left=left,
        value_getter=lambda result, runtime: getattr(result, runtime).cpu_ms,
        suffix=" ms",
    )

    legend_y = height - 34
    parts.append(f'<rect x="{left}" y="{legend_y-14}" width="16" height="16" rx="3" fill="#3776ab"/>')
    parts.append(f'<text class="subtitle" x="{left+24}" y="{legend_y}">Python</text>')
    parts.append(f'<rect x="{left+115}" y="{legend_y-14}" width="16" height="16" rx="3" fill="#00add8"/>')
    parts.append(f'<text class="subtitle" x="{left+139}" y="{legend_y}">Typhon Go VM</text>')
    parts.append("</svg>")
    (RESULTS_DIR / "python_vs_typhon_resources.svg").write_text("\n".join(parts), encoding="utf-8")


def draw_panel(parts, results, title, top, max_value, chart_width, left, value_getter, suffix) -> None:
    row_height = 42
    bottom = top + row_height * len(results) + 38
    parts.append(f'<text class="panel" x="40" y="{top-20}">{html.escape(title)}</text>')
    parts.append(f'<line class="axis" x1="{left}" y1="{bottom}" x2="{left+chart_width}" y2="{bottom}"/>')
    for i in range(6):
        value = max_value * i / 5
        x = left + chart_width * i / 5
        parts.append(f'<line class="grid" x1="{x:.1f}" y1="{top-5}" x2="{x:.1f}" y2="{bottom}"/>')
        parts.append(f'<text class="subtitle" x="{x:.1f}" y="{bottom+24}" text-anchor="middle">{value:.0f}</text>')
    for index, result in enumerate(results):
        y = top + index * row_height
        py_value = value_getter(result, "python")
        ty_value = value_getter(result, "typhon")
        py_width = chart_width * py_value / max_value if max_value else 0
        ty_width = chart_width * ty_value / max_value if max_value else 0
        parts.append(f'<text class="label" x="{left-15}" y="{y+26}" text-anchor="end">{html.escape(result.case.label)}</text>')
        parts.append(f'<rect x="{left}" y="{y+5}" width="{py_width:.1f}" height="16" rx="4" fill="#3776ab"/>')
        parts.append(f'<rect x="{left}" y="{y+25}" width="{ty_width:.1f}" height="16" rx="4" fill="#00add8"/>')
        parts.append(f'<text class="value" x="{left+py_width+8:.1f}" y="{y+18}">{py_value:.2f}{suffix}</text>')
        parts.append(f'<text class="value" x="{left+ty_width+8:.1f}" y="{y+38}">{ty_value:.2f}{suffix}</text>')


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
