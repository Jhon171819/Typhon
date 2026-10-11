from __future__ import annotations

import argparse
import subprocess
import sys
from pathlib import Path

from typhon import __version__
from typhon.runner import transpile_file


GO_COMMANDS = {"run", "check", "bytecode"}


def run_go_engine(args: list[str]) -> int:
    root = Path(__file__).resolve().parent.parent
    exe = root / "typhon.exe"
    if exe.exists():
        command = [str(exe), *args]
    else:
        command = ["go", "run", "./cmd/typhon", *args]

    completed = subprocess.run(command, cwd=root)
    return completed.returncode


def main(argv: list[str] | None = None) -> int:
    argv = sys.argv[1:] if argv is None else argv

    if argv and argv[0] not in {"run", "check", "bytecode", "transpile", "-h", "--help", "--version"}:
        if len(argv) > 1:
            print("usage: typhon [--version] <file> | typhon <command> [args]", file=sys.stderr)
            return 2
        return run_go_engine(["run", argv[0]])

    if argv and argv[0] in GO_COMMANDS:
        return run_go_engine(argv)

    parser = argparse.ArgumentParser(
        prog="typhon",
        description="Run Typhon through the native Go VM, or print legacy Python transpilation.",
    )
    parser.add_argument("--version", action="version", version=f"typhon {__version__}")
    subparsers = parser.add_subparsers(dest="command")

    transpile_parser = subparsers.add_parser(
        "transpile",
        help="Validate a Typhon file and print legacy generated Python.",
    )
    transpile_parser.add_argument("file", type=Path)

    args = parser.parse_args(argv)

    if args.command == "transpile":
        print(transpile_file(args.file))
        return 0

    parser.print_help()
    return 2
