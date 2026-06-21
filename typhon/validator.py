from __future__ import annotations

import ast
import re

from typhon.errors import TyphonSyntaxError
from typhon.transpiler import FOR_ANNOTATION_RE, TYPE_ALIAS_RE, normalize_typhon_source


UNTYPED_FOR_RE = re.compile(r"^\s*for\s+([A-Za-z_]\w*)\s+in\s+.+:")


class MandatoryTypeValidator(ast.NodeVisitor):
    def __init__(self, type_alias_lines: set[int]) -> None:
        self.errors: list[TyphonSyntaxError] = []
        self.class_depth = 0
        self.type_alias_lines = type_alias_lines

    def fail(self, node: ast.AST, message: str) -> None:
        self.errors.append(TyphonSyntaxError(message, getattr(node, "lineno", None)))

    def visit_FunctionDef(self, node: ast.FunctionDef) -> None:
        for arg in [*node.args.posonlyargs, *node.args.args, *node.args.kwonlyargs]:
            if arg.arg in {"self", "cls"}:
                continue
            if arg.annotation is None:
                self.fail(arg, f"parameter '{arg.arg}' must have an explicit type")

        if node.args.vararg is not None and node.args.vararg.annotation is None:
            self.fail(node.args.vararg, f"parameter '*{node.args.vararg.arg}' must have an explicit type")

        if node.args.kwarg is not None and node.args.kwarg.annotation is None:
            self.fail(node.args.kwarg, f"parameter '**{node.args.kwarg.arg}' must have an explicit type")

        if node.returns is None:
            self.fail(node, f"function '{node.name}' must declare a return type")

        self.generic_visit(node)

    def visit_AsyncFunctionDef(self, node: ast.AsyncFunctionDef) -> None:
        self.visit_FunctionDef(node)

    def visit_ClassDef(self, node: ast.ClassDef) -> None:
        self.class_depth += 1
        for statement in node.body:
            if isinstance(statement, ast.Assign):
                self.fail(statement, "class fields must use annotated assignments")
            if isinstance(statement, ast.Expr) and isinstance(statement.value, ast.Name):
                self.fail(statement, f"class field '{statement.value.id}' must have an explicit type")
        self.generic_visit(node)
        self.class_depth -= 1

    def visit_Assign(self, node: ast.Assign) -> None:
        if getattr(node, "lineno", None) in self.type_alias_lines:
            return
        if all(isinstance(target, ast.Attribute) and isinstance(target.value, ast.Name) and target.value.id == "self" for target in node.targets):
            return
        self.fail(node, "variable declarations must use an explicit type")
        self.generic_visit(node)

    def visit_AnnAssign(self, node: ast.AnnAssign) -> None:
        if isinstance(node.annotation, ast.Name) and node.annotation.id in {"list", "dict", "set", "tuple"}:
            self.fail(node, f"collection '{node.annotation.id}' must include element types")
        self.generic_visit(node)


def validate_for_annotations(source: str) -> list[TyphonSyntaxError]:
    errors: list[TyphonSyntaxError] = []
    for line_number, line in enumerate(source.splitlines(), start=1):
        if UNTYPED_FOR_RE.match(line) and not FOR_ANNOTATION_RE.match(line):
            errors.append(TyphonSyntaxError("for-loop variables must have an explicit type", line_number))
    return errors


def find_type_alias_lines(source: str) -> set[int]:
    return {
        line_number
        for line_number, line in enumerate(source.splitlines(), start=1)
        if TYPE_ALIAS_RE.match(line)
    }


def validate_source(source: str, filename: str = "<typhon>") -> None:
    normalized = normalize_typhon_source(source)
    try:
        tree = ast.parse(normalized, filename=filename)
    except SyntaxError as error:
        raise TyphonSyntaxError(error.msg, error.lineno) from error

    validator = MandatoryTypeValidator(find_type_alias_lines(source))
    validator.visit(tree)
    errors = [*validator.errors, *validate_for_annotations(source)]

    if errors:
        details = "\n".join(str(error) for error in sorted(errors, key=lambda item: item.line or 0))
        raise TyphonSyntaxError(f"Typhon type validation failed:\n{details}")
