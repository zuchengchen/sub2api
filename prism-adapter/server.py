"""Loopback-only Prism browser adapter for OpenAI OAuth accounts.

Prism's web UI owns session, sandbox, and start/status requests. This adapter
observes their terminal result and never fabricates token deltas or usage.
"""

import hmac
import hashlib
import json
import os
import re
import threading
import queue
from concurrent.futures import Future
import time
import sys
import traceback
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from types import SimpleNamespace
from urllib.parse import parse_qs, urlparse

from playwright.sync_api import sync_playwright

from model_selection import MODELS, EFFORTS, select_options
from tool_bridge import ToolBridge, has_tools, strict_json
from tool_state import ToolState, digest
from response_events import completed_events


BASE = "https://prism.openai.com"
START = "/api/llm/response_with_tools_start"
STATUS = "/api/llm/response_with_tools_status"
MAX_REQUEST_BYTES = 1 << 20
MAX_PROMPT_CHARS = 32000
SESSION_ID = re.compile(r"^[0-9a-f]{64}$")
MODEL = "gpt-6.1-sol"
PROJECT_ID = re.compile(r"^[0-9a-f]{8}-[0-9a-f-]{27,}$")
ACCOUNT_ID = re.compile(r"^[1-9][0-9]{0,18}$")
USER_AGENT = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/153 Safari/537.36"


class AdapterError(Exception):
    def __init__(self, status, code, message, *, not_submitted=False):
        super().__init__(message)
        self.status = status
        self.code = code
        self.not_submitted = not_submitted


def parse_prompt(payload):
    if not isinstance(payload, dict) or not isinstance(payload.get("model"), str) or payload["model"] not in MODELS:
        raise AdapterError(422, "unsupported_model", "Unsupported Prism model; choose " + ", ".join(MODELS))
    if payload.get("tools") or payload.get("additional_tools") or payload.get("previous_response_id") or payload.get("conversation"):
        raise AdapterError(422, "unsupported_request", "Prism adapter does not yet support tools or server-side conversation state")
    if any(payload.get(key) is not None for key in ("max_output_tokens", "temperature", "top_p")) or payload.get("background") or payload.get("store"):
        raise AdapterError(422, "unsupported_request", "Generation limits, sampling, background and storage options are not supported")
    # Codex requests optional reasoning fields even on text-only turns. Prism
    # does not supply encrypted reasoning; accepting the request invents none.
    if (payload.get("tool_choice", "none") not in ("none", "auto")
            or payload.get("include") not in (None, [], ['reasoning.encrypted_content']) or payload.get("service_tier")):
        raise AdapterError(422, "unsupported_request", "Requested response options are not supported")
    text_options = payload.get("text") or {}
    if not isinstance(text_options, dict):
        raise AdapterError(422, "unsupported_request", "Only plain text output is supported")
    text_format = text_options.get("format")
    if text_format is None:
        text_format = {"type": "text"}
    if not isinstance(text_format, dict) or text_format.get("type", "text") != "text":
        raise AdapterError(422, "unsupported_request", "Only plain text output is supported")
    reasoning = payload.get("reasoning") or {}
    if (not isinstance(reasoning, dict) or not isinstance(reasoning.get("effort", "medium"), str)
            or reasoning.get("effort", "medium") not in EFFORTS
            or reasoning.get("summary") not in (None, "none", "auto", "concise", "detailed")):
        raise AdapterError(422, "unsupported_reasoning", "Unsupported Prism reasoning effort")
    if not isinstance(payload.get("stream", False), bool):
        raise AdapterError(400, "invalid_request", "stream must be a boolean")
    items = payload.get("input")
    if isinstance(items, str):
        items = [{"role": "user", "content": items}]
    if not isinstance(items, list) or not items:
        raise AdapterError(400, "invalid_request", "input must contain text")
    parts = []
    instructions = payload.get("instructions", "")
    if instructions:
        if not isinstance(instructions, str):
            raise AdapterError(400, "invalid_request", "instructions must be text")
        parts.append("[instructions]\n" + instructions)
    for item in items:
        if not isinstance(item, dict) or item.get("type", "message") != "message":
            raise AdapterError(422, "unsupported_input", "Prism adapter accepts text messages only")
        role = item.get("role", "user")
        if role not in ("user", "assistant", "system", "developer"):
            raise AdapterError(400, "invalid_request", "invalid message role")
        content = item.get("content")
        if isinstance(content, str):
            text = content
        elif isinstance(content, list) and content and all(isinstance(x, dict) and x.get("type") in ("input_text", "output_text", "text") and isinstance(x.get("text"), str) for x in content):
            text = "\n".join(x["text"] for x in content)
        else:
            raise AdapterError(422, "unsupported_input", "Prism adapter accepts text messages only")
        if not text.strip():
            raise AdapterError(400, "invalid_request", "message content must not be empty")
        parts.append("[" + role + "]\n" + text)
    prompt = "\n\n".join(parts)
    if not prompt.strip() or len(prompt) > MAX_PROMPT_CHARS:
        raise AdapterError(400, "invalid_request", "text input is empty or too long")
    return prompt, payload.get("stream", False)


def terminal_text(data):
    if not isinstance(data, dict):
        return None
    response = data.get("response") or {}
    if data.get("status") not in ("completed", "failed", "error"):
        return None
    if data.get("status") in ("failed", "error"):
        return AdapterError(502, "prism_failed", "Prism turn failed")
    if not isinstance(response, dict) or response.get("status") not in ("success", "failed", "error"):
        return None
    if response.get("status") in ("failed", "error"):
        return AdapterError(502, "prism_failed", "Prism turn failed")
    output = (response.get("payload") or {}).get("output") or []
    texts = [part.get("text", "") for item in output if isinstance(item, dict) and item.get("type") == "message"
             for part in item.get("content", []) if isinstance(part, dict) and isinstance(part.get("text"), str)]
    if not texts:
        return AdapterError(502, "unsupported_output", "Prism returned no text message")
    return "".join(texts)


class State:
    def __init__(self, directory):
        self.directory = Path(directory)
        self.directory.mkdir(mode=0o700, parents=True, exist_ok=True)
        self.pending = self.directory / "pending"
        self.pending.mkdir(mode=0o700, exist_ok=True)
        self.projects = self.directory / "projects.json"
        self.receipts = self.directory / "receipts"
        self.receipts.mkdir(mode=0o700, exist_ok=True)

    @staticmethod
    def sync_directory(directory):
        fd = os.open(directory, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(fd)
        finally:
            os.close(fd)

    @staticmethod
    def atomic_write(path, data):
        tmp = path.with_name(path.name + ".tmp")
        fd = os.open(tmp, os.O_CREAT | os.O_TRUNC | os.O_WRONLY, 0o600)
        with os.fdopen(fd, "w") as file:
            json.dump(data, file)
            file.flush()
            os.fsync(file.fileno())
        os.replace(tmp, path)
        State.sync_directory(path.parent)

    def ensure_idle(self, account_id):
        if (self.pending / account_id).exists():
            raise AdapterError(409, "pending_turn", "Previous Prism turn outcome is unknown; inspect it before a new request")

    def project(self, account_id):
        try:
            project = json.loads(self.projects.read_text()).get(account_id)
        except FileNotFoundError:
            return None
        except (ValueError, OSError):
            raise AdapterError(503, "invalid_state", "Prism project state is unreadable") from None
        return project if isinstance(project, str) and PROJECT_ID.fullmatch(project) else None

    def save_project(self, account_id, project_id):
        if not PROJECT_ID.fullmatch(project_id):
            raise AdapterError(502, "invalid_project", "Prism returned an invalid project identifier")
        try:
            projects = json.loads(self.projects.read_text())
        except FileNotFoundError:
            projects = {}
        if not isinstance(projects, dict):
            raise AdapterError(503, "invalid_state", "Prism project state is invalid")
        projects[account_id] = project_id
        self.atomic_write(self.projects, projects)

    def begin(self, account_id, project_id=None):
        try:
            fd = os.open(self.pending / account_id, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
        except FileExistsError:
            raise AdapterError(409, "pending_turn", "Previous Prism turn outcome is unknown; inspect it before a new request") from None
        with os.fdopen(fd, "w") as file:
            json.dump({"stage": "submitting", "project_id": project_id, "at": int(time.time())}, file)
            file.flush()
            os.fsync(file.fileno())
        self.sync_directory(self.pending)

    def finish(self, account_id):
        (self.pending / account_id).unlink()
        self.sync_directory(self.pending)

    def update(self, account_id, data):
        path = self.pending / account_id
        previous = json.loads(path.read_text())
        previous.update(data)
        self.atomic_write(path, previous)

    def receipt(self, account_id, request_id, start_count, status_count, result, cache_hit=False, *, model=MODEL, effort="medium"):
        receipt_id = hashlib.sha256(request_id.encode()).hexdigest()
        data = {"account_id": account_id, "request_id": request_id, "model": model, "reasoning_effort": effort,
                "start_count": start_count, "status_count": status_count, "completed_at": int(time.time()),
                "status": "failed" if isinstance(result, AdapterError) else "completed", "usage_source": "unavailable",
                "session_cache_hit": cache_hit}
        if isinstance(result, str):
            data["answer_sha256"] = hashlib.sha256(result.encode()).hexdigest()
            data["answer_chars"] = len(result)
        self.atomic_write(self.receipts / (receipt_id + ".json"), data)


class StartGate:
    """Authorize one exact start; browser retries are rejected before sending."""
    def __init__(self, model=MODEL, effort="medium"):
        self.model, self.effort = model, effort
        self.armed = False
        self.sent = False
        self.error = None

    def accept(self, body):
        metadata = body.get("metadata") if isinstance(body, dict) else None
        if (not self.armed or self.sent or not isinstance(metadata, dict)
                or metadata.get("model") != self.model or metadata.get("reasoning_effort") != self.effort):
            self.error = "Prism attempted an unarmed, repeated or mismatched model start"
            return False
        self.sent = True
        return True


class BrowserSession:
    """One private browser context and project, owned by the browser worker."""
    def __init__(self, browser, token):
        self.context = browser.new_context(user_agent=USER_AGENT, service_workers="block")
        try:
            self.context.add_cookies([{"name": "prism_oai_access_token", "value": token,
                                      "domain": "prism.openai.com", "path": "/", "secure": True}])
            self.page = self.context.new_page()
            self.page.set_default_timeout(60000)
            self.project = None
            self.created = self.used = time.monotonic()
            self.active = None
            self.page.route("**/api/llm/**", self.route)
            self.page.on("response", self.response)
        except BaseException:
            self.context.close()
            raise

    def route(self, route):
        if self.active is None:
            route.abort()
        else:
            self.active.route(route)

    def response(self, response):
        if self.active is not None:
            self.active.response(response)

    def close(self):
        self.active = None
        self.context.close()


class BrowserRequest:
    def __init__(self, state, account_id, session, model=MODEL, effort="medium"):
        self.state, self.account_id, self.session = state, account_id, session
        self.gate = StartGate(model, effort)
        self.request_id = ""
        self.polls = 0
        self.accepted = set()
        self.deferred_poll = None
        self.terminal = None
        self.began = False

    def route(self, route):
        request = route.request
        try:
            body = request.post_data_json
            if request.url == BASE + STATUS:
                # response.json() yields to Playwright's event loop. The page
                # may poll before the start response callback has decoded its
                # ID. Hold one route until that trusted ID is available.
                if self.gate.sent and not self.request_id and self.deferred_poll is None and isinstance(body, dict):
                    self.deferred_poll = route
                    return
                if (not self.terminal and self.gate.sent and self.request_id and isinstance(body, dict)
                        and body.get("request_id") == self.request_id):
                    self.accepted.add(request)
                    self.polls += 1
                    route.continue_()
                    return
            elif request.url == BASE + START:
                if self.gate.accept(body):
                    # A cached page must never submit against another project.
                    if (body.get("metadata") or {}).get("projectId") != self.session.project:
                        self.gate.sent = False
                        self.gate.error = "Prism attempted to use another project"
                    else:
                        self.accepted.add(request)
                        route.continue_()
                        return
        except Exception:
            self.gate.error = "Prism request could not be validated"
        route.abort()

    def response(self, response):
        # An old poll may arrive after a cached page starts a new turn. Correlate
        # the actual browser request as well as the upstream request identifier.
        if response.request not in self.accepted:
            return
        try:
            data = response.json()
            if not isinstance(data, dict):
                return
            if self.request_id and data.get("request_id") not in (None, "", self.request_id):
                self.gate.error = "Prism returned a foreign request identifier"
                return
            if response.url == BASE + START and isinstance(data.get("request_id"), str):
                self.request_id = data["request_id"]
            if self.request_id:
                update = {"stage": "polling", "request_id": self.request_id}
                if isinstance(data.get("turn_state"), (str, dict)) and data["turn_state"]:
                    update["turn_state"] = data["turn_state"]
                self.state.update(self.account_id, update)
            result = terminal_text(data)
            if result is not None:
                self.terminal = (response.status, result)
        except (ValueError, TypeError):
            pass
        if self.request_id and self.deferred_poll is not None:
            route, self.deferred_poll = self.deferred_poll, None
            self.route(route)

    def run(self, prompt, cache_hit):
        page = self.session.page
        textarea = page.locator('textarea[placeholder="Ask anything"]')
        textarea.wait_for(state="visible", timeout=90000)
        select_options(page, self.gate.model, self.gate.effort, AdapterError)
        textarea.fill(prompt)
        self.state.begin(self.account_id, self.session.project)
        self.began = True
        self.state.update(self.account_id, {"model": self.gate.model, "reasoning_effort": self.gate.effort})
        self.gate.armed = True
        textarea.press("Enter")
        deadline = time.monotonic() + 240
        while time.monotonic() < deadline and not self.terminal and not self.gate.error:
            page.wait_for_timeout(500)
        if not self.gate.sent:
            raise AdapterError(502, "start_not_sent", "Prism did not submit the turn; no request was sent upstream")
        if self.gate.error:
            raise AdapterError(502, "unexpected_start", "Prism did not start exactly one turn with the requested model")
        if not self.terminal:
            raise AdapterError(504, "unknown_outcome", "Prism turn has no terminal result; pending state retained")
        status, result = self.terminal
        if status != 200:
            raise AdapterError(502, "prism_failed", "Prism returned a failed turn")
        if not self.request_id:
            raise AdapterError(502, "missing_request_id", "Prism turn lacks a request identifier; pending state retained")
        self.state.receipt(self.account_id, self.request_id, 1, self.polls, result, cache_hit,
                           model=self.gate.model, effort=self.gate.effort)
        self.state.finish(self.account_id)
        if isinstance(result, AdapterError):
            raise result
        return self.request_id, result


class BrowserTurn:
    """Bounded session cache. Every method runs on the same worker thread."""
    def __init__(self, state, chrome, max_sessions=1, idle_seconds=300):
        if not 1 <= max_sessions <= 2 or not 30 <= idle_seconds <= 900:
            raise ValueError("session cache limits are out of range")
        self.state, self.chrome = state, chrome
        self.max_sessions, self.idle_seconds = max_sessions, idle_seconds
        self.sessions = {}
        self.manager = self.browser = None

    def _browser(self):
        if self.browser is None:
            self.manager = sync_playwright()
            playwright = self.manager.__enter__()
            try:
                self.browser = playwright.chromium.launch(executable_path=self.chrome, headless=True, chromium_sandbox=True)
            except BaseException:
                self.manager.__exit__(*sys.exc_info())
                self.manager = None
                raise
        return self.browser

    def discard(self, key):
        session = self.sessions.pop(key, None)
        if session is not None:
            session.close()

    def prune(self):
        now = time.monotonic()
        for key, session in list(self.sessions.items()):
            if now - session.used >= self.idle_seconds or now - session.created >= 900:
                self.discard(key)
        if not self.sessions:
            self.close()

    def close(self):
        # Dispose all contexts even if closing one already-crashed page fails.
        for key in list(self.sessions):
            try:
                self.discard(key)
            except Exception:
                pass
        try:
            if self.browser is not None:
                self.browser.close()
        finally:
            self.browser = None
            if self.manager is not None:
                manager, self.manager = self.manager, None
                manager.__exit__(None, None, None)

    def run(self, account_id, token, prompt, session_id=None, model=MODEL, effort="medium", reuse_project=True):
        self.state.ensure_idle(account_id)
        self.prune()
        # Rotate credentials by discarding every cached context for that account.
        # No access token, cookie, page or sandbox token is persisted by this cache.
        identity = hashlib.sha256(token.encode()).digest()
        for old in list(self.sessions):
            if old[0] == account_id and old[2] != identity:
                self.discard(old)
        key = (account_id, session_id if reuse_project else None, identity)
        session = self.sessions.get(key) if session_id and reuse_project else None
        cache_hit = session is not None
        request = None
        succeeded = False
        try:
            if session is None:
                while len(self.sessions) >= self.max_sessions:
                    self.discard(min(self.sessions, key=lambda k: self.sessions[k].used))
                session = BrowserSession(self._browser(), token)
                # Include transient contexts in the limit and teardown path too.
                self.sessions[key] = session
                page = session.page
                page.goto(BASE, wait_until="domcontentloaded", timeout=60000)
                page.get_by_role("button", name="New", exact=True).click(timeout=60000)
                page.get_by_role("menuitem", name="Blank project").click(timeout=60000)
                page.wait_for_function("new URL(location.href).searchParams.has('u')", timeout=60000)
                session.project = parse_qs(urlparse(page.url).query).get("u", [""])[0]
                self.state.save_project(account_id, session.project)
                page.goto(BASE + "/?u=" + session.project + "&pg=1", wait_until="domcontentloaded", timeout=60000)
            else:
                # Project files belong to this explicit client session. Start a
                # new chat tab so caller-supplied history is not appended twice.
                session.page.get_by_role("button", name="New chat tab", exact=True).click(timeout=60000)
            request = BrowserRequest(self.state, account_id, session, model, effort)
            session.active = request
            result = request.run(prompt, cache_hit)
            succeeded = True
            session.used = time.monotonic()
            return result
        except Exception as error:
            error.not_submitted = request is None or not request.gate.sent
            raise
        finally:
            if session is not None:
                session.active = None
            try:
                if not succeeded or not session_id or not reuse_project:
                    self.discard(key)
            finally:
                # Discarding the cache never clears an ambiguous submission.
                if request is not None and request.began and not request.gate.sent:
                    self.state.finish(account_id)
                if not self.sessions:
                    self.close()


class BrowserWorker:
    """Own synchronous Playwright on one thread, including idle expiration."""
    def __init__(self, factory):
        self.factory = factory
        self.jobs = queue.Queue(maxsize=1)
        self.busy = threading.Lock()
        self.lifecycle = threading.Lock()
        self.stopping = threading.Event()
        self.ready = threading.Event()
        self.startup_error = None
        self.thread = threading.Thread(target=self._serve, name="prism-browser", daemon=True)
        self.thread.start()

    def _serve(self):
        try:
            browser = self.factory()
        except BaseException as error:
            self.startup_error = error
            self.ready.set()
            return
        self.ready.set()
        try:
            while True:
                try:
                    job = self.jobs.get(timeout=1)
                except queue.Empty:
                    if self.stopping.is_set():
                        break
                    try:
                        browser.prune()
                    except Exception:
                        break
                    continue
                args, future = job
                if self.stopping.is_set():
                    if not future.done():
                        future.set_exception(AdapterError(503, "prism_unavailable", "Prism browser worker is stopping"))
                    continue
                try:
                    result = browser.run(*args)
                    if not future.done():
                        future.set_result(result)
                except Exception as error:
                    if not future.done():
                        future.set_exception(error)
                except BaseException:
                    if not future.done():
                        future.set_exception(AdapterError(503, "prism_unavailable", "Prism browser worker stopped"))
                    return
        finally:
            with self.lifecycle:
                self.stopping.set()
                while not self.jobs.empty():
                    _, future = self.jobs.get_nowait()
                    if not future.done():
                        future.set_exception(AdapterError(503, "prism_unavailable", "Prism browser worker stopped"))
            try:
                browser.close()
            except Exception as error:
                # Browser errors can contain page data; never dump their text.
                print(json.dumps({"event": "prism_worker_close_error", "class": type(error).__name__}), file=sys.stderr, flush=True)

    def run(self, *args):
        if not self.busy.acquire(blocking=False):
            raise AdapterError(429, "prism_busy", "Prism browser is busy; request was not submitted")
        try:
            if not self.ready.wait(timeout=5):
                raise AdapterError(503, "prism_unavailable", "Prism browser worker did not start", not_submitted=True)
            if self.startup_error is not None:
                raise AdapterError(503, "prism_unavailable", "Prism browser worker failed to start", not_submitted=True) from self.startup_error
            future = Future()
            with self.lifecycle:
                if self.stopping.is_set() or not self.thread.is_alive():
                    raise AdapterError(503, "prism_unavailable", "Prism browser worker is unavailable", not_submitted=True)
                self.jobs.put_nowait((args, future))
            return future.result()
        finally:
            self.busy.release()

    def close(self):
        with self.lifecycle:
            self.stopping.set()
        self.thread.join(timeout=8)


def response_payload(request_id, text, model=MODEL, effort="medium"):
    safe_id = re.sub(r"[^a-zA-Z0-9_-]", "_", request_id)[:100]
    if not safe_id:
        raise AdapterError(502, "missing_request_id", "Prism did not return a request identifier")
    return {
        "id": "resp_prism_" + safe_id,
        "object": "response",
        "created_at": int(time.time()),
        "model": model,
        "reasoning": {"effort": effort},
        "status": "completed",
        "usage": None,
        "output": [{"id": "msg_prism_" + safe_id, "type": "message", "role": "assistant",
                    "status": "completed", "content": [{"type": "output_text", "text": text, "annotations": []}]}],
    }


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    state = None
    browser_turn = None
    api_key = None
    lock = threading.Lock()
    serialize_requests = True
    tool_state = None

    def setup(self):
        super().setup()
        self.connection.settimeout(30)

    def log_message(self, *_args):
        pass

    def send_json(self, status, payload):
        body = json.dumps(payload, ensure_ascii=False, separators=(",", ":")).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Connection", "close")
        self.close_connection = True
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path == "/health":
            self.send_json(200, {"status": "ok"})
        else:
            self.send_json(404, {"error": {"type": "not_found"}})

    def do_POST(self):
        if self.path != "/v1/responses":
            self.send_json(404, {"error": {"type": "not_found"}})
            return
        bearer = self.headers.get("Authorization", "")
        if not hmac.compare_digest(bearer, "Bearer " + self.api_key):
            self.send_json(401, {"error": {"type": "unauthorized"}})
            return
        account_id = self.headers.get("X-Prism-Account-ID", "")
        token = self.headers.get("X-Prism-OAuth-Token", "")
        session_ids = self.headers.get_all("X-Prism-Session-ID", [])
        session_id = session_ids[0] if session_ids else ""
        if len(session_ids) > 1 or (session_ids and not SESSION_ID.fullmatch(session_id)):
            self.send_json(400, {"error": {"type": "invalid_request", "message": "session identity is invalid"}})
            return
        session_id = session_id or None
        if not ACCOUNT_ID.fullmatch(account_id) or not token or "\n" in token or "\r" in token:
            self.send_json(400, {"error": {"type": "invalid_request", "message": "account identity is required"}})
            return
        try:
            if self.headers.get("Transfer-Encoding") or len(self.headers.get_all("Content-Length", [])) != 1:
                raise AdapterError(400, "invalid_request", "One Content-Length header is required")
            length = int(self.headers.get("Content-Length", "0"))
            if length < 1 or length > MAX_REQUEST_BYTES:
                raise AdapterError(413, "request_too_large", "request body is empty or too large")
            payload = strict_json(self.rfile.read(length))
            bridge = None
            scope = None
            if has_tools(payload):
                if self.tool_state is None:
                    raise AdapterError(422, 'tools_disabled', 'Prism client tool bridge is disabled')
                callers = self.headers.get_all('X-Prism-Caller-ID', [])
                if len(callers) != 1 or not SESSION_ID.fullmatch(callers[0]):
                    raise AdapterError(400, 'invalid_caller', 'Tool requests require a gateway-derived caller identity')
                scope = digest([account_id, callers[0], session_id])
                bridge = ToolBridge(payload, SimpleNamespace(AdapterError=AdapterError, parse_prompt=parse_prompt))
                prompt, stream = bridge.prompt, bridge.stream
            else:
                prompt, stream = parse_prompt(payload)
            model, effort = payload["model"], (payload.get("reasoning") or {}).get("effort", "medium")
            if self.serialize_requests and not self.lock.acquire(blocking=False):
                raise AdapterError(429, "prism_busy", "Prism browser is busy; request was not submitted")
            try:
                if bridge is not None:
                    self.tool_state.reserve(scope, bridge.calls, bridge.results, bridge.lease, bridge.needs_fresh)
                try:
                    args = (account_id, token, prompt, session_id, model, effort)
                    if bridge is not None:
                        # Client results already contain expanded history. A
                        # native chat/project is not their continuation state.
                        args += (False,)
                    request_id, answer = self.browser_turn.run(*args)
                except Exception as error:
                    if bridge is not None and (getattr(error,'not_submitted',False) or
                            getattr(error,'code',None) in ('prism_busy','resource_pressure','credential_rotation')):
                        self.tool_state.not_sent(scope, bridge.lease)
                    raise
            finally:
                if self.serialize_requests:
                    self.lock.release()
            response = response_payload(request_id, answer, model, effort)
            if bridge is not None:
                try:
                    output, calls = bridge.output(answer, request_id)
                except AdapterError:
                    # The upstream terminal is known, but no unvalidated call
                    # is released. These results were already fed to the model.
                    self.tool_state.complete(scope, bridge.lease, response['id'], [])
                    raise
                self.tool_state.complete(scope, bridge.lease, response['id'], calls)
                response['output'] = output
                if bridge.unavailable:
                    response['metadata'] = {'prism_unavailable_tools': ','.join(sorted(bridge.unavailable))}
            if stream:
                created = dict(response, status="in_progress", output=[])
                events = [
                    ("response.created", {"type": "response.created", "sequence_number": 0, "response": created}),
                    ("response.completed", {"type": "response.completed", "sequence_number": 1, "response": response}),
                ]
                if bridge is not None:
                    events = completed_events(response)
                body = "".join("event: " + name + "\ndata: " + json.dumps(data, ensure_ascii=False, separators=(",", ":")) + "\n\n" for name, data in events).encode()
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.send_header("Content-Length", str(len(body)))
                self.send_header("X-Request-Id", response["id"])
                self.send_header("Connection", "close")
                self.close_connection = True
                self.end_headers()
                self.wfile.write(body)
            else:
                self.send_json(200, response)
        except (BrokenPipeError, ConnectionResetError):
            self.close_connection = True
        except AdapterError as error:
            self.send_json(error.status, {"error": {"type": error.code, "message": str(error)}})
        except (ValueError, TypeError):
            self.send_json(400, {"error": {"type": "invalid_request", "message": "invalid JSON request"}})
        except Exception as error:
            # Never log exception messages: Playwright embeds headers and page
            # data in some failures. Frames and class identify the local phase.
            frames = traceback.extract_tb(error.__traceback__)
            own_frames = [frame.lineno for frame in frames if Path(frame.filename).name == "server.py"]
            print(json.dumps({"event": "prism_adapter_error", "class": type(error).__name__, "lines": own_frames}), file=sys.stderr, flush=True)
            self.send_json(502, {"error": {"type": "prism_unavailable", "message": "Prism browser request failed; inspect pending state before retrying"}})


def configure_client_tools(state):
    enabled = os.environ.get('PRISM_ADAPTER_CLIENT_TOOLS_ENABLED', 'true')
    if enabled not in ('true', 'false'):
        raise SystemExit('PRISM_ADAPTER_CLIENT_TOOLS_ENABLED must be true or false')
    if enabled == 'false':
        return None
    # Fail startup on an incomplete adapter upgrade, before accepting traffic.
    try:
        from jsonschema import Draft202012Validator
        from lark import Lark
        Draft202012Validator.check_schema({'type': 'object'})
        Lark('start: "ok"').parse('ok')
    except ImportError:
        raise SystemExit('Install prism-adapter/requirements.txt before enabling client tools') from None
    return ToolState(state.directory, AdapterError)


def main():
    if os.geteuid() == 0:
        raise SystemExit("Prism adapter must run as a non-root user")
    key = os.environ.get("PRISM_ADAPTER_API_KEY", "")
    chrome = os.environ.get("PRISM_ADAPTER_CHROME", "")
    if len(key) < 32 or not Path(chrome).is_file() or not os.environ.get("CHROME_DEVEL_SANDBOX"):
        raise SystemExit("adapter key, Chromium binary, and Chromium sandbox are required")
    Handler.api_key = key
    Handler.state = State(os.environ.get("PRISM_ADAPTER_STATE_DIR", "/var/lib/sub2api-prism"))
    Handler.tool_state = configure_client_tools(Handler.state)
    max_sessions = int(os.environ.get("PRISM_ADAPTER_MAX_SESSIONS", "1"))
    idle_seconds = int(os.environ.get("PRISM_ADAPTER_SESSION_TTL_SECONDS", "300"))
    if not 1 <= max_sessions <= 2 or not 30 <= idle_seconds <= 900:
        raise SystemExit("session cache limits are out of range")
    mode = os.environ.get("PRISM_ADAPTER_MODE", "browser")
    if mode == "browser":
        Handler.serialize_requests = True
        Handler.browser_turn = BrowserWorker(lambda: BrowserTurn(Handler.state, chrome, max_sessions, idle_seconds))
    elif mode == "multiplex":
        from multiplex_browser import MultiplexBrowser
        from multiplex_runtime import AsyncBrowserWorker, Admission
        active = int(os.environ.get("PRISM_ADAPTER_MAX_INFLIGHT", "20"))
        per_account = int(os.environ.get("PRISM_ADAPTER_ACCOUNT_MAX_INFLIGHT", str(active)))
        queued = int(os.environ.get("PRISM_ADAPTER_MAX_QUEUED", "30"))
        bootstrap = int(os.environ.get("PRISM_ADAPTER_BOOTSTRAP_CONCURRENCY", "1"))
        if not 1 <= bootstrap <= 2:
            raise SystemExit("PRISM_ADAPTER_BOOTSTRAP_CONCURRENCY must be 1 or 2")
        # Validate before starting the worker so invalid settings fail startup.
        api = sys.modules[__name__]
        Admission(api, active, per_account, queued)
        Handler.serialize_requests = False
        Handler.browser_turn = AsyncBrowserWorker(lambda: MultiplexBrowser(
            Handler.state, chrome, api, active=active, per_account=per_account,
            queued=queued, bootstrap=bootstrap, idle_seconds=idle_seconds), api)
    else:
        raise SystemExit("PRISM_ADAPTER_MODE must be browser or multiplex")
    server = ThreadingHTTPServer(("127.0.0.1", 8319), Handler)
    server.daemon_threads = True
    try:
        server.serve_forever()
    finally:
        server.server_close()
        Handler.browser_turn.close()


if __name__ == "__main__":
    main()
