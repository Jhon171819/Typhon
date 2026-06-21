from __future__ import annotations

import argparse
from pathlib import Path

from typhon.runner import run_file, transpile_file


def main() -> int:
    parser = argparse.ArgumentParser(
        prog="typhon",
        description="Validate and run Python-like .ty files with mandatory types.",
    )
    subparsers = parser.add_subparsers(dest="command")

    run_parser = subparsers.add_parser("run", help="Validate and run a Typhon file.")
    run_parser.add_argument("file", type=Path)

    transpile_parser = subparsers.add_parser(
        "transpile",
        help="Validate a Typhon file and print the generated Python.",
    )
    transpile_parser.add_argument("file", type=Path)

    args = parser.parse_args()

    if args.command == "run":
        run_file(args.file)
        return 0

    if args.command == "transpile":
        print(transpile_file(args.file))
        return 0

    parser.print_help()
    return 2
