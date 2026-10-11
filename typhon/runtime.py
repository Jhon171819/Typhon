from __future__ import annotations

import functools
import inspect
import types
from typing import Any, Callable, Literal, get_args, get_origin, get_type_hints

from typhon.errors import TyphonTypeError


def typhon_enforce(func: Callable[..., Any]) -> Callable[..., Any]:
    signature = inspect.signature(func)
    state: dict[str, Any] = {}

    def resolve() -> dict[str, Any]:
        cached = state.get("resolved")
        if cached is not None:
            return cached

        hints = get_type_hints(func)
        argument_checkers = {
            name: build_type_checker(expected)
            for name, expected in hints.items()
            if name != "return"
        }
        resolved = {
            "hints": hints,
            "argument_checkers": argument_checkers,
            "return_checker": build_type_checker(hints["return"]) if "return" in hints else None,
        }
        state["resolved"] = resolved
        return resolved

    @functools.wraps(func)
    def wrapper(*args: Any, **kwargs: Any) -> Any:
        resolved = resolve()
        hints = resolved["hints"]
        argument_checkers = resolved["argument_checkers"]
        bound = signature.bind(*args, **kwargs)
        bound.apply_defaults()

        for name, value in bound.arguments.items():
            if name in {"self", "cls"}:
                continue
            checker = argument_checkers.get(name)
            if checker is not None and not checker(value):
                raise TyphonTypeError(
                    f"{func.__qualname__} argument '{name}' expected {format_type(hints[name])}, got {format_received(value, hints[name])}"
                )

        result = func(*args, **kwargs)
        return_checker = resolved["return_checker"]
        if return_checker is not None and not return_checker(result):
            expected_return = hints["return"]
            raise TyphonTypeError(
                f"{func.__qualname__} return expected {format_type(expected_return)}, got {format_received(result, expected_return)}"
            )
        return result

    return wrapper


def typhon_enforce_class(cls: type[Any]) -> type[Any]:
    original_setattr = cls.__setattr__
    state: dict[str, Any] = {}

    def resolve() -> dict[str, Any]:
        cached = state.get("resolved")
        if cached is not None:
            return cached

        hints = get_type_hints(cls)
        resolved = {
            "hints": hints,
            "field_checkers": {
                name: build_type_checker(expected)
                for name, expected in hints.items()
            },
        }
        state["resolved"] = resolved
        return resolved

    def checked_setattr(self: Any, name: str, value: Any) -> None:
        resolved = resolve()
        hints = resolved["hints"]
        expected = hints.get(name)
        checker = resolved["field_checkers"].get(name)
        if checker is not None and not checker(value):
            raise TyphonTypeError(
                f"{cls.__name__}.{name} expected {format_type(expected)}, got {format_received(value, expected)}"
            )
        original_setattr(self, name, value)

    cls.__setattr__ = checked_setattr
    return cls


def build_type_checker(expected: Any) -> Callable[[Any], bool]:
    if expected is Any:
        return lambda value: True

    if expected is None or expected is type(None):
        return lambda value: value is None

    origin = get_origin(expected)
    args = get_args(expected)

    if origin is Literal:
        return lambda value: value in args

    if is_union_origin(origin):
        checkers = [build_type_checker(item) for item in args]
        return lambda value: any(checker(value) for checker in checkers)

    if origin is list:
        if not args:
            return lambda value: isinstance(value, list)
        item_checker = build_type_checker(args[0])
        return lambda value: isinstance(value, list) and all(item_checker(item) for item in value)

    if origin is dict:
        if not args:
            return lambda value: isinstance(value, dict)
        key_checker = build_type_checker(args[0])
        value_checker = build_type_checker(args[1])
        return lambda value: isinstance(value, dict) and all(
            key_checker(key) and value_checker(item)
            for key, item in value.items()
        )

    if origin is set:
        if not args:
            return lambda value: isinstance(value, set)
        item_checker = build_type_checker(args[0])
        return lambda value: isinstance(value, set) and all(item_checker(item) for item in value)

    if origin is tuple:
        if not args:
            return lambda value: isinstance(value, tuple)
        if len(args) == 2 and args[1] is Ellipsis:
            item_checker = build_type_checker(args[0])
            return lambda value: isinstance(value, tuple) and all(item_checker(item) for item in value)
        item_checkers = [build_type_checker(item_type) for item_type in args]
        return lambda value: isinstance(value, tuple) and len(value) == len(item_checkers) and all(
            checker(item)
            for item, checker in zip(value, item_checkers)
        )

    def check_instance(value: Any) -> bool:
        try:
            return isinstance(value, expected)
        except TypeError:
            return True

    return check_instance


def matches_type(value: Any, expected: Any) -> bool:
    if expected is Any:
        return True

    if expected is None or expected is type(None):
        return value is None

    origin = get_origin(expected)
    args = get_args(expected)

    if origin is Literal:
        return value in args

    if is_union_origin(origin):
        return any(matches_type(value, item) for item in args)

    if origin is list:
        return isinstance(value, list) and all(matches_type(item, args[0]) for item in value) if args else isinstance(value, list)

    if origin is dict:
        if not isinstance(value, dict):
            return False
        if not args:
            return True
        key_type, value_type = args
        return all(matches_type(key, key_type) and matches_type(item, value_type) for key, item in value.items())

    if origin is set:
        return isinstance(value, set) and all(matches_type(item, args[0]) for item in value) if args else isinstance(value, set)

    if origin is tuple:
        if not isinstance(value, tuple):
            return False
        if len(args) == 2 and args[1] is Ellipsis:
            return all(matches_type(item, args[0]) for item in value)
        return len(value) == len(args) and all(matches_type(item, item_type) for item, item_type in zip(value, args))

    try:
        return isinstance(value, expected)
    except TypeError:
        return True


def is_union_origin(origin: Any) -> bool:
    return origin in {types.UnionType, getattr(types, "UnionType", object)} or (
        origin is not None and str(origin) == "typing.Union"
    )


def format_type(expected: Any) -> str:
    origin = get_origin(expected)
    args = get_args(expected)

    if origin is Literal:
        return " or ".join(repr(item) for item in args)

    if is_union_origin(origin):
        return " or ".join(format_type(item) for item in args)

    name = getattr(expected, "__name__", None)
    return name if name is not None else str(expected)


def format_received(value: Any, expected: Any) -> str:
    if contains_literal(expected):
        return repr(value)
    return type(value).__name__


def contains_literal(expected: Any) -> bool:
    origin = get_origin(expected)
    args = get_args(expected)

    if origin is Literal:
        return True

    if is_union_origin(origin):
        return any(contains_literal(item) for item in args)

    return False
