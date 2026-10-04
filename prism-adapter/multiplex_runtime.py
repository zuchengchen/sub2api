"""Bounded async execution and per-conversation journals for Prism.

All browser objects live on one asyncio thread. HTTP handler threads only submit
work; they never touch Playwright objects. An uncertain start is never replayed.
"""
import asyncio
import concurrent.futures
import errno
import hashlib
import json
import os
import threading
import time
import uuid
from contextlib import asynccontextmanager


class TurnJournal:
    def __init__(self, state, api, account_id, session_id):
        self.state, self.api, self.account_id = state, api, account_id
        self.local_id = uuid.uuid4().hex
        scope = "session:" + session_id if session_id else "request:" + self.local_id
        self.scope = hashlib.sha256(scope.encode()).hexdigest()
        self.directory = state.pending / account_id
        self.path = self.directory / (self.scope + ".json")
        self.owned = False

    def begin(self):
        # A v1 uncertain turn remains an account-wide block. Conversely, old
        # adapters see this account directory and fail closed after rollback.
        try:
            self.directory.mkdir(mode=0o700)
        except FileExistsError:
            if not self.directory.is_dir():
                raise self.api.AdapterError(409, "pending_turn", "Legacy Prism turn requires inspection") from None
        try:
            fd = os.open(self.path, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
        except FileExistsError:
            raise self.api.AdapterError(409, "pending_turn", "Previous Prism turn in this conversation is unresolved") from None
        self.owned = True
        with os.fdopen(fd, "w") as file:
            json.dump({"version": 2, "local_id": self.local_id, "account_id": self.account_id,
                       "stage": "preparing", "at": int(time.time())}, file)
            file.flush()
            os.fsync(file.fileno())
        self.state.sync_directory(self.directory)
        self.state.sync_directory(self.state.pending)

    def update(self, **fields):
        previous = json.loads(self.path.read_text())
        previous.update(fields)
        self.state.atomic_write(self.path, previous)

    def finish(self):
        if not self.owned:
            return
        self.path.unlink()
        self.state.sync_directory(self.directory)
        try:
            self.directory.rmdir()
        except OSError as error:
            if error.errno != errno.ENOTEMPTY:
                raise
        self.state.sync_directory(self.state.pending)
        self.owned = False


class Admission:
    """Bound queued + active jobs before they reserve browser resources."""
    def __init__(self, api, active=20, per_account=20, queued=30, wait_seconds=15):
        if not 1 <= per_account <= active <= 30 or not 0 <= queued <= 60 or not 0 < wait_seconds <= 60:
            raise ValueError("invalid Prism concurrency limits")
        self.api, self.limit, self.per_account = api, active, per_account
        self.queued, self.wait_seconds = queued, wait_seconds
        self.global_slots = asyncio.Semaphore(active)
        self.accounts, self.scopes = {}, {}
        self.outstanding = self.running = 0
        self.closed = False

    @asynccontextmanager
    async def enter(self, account_id, session_id):
        if self.closed:
            raise self.api.AdapterError(503, "prism_unavailable", "Prism executor is stopping")
        if self.outstanding >= self.limit + self.queued:
            raise self.api.AdapterError(429, "prism_busy", "Prism queue is full; request was not submitted")
        self.outstanding += 1
        account = self.accounts.setdefault(account_id, [asyncio.Semaphore(self.per_account), 0])
        account[1] += 1
        key = (account_id, session_id) if session_id else None
        scope = self.scopes.setdefault(key, [asyncio.Lock(), 0]) if key else None
        if scope:
            scope[1] += 1
        acquired, running = [], False
        try:
            try:
                async with asyncio.timeout(self.wait_seconds):
                    # Waiting for a preceding turn must not occupy global slots.
                    for lock in ([scope[0]] if scope else []) + [account[0], self.global_slots]:
                        await lock.acquire()
                        acquired.append(lock)
            except TimeoutError:
                raise self.api.AdapterError(429, "prism_busy", "Prism queue wait expired; request was not submitted") from None
            self.running += 1
            running = True
            yield
        finally:
            if running:
                self.running -= 1
            for lock in reversed(acquired):
                lock.release()
            self.outstanding -= 1
            account[1] -= 1
            if account[1] == 0:
                self.accounts.pop(account_id, None)
            if scope:
                scope[1] -= 1
                if scope[1] == 0:
                    self.scopes.pop(key, None)


class AsyncBrowserWorker:
    """Expose the existing synchronous run() seam to threaded HTTP handlers."""
    def __init__(self, factory, api, request_timeout=285):
        self.factory, self.api = factory, api
        self.request_timeout = request_timeout
        self.ready = threading.Event()
        self.lifecycle = threading.Lock()
        self.stopping = False
        self.failed = False
        self.loop = self.stop = None
        self.tasks = set()
        self.thread = threading.Thread(target=self._serve, name="prism-async", daemon=True)
        self.thread.start()

    def _serve(self):
        try:
            asyncio.run(self._main())
        except BaseException:
            # Browser exception text may contain private page state.
            self.failed = True
        finally:
            with self.lifecycle:
                self.stopping = True
            self.ready.set()

    async def _main(self):
        self.loop = asyncio.get_running_loop()
        self.stop = asyncio.Event()
        self.engine = self.factory()
        self.ready.set()
        janitor = asyncio.create_task(self._expire())
        try:
            await self.stop.wait()
        finally:
            janitor.cancel()
            await asyncio.gather(janitor, return_exceptions=True)
            for task in list(self.tasks):
                task.cancel()
            if self.tasks:
                await asyncio.gather(*list(self.tasks), return_exceptions=True)
            await self.engine.close()

    async def _expire(self):
        while True:
            await asyncio.sleep(1)
            try:
                await self.engine.prune()
            except Exception:
                # Stop admission if cleanup fails; pending journals survive.
                with self.lifecycle:
                    self.stopping = True
                self.stop.set()
                return

    async def _run(self, args):
        task = asyncio.current_task()
        self.tasks.add(task)
        try:
            async with asyncio.timeout(self.request_timeout):
                return await self.engine.run(*args)
        except TimeoutError:
            raise self.api.AdapterError(504, "unknown_outcome", "Prism request deadline exceeded; inspect pending state") from None
        finally:
            self.tasks.discard(task)

    def run(self, *args):
        if not self.ready.wait(5):
            raise self.api.AdapterError(503, "prism_unavailable", "Prism executor did not start", not_submitted=True)
        with self.lifecycle:
            if self.stopping or self.failed or not self.thread.is_alive():
                raise self.api.AdapterError(503, "prism_unavailable", "Prism executor is unavailable", not_submitted=True)
            future = asyncio.run_coroutine_threadsafe(self._run(args), self.loop)
        try:
            return future.result(timeout=self.request_timeout + 10)
        except concurrent.futures.TimeoutError:
            future.cancel()
            raise self.api.AdapterError(504, "unknown_outcome", "Prism executor did not settle; inspect pending state") from None
        except concurrent.futures.CancelledError:
            raise self.api.AdapterError(503, "prism_unavailable", "Prism executor stopped; inspect pending state") from None

    def close(self):
        with self.lifecycle:
            self.stopping = True
            if self.loop and self.stop and self.thread.is_alive():
                try:
                    self.loop.call_soon_threadsafe(self.stop.set)
                except RuntimeError:
                    pass  # Startup failed or the event loop just finished.
        self.thread.join(timeout=10)
