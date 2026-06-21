from __future__ import annotations

import sys
import types
from pathlib import Path

from typhon.transpiler import transpile_source
from typhon.validator import validate_source


def transpile_file(path: Path) -> str:
    source = path.read_text(encoding="utf-8")
    validate_source(source, filename=str(path))
    return transpile_source(source, filename=str(path))


def run_file(path: Path) -> None:
    script_path = path.resolve()
    script_dir = str(script_path.parent)
    code = transpile_file(script_path)

    if not sys.path or sys.path[0] != script_dir:
        sys.path.insert(0, script_dir)

    module = types.ModuleType("__main__")
    module.__file__ = str(script_path)
    module.__package__ = None
    module.__loader__ = None
    module.__spec__ = None
    sys.modules["__main__"] = module
    exec(compile(code, str(script_path), "exec"), module.__dict__)
