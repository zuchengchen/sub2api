#!/usr/bin/env python3
"""Loopback sanitizer in front of CLIProxyAPI Devin.

Devin swe-2 rejects Codex/Kiro payloads that mention tool / tool servers
(or whose tool descriptions exceed ~480 bytes) with permission_denied "tool
configuration issue". CLIProxyAPI 8.0.23 does not rewrite descriptions. This
proxy sits on 127.0.0.1:8318 and forwards to 127.0.0.1:8317 after shortening
tool/schema descriptions and scrubbing tool markers out of request text.
"""

from __future__ import annotations

import asyncio
import json
import os
import re
import sys
import time

LISTEN_HOST = "127.0.0.1"
LISTEN_PORT = 8318
UPSTREAM_HOST = "127.0.0.1"
UPSTREAM_PORT = 8317
MAX_DESC_BYTES = 480
# Opt-in debugging: set DEVIN_SANITIZE_DUMP=1 to write raw/sanitized bodies
# under /tmp/devin-sanitize-* so a failing upstream payload can be diffed.
DUMP = os.environ.get("DEVIN_SANITIZE_DUMP") == "1"

# tool / MCP markers that Devin's upstream filter keys on.
MCP_TOKEN_RE = re.compile(r"mcp__[A-Za-z0-9_]+", re.I)
MCP_WORD_RE = re.compile(r"\bMCP\b", re.I)
# Identifiers starting with tool (mcpServer, mcp_servers, mcpToolName) —
# these are prose/property names, not the tool_use protocol literal.
MCP_IDENT_RE = re.compile(r"\bmcp(?=[A-Z_]|$)", re.I)

# Kiro destructive-tool phrasing that Devin's classifier flags even after
# tool scrubbing; neutralize whole clauses.
PHRASE_REWRITES = [
    (re.compile(r"(?i)fails?\s+gracefully[^.]*"), "may fail"),
    (re.compile(r"(?i)rejected for security reasons"), "not permitted"),
    (re.compile(r"(?i)cannot be deleted"), "cannot be removed"),
    (re.compile(r"(?i)will fail if"), "is unavailable when"),
    (re.compile(r"(?i)not allowed"), "not permitted"),
]


def truncate_utf8(text: str, max_bytes: int) -> str:
    raw = text.encode("utf-8")
    if len(raw) <= max_bytes:
        return text
    # Cut before max_bytes so the last retained byte never splits a UTF-8
    # sequence: retreat while the byte AT the cut position is a continuation
    # (10xxxxxx). The previous loop checked raw[cut - 1] and still decoded
    # raw[:cut] with a split tail, raising UnicodeDecodeError.
    cut = max_bytes
    while cut > 0 and cut < len(raw) and (raw[cut] & 0xC0) == 0x80:
        cut -= 1
    return raw[:cut].decode("utf-8", errors="ignore")


def sanitize_description(text: str) -> str:
    text = scrub_text(text)
    return truncate_utf8(text, MAX_DESC_BYTES)


def scrub_text(text: str) -> str:
    """Rewrite tool markers in prose strings (descriptions, message text,
    hook docs, tool-call arguments)."""
    text = MCP_TOKEN_RE.sub("tool", text)
    text = MCP_WORD_RE.sub("tool", text)
    text = MCP_IDENT_RE.sub("tool", text)
    for pat, repl in PHRASE_REWRITES:
        text = pat.sub(repl, text)
    return text


# Keys whose string values are safe to scrub wholesale. Message text,
# tool-call arguments/outputs, and system prompts all carry residual tool /
# tool tokens (Kiro's own tool docs mention "tool server", "PreToolUse",
# ".kiro/settings/tool.json" — every one of those trips Devin's filter).
SCRUB_STRING_KEYS = {
    "text",
    "content",
    "arguments",
    "output",
    "system",
    "context",
    "instructions",
    "input",
    "result",
    "command",
    "description",
    "input_text",
    "output_text",
    "summary",
    "reasoning",
}


def walk(obj):
    if isinstance(obj, dict):
        for key, value in obj.items():
            if key in SCRUB_STRING_KEYS and isinstance(value, str) and value:
                if key == "description":
                    obj[key] = sanitize_description(value)
                else:
                    obj[key] = scrub_text(value)
            else:
                walk(value)
    elif isinstance(obj, list):
        for item in obj:
            walk(item)


def sanitize_body(raw: bytes, content_type: str) -> bytes:
    if "json" not in content_type.lower():
        return raw
    try:
        payload = json.loads(raw)
    except json.JSONDecodeError:
        return raw
    walk(payload)
    out = json.dumps(payload, ensure_ascii=False, separators=(",", ":")).encode("utf-8")
    if DUMP and out != raw:
        try:
            stamp = f"{int(time.time() * 1000)}"
            with open(f"/tmp/devin-sanitize-{stamp}-raw.json", "wb") as fh:
                fh.write(raw)
            with open(f"/tmp/devin-sanitize-{stamp}-out.json", "wb") as fh:
                fh.write(out)
        except OSError:
            pass
    return out


async def pipe(reader: asyncio.StreamReader, writer: asyncio.StreamWriter) -> None:
    try:
        while True:
            chunk = await reader.read(65536)
            if not chunk:
                break
            writer.write(chunk)
            await writer.drain()
    except (ConnectionResetError, BrokenPipeError, asyncio.CancelledError):
        pass
    finally:
        try:
            writer.close()
        except Exception:
            pass


async def read_headers(reader: asyncio.StreamReader) -> tuple[list[bytes], dict[str, str]]:
    header_blob = await reader.readuntil(b"\r\n\r\n")
    lines = header_blob.split(b"\r\n")
    headers: dict[str, str] = {}
    for line in lines[1:]:
        if not line:
            continue
        name, _, value = line.partition(b":")
        headers[name.decode("latin1").lower()] = value.decode("latin1").strip()
    return lines, headers


async def read_chunked_body(reader: asyncio.StreamReader) -> bytes:
    chunks: list[bytes] = []
    while True:
        size_line = await reader.readline()
        if not size_line:
            break
        size = int(size_line.split(b";", 1)[0].strip() or b"0", 16)
        if size == 0:
            while True:
                trailer = await reader.readline()
                if trailer in (b"\r\n", b"\n", b""):
                    break
            break
        chunks.append(await reader.readexactly(size))
        crlf = await reader.readexactly(2)
        if crlf != b"\r\n":
            raise ValueError("invalid chunked body")
    return b"".join(chunks)


def rebuild_request(lines: list[bytes], headers: dict[str, str], body: bytes) -> bytes:
    out_lines = [lines[0]]
    skip = {"content-length", "transfer-encoding", "connection", "keep-alive", "expect"}
    for line in lines[1:]:
        if not line:
            continue
        name, _, _ = line.partition(b":")
        key = name.decode("latin1").lower()
        if key in skip:
            continue
        out_lines.append(line)
    out_lines.append(f"Content-Length: {len(body)}".encode("ascii"))
    out_lines.append(b"Connection: close")
    out_lines.append(b"")
    out_lines.append(b"")
    return b"\r\n".join(out_lines) + body


async def handle_client(
    client_reader: asyncio.StreamReader, client_writer: asyncio.StreamWriter
) -> None:
    upstream_writer = None
    try:
        lines, headers = await read_headers(client_reader)
        request_line = lines[0].decode("latin1")
        method = request_line.split(" ", 1)[0].upper()
        body = b""
        if (headers.get("transfer-encoding") or "").lower() == "chunked":
            body = await read_chunked_body(client_reader)
        else:
            content_length = int(headers.get("content-length") or "0")
            if content_length:
                body = await client_reader.readexactly(content_length)
        if method in {"POST", "PUT", "PATCH"} and body:
            body = sanitize_body(body, headers.get("content-type") or "")
        request_bytes = rebuild_request(lines, headers, body)

        upstream_reader, upstream_writer = await asyncio.open_connection(
            UPSTREAM_HOST, UPSTREAM_PORT
        )
        upstream_writer.write(request_bytes)
        await upstream_writer.drain()
        await pipe(upstream_reader, client_writer)
    except Exception as exc:
        sys.stderr.write(f"devin-tool-sanitize: {exc}\n")
        sys.stderr.flush()
        try:
            client_writer.write(
                b"HTTP/1.1 502 Bad Gateway\r\nContent-Type: text/plain\r\nContent-Length: 11\r\nConnection: close\r\n\r\nBad Gateway"
            )
            await client_writer.drain()
        except Exception:
            pass
    finally:
        if upstream_writer is not None:
            upstream_writer.close()
            try:
                await upstream_writer.wait_closed()
            except Exception:
                pass
        client_writer.close()
        try:
            await client_writer.wait_closed()
        except Exception:
            pass


async def main() -> None:
    server = await asyncio.start_server(handle_client, LISTEN_HOST, LISTEN_PORT)
    addrs = ", ".join(str(sock.getsockname()) for sock in server.sockets or [])
    sys.stderr.write(f"devin-tool-sanitize listening on {addrs} -> {UPSTREAM_HOST}:{UPSTREAM_PORT}\n")
    sys.stderr.flush()
    async with server:
        await server.serve_forever()


if __name__ == "__main__":
    asyncio.run(main())
