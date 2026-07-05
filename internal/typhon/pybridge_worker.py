from __future__ import annotations

import importlib
import json
import sys
import traceback
from typing import Any


objects: dict[int, Any] = {}
next_id = 1


def store(value: Any) -> dict[str, Any]:
    global next_id

    if value is None:
        return {"kind": "none"}
    if isinstance(value, bool):
        return {"kind": "bool", "value": value}
    if isinstance(value, int) and not isinstance(value, bool):
        return {"kind": "int", "value": value}
    if isinstance(value, float):
        return {"kind": "float", "value": value}
    if isinstance(value, str):
        return {"kind": "str", "value": value}
    if isinstance(value, (list, tuple)):
        return {"kind": "list", "items": [store(item) for item in value]}

    object_id = next_id
    next_id += 1
    objects[object_id] = value
    return {"kind": "object", "id": object_id, "repr": repr(value)}


def load(value: dict[str, Any]) -> Any:
    kind = value.get("kind")
    if kind == "none":
        return None
    if kind in {"bool", "int", "str"}:
        return value.get("value")
    if kind == "float":
        return float(value.get("value"))
    if kind == "list":
        return [load(item) for item in value.get("items", [])]
    if kind == "object":
        return objects[value["id"]]
    raise ValueError(f"unsupported Typhon value for Python bridge: {kind}")


def handle(request: dict[str, Any]) -> dict[str, Any]:
    op = request.get("op")
    if op == "ping":
        return {"ok": True, "value": {"kind": "none"}}
    if op == "import":
        module = importlib.import_module(request["module"])
        return {"ok": True, "value": store(module)}
    if op == "getattr":
        target = objects[request["id"]]
        return {"ok": True, "value": store(getattr(target, request["name"]))}
    if op == "call":
        target = objects[request["id"]]
        args = [load(item) for item in request.get("args", [])]
        kwargs = {
            name: load(item)
            for name, item in request.get("kwargs", {}).items()
        }
        return {"ok": True, "value": store(target(*args, **kwargs))}
    return {"ok": False, "error": f"unknown Python bridge op: {op}"}


def main() -> int:
    for line in sys.stdin:
        try:
            request = json.loads(line)
            response = handle(request)
        except Exception as error:
            response = {
                "ok": False,
                "error": str(error),
                "traceback": traceback.format_exc(),
            }

        print(json.dumps(response), flush=True)

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
