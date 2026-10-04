"""Real Chromium smoke test against a loopback fixture, never Prism upstream.

Usage: python3 prism-adapter/smoke_browser.py --chrome /path/to/chromium
"""
import argparse
import json
import tempfile
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import server as adapter
from smoke_model_fixture import PICKER


PAGE = '''<button onclick="document.querySelector('[role=menuitem]').hidden=false">New</button>
<button role="menuitem" hidden onclick="location.href='/?u='+crypto.randomUUID()">Blank project</button>
<button onclick="window.chat=[]">New chat tab</button>
<textarea placeholder="Ask anything"></textarea><script>
window.chat=[];
document.querySelector('textarea').addEventListener('keydown', async (e) => {
  if (e.key !== 'Enter') return;
  e.preventDefault();
  window.chat.push(e.target.value);
  const start = await fetch('/api/llm/response_with_tools_start', {
    method: 'POST', body: JSON.stringify({metadata: {
      model: currentModel, reasoning_effort: currentEffort,
      projectId: new URL(location.href).searchParams.get('u')
    }, input: window.chat})
  }).then(r => r.json());
  await fetch('/api/llm/response_with_tools_status', {
    method: 'POST', body: JSON.stringify(start)
  });
});</script>''' + PICKER


class FixtureHandler(BaseHTTPRequestHandler):
    turns = []

    def log_message(self, *_args):
        pass

    def send(self, data, content_type='application/json'):
        raw = data.encode()
        self.send_response(200)
        self.send_header('Content-Type', content_type)
        self.send_header('Content-Length', str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def do_GET(self):
        self.send(PAGE, 'text/html')

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        if self.path.endswith('_start'):
            self.turns.append(body)
            self.send(json.dumps({'request_id': str(len(self.turns))}))
        else:
            prompt = self.turns[int(body['request_id']) - 1]['input'][-1]
            self.send(json.dumps({
                'request_id': body['request_id'], 'status': 'completed',
                'response': {'status': 'success', 'payload': {'output': [
                    {'type': 'message', 'content': [{'text': prompt}]}]}}
            }))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--chrome', required=True)
    args = parser.parse_args()
    server = ThreadingHTTPServer(('127.0.0.1', 0), FixtureHandler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    adapter.BASE = f'http://127.0.0.1:{server.server_port}'
    try:
        with tempfile.TemporaryDirectory() as directory:
            state = adapter.State(directory)
            worker = adapter.BrowserWorker(lambda: adapter.BrowserTurn(state, args.chrome))
            try:
                for n, session in enumerate(('a' * 64, 'a' * 64, 'b' * 64)):
                    models, efforts = list(adapter.MODELS), list(adapter.EFFORTS)
                    model, effort = models[n % len(models)], efforts[n % len(efforts)]
                    request_id, answer = worker.run('300', 'synthetic-fixture-token', f'fixture-{n}', session, model, effort)
                    assert answer == f'fixture-{n}', (request_id, answer)
                    assert FixtureHandler.turns[n]['metadata']['model'] == model
                    assert FixtureHandler.turns[n]['metadata']['reasoning_effort'] == effort
                assert [t['input'] for t in FixtureHandler.turns] == [['fixture-0'], ['fixture-1'], ['fixture-2']]
                projects = [t['metadata']['projectId'] for t in FixtureHandler.turns]
                assert projects[0] == projects[1] and projects[1] != projects[2]
                receipts = [json.loads(p.read_text()) for p in state.receipts.iterdir()]
                assert sorted(r['session_cache_hit'] for r in receipts) == [False, False, True]
                print(json.dumps({
                    'browser_smoke': 'passed', 'start_count': len(FixtureHandler.turns),
                    'fresh_projects': len(set(projects)),
                    'cache_hits': sum(r['session_cache_hit'] for r in receipts), 'real_upstream': False,
                }))
            finally:
                worker.close()
    finally:
        server.shutdown()
        server.server_close()


if __name__ == '__main__':
    main()
