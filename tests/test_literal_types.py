from __future__ import annotations

import unittest
import types

from typhon.errors import TyphonTypeError
from typhon.transpiler import normalize_typhon_source, transpile_source


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


if __name__ == "__main__":
    unittest.main()
