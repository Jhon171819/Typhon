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
    code = transpile_file(path)
    module_name = "__typhon_main__"
    module = types.ModuleType(module_name)
    module.__file__ = str(path)
    sys.modules[module_name] = module
    exec(compile(code, str(path), "exec"), module.__dict__)
