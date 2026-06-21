from __future__ import annotations


class TyphonError(Exception):
    """Base error for Typhon validation and runtime failures."""


class TyphonSyntaxError(TyphonError):
    def __init__(self, message: str, line: int | None = None) -> None:
        if line is None:
            super().__init__(message)
        else:
            super().__init__(f"line {line}: {message}")
        self.line = line


class TyphonTypeError(TyphonError, TypeError):
    pass
