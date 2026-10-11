from __future__ import annotations

import importlib.abc
import importlib.machinery
import importlib.util
import sys
from pathlib import Path

from typhon.transpiler import transpile_source
from typhon.validator import validate_source


class TyphonModuleLoader(importlib.abc.SourceLoader):
    def __init__(self, fullname: str, path: Path) -> None:
        self.fullname = fullname
        self.path = path

    def get_filename(self, fullname: str) -> str:
        return str(self.path)

    def get_data(self, path: str) -> bytes:
        return Path(path).read_bytes()

    def source_to_code(self, data: bytes, path: str, *, _optimize: int = -1) -> object:
        source = data.decode("utf-8")
        validate_source(source, filename=path)
        python_source = transpile_source(source, filename=path)
        return compile(python_source, path, "exec", dont_inherit=True, optimize=_optimize)


class TyphonModuleFinder(importlib.abc.MetaPathFinder):
    def find_spec(
        self,
        fullname: str,
        path: list[str] | None,
        target: object | None = None,
    ) -> importlib.machinery.ModuleSpec | None:
        module_name = fullname.rsplit(".", 1)[-1]
        search_paths = path if path is not None else sys.path

        for entry in search_paths:
            if not entry:
                entry = "."

            base = Path(entry)
            module_path = base / f"{module_name}.ty"
            if module_path.is_file():
                loader = TyphonModuleLoader(fullname, module_path)
                return importlib.util.spec_from_loader(fullname, loader, origin=str(module_path))

            package_path = base / module_name / "__init__.ty"
            if package_path.is_file():
                loader = TyphonModuleLoader(fullname, package_path)
                spec = importlib.util.spec_from_loader(
                    fullname,
                    loader,
                    origin=str(package_path),
                    is_package=True,
                )
                if spec is not None:
                    spec.submodule_search_locations = [str(package_path.parent)]
                return spec

        return None


def install_typhon_importer() -> None:
    if any(isinstance(finder, TyphonModuleFinder) for finder in sys.meta_path):
        return

    sys.meta_path.insert(0, TyphonModuleFinder())
