"""Durable tool identities and result leases; no prompts/arguments/results stored."""
import hashlib
import json
import os
import sqlite3
import threading
import time
from contextlib import contextmanager
from pathlib import Path


def digest(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, ensure_ascii=False,
        separators=(',', ':'), allow_nan=False).encode()).hexdigest()


class ToolState:
    def __init__(self, directory, error):
        self.error = error
        root = Path(directory) / 'tools'
        root.mkdir(mode=0o700, parents=True, exist_ok=True)
        self.path = root / 'v1.sqlite3'
        fd = os.open(self.path, os.O_CREAT | os.O_WRONLY, 0o600)
        os.close(fd)
        os.chmod(self.path, 0o600)
        self.lock = threading.Lock()
        with self.lock, self.connect() as db:
            db.execute('''CREATE TABLE IF NOT EXISTS calls (
                scope TEXT NOT NULL, call_id TEXT PRIMARY KEY, fingerprint TEXT NOT NULL,
                group_id TEXT NOT NULL, state TEXT NOT NULL, result_hash TEXT,
                lease TEXT, created_at INTEGER NOT NULL)''')
            db.execute('CREATE INDEX IF NOT EXISTS call_group ON calls(scope, group_id)')

    @contextmanager
    def connect(self):
        db = sqlite3.connect(self.path, timeout=5)
        db.row_factory = sqlite3.Row
        try:
            with db:
                yield db
        finally:
            db.close()

    def reserve(self, scope, calls, results, lease, needs_fresh):
        fresh = []
        with self.lock, self.connect() as db:
            db.execute('BEGIN IMMEDIATE')
            for call_id, call in calls.items():
                row = db.execute('SELECT * FROM calls WHERE call_id=? AND scope=?', (call_id, scope)).fetchone()
                if row is None or row['fingerprint'] != digest(call):
                    raise self.error(409, 'unknown_tool_call', 'Tool call does not belong to this caller, account and conversation')
                result_hash = digest(results[call_id])
                if row['result_hash'] is not None and row['result_hash'] != result_hash:
                    raise self.error(409, 'tool_result_conflict', 'Tool result changed after it was submitted')
                if row['state'] == 'reserved':
                    raise self.error(409, 'pending_tool_result', 'Previous continuation is unresolved; it was not replayed')
                if row['state'] == 'issued':
                    group = db.execute('SELECT call_id FROM calls WHERE scope=? AND group_id=?',
                                       (scope, row['group_id'])).fetchall()
                    if any(r['call_id'] not in results for r in group):
                        raise self.error(422, 'missing_tool_result', 'Return all results from the previous response before continuing')
                    fresh.append(call_id)
                    db.execute("UPDATE calls SET state='reserved', result_hash=?, lease=? WHERE call_id=?",
                               (result_hash, lease, call_id))
            if needs_fresh and not fresh:
                raise self.error(409, 'tool_result_replayed', 'These tool results were already consumed; continuation was not replayed')
        return fresh

    def complete(self, scope, lease, group, calls):
        with self.lock, self.connect() as db:
            db.execute('BEGIN IMMEDIATE')
            if db.execute('SELECT COUNT(*) FROM calls').fetchone()[0] + len(calls) > 50000:
                raise self.error(503, 'tool_state_full', 'Tool identity storage requires maintenance')
            for call in calls:
                db.execute('INSERT INTO calls VALUES (?,?,?,?,?,?,?,?)',
                    (scope, call['call_id'], digest(call), group, 'issued', None, None, int(time.time())))
            db.execute("UPDATE calls SET state='consumed', lease=NULL WHERE scope=? AND lease=?", (scope, lease))

    def not_sent(self, scope, lease):
        with self.lock, self.connect() as db:
            db.execute("UPDATE calls SET state='issued', result_hash=NULL, lease=NULL WHERE scope=? AND lease=?", (scope, lease))
