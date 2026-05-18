"""Tiny HTTP server that mocks the GitHub release API for install.sh testing.

Serves:
  GET /repos/<owner>/<repo>/releases/latest  ->  JSON with tag_name
  GET /<owner>/<repo>/releases/download/<tag>/<asset>  ->  raw binary
  GET /install.sh  ->  the install script

Usage:
  python3 mock_github_server.py <port> <binary_path> <install_script_path>

The server exposes the binary using the same raw asset naming convention as
the real GitHub releases: clawvisor-server-<os>-<arch>.
"""

import hashlib
import http.server
import json
import os
import platform
import sys
import traceback

VERSION = "v0.0.0-e2e"
REPO = "clawvisor/clawvisor"


def detect_platform():
    os_name = platform.system().lower()
    machine = platform.machine()
    if machine in ("x86_64", "AMD64"):
        arch = "amd64"
    elif machine in ("aarch64", "arm64"):
        arch = "arm64"
    else:
        arch = machine
    return os_name, arch


def main():
    if len(sys.argv) != 4:
        print(f"Usage: {sys.argv[0]} <port> <binary_path> <install_script_path>",
              file=sys.stderr, flush=True)
        sys.exit(1)

    port = int(sys.argv[1])
    binary_path = sys.argv[2]
    install_script_path = sys.argv[3]

    # Validate inputs exist before doing anything else.
    for path, label in [(binary_path, "binary"), (install_script_path, "install script")]:
        if not os.path.isfile(path):
            print(f"ERROR: {label} not found at {path}", file=sys.stderr, flush=True)
            sys.exit(1)

    os_name, arch = detect_platform()
    asset_name = f"clawvisor-server-{os_name}-{arch}"

    with open(binary_path, "rb") as f:
        binary_data = f.read()
    with open(install_script_path, "rb") as f:
        install_script_data = f.read()

    # Compute the checksums.txt contents served alongside the binary. Format
    # matches `sha256sum`/`shasum -a 256` ("<hash>  <filename>"), which is what
    # install.sh parses.
    binary_sha256 = hashlib.sha256(binary_data).hexdigest()
    checksums_data = f"{binary_sha256}  {asset_name}\n".encode()

    print(f"Binary ready: {asset_name} ({len(binary_data)} bytes)", flush=True)

    release_json = json.dumps({
        "tag_name": VERSION,
        "name": f"Release {VERSION}",
    }, indent=2).encode()

    class Handler(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            if self.path == f"/repos/{REPO}/releases/latest":
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(release_json)
            elif self.path == f"/{REPO}/releases/download/{VERSION}/{asset_name}":
                self.send_response(200)
                self.send_header("Content-Type", "application/octet-stream")
                self.end_headers()
                self.wfile.write(binary_data)
            elif self.path == f"/{REPO}/releases/download/{VERSION}/checksums.txt":
                self.send_response(200)
                self.send_header("Content-Type", "text/plain")
                self.end_headers()
                self.wfile.write(checksums_data)
            elif self.path == "/install.sh":
                self.send_response(200)
                self.send_header("Content-Type", "text/plain")
                self.end_headers()
                self.wfile.write(install_script_data)
            else:
                self.send_response(404)
                self.end_headers()
                self.wfile.write(b"not found: " + self.path.encode())

        def log_message(self, fmt, *args):
            pass

    # Bind the socket — connections are accepted into the OS backlog from here.
    server = http.server.HTTPServer(("127.0.0.1", port), Handler)

    # Write ready marker AFTER the socket is bound and the server is about to
    # call serve_forever(). We start a quick serve_forever in the same
    # statement block so there's no gap.
    ready_path = os.path.join(os.environ.get("HOME", "/tmp"), ".mock-server-ready")
    with open(ready_path, "w") as f:
        f.write(str(port))

    print(f"Mock server listening on 127.0.0.1:{port}", flush=True)
    server.serve_forever()


if __name__ == "__main__":
    try:
        main()
    except Exception:
        traceback.print_exc()
        sys.exit(1)
