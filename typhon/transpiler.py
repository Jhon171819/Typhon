from __future__ import annotations

import ast
import re


TYPE_ALIAS_RE = re.compile(r"^(?P<indent>\s*)type\s+(?P<name>[A-Za-z_]\w*)\s*=\s*(?P<value>.+)$")
FOR_ANNOTATION_RE = re.compile(
    r"^(?P<indent>\s*)for\s+(?P<name>[A-Za-z_]\w*)\s*:\s*(?P<type>.+?)\s+in\s+(?P<iter>.+):(?P<trailing>\s*(?:#.*)?)$"
)


class RuntimeDecoratorTransformer(ast.NodeTransformer):
    def normalize_annotation(self, annotation: ast.expr | None) -> ast.expr | None:
        if annotation is None:
            return None

        return VoidAnnotationTransformer().visit(annotation)

    def visit_ClassDef(self, node: ast.ClassDef) -> ast.AST:
        self.generic_visit(node)
        node.decorator_list.insert(0, ast.Name(id="typhon_enforce_class", ctx=ast.Load()))
        return node

    def visit_FunctionDef(self, node: ast.FunctionDef) -> ast.AST:
        self.generic_visit(node)
        self.normalize_function_annotations(node)
        node.decorator_list.insert(0, ast.Name(id="typhon_enforce", ctx=ast.Load()))
        return node

    def visit_AsyncFunctionDef(self, node: ast.AsyncFunctionDef) -> ast.AST:
        self.generic_visit(node)
        self.normalize_function_annotations(node)
        node.decorator_list.insert(0, ast.Name(id="typhon_enforce", ctx=ast.Load()))
        return node

    def visit_AnnAssign(self, node: ast.AnnAssign) -> ast.AST:
        self.generic_visit(node)
        node.annotation = self.normalize_annotation(node.annotation)
        return node

    def normalize_function_annotations(self, node: ast.FunctionDef | ast.AsyncFunctionDef) -> None:
        for arg in [
            *node.args.posonlyargs,
            *node.args.args,
            *node.args.kwonlyargs,
        ]:
            arg.annotation = self.normalize_annotation(arg.annotation)

        if node.args.vararg is not None:
            node.args.vararg.annotation = self.normalize_annotation(node.args.vararg.annotation)

        if node.args.kwarg is not None:
            node.args.kwarg.annotation = self.normalize_annotation(node.args.kwarg.annotation)

        node.returns = self.normalize_annotation(node.returns)


class VoidAnnotationTransformer(ast.NodeTransformer):
    def visit_Name(self, node: ast.Name) -> ast.AST:
        if node.id == "void":
            return ast.Constant(value=None)
        return node


def normalize_typhon_source(source: str) -> str:
    lines: list[str] = []

    for line in source.splitlines():
        alias_match = TYPE_ALIAS_RE.match(line)
        if alias_match:
            lines.append(
                f"{alias_match.group('indent')}{alias_match.group('name')} = {alias_match.group('value')}"
            )
            continue

        for_match = FOR_ANNOTATION_RE.match(line)
        if for_match:
            lines.append(
                f"{for_match.group('indent')}for {for_match.group('name')} in {for_match.group('iter')}:{for_match.group('trailing')}"
            )
            continue

        lines.append(line)

    return "\n".join(lines) + "\n"


def transpile_source(source: str, filename: str = "<typhon>") -> str:
    normalized = normalize_typhon_source(source)
    tree = ast.parse(normalized, filename=filename)
    tree = RuntimeDecoratorTransformer().visit(tree)
    ast.fix_missing_locations(tree)

    body = ast.unparse(tree)
    return (
        "from __future__ import annotations\n"
        "from typhon.runtime import typhon_enforce, typhon_enforce_class\n\n"
        f"{body}\n"
    )
