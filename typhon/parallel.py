from __future__ import annotations

import atexit
from concurrent.futures import Future, ThreadPoolExecutor
from os import cpu_count
from typing import Any, Callable, Iterable


_executor = ThreadPoolExecutor(max_workers=cpu_count() or 4, thread_name_prefix="typhon")
atexit.register(_executor.shutdown)


class TyphonTask:
    def __init__(self, future: Future[Any]) -> None:
        self._future = future

    def join(self) -> Any:
        return self._future.result()

    def done(self) -> bool:
        return self._future.done()


def spawn(func: Callable[..., Any], *args: Any, **kwargs: Any) -> TyphonTask:
    return TyphonTask(_executor.submit(func, *args, **kwargs))


def join(task: TyphonTask) -> Any:
    return task.join()


def parallel_map(func: Callable[[Any], Any], values: Iterable[Any]) -> list[Any]:
    return list(_executor.map(func, values))
