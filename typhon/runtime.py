from __future__ import annotations

import functools
import inspect
import types
from typing import Any, Callable, Literal, get_args, get_origin, get_type_hints

from typhon.errors import TyphonTypeError


def typhon_enforce(func: Callable[..., Any]) -> Callable[..., Any]:
    signature = inspect.signature(func)

    @functools.wraps(func)
    def wrapper(*args: Any, **kwargs: Any) -> Any:
        hints = get_type_hints(func)
        bound = signature.bind(*args, **kwargs)
        bound.apply_defaults()

        for name, value in bound.arguments.items():
            if name in {"self", "cls"}:
                continue
            if name in hints and not matches_type(value, hints[name]):
                raise TyphonTypeError(
                    f"{func.__qualname__} argument '{name}' expected {format_type(hints[name])}, got {format_received(value, hints[name])}"
                )

        result = func(*args, **kwargs)
        expected_return = hints.get("return")
        if expected_return is not None and not matches_type(result, expected_return):
            raise TyphonTypeError(
                f"{func.__qualname__} return expected {format_type(expected_return)}, got {format_received(result, expected_return)}"
            )
        return result

    return wrapper


def typhon_enforce_class(cls: type[Any]) -> type[Any]:
    original_setattr = cls.__setattr__

    def checked_setattr(self: Any, name: str, value: Any) -> None:
        hints = get_type_hints(cls)
        expected = hints.get(name)
        if expected is not None and not matches_type(value, expected):
            raise TyphonTypeError(
                f"{cls.__name__}.{name} expected {format_type(expected)}, got {format_received(value, expected)}"
            )
        original_setattr(self, name, value)

    cls.__setattr__ = checked_setattr
    return cls


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
