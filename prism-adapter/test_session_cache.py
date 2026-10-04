"""Offline lifecycle tests: no real credentials or upstream requests."""
import contextlib
import json
import tempfile
import threading
import types
import unittest
from concurrent.futures import ThreadPoolExecutor
from unittest import mock

from test_server import adapter, FakePage, VALID_START


def completed(request_id, answer="ok"):
    return {"request_id": request_id, "status": "completed", "response": {
        "status": "success", "payload": {"output": [
            {"type": "message", "content": [{"text": answer}]}]}}}


class SessionCacheTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.state = adapter.State(self.tmp.name)
        self.contexts, self.starts = [], []
        self.now = 0
        self.submit = self.success
        self.browser = mock.Mock()
        self.browser.new_context.side_effect = self.new_context
        playwright = types.SimpleNamespace(chromium=types.SimpleNamespace(launch=lambda **_kw: self.browser))
        self.enterContext(mock.patch.object(adapter, "sync_playwright", lambda: contextlib.nullcontext(playwright)))
        self.enterContext(mock.patch.object(adapter.time, "monotonic", lambda: self.now + sum(c.page.clock for c in self.contexts)))
        self.turn = adapter.BrowserTurn(self.state, "/fixture/chromium")
        self.addCleanup(self.turn.close)

    def new_context(self, **_kwargs):
        context = mock.Mock()
        context.page = FakePage(lambda page: self.submit(page))
        context.new_page.return_value = context.page
        self.contexts.append(context)
        return context

    def success(self, page):
        request_id = "request-" + str(len(self.starts))
        route = page.browser_request(adapter.START, VALID_START)
        self.assertEqual(route.outcome, "continued")
        self.starts.append(route)
        page.server_response(adapter.START, {"request_id": request_id, "turn_state": "fixture-state"})
        route = page.browser_request(adapter.STATUS, {"request_id": request_id})
        self.assertEqual(route.outcome, "continued")
        page.server_response(adapter.STATUS, completed(request_id, request_id))

    def run_turn(self, account="300", token="fixture-token", session="a" * 64):
        return self.turn.run(account, token, "[user]\nfixture", session)

    def test_same_session_reuses_context_project_but_not_chat_or_answer(self):
        self.assertEqual(self.run_turn(), ("request-0", "request-0"))
        self.assertEqual(self.run_turn(), ("request-1", "request-1"))
        self.assertEqual(len(self.contexts), 1)
        page = self.contexts[0].page
        self.assertEqual(page.clicks, ["New", "Blank project", "New chat tab"])
        receipts = [json.loads(p.read_text()) for p in self.state.receipts.iterdir()]
        self.assertEqual(sorted(r["session_cache_hit"] for r in receipts), [False, True])
        self.assertEqual([r["start_count"] for r in receipts], [1, 1])
        self.contexts[0].close.assert_not_called()
        # Background traffic cannot submit another model turn while idle.
        self.assertEqual(page.browser_request(adapter.START, VALID_START).outcome, "aborted")

    def test_other_session_account_and_rotated_token_get_private_contexts(self):
        for args in ({}, {"session": "b" * 64}, {"account": "301", "session": "b" * 64},
                     {"account": "301", "session": "b" * 64, "token": "rotated-token"}):
            self.run_turn(**args)
        self.assertEqual(len(self.contexts), 4)
        for context in self.contexts[:-1]:
            context.close.assert_called_once()
        self.contexts[-1].add_cookies.assert_called_once()
        self.assertEqual(self.contexts[-1].add_cookies.call_args.args[0][0]["value"], "rotated-token")

    def test_lru_limit_and_idle_expiry_close_resources(self):
        self.turn.max_sessions = 2
        self.run_turn(session="a" * 64)
        self.now = 1
        self.run_turn(session="b" * 64)
        self.now = 2
        self.run_turn(session="a" * 64)
        self.now = 3
        self.run_turn(session="c" * 64)
        self.contexts[1].close.assert_called_once()  # b was least recently used
        self.contexts[0].close.assert_not_called()
        self.now = 303
        self.turn.prune()
        self.assertEqual(self.turn.sessions, {})
        for context in self.contexts:
            context.close.assert_called_once()
        self.browser.close.assert_called_once()

    def test_absolute_lifetime_expires_even_when_recently_used(self):
        self.run_turn()
        for self.now in (250, 500, 750):
            self.run_turn()
        self.assertEqual(len(self.contexts), 1)
        self.now = 900
        self.run_turn()
        self.assertEqual(len(self.contexts), 2)
        self.contexts[0].close.assert_called_once()

    def test_missing_session_never_reuses_context(self):
        self.run_turn(session=None)
        self.run_turn(session=None)
        self.assertEqual(len(self.contexts), 2)
        self.assertFalse(self.turn.sessions)
        for context in self.contexts:
            context.close.assert_called_once()

    def test_unknown_outcome_discards_cache_but_blocks_replay(self):
        self.run_turn()
        def timeout(page):
            self.starts.append(page.browser_request(adapter.START, VALID_START))
            page.server_response(adapter.START, {"request_id": "unknown-turn", "turn_state": {"opaque": "fixture"}})
        self.submit = timeout
        with self.assertRaises(adapter.AdapterError) as raised:
            self.run_turn()
        self.assertEqual(raised.exception.code, "unknown_outcome")
        self.contexts[0].close.assert_called_once()
        self.assertFalse(self.turn.sessions)
        for session in ("a" * 64, "b" * 64, None):
            with self.assertRaises(adapter.AdapterError) as blocked:
                self.run_turn(session=session)
            self.assertEqual(blocked.exception.code, "pending_turn")
        self.assertEqual(len(self.starts), 2)
        self.assertEqual(json.loads((self.state.pending / "300").read_text())["turn_state"], {"opaque": "fixture"})

    def test_late_response_from_previous_turn_cannot_complete_new_turn(self):
        self.run_turn()
        old_poll = self.contexts[0].page.requests[adapter.STATUS]
        def submit(page):
            self.starts.append(page.browser_request(adapter.START, VALID_START))
            page.server_response(adapter.START, {"request_id": "new-turn"})
            # Even an identifier-free stale response must be ignored.
            stale = completed("old-turn", "stale answer")
            stale.pop("request_id")
            page.server_response(adapter.STATUS, stale, request=old_poll)
            page.browser_request(adapter.STATUS, {"request_id": "new-turn"})
            page.server_response(adapter.STATUS, completed("new-turn", "fresh answer"))
        self.submit = submit
        self.assertEqual(self.run_turn(), ("new-turn", "fresh answer"))

    def test_early_poll_waits_for_trusted_start_id_before_dispatch(self):
        routes = []
        def submit(page):
            page.browser_request(adapter.START, VALID_START)
            # Playwright may reenter the route listener while response.json()
            # is still reading the start response. No upstream ID is trusted yet.
            routes.append(page.browser_request(adapter.STATUS, {"request_id": "early"}))
            self.assertIsNone(routes[0].outcome)
            page.server_response(adapter.START, {"request_id": "early"})
            self.assertEqual(routes[0].outcome, "continued")
            page.server_response(adapter.STATUS, completed("early"))
        self.submit = submit
        self.assertEqual(self.run_turn(), ("early", "ok"))

    def test_early_poll_for_wrong_id_is_aborted_after_start_decodes(self):
        def submit(page):
            page.browser_request(adapter.START, VALID_START)
            foreign = page.browser_request(adapter.STATUS, {"request_id": "foreign"})
            page.server_response(adapter.START, {"request_id": "current"})
            self.assertEqual(foreign.outcome, "aborted")
            page.browser_request(adapter.STATUS, {"request_id": "current"})
            page.server_response(adapter.STATUS, completed("current"))
        self.submit = submit
        self.assertEqual(self.run_turn(), ("current", "ok"))

    def test_wrong_project_never_leaves_browser(self):
        self.run_turn()
        routes = []
        self.submit = lambda page: routes.append(page.browser_request(adapter.START, {
            "metadata": dict(VALID_START["metadata"], projectId="another-project")}))
        with self.assertRaises(adapter.AdapterError) as raised:
            self.run_turn()
        self.assertEqual(raised.exception.code, "start_not_sent")
        self.assertEqual(routes[0].outcome, "aborted")
        self.state.ensure_idle("300")
        self.assertFalse(self.turn.sessions)


class WorkerTests(unittest.TestCase):
    def setUp(self):
        self.thread_errors = []
        self.enterContext(mock.patch.object(threading, "excepthook", self.thread_errors.append))
        self.addCleanup(lambda: self.assertEqual(self.thread_errors, [], "worker exceptions must fail the test"))

    def test_all_browser_operations_share_one_thread_and_busy_request_is_rejected(self):
        entered, release, pruned = threading.Event(), threading.Event(), threading.Event()
        owners = []
        class Browser:
            def __init__(self):
                owners.append(threading.get_ident())
            def run(self, *_args):
                owners.append(threading.get_ident())
                entered.set()
                if not release.wait(5):
                    raise AssertionError("test did not release worker")
                return "id", "answer"
            def prune(self):
                owners.append(threading.get_ident())
                pruned.set()
            def close(self):
                owners.append(threading.get_ident())
        worker = adapter.BrowserWorker(Browser)
        self.addCleanup(worker.close)
        with ThreadPoolExecutor(max_workers=1) as pool:
            future = pool.submit(worker.run, "fixture")
            try:
                self.assertTrue(entered.wait(5))
                with self.assertRaises(adapter.AdapterError) as raised:
                    worker.run("second")
                self.assertEqual(raised.exception.status, 429)
            finally:
                release.set()
            self.assertEqual(future.result(5), ("id", "answer"))
        self.assertTrue(pruned.wait(5))
        worker.close()
        self.assertEqual(set(owners), {worker.thread.ident})
        self.assertNotEqual(owners[0], threading.get_ident())
        with self.assertRaises(adapter.AdapterError):
            worker.run("after-stop")

    def test_shutdown_resolves_job_waiting_behind_idle_cleanup(self):
        pruning, release = threading.Event(), threading.Event()
        browser = mock.Mock()
        def prune():
            pruning.set()
            release.wait(5)
        browser.prune.side_effect = prune
        worker = adapter.BrowserWorker(lambda: browser)
        self.addCleanup(worker.close)
        self.assertTrue(pruning.wait(5))
        enqueued = threading.Event()
        put = worker.jobs.put_nowait
        def enqueue(job):
            put(job)
            enqueued.set()
        with mock.patch.object(worker.jobs, "put_nowait", enqueue), ThreadPoolExecutor(max_workers=2) as pool:
            future = pool.submit(worker.run, "waiting")
            try:
                self.assertTrue(enqueued.wait(5))
                closing = pool.submit(worker.close)
                self.assertTrue(worker.stopping.wait(5))
            finally:
                release.set()
            with self.assertRaises(adapter.AdapterError) as raised:
                future.result(5)
            self.assertEqual(raised.exception.status, 503)
            closing.result(5)
        browser.run.assert_not_called()
        self.assertFalse(worker.thread.is_alive())

    def test_startup_failure_does_not_hang_request(self):
        def fail():
            raise ValueError("fixture")
        worker = adapter.BrowserWorker(fail)
        self.addCleanup(worker.close)
        with self.assertRaises(adapter.AdapterError) as raised:
            worker.run("fixture")
        self.assertEqual(raised.exception.status, 503)
