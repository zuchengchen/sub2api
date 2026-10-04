import contextlib
import importlib.util
import json
import tempfile
import threading
import time
import types
import unittest
from pathlib import Path
from unittest import mock
from http.server import ThreadingHTTPServer
from urllib.error import HTTPError
from urllib.request import Request, urlopen


spec = importlib.util.spec_from_file_location("prism_adapter", Path(__file__).with_name("server.py"))
adapter = importlib.util.module_from_spec(spec)
spec.loader.exec_module(adapter)


class AdapterTests(unittest.TestCase):
    def test_codex_optional_reasoning_on_all_text_models(self):
        for model in adapter.MODELS:
            payload = {'model': model, 'input': 'hi', 'include': ['reasoning.encrypted_content'],
                       'reasoning': {'effort': 'high', 'summary': 'auto'}}
            self.assertEqual(adapter.parse_prompt(payload), ('[user]\nhi', False))
        for fields in ({'include': ['web_search_call.action.sources']},
                       {'reasoning': {'summary': 'detailed'}},
                       {'input': [{'type': 'reasoning', 'encrypted_content': 'fixture'}]}):
            with self.assertRaises(adapter.AdapterError):
                adapter.parse_prompt({'model': adapter.MODEL, 'input': 'hi', **fields})

    def test_client_tools_enabled_by_default_with_explicit_opt_out(self):
        with tempfile.TemporaryDirectory() as directory:
            state = adapter.State(directory)
            with mock.patch.dict('os.environ', {}, clear=True):
                self.assertIsNotNone(adapter.configure_client_tools(state))
            with mock.patch.dict('os.environ', {'PRISM_ADAPTER_CLIENT_TOOLS_ENABLED': 'false'}):
                self.assertIsNone(adapter.configure_client_tools(state))
            with mock.patch.dict('os.environ', {'PRISM_ADAPTER_CLIENT_TOOLS_ENABLED': 'invalid'}):
                with self.assertRaises(SystemExit):
                    adapter.configure_client_tools(state)

    def test_supported_model_and_efforts_are_preserved_without_aliases(self):
        for model in adapter.MODELS:
            for effort in adapter.EFFORTS:
                with self.subTest(model=model, effort=effort):
                    self.assertEqual(adapter.parse_prompt({"model":model,"reasoning":{"effort":effort},"input":"hi"}),
                                     ("[user]\nhi", False))
                    response = adapter.response_payload("fixture", "answer", model, effort)
                    self.assertEqual((response["model"], response["reasoning"]["effort"]), (model, effort))
                    gate = adapter.StartGate(model, effort)
                    gate.armed = True
                    self.assertTrue(gate.accept({"metadata":{"model":model,"reasoning_effort":effort}}))
                    self.assertFalse(gate.accept({"metadata":{"model":model,"reasoning_effort":effort}}))
        for value in (None, [], {}, 42, "6.1-sol", "gpt-6-astra"):
            with self.subTest(invalid_model=value), self.assertRaises(adapter.AdapterError) as raised:
                adapter.parse_prompt({"model":value,"input":"hi"})
            self.assertEqual(raised.exception.code, "unsupported_model")

    def test_valid_other_model_or_effort_cannot_replace_requested_options(self):
        for model in adapter.MODELS:
            for other in list(adapter.MODELS) + ['gpt-5.6-sol']:
                if model == other:
                    continue
                gate = adapter.StartGate(model, "xhigh")
                gate.armed = True
                self.assertFalse(gate.accept({"metadata":{"model":other,"reasoning_effort":"xhigh"}}))
                self.assertFalse(gate.sent)
            gate = adapter.StartGate(model, "xhigh")
            gate.armed = True
            self.assertFalse(gate.accept({"metadata":{"model":model,"reasoning_effort":"medium"}}))
            self.assertFalse(gate.sent)

    def test_text_request_keeps_model_and_stream(self):
        prompt, stream = adapter.parse_prompt({
            "model": "gpt-6.1-sol", "stream": True,
            "instructions": "Answer exactly.",
            "input": [{"role": "user", "content": [{"type": "input_text", "text": "hi"}]}],
        })
        self.assertEqual(prompt, "[instructions]\nAnswer exactly.\n\n[user]\nhi")
        self.assertTrue(stream)

    def test_unsupported_features_fail_closed(self):
        for change in ({"model": "gpt-6-astra"}, {"tools": [{"type": "function", "name": "x"}]},
                       {"previous_response_id": "resp_1"}, {"reasoning": {"effort": "unsupported"}}):
            request = {"model": "gpt-6.1-sol", "input": "hi", **change}
            with self.assertRaises(adapter.AdapterError):
                adapter.parse_prompt(request)

    def test_terminal_output_and_unknown_state(self):
        self.assertIsNone(adapter.terminal_text({"status": "running"}))
        self.assertEqual(adapter.terminal_text({"status": "completed", "response": {
            "status": "success", "payload": {"output": [{"type": "message", "content": [{"text": "21"}]}]}
        }}), "21")

    def test_pending_journal_blocks_ambiguous_replay(self):
        with tempfile.TemporaryDirectory() as directory:
            state = adapter.State(directory)
            state.begin("300")
            with self.assertRaises(adapter.AdapterError) as raised:
                state.begin("300")
            self.assertEqual(raised.exception.status, 409)
            state.finish("300")
            state.begin("300")
            state.finish("300")

    def test_http_boundary_uses_real_terminal_without_usage(self):
        class FakeBrowser:
            def run(self, account_id, token, prompt, session_id=None, model=adapter.MODEL, effort="medium"):
                self.assert_values = (account_id, token, prompt, session_id, model, effort)
                return "prism-123", "21"

        fake = FakeBrowser()
        handler = type("TestHandler", (adapter.Handler,), {"api_key": "test-key", "browser_turn": fake})
        server = ThreadingHTTPServer(("127.0.0.1", 0), handler)
        server.daemon_threads = True
        worker = threading.Thread(target=server.serve_forever, daemon=True)
        worker.start()
        try:
            url = f"http://127.0.0.1:{server.server_port}/v1/responses"
            data = json.dumps({"model": "gpt-6.1-sol", "reasoning":{"effort":"xhigh"}, "input": "candy"}).encode()
            headers = {"Authorization": "Bearer test-key", "X-Prism-Account-ID": "300",
                       "X-Prism-OAuth-Token": "oauth-token", "X-Prism-Session-ID": "a" * 64,
                       "Content-Type": "application/json"}
            with urlopen(Request(url, data=data, headers=headers), timeout=5) as response:
                body = json.load(response)
            self.assertEqual(fake.assert_values, ("300", "oauth-token", "[user]\ncandy", "a" * 64, "gpt-6.1-sol", "xhigh"))
            self.assertEqual((body["model"], body["reasoning"]["effort"]), ("gpt-6.1-sol", "xhigh"))
            self.assertEqual(body["output"][0]["content"][0]["text"], "21")
            self.assertIsNone(body["usage"])
            with self.assertRaises(HTTPError) as denied:
                urlopen(Request(url, data=data, headers={"Content-Type": "application/json"}), timeout=5)
            self.assertEqual(denied.exception.code, 401)
        finally:
            server.shutdown()
            server.server_close()

    def test_gate_blocks_retry_and_model_downgrade_before_send(self):
        gate = adapter.StartGate()
        body = {"metadata": {"model": "gpt-6.1-sol", "reasoning_effort": "medium"}}
        self.assertFalse(gate.accept(body))
        gate = adapter.StartGate()
        gate.armed = True
        self.assertTrue(gate.accept(body))
        self.assertFalse(gate.accept(body))
        for metadata in ({"model": "gpt-6-astra", "reasoning_effort": "medium"},
                         {"model": "gpt-6.1-sol", "reasoning_effort": "high"}):
            gate = adapter.StartGate()
            gate.armed = True
            self.assertFalse(gate.accept({"metadata": metadata}))
            self.assertFalse(gate.sent)

    def test_success_is_not_terminal_without_completed_status(self):
        self.assertIsNone(adapter.terminal_text({"status": "running", "response": {"status": "success"}}))
        self.assertIsNone(adapter.terminal_text({"status": "completed"}))
        self.assertIsNone(adapter.terminal_text({"status": "completed", "response": "invalid"}))

    def test_journal_retains_latest_turn_state_and_writes_redacted_receipt(self):
        with tempfile.TemporaryDirectory() as directory:
            state = adapter.State(directory)
            state.begin("300", "fixture-project")
            state.update("300", {"request_id": "fixture-request", "turn_state": "fixture-state-one"})
            state.update("300", {"turn_state": "fixture-state-two"})
            journal = json.loads((state.pending / "300").read_text())
            self.assertEqual(journal["turn_state"], "fixture-state-two")
            self.assertEqual(journal["request_id"], "fixture-request")
            state.receipt("300", "fixture-request", 1, 2, "secret answer text")
            receipt = next(state.receipts.iterdir()).read_text()
            self.assertNotIn("secret answer text", receipt)
            self.assertNotIn("turn_state", receipt)
            self.assertEqual(json.loads(receipt)["start_count"], 1)

    def test_unsupported_options_and_empty_text_do_not_dispatch(self):
        for fields in ({"additional_tools": [{"name": "shell"}]}, {"background": True},
                       {"max_output_tokens": 10}, {"input": "   "}, {"store": True},
                       {"text": {"format": {"type": "json_schema"}}},
                       {"text": {"format": {"type": "json_object"}}}):
            with self.assertRaises(adapter.AdapterError):
                adapter.parse_prompt({"model": adapter.MODEL, "input": "hi", **fields})

    def test_codex_plain_text_verbosity_is_accepted(self):
        for text in ({"verbosity": "low"}, {"format": {"type": "text", "name": "answer"}},
                     {"format": None, "verbosity": "medium"}):
            self.assertEqual(adapter.parse_prompt({"model": adapter.MODEL, "input": "hi", "text": text}),
                             ("[user]\nhi", False))


PROJECT = "0123abcd-0000-4000-8000-00000000abcd"
VALID_START = {"metadata": {"model": adapter.MODEL, "reasoning_effort": "medium", "projectId": PROJECT}}


class FakeRoute:
    def __init__(self, request):
        self.request = request
        self.outcome = None

    def continue_(self):
        self.outcome = "continued"

    def abort(self):
        self.outcome = "aborted"


class FakeMessage:
    """A page request or response as the adapter's listeners see it."""

    def __init__(self, path, body, status=200):
        self.url = adapter.BASE + path
        self.post_data_json = body
        self.status = status
        self.request = self

    def json(self):
        return self.post_data_json


class FakeControl:
    def __init__(self, page, name=None):
        self.page = page
        self.name = name

    def click(self, **_kwargs):
        self.page.clicks.append(self.name)

    def hover(self, **_kwargs):
        pass

    def wait_for(self, **_kwargs):
        pass

    def inner_text(self):
        return "6.1 Sol\nMedium"

    def fill(self, _text):
        pass

    def press(self, key):
        if key == "Enter":
            self.page.on_submit(self.page)


class FakePage:
    """Prism's web page: project setup succeeds, on_submit plays the turn."""

    def __init__(self, on_submit):
        self.url = adapter.BASE
        self.on_submit = on_submit
        self.route_handler = None
        self.listeners = {}
        self.clock = 0.0
        self.requests = {}
        self.clicks = []

    def set_default_timeout(self, _timeout):
        pass

    def route(self, _pattern, handler):
        self.route_handler = handler

    def goto(self, url, **_kwargs):
        self.url = url

    def wait_for_function(self, _expression, **_kwargs):
        self.url = adapter.BASE + "/?u=" + PROJECT

    def evaluate(self, _expression):
        return True

    def get_by_role(self, *_args, **_kwargs):
        return FakeControl(self, _kwargs.get("name"))

    def locator(self, _selector):
        return FakeControl(self)

    def on(self, event, listener):
        self.listeners[event] = listener

    def wait_for_timeout(self, timeout):
        self.clock += timeout / 1000

    def browser_request(self, path, body):
        """The page issues a request; it passes the listener and the route gate."""
        request = FakeMessage(path, body)
        self.requests[path] = request
        route = FakeRoute(request)
        self.route_handler(route)
        return route

    def server_response(self, path, body, status=200, request=None):
        response = FakeMessage(path, body, status)
        response.request = request or self.requests[path]
        self.listeners["response"](response)


class FakeBrowser:
    def __init__(self, page):
        self.page = page
        self.contexts = []
        self.closed = False

    def new_context(self, **_kwargs):
        self.contexts.append(self)
        return self

    def add_cookies(self, _cookies):
        pass

    def new_page(self):
        return self.page

    def close(self):
        self.closed = True


def run_turn(state, on_submit):
    page = FakePage(on_submit)
    browser = FakeBrowser(page)
    playwright = types.SimpleNamespace(chromium=types.SimpleNamespace(launch=lambda **_kwargs: browser))
    clock = types.SimpleNamespace(monotonic=lambda: page.clock, time=time.time)
    with mock.patch.object(adapter, "sync_playwright", lambda: contextlib.nullcontext(playwright)), \
            mock.patch.object(adapter, "time", clock):
        return adapter.BrowserTurn(state, "/fixture/chromium").run("300", "fixture-oauth", "[user]\nhi")


class BrowserTurnTests(unittest.TestCase):
    def test_turn_that_never_left_the_browser_releases_the_account(self):
        # Enter was pressed but the page sent no start, e.g. the editor or
        # model control had not initialized. Nothing reached Prism.
        with tempfile.TemporaryDirectory() as directory:
            state = adapter.State(directory)
            with self.assertRaises(adapter.AdapterError) as raised:
                run_turn(state, lambda page: None)
            self.assertFalse((state.pending / "300").exists(), "nothing was submitted, so the account must not stay locked")
            state.ensure_idle("300")
            self.assertEqual(raised.exception.code, "start_not_sent")

    def test_start_rejected_by_the_gate_releases_the_account(self):
        routes = []

        def submit(page):
            routes.append(page.browser_request(adapter.START, {"metadata": {"model": adapter.MODEL, "reasoning_effort": "high"}}))

        with tempfile.TemporaryDirectory() as directory:
            state = adapter.State(directory)
            with self.assertRaises(adapter.AdapterError) as raised:
                run_turn(state, submit)
            self.assertEqual([route.outcome for route in routes], ["aborted"])
            self.assertFalse((state.pending / "300").exists(), "the aborted start never reached Prism")
            self.assertEqual(raised.exception.code, "start_not_sent")

    def test_submitted_turn_without_terminal_keeps_the_account_locked(self):
        routes = []

        def submit(page):
            routes.append(page.browser_request(adapter.START, VALID_START))
            page.server_response(adapter.START, {"request_id": "fixture-request", "turn_state": "fixture-state"})

        with tempfile.TemporaryDirectory() as directory:
            state = adapter.State(directory)
            with self.assertRaises(adapter.AdapterError) as raised:
                run_turn(state, submit)
            self.assertEqual([route.outcome for route in routes], ["continued"])
            self.assertEqual((raised.exception.status, raised.exception.code), (504, "unknown_outcome"))
            journal = json.loads((state.pending / "300").read_text())
            self.assertEqual((journal["request_id"], journal["turn_state"]), ("fixture-request", "fixture-state"))
            with self.assertRaises(adapter.AdapterError) as locked:
                state.ensure_idle("300")
            self.assertEqual(locked.exception.status, 409)

    def test_completed_turn_returns_text_and_releases_the_account(self):
        routes = []

        def submit(page):
            routes.append(page.browser_request(adapter.START, VALID_START))
            page.server_response(adapter.START, {"request_id": "fixture-request", "turn_state": "fixture-state-one"})
            routes.append(page.browser_request(adapter.STATUS, {"request_id": "fixture-request", "turn_state": "fixture-state-one"}))
            page.server_response(adapter.STATUS, {
                "request_id": "fixture-request", "turn_state": "fixture-state-two", "status": "completed",
                "response": {"status": "success", "payload": {"output": [{"type": "message", "content": [{"text": "21"}]}]}},
            })

        with tempfile.TemporaryDirectory() as directory:
            state = adapter.State(directory)
            self.assertEqual(run_turn(state, submit), ("fixture-request", "21"))
            self.assertEqual([route.outcome for route in routes], ["continued", "continued"])
            self.assertFalse((state.pending / "300").exists())
            receipt = json.loads(next(state.receipts.iterdir()).read_text())
            self.assertEqual((receipt["start_count"], receipt["status_count"]), (1, 1))


if __name__ == "__main__":
    unittest.main()
