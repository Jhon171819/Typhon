from __future__ import annotations

import unittest
import types

from typhon.errors import TyphonSyntaxError, TyphonTypeError
from typhon.transpiler import normalize_typhon_source, transpile_source
from typhon.validator import validate_source


def run_source(source: str) -> None:
    code = transpile_source(source)
    module = types.ModuleType("__typhon_test__")
    exec(compile(code, "<typhon-test>", "exec"), module.__dict__)


class LiteralTypeTests(unittest.TestCase):
    def test_or_literal_annotation_transpiles_to_literal_union(self) -> None:
        source = 'def choose(flow: "THIS" or "THAT") -> str:\n    return flow\n'

        transpiled = transpile_source(source)

        self.assertIn("Literal['THIS'] | Literal['THAT']", transpiled)

    def test_pipe_literal_annotation_transpiles_to_literal_union(self) -> None:
        source = 'def choose(flow: "THIS" | "THAT") -> str:\n    return flow\n'

        transpiled = transpile_source(source)

        self.assertIn("Literal['THIS'] | Literal['THAT']", transpiled)

    def test_literal_union_rejects_unlisted_value(self) -> None:
        source = (
            'def choose(flow: "THIS" or "THAT") -> str:\n'
            "    return flow\n"
            '\nchoose("OTHER")\n'
        )

        with self.assertRaises(TyphonTypeError) as error:
            run_source(source)

        self.assertIn("expected 'THIS' or 'THAT'", str(error.exception))
        self.assertIn("got 'OTHER'", str(error.exception))

    def test_regular_union_error_lists_expected_types(self) -> None:
        source = (
            "def stringify(value: int | str) -> str:\n"
            "    return str(value)\n"
            "\nstringify(1.5)\n"
        )

        with self.assertRaises(TyphonTypeError) as error:
            run_source(source)

        self.assertIn("expected int or str", str(error.exception))

    def test_literal_type_alias_is_normalized_before_runtime(self) -> None:
        source = (
            'type Flow = "THIS" or "THAT"\n'
            "\n"
            "def choose(flow: Flow) -> str:\n"
            "    return flow\n"
            '\nchoose("THIS")\n'
        )

        normalized = normalize_typhon_source(source)

        self.assertIn("Flow = Literal['THIS'] | Literal['THAT']", normalized)
        run_source(source)

    def test_outer_decorator_receives_runtime_checked_function(self) -> None:
        source = (
            "captured: list[object] = []\n"
            "\n"
            "def route(func: object) -> object:\n"
            "    captured.append(func)\n"
            "    return func\n"
            "\n"
            "@route\n"
            "def handler() -> void:\n"
            "    return 'not void'\n"
            "\n"
            "captured[0]()\n"
        )

        transpiled = transpile_source(source)

        self.assertIn("@route\n@typhon_enforce\ndef handler() -> None:", transpiled)
        with self.assertRaises(TyphonTypeError) as error:
            run_source(source)

        self.assertIn("handler return expected NoneType, got str", str(error.exception))

    def test_void_function_cannot_return_value(self) -> None:
        source = "def handler() -> void:\n    return 'not void'\n"

        with self.assertRaises(TyphonSyntaxError) as error:
            validate_source(source)

        self.assertIn("void functions cannot return a value", str(error.exception))

    def test_void_function_can_return_without_value(self) -> None:
        validate_source("def log() -> void:\n    return\n")
        validate_source("def log() -> void:\n    return None\n")


if __name__ == "__main__":
    unittest.main()
