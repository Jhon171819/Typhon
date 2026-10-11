from __future__ import annotations

import sys
import tempfile
import textwrap
import types
import unittest
from pathlib import Path

from typhon.errors import TyphonTypeError
from typhon.runner import run_file
from typhon.transpiler import transpile_source


def run_source(source: str) -> None:
    code = transpile_source(textwrap.dedent(source))
    module = types.ModuleType("__typhon_test__")
    exec(compile(code, "<typhon-test>", "exec"), module.__dict__)


class RuntimeFeatureTests(unittest.TestCase):
    def test_void_return_rejects_returned_value(self) -> None:
        with self.assertRaises(TyphonTypeError) as error:
            run_source(
                """
                def fail() -> void:
                    return 1

                fail()
                """
            )

        self.assertIn("return expected NoneType", str(error.exception))

    def test_typhon_file_can_import_another_typhon_file(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            (root / "helper.ty").write_text(
                textwrap.dedent(
                    """
                    def shout(value: str) -> str:
                        return value.upper()
                    """
                ),
                encoding="utf-8",
            )
            main = root / "main.ty"
            main.write_text(
                textwrap.dedent(
                    """
                    from helper import shout

                    result: str = shout("typhon")
                    if result != "TYPHON":
                        raise RuntimeError(result)
                    """
                ),
                encoding="utf-8",
            )

            previous_main = sys.modules.get("__main__")
            try:
                run_file(main)
            finally:
                if previous_main is None:
                    sys.modules.pop("__main__", None)
                else:
                    sys.modules["__main__"] = previous_main

    def test_parallel_spawn_and_join(self) -> None:
        run_source(
            """
            from typhon.parallel import TyphonTask, join, spawn

            def double(value: int) -> int:
                return value * 2

            task: TyphonTask = spawn(double, 21)
            result: int = join(task)
            if result != 42:
                raise RuntimeError(result)
            """
        )


if __name__ == "__main__":
    unittest.main()
