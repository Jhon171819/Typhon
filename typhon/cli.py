from __future__ import annotations

import argparse
import sys
from pathlib import Path

from typhon import __version__
from typhon.runner import run_file, transpile_file


def main(argv: list[str] | None = None) -> int:
    argv = sys.argv[1:] if argv is None else argv

    if argv and argv[0] not in {"run", "transpile", "-h", "--help", "--version"}:
        if len(argv) > 1:
            print("usage: typhon [--version] <file> | typhon <command> [args]", file=sys.stderr)
            return 2
        run_file(Path(argv[0]))
        return 0

    parser = argparse.ArgumentParser(
        prog="typhon",
        description="Validate and run Python-like .ty files with mandatory types.",
    )
    parser.add_argument("--version", action="version", version=f"typhon {__version__}")
    subparsers = parser.add_subparsers(dest="command")

    run_parser = subparsers.add_parser("run", help="Validate and run a Typhon file.")
    run_parser.add_argument("file", type=Path)

    transpile_parser = subparsers.add_parser(
        "transpile",
        help="Validate a Typhon file and print the generated Python.",
    )
    transpile_parser.add_argument("file", type=Path)

    args = parser.parse_args(argv)

    if args.command == "run":
        run_file(args.file)
        return 0

    if args.command == "transpile":
        print(transpile_file(args.file))
        return 0

    parser.print_help()
    return 2
