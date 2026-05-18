#!/usr/bin/env python3
"""Serve the Clawvisor raw log viewer against a local logs directory."""

from __future__ import annotations

import argparse
import json
import mimetypes
import os
import pathlib
import socket
import sys
import threading
import webbrowser
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlparse


ROOT = pathlib.Path(__file__).resolve().parent
VIEWER = ROOT / "raw-log-viewer.html"
DEFAULT_LOG_DIR = pathlib.Path.home() / ".clawvisor" / "logs"
DEFAULT_FILES = (
    "lite-proxy-raw.jsonl",
    "lite-proxy-trace.jsonl",
)


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Serve a local browser UI for Clawvisor proxy-lite raw logs.",
    )
    parser.add_argument(
        "logs_dir",
        nargs="?",
        default=str(DEFAULT_LOG_DIR),
        help="Directory containing lite-proxy-raw.jsonl and lite-proxy-trace.jsonl.",
    )
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=0, help="0 chooses an available port.")
    parser.add_argument(
        "--tail-lines",
        type=int,
        default=5000,
        help="Maximum lines to load from each JSONL file. Use 0 for all lines.",
    )
    parser.add_argument("--no-open", action="store_true", help="Do not open the browser.")
    return parser.parse_args()


def tail_text(path: pathlib.Path, max_lines: int) -> str:
    if max_lines <= 0:
        return path.read_text(errors="replace")
    with path.open("rb") as fh:
        fh.seek(0, os.SEEK_END)
        end = fh.tell()
        block_size = 8192
        blocks: list[bytes] = []
        lines = 0
        pos = end
        while pos > 0 and lines <= max_lines:
            read_size = min(block_size, pos)
            pos -= read_size
            fh.seek(pos)
            block = fh.read(read_size)
            blocks.append(block)
            lines += block.count(b"\n")
        data = b"".join(reversed(blocks))
    if lines > max_lines:
        data = b"\n".join(data.splitlines()[-max_lines:]) + b"\n"
    return data.decode("utf-8", errors="replace")


def log_files(logs_dir: pathlib.Path) -> list[pathlib.Path]:
    files = [logs_dir / name for name in DEFAULT_FILES if (logs_dir / name).is_file()]
    if files:
        return files
    return sorted(path for path in logs_dir.glob("*.jsonl") if path.is_file())


def make_handler(logs_dir: pathlib.Path, tail_lines: int) -> type[BaseHTTPRequestHandler]:
    class Handler(BaseHTTPRequestHandler):
        server_version = "ClawvisorRawLogViewer/1.0"

        def log_message(self, fmt: str, *args: object) -> None:
            sys.stderr.write("%s - %s\n" % (self.address_string(), fmt % args))

        def do_GET(self) -> None:
            parsed = urlparse(self.path)
            if parsed.path in ("", "/"):
                self.serve_file(VIEWER, "text/html; charset=utf-8")
                return
            if parsed.path == "/api/logs":
                self.serve_logs()
                return
            self.send_error(404)

        def serve_file(self, path: pathlib.Path, content_type: str | None = None) -> None:
            if not path.is_file():
                self.send_error(404)
                return
            data = path.read_bytes()
            self.send_response(200)
            self.send_header("Content-Type", content_type or mimetypes.guess_type(str(path))[0] or "application/octet-stream")
            self.send_header("Content-Length", str(len(data)))
            self.send_header("Cache-Control", "no-store")
            self.end_headers()
            self.wfile.write(data)

        def serve_logs(self) -> None:
            payload = {
                "logs_dir": str(logs_dir),
                "tail_lines": tail_lines,
                "files": [],
            }
            for path in log_files(logs_dir):
                try:
                    rel = path.relative_to(logs_dir)
                except ValueError:
                    rel = path.name
                try:
                    text = tail_text(path, tail_lines)
                except OSError as err:
                    payload.setdefault("errors", []).append({"file": str(path), "error": str(err)})
                    continue
                payload["files"].append({
                    "name": str(rel),
                    "bytes": path.stat().st_size,
                    "text": text,
                })
            data = json.dumps(payload).encode("utf-8")
            self.send_response(200)
            self.send_header("Content-Type", "application/json; charset=utf-8")
            self.send_header("Content-Length", str(len(data)))
            self.send_header("Cache-Control", "no-store")
            self.end_headers()
            self.wfile.write(data)

    return Handler


def choose_server(host: str, port: int, handler: type[BaseHTTPRequestHandler]) -> ThreadingHTTPServer:
    if port != 0:
        return ThreadingHTTPServer((host, port), handler)
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
        sock.bind((host, 0))
        chosen = sock.getsockname()[1]
    return ThreadingHTTPServer((host, chosen), handler)


def main() -> int:
    args = parse_args()
    logs_dir = pathlib.Path(args.logs_dir).expanduser().resolve()
    if not VIEWER.is_file():
        print(f"viewer HTML not found: {VIEWER}", file=sys.stderr)
        return 1
    if not logs_dir.is_dir():
        print(f"logs directory not found: {logs_dir}", file=sys.stderr)
        return 1
    handler = make_handler(logs_dir, max(0, args.tail_lines))
    server = choose_server(args.host, args.port, handler)
    host, port = server.server_address[:2]
    url = f"http://{host}:{port}/"
    print(f"Serving Clawvisor raw log viewer at {url}")
    print(f"Logs: {logs_dir}")
    print(f"Tail lines per file: {'all' if args.tail_lines <= 0 else args.tail_lines}")
    if not args.no_open:
        threading.Timer(0.2, lambda: webbrowser.open(url)).start()
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        print("\nStopping raw log viewer.")
        return 0


if __name__ == "__main__":
    raise SystemExit(main())
