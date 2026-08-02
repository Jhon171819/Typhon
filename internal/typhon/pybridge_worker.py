from __future__ import annotations

import argparse
import importlib
import json
import os
import socket
import sys
import threading
import time
import traceback
from pathlib import Path
from typing import Any, TextIO


PROTOCOL_VERSION = 1


class Session:
    def __init__(self) -> None:
        self.objects: dict[int, Any] = {}
        self.next_id = 1

    def store(self, value: Any) -> dict[str, Any]:
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
            return {"kind": "list", "items": [self.store(item) for item in value]}

        object_id = self.next_id
        self.next_id += 1
        self.objects[object_id] = value
        return {"kind": "object", "id": object_id, "repr": repr(value)}

    def load(self, value: dict[str, Any]) -> Any:
        kind = value.get("kind")
        if kind == "none":
            return None
        if kind in {"bool", "int", "str"}:
            return value.get("value")
        if kind == "float":
            return float(value.get("value"))
        if kind == "list":
            return [self.load(item) for item in value.get("items", [])]
        if kind == "object":
            return self.objects[value["id"]]
        raise ValueError(f"unsupported Typhon value for Python bridge: {kind}")

    def handle(self, request: dict[str, Any]) -> dict[str, Any]:
        op = request.get("op")
        if op == "ping":
            return {"ok": True, "value": {"kind": "none"}}
        if op == "import":
            module = importlib.import_module(request["module"])
            return {"ok": True, "value": self.store(module)}
        if op == "getattr":
            target = self.objects[request["id"]]
            return {
                "ok": True,
                "value": self.store(getattr(target, request["name"])),
            }
        if op == "call":
            target = self.objects[request["id"]]
            args = [self.load(item) for item in request.get("args", [])]
            kwargs = {
                name: self.load(item)
                for name, item in request.get("kwargs", {}).items()
            }
            return {"ok": True, "value": self.store(target(*args, **kwargs))}
        return {"ok": False, "error": f"unknown Python bridge op: {op}"}


def error_response(error: Exception) -> dict[str, Any]:
    return {
        "ok": False,
        "error": str(error),
        "traceback": traceback.format_exc(),
    }


def serve_stream(
    reader: TextIO,
    writer: TextIO,
    token: str | None = None,
    request_lock: threading.Lock | None = None,
) -> None:
    session = Session()
    authenticated = token is None

    for line in reader:
        try:
            request = json.loads(line)
            if not authenticated:
                if request.get("op") != "hello" or request.get("token") != token:
                    response = {"ok": False, "error": "python bridge authentication failed"}
                    print(json.dumps(response), file=writer, flush=True)
                    return
                authenticated = True
                response = {
                    "ok": True,
                    "protocol": PROTOCOL_VERSION,
                    "value": {"kind": "none"},
                }
            else:
                if request_lock is None:
                    response = session.handle(request)
                else:
                    with request_lock:
                        response = session.handle(request)
        except Exception as error:
            response = error_response(error)

        print(json.dumps(response), file=writer, flush=True)


class Daemon:
    def __init__(self, listener: socket.socket, token: str, idle_seconds: float) -> None:
        self.listener = listener
        self.token = token
        self.idle_seconds = idle_seconds
        self.active_clients = 0
        self.last_activity = time.monotonic()
        self.lock = threading.Lock()
        self.request_lock = threading.Lock()

    def serve(self) -> None:
        self.listener.settimeout(1.0)
        while True:
            try:
                connection, _ = self.listener.accept()
            except TimeoutError:
                with self.lock:
                    idle = time.monotonic() - self.last_activity
                    if self.active_clients == 0 and idle >= self.idle_seconds:
                        return
                continue

            with self.lock:
                self.active_clients += 1
                self.last_activity = time.monotonic()
            thread = threading.Thread(
                target=self.serve_connection,
                args=(connection,),
                daemon=True,
            )
            thread.start()

    def serve_connection(self, connection: socket.socket) -> None:
        try:
            with connection:
                reader = connection.makefile("r", encoding="utf-8", newline="\n")
                writer = connection.makefile("w", encoding="utf-8", newline="\n")
                try:
                    serve_stream(reader, writer, self.token, self.request_lock)
                finally:
                    writer.close()
                    reader.close()
        finally:
            with self.lock:
                self.active_clients -= 1
                self.last_activity = time.monotonic()


def write_state(path: Path, state: dict[str, Any]) -> None:
    temporary = path.with_name(f"{path.name}.{os.getpid()}.tmp")
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
    descriptor = os.open(temporary, flags, 0o600)
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as stream:
            json.dump(state, stream)
            stream.write("\n")
        os.replace(temporary, path)
    finally:
        try:
            temporary.unlink()
        except FileNotFoundError:
            pass


def remove_own_state(path: Path, token: str) -> None:
    try:
        state = json.loads(path.read_text(encoding="utf-8"))
        if state.get("token") == token:
            path.unlink(missing_ok=True)
    except (FileNotFoundError, OSError, ValueError):
        pass


def run_daemon(state_file: Path, token: str, idle_seconds: float) -> int:
    listener = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    listener.bind(("127.0.0.1", 0))
    listener.listen()
    host, port = listener.getsockname()
    state = {
        "address": f"{host}:{port}",
        "pid": os.getpid(),
        "protocol": PROTOCOL_VERSION,
        "token": token,
    }
    write_state(state_file, state)
    try:
        Daemon(listener, token, idle_seconds).serve()
    finally:
        listener.close()
        remove_own_state(state_file, token)
    return 0


def main() -> int:
    parser = argparse.ArgumentParser(add_help=False)
    parser.add_argument("--daemon", action="store_true")
    parser.add_argument("--state-file")
    parser.add_argument("--token")
    parser.add_argument("--idle-seconds", type=float, default=300.0)
    args = parser.parse_args()

    if not args.daemon:
        serve_stream(sys.stdin, sys.stdout)
        return 0
    if not args.state_file or not args.token:
        parser.error("--daemon requires --state-file and --token")
    if args.idle_seconds <= 0:
        parser.error("--idle-seconds must be positive")
    return run_daemon(Path(args.state_file), args.token, args.idle_seconds)


if __name__ == "__main__":
    raise SystemExit(main())
