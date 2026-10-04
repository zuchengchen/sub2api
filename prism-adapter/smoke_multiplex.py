"""Exercise the real multiplex adapter and Chromium against a local fixture.

No OAuth credentials or real Prism requests. Includes HTTP admission, UI-authored
start, bounded preparation pages, shared polling, state rotation and receipts.
"""
import argparse
import asyncio
import json
import tempfile
import threading
import time
import uuid
from concurrent.futures import ThreadPoolExecutor
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.request import Request, urlopen

import server as api
from multiplex_browser import MultiplexBrowser
from multiplex_runtime import AsyncBrowserWorker
from smoke_model_fixture import PICKER


APP = '''const originalFetch = window.fetch.bind(window);
window.fetch = (...args) => originalFetch(...args);
window.SentinelSDK = {token: async () => 'fixture-only'};'''


PAGE = PICKER + '''<script src="/fixture-app.js"></script><button onclick="document.querySelector('[role=menuitem]').hidden=false">New</button>
<button role="menuitem" hidden onclick="location.href='/?u='+crypto.randomUUID()">Blank project</button>
<button onclick="window.chat=[]">New chat tab</button>
<textarea placeholder="Ask anything"></textarea><script>
window.chat=[];
document.querySelector('textarea').addEventListener('keydown', async (e) => {
  if (e.key !== 'Enter') return;
  e.preventDefault(); window.chat.push(e.target.value);
  const result = await fetch('/api/llm/response_with_tools_start', {
    method:'POST', body:JSON.stringify({metadata:{model:currentModel,reasoning_effort:currentEffort,
    projectId:new URL(location.href).searchParams.get('u')},input:window.chat})
  }).then(r=>r.json());
  await fetch('/api/llm/response_with_tools_status', {method:'POST',
    body:JSON.stringify({request_id:result.request_id,turn_state:result.turn_state})});
});</script>'''


class Fixture(BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.1'

    def log_message(self, *_args):
        pass

    def reply(self, code, body, content_type='application/json', cache=False):
        raw = body.encode()
        self.send_response(code)
        self.send_header('Content-Type', content_type)
        self.send_header('Content-Length', str(len(raw)))
        if cache:
            self.send_header('Cache-Control', 'public, max-age=3600')
        self.end_headers()
        self.wfile.write(raw)

    def do_GET(self):
        if self.path == '/fixture-app.js':
            with self.server.lock:
                self.server.asset_requests += 1
            self.reply(200, APP, 'application/javascript', cache=True)
            return
        self.reply(200, PAGE, 'text/html')

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        with self.server.lock:
            if self.path == '/api/projects':
                self.reply(200, json.dumps({'uuid':body['project_uuid']}))
                return
            if self.path == api.START:
                if len(body['input']) != 1:
                    self.reply(409, '{"error":"duplicate input"}')
                    return
                rid = uuid.uuid4().hex
                self.server.jobs[rid] = {'input':body['input'][0], 'project':body['metadata']['projectId'],
                    'state':uuid.uuid4().hex, 'polls':0, 'model':body['metadata']['model'],
                    'effort':body['metadata']['reasoning_effort']}
                # Keep model jobs open until the whole burst has started. A
                # serial executor cannot finish the first job to release slots.
                if len(self.server.jobs) == self.server.target:
                    self.server.release = time.monotonic() + 0.2
                result = {'request_id':rid, 'turn_state':self.server.jobs[rid]['state'], 'status':'started'}
            elif self.path == api.STATUS:
                rid = body['request_id']
                job = self.server.jobs[rid]
                if body['turn_state'] != job['state']:
                    self.server.mismatches += 1
                    self.reply(409, '{"error":"wrong turn state"}')
                    return
                job['state'] = uuid.uuid4().hex
                job['polls'] += 1
                result = {'request_id':rid, 'turn_state':job['state'], 'status':'running'}
                if self.server.release and time.monotonic() >= self.server.release:
                    result.update(status='completed', response={'status':'success','payload':{'output':[
                        {'type':'message','content':[{'text':job['input']}]}]}})
            else:
                self.reply(404, '{}')
                return
        self.reply(200, json.dumps(result))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--chrome', required=True)
    parser.add_argument('--concurrency', type=int, default=20, choices=range(1, 31))
    parser.add_argument('--output')
    args = parser.parse_args()
    upstream = ThreadingHTTPServer(('127.0.0.1', 0), Fixture)
    upstream.daemon_threads = True
    upstream.lock = threading.Lock()
    upstream.asset_requests = 0
    upstream.jobs, upstream.release, upstream.mismatches, upstream.target = {}, None, 0, args.concurrency
    threading.Thread(target=upstream.serve_forever, daemon=True).start()
    api.BASE = f'http://127.0.0.1:{upstream.server_port}'
    with tempfile.TemporaryDirectory() as directory:
        state = api.State(directory)
        worker = AsyncBrowserWorker(lambda:MultiplexBrowser(state, args.chrome, api,
            active=args.concurrency, per_account=args.concurrency, poll_seconds=0.1), api)
        handler = type('ConcurrentHandler', (api.Handler,), {
            'api_key':'fixture-bridge', 'browser_turn':worker, 'serialize_requests':False})
        gateway = ThreadingHTTPServer(('127.0.0.1', 0), handler)
        gateway.daemon_threads = True
        threading.Thread(target=gateway.serve_forever, daemon=True).start()
        worker.ready.wait(5)
        async def observe():
            peak = {'pages':0,'contexts':0,'active':0}
            try:
                while True:
                    actors = list(worker.engine.actors.values())
                    peak['pages'] = max(peak['pages'], sum(len(a.context.pages) for a in actors if a.context))
                    peak['contexts'] = max(peak['contexts'], len(actors))
                    peak['active'] = max(peak['active'], worker.engine.admission.running)
                    await asyncio.sleep(0.01)
            except asyncio.CancelledError:
                return peak
        async def start_observer():
            return asyncio.create_task(observe())
        observer = asyncio.run_coroutine_threadsafe(start_observer(), worker.loop).result(5)
        started = time.monotonic()
        try:
            def call(index):
                models, efforts = list(api.MODELS), list(api.EFFORTS)
                model, effort = models[index % len(models)], efforts[(index // max(len(models), 1)) % len(efforts)]
                payload = json.dumps({'model':model,'reasoning':{'effort':effort},'input':f'fixture-{index}'}).encode()
                headers = {'Authorization':'Bearer fixture-bridge', 'Content-Type':'application/json',
                    'X-Prism-Account-ID':'300', 'X-Prism-OAuth-Token':'synthetic-fixture-token'}
                with urlopen(Request(f'http://127.0.0.1:{gateway.server_port}/v1/responses', data=payload, headers=headers), timeout=150) as response:
                    data = json.load(response)
                assert data['model'] == model and data['reasoning']['effort'] == effort and data['usage'] is None
                job = upstream.jobs[data['id'].removeprefix('resp_prism_')]
                assert (job['model'], job['effort']) == (model, effort)
                assert data['output'][0]['content'][0]['text'] == f'[user]\nfixture-{index}'
                return data['id']
            with ThreadPoolExecutor(max_workers=args.concurrency) as pool:
                ids = list(pool.map(call, range(args.concurrency)))
            assert len(set(ids)) == args.concurrency
            assert len(upstream.jobs) == args.concurrency
            assert len({job['project'] for job in upstream.jobs.values()}) == args.concurrency
            assert not list(state.pending.iterdir())
            assert len(list(state.receipts.iterdir())) == args.concurrency
            for path in state.receipts.iterdir():
                receipt = json.loads(path.read_text())
                job = upstream.jobs[receipt['request_id']]
                assert (receipt['model'], receipt['reasoning_effort']) == (job['model'], job['effort'])
            async def finish_observer():
                observer.cancel()
                return await observer
            peak = asyncio.run_coroutine_threadsafe(finish_observer(), worker.loop).result(5)
            assert peak['pages'] <= 2 and peak['contexts'] == 1
            assert upstream.asset_requests == 1, upstream.asset_requests
            assert peak['active'] == args.concurrency and upstream.mismatches == 0
            result = {'result':'passed','scope':'real adapter + real browser + mock upstream',
                'concurrency':args.concurrency,'completed':len(ids),'starts':len(upstream.jobs),
                'projects':len({j['project'] for j in upstream.jobs.values()}),'peak':peak,
                'state_mismatches':upstream.mismatches,'real_prism_requests':0,
                'asset_network_requests':upstream.asset_requests,
                'elapsed_seconds':round(time.monotonic()-started,3)}
            print(json.dumps(result), flush=True)
            if args.output:
                Path(args.output).write_text(json.dumps(result,indent=2)+'\n')
        finally:
            gateway.shutdown()
            gateway.server_close()
            worker.close()
            upstream.shutdown()
            upstream.server_close()


if __name__ == '__main__':
    main()
