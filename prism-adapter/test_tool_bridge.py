import copy
import json
import tempfile
import threading
import unittest
from concurrent.futures import ThreadPoolExecutor
from http.server import ThreadingHTTPServer
from urllib.error import HTTPError
from urllib.request import Request, urlopen

from test_server import adapter
from tool_bridge import ToolBridge, has_tools, validate_batch
from tool_state import ToolState, digest
from response_events import completed_events


FUNCTION = {'type':'function','name':'lookup','description':'Read a value from the client fixture.',
    'parameters':{'type':'object','properties':{'key':{'type':'string'}},'required':['key'],'additionalProperties':False},'strict':True}
CUSTOM = {'type':'custom','name':'echo','description':'Return the exact client input.', 'format':{'type':'text'}}


def request(**changes):
    return dict({'model':'gpt-6.1-sol','reasoning':{'effort':'medium'},'tools':[FUNCTION,CUSTOM],
                 'input':[{'role':'user','content':'Use the declared client tools.'}]}, **changes)


def framed(bridge, value):
    return bridge.marker+'\n'+json.dumps(value,ensure_ascii=False)


def call_output(bridge, name='lookup', value=None):
    call = {'name':name}
    call['arguments' if name.endswith('lookup') else 'input'] = {'key':'first'} if value is None else value
    return bridge.output(framed(bridge, {'kind':'calls','calls':[call]}),'upstream-fixture')


class ProtocolTests(unittest.TestCase):
    def test_namespace_and_structural_additional_tools(self):
        payload = request(tools=[], input=[{'type':'additional_tools','tools':[
            {'type':'namespace','name':'client','tools':[FUNCTION,CUSTOM]}]}, {'role':'user','content':'lookup'}])
        bridge = ToolBridge(payload,adapter)
        self.assertEqual(set(bridge.tools), {'client.lookup','client.echo'})
        output, calls = call_output(bridge,'client.lookup')
        self.assertEqual((output[0]['namespace'],output[0]['name']),('client','lookup'))
        self.assertEqual(calls[0]['arguments'],'{"key":"first"}')
        self.assertFalse(has_tools({'input':'additional_tools function_call are just words'}))

    def test_plain_or_unframed_output_never_becomes_a_tool(self):
        bridge = ToolBridge(request(),adapter)
        invalid = ['echo hello','```json\n{}\n```', 'prefix '+framed(bridge,{'kind':'final','text':'ok'}),
                   bridge.marker+'\n{"kind":"final","kind":"calls","text":"x"}',
                   framed(bridge,{'kind':'calls','calls':[{'name':'undeclared','arguments':{}}]})]
        for answer in invalid:
            with self.subTest(answer=answer[:50]),self.assertRaises(adapter.AdapterError) as raised:
                bridge.output(answer,'fixture')
            self.assertEqual(raised.exception.code,'invalid_tool_output')

    def test_schema_argument_and_choice_constraints(self):
        bridge = ToolBridge(request(),adapter)
        for value in ({'key':3},{'key':'x','extra':1},{}):
            with self.subTest(value=value),self.assertRaises(adapter.AdapterError):
                call_output(bridge,value=value)
        required = ToolBridge(request(tool_choice='required'),adapter)
        with self.assertRaises(adapter.AdapterError):
            required.output(framed(required,{'kind':'final','text':'No call'}),'fixture')
        none = ToolBridge(request(tool_choice='none'),adapter)
        with self.assertRaises(adapter.AdapterError):
            call_output(none)
        forced = ToolBridge(request(tool_choice={'type':'custom','name':'echo'}),adapter)
        with self.assertRaises(adapter.AdapterError):
            call_output(forced)
        limited = ToolBridge(request(parallel_tool_calls=False),adapter)
        with self.assertRaises(adapter.AdapterError):
            limited.output(framed(limited,{'kind':'calls','calls':[
                {'name':'lookup','arguments':{'key':'one'}},{'name':'lookup','arguments':{'key':'two'}}]}),'fixture')

    def test_custom_grammar_preserves_raw_input(self):
        raw = 'first\r\n\tsecond \\ literal'
        bridge = ToolBridge(request(),adapter)
        output,_ = call_output(bridge,'echo',raw)
        self.assertEqual(output[0]['input'],raw)
        for syntax, definition, good, bad in (
            ('regex',r'[A-Z]{3}:[0-9]+','ABC:42','wrong'),
            ('lark','start: "value=" INT\n%import common.INT','value=42','value=no')):
            custom = dict(CUSTOM,format={'type':'grammar','syntax':syntax,'definition':definition})
            grammar = ToolBridge(request(tools=[custom]),adapter)
            self.assertEqual(call_output(grammar,'echo',good)[0][0]['input'],good)
            with self.assertRaises(adapter.AdapterError):
                call_output(grammar,'echo',bad)

    def test_external_schema_and_grammar_imports_are_rejected(self):
        for tool in (
            dict(FUNCTION,parameters={'$ref':'https://example.invalid/private-schema'}),
            dict(CUSTOM,format={'type':'grammar','syntax':'lark','definition':'%import .private\nstart: private'})):
            with self.assertRaises(adapter.AdapterError):
                ToolBridge(request(tools=[tool]),adapter)

    def test_invalid_result_and_duplicate_definitions_fail_before_dispatch(self):
        cases = [request(input=[{'type':'function_call_output','call_id':'absent','output':'x'}]),
                 request(tools=[FUNCTION,dict(FUNCTION,description='conflicting')]),
                 request(tools=[{'type':'web_search','name':'search'}]),
                 request(input=[{'type':'reasoning','encrypted_content':'opaque'},{'role':'user','content':'go'}])]
        for payload in cases:
            with self.assertRaises(adapter.AdapterError):
                ToolBridge(payload,adapter)

    def test_hosted_tools_are_reported_unavailable_without_breaking_client_tools(self):
        bridge = ToolBridge(request(tools=[FUNCTION,{'type':'web_search'}]),adapter)
        self.assertEqual(bridge.unavailable,{'web_search'})
        self.assertEqual(set(bridge.tools),{'lookup'})
        self.assertIn('Unavailable hosted tools: web_search',bridge.prompt)
        with self.assertRaises(adapter.AdapterError):
            bridge.output(framed(bridge,{'kind':'calls','calls':[{'name':'web_search','arguments':{}}]}),'fixture')

    def test_done_events_contain_verified_items_without_fake_token_deltas(self):
        bridge = ToolBridge(request(),adapter)
        output,_ = call_output(bridge)
        response = adapter.response_payload('fixture','ignored','gpt-6.1-sol')
        response['output'] = output
        events = completed_events(response)
        self.assertEqual([name for name,_ in events], ['response.created','response.in_progress',
            'response.output_item.added','response.function_call_arguments.done','response.output_item.done','response.completed'])
        self.assertEqual([data['sequence_number'] for _,data in events],list(range(len(events))))
        self.assertEqual(events[-2][1]['item'], output[0])


class ToolStateTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.state = ToolState(self.tmp.name,adapter.AdapterError)
        self.scope = digest(['300','caller-a','session-a'])
        self.payload = request()
        self.first = ToolBridge(self.payload,adapter)
        self.output,self.calls = call_output(self.first)
        self.state.complete(self.scope,self.first.lease,'response-first',self.calls)

    def continuation(self, **change):
        history = self.payload['input'] + self.output + [{'type':'function_call_output',
            'call_id':self.calls[0]['call_id'],'output':'client-only-value'}]
        return ToolBridge(request(input=history,**change),adapter)

    def reserve(self, bridge, scope=None):
        return self.state.reserve(scope or self.scope,bridge.calls,bridge.results,bridge.lease,bridge.needs_fresh)

    def test_two_rounds_and_old_history_are_correlated(self):
        second = self.continuation()
        self.reserve(second)
        output,calls = call_output(second,'echo','client-only-value')
        self.state.complete(self.scope,second.lease,'response-second',calls)
        history = self.payload['input'] + self.output + [{'type':'function_call_output',
            'call_id':self.calls[0]['call_id'],'output':'client-only-value'}] + output + [
            {'type':'custom_tool_call_output','call_id':calls[0]['call_id'],'output':'confirmed'}]
        third = ToolBridge(request(input=history),adapter)
        self.assertEqual(len(self.reserve(third)),1)
        final,_ = third.output(framed(third,{'kind':'final','text':'confirmed'}),'last')
        self.state.complete(self.scope,third.lease,'last',[])
        self.assertEqual(final[0]['content'][0]['text'],'confirmed')
        with self.assertRaises(adapter.AdapterError) as raised:
            self.reserve(ToolBridge(request(input=history),adapter))
        self.assertEqual(raised.exception.code,'tool_result_replayed')
        raw = self.state.path.read_bytes()
        self.assertNotIn(b'client-only-value',raw)
        self.assertNotIn(b'confirmed',raw)
        self.assertEqual(self.state.path.stat().st_mode & 0o777,0o600)

    def test_other_callers_accounts_sessions_and_tampered_calls_rejected(self):
        second = self.continuation()
        for scope in (digest(['301','caller-a','session-a']),digest(['300','caller-b','session-a']),digest(['300','caller-a','session-b'])):
            with self.assertRaises(adapter.AdapterError):
                self.reserve(second,scope)
        changed = copy.deepcopy(second.calls)
        changed[self.calls[0]['call_id']]['arguments']='{"key":"tampered"}'
        with self.assertRaises(adapter.AdapterError):
            self.state.reserve(self.scope,changed,second.results,second.lease,True)

    def test_uncertain_result_survives_restart_and_pre_send_can_release(self):
        second = self.continuation()
        self.reserve(second)
        self.state = ToolState(self.tmp.name,adapter.AdapterError)
        with self.assertRaises(adapter.AdapterError) as raised:
            self.reserve(self.continuation())
        self.assertEqual(raised.exception.code,'pending_tool_result')
        self.state.not_sent(self.scope,second.lease)
        self.assertEqual(len(self.reserve(self.continuation())),1)

    def test_competing_continuations_cannot_both_reserve(self):
        first,second = self.continuation(),self.continuation()
        def reserve(bridge):
            try:
                self.reserve(bridge)
                return 'accepted'
            except adapter.AdapterError as error:
                return error.code
        with ThreadPoolExecutor(max_workers=2) as pool:
            values = list(pool.map(reserve,[first,second]))
        self.assertCountEqual(values,['accepted','pending_tool_result'])


class ToolHTTPTests(unittest.TestCase):
    def test_function_and_custom_roundtrip_over_real_http_sse(self):
        class Browser:
            count = 0
            def run(inner, account, token, prompt, session, model, effort, reuse_project=True):
                import re
                self.assertFalse(reuse_project)
                self.assertEqual(session,'a'*64)
                inner.count += 1
                marker = re.search(r'PRISM_CLIENT_TOOLS_V1:[a-f0-9]+',prompt).group()
                if inner.count == 1:
                    result={'kind':'calls','calls':[{'name':'lookup','arguments':{'key':'fixture'}}]}
                elif inner.count == 2:
                    self.assertIn('client-value',prompt)
                    result={'kind':'calls','calls':[{'name':'echo','input':'client-value'}]}
                else:
                    self.assertIn('confirmed-client-value',prompt)
                    result={'kind':'final','text':'confirmed-client-value'}
                return 'fixture-'+str(inner.count),marker+'\n'+json.dumps(result)
        with tempfile.TemporaryDirectory() as directory:
            browser=Browser()
            handler=type('ToolsHandler',(adapter.Handler,),{'api_key':'fixture-key','browser_turn':browser,
                'serialize_requests':False,'tool_state':ToolState(directory,adapter.AdapterError)})
            server=ThreadingHTTPServer(('127.0.0.1',0),handler)
            threading.Thread(target=server.serve_forever,daemon=True).start()
            try:
                payload=request(stream=True, include=['reasoning.encrypted_content'], reasoning={'effort':'medium','summary':'auto'})
                headers={'Authorization':'Bearer fixture-key','X-Prism-Account-ID':'300',
                    'X-Prism-OAuth-Token':'synthetic','X-Prism-Session-ID':'a'*64,'X-Prism-Caller-ID':'b'*64,'Content-Type':'application/json'}
                def send(headers_=headers):
                    req=Request(f'http://127.0.0.1:{server.server_port}/v1/responses',data=json.dumps(payload).encode(),headers=headers_)
                    with urlopen(req,timeout=10) as reply:
                        events=[json.loads(line[6:]) for line in reply.read().decode().splitlines() if line.startswith('data: ')]
                    self.assertEqual(events[-1]['type'],'response.completed')
                    self.assertFalse(any(e['type'].endswith('.delta') for e in events))
                    return events[-1]['response']
                with self.assertRaises(HTTPError) as missing:
                    send({k:v for k,v in headers.items() if k!='X-Prism-Caller-ID'})
                self.assertEqual(missing.exception.code,400)
                first=send();payload['input'] += first['output'] + [{'type':'function_call_output','call_id':first['output'][0]['call_id'],'output':'client-value'}]
                second=send();payload['input'] += second['output'] + [{'type':'custom_tool_call_output','call_id':second['output'][0]['call_id'],'output':'confirmed-client-value'}]
                third=send()
                self.assertEqual(third['output'][0]['content'][0]['text'],'confirmed-client-value')
                self.assertIsNone(third['usage'])
                with self.assertRaises(HTTPError) as duplicate:
                    send()
                self.assertEqual(duplicate.exception.code,409)
                self.assertEqual(browser.count,3)
            finally:
                server.shutdown();server.server_close()
