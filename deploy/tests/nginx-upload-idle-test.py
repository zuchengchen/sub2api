#!/usr/bin/env python3
"""Exercise the real upload-idle boundary using an isolated local nginx.

No production listeners, credentials or upstream services are used. Takes
about 65 seconds; requires nginx in PATH (or /usr/sbin/nginx).
"""

import hashlib
import http.server
import json
from pathlib import Path
import shutil
import socket
import subprocess
import tempfile
import threading
import time


BODY = b"synthetic-upload\n" * (1024 * 1024)
DIGEST = hashlib.sha256(BODY).hexdigest()


class Receiver(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers["Content-Length"])
        data = self.rfile.read(length)
        if len(data) != length:
            return  # nginx/client canceled the incomplete upload.
        result = json.dumps({"bytes": length, "sha256": hashlib.sha256(data).hexdigest()}).encode()
        self.send_response(200)
        self.send_header("Content-Length", str(len(result)))
        self.end_headers()
        self.wfile.write(result)

    def log_message(self, *_):
        pass


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def request(port, length):
    sock = socket.create_connection(("127.0.0.1", port), timeout=10)
    sock.sendall((f"POST /v1/responses HTTP/1.1\r\nHost: localhost\r\n"
                  f"Content-Length: {length}\r\nConnection: close\r\n\r\n").encode())
    return sock


def response(sock):
    chunks = []
    while True:
        part = sock.recv(65536)
        if not part:
            break
        chunks.append(part)
    return b"".join(chunks)


def main():
    nginx = shutil.which("nginx") or "/usr/sbin/nginx"
    snippet = Path(__file__).resolve().parents[1] / "nginx/sub2api-client-body-timeout.conf"
    upstream = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Receiver)
    threading.Thread(target=upstream.serve_forever, daemon=True).start()
    baseline_port, candidate_port = free_port(), free_port()
    while candidate_port == baseline_port:
        candidate_port = free_port()
    with tempfile.TemporaryDirectory(prefix="sub2api-upload-idle-") as directory:
        root = Path(directory)
        servers = []
        for port, timeout in [(baseline_port, "client_body_timeout 60s;"),
                              (candidate_port, f'include "{snippet}";')]:
            servers.append(f"""
            server {{
                listen 127.0.0.1:{port};
                client_max_body_size 256m;
                {timeout}
                location / {{
                    proxy_pass http://127.0.0.1:{upstream.server_port};
                    proxy_http_version 1.1;
                    proxy_request_buffering off;
                    proxy_buffering off;
                    proxy_read_timeout 240s;
                    proxy_send_timeout 240s;
                }}
            }}""")
        config = root / "nginx.conf"
        config.write_text(f"""
            daemon off;
            master_process off;
            pid {root}/nginx.pid;
            error_log {root}/error.log warn;
            events {{ worker_connections 32; }}
            http {{
                log_format outcome '$server_port $status';
                access_log {root}/access.log outcome;
                {''.join(servers)}
            }}
        """)
        process = subprocess.Popen([nginx, "-p", directory, "-c", str(config)],
                                   stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
        try:
            for _ in range(50):
                if process.poll() is not None:
                    raise RuntimeError(process.stderr.read().decode())
                try:
                    with socket.create_connection(("127.0.0.1", candidate_port), timeout=.1):
                        break
                except OSError:
                    time.sleep(.1)
            with request(baseline_port, len(BODY)) as baseline, request(candidate_port, len(BODY)) as candidate:
                baseline.sendall(BODY[:65536])
                candidate.sendall(BODY[:65536])
                print("Testing a 65-second upload pause across the former 60-second boundary...", flush=True)
                deadline = time.monotonic() + 65
                while time.monotonic() < deadline:
                    time.sleep(max(0, min(1, deadline - time.monotonic())))
                old = response(baseline)
                # With streaming proxy uploads nginx may close the client
                # without a response body; its access status is authoritative.
                assert not old or old.startswith(b"HTTP/1.1 408"), old[:150]
                assert f"{baseline_port} 408" in (root / "access.log").read_text()
                candidate.sendall(BODY[65536:])
                new = response(candidate)
                assert new.startswith(b"HTTP/1.1 200"), new[:150]
                payload = json.loads(new.split(b"\r\n\r\n", 1)[1])
                assert payload == {"bytes": len(BODY), "sha256": DIGEST}, payload
            with request(candidate_port, 256 * 1024 * 1024 + 1) as too_large:
                limited = response(too_large)
                assert limited.startswith(b"HTTP/1.1 413"), limited[:150]
            print(json.dumps({"baseline_status": 408, "candidate_status": 200,
                              "pause_seconds": 65, "verified_bytes": len(BODY),
                              "oversized_status": 413}))
        finally:
            process.terminate()
            process.wait(timeout=10)
            upstream.shutdown()
            upstream.server_close()


if __name__ == "__main__":
    main()
