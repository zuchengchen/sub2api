"""Intercept only model API requests without disabling Chromium's asset cache.

Playwright route() disables the HTTP cache for the page. We use Chromium's Fetch
request-stage interception directly, while leaving normal browser/SDK requests
and cache policy intact. No verification headers or request bodies are rewritten.
"""
import json


class PausedRequest:
    def __init__(self, session, event, body):
        self.session, self.identifier = session, event['requestId']
        self.request = self
        self.url = event['request']['url']
        self.method = event['request']['method']
        self.body = body
        self.settled = False

    @property
    def post_data_json(self):
        return json.loads(self.body)

    async def continue_(self):
        if self.settled:
            return
        self.settled = True
        await self.session.send('Fetch.continueRequest', {'requestId':self.identifier})

    async def abort(self):
        if self.settled:
            return
        self.settled = True
        await self.session.send('Fetch.failRequest', {'requestId':self.identifier,'errorReason':'BlockedByClient'})


class BrowserGate:
    def __init__(self, session, handler):
        self.session, self.handler = session, handler

    @classmethod
    async def install(cls, context, page, origin, handler):
        session = await context.new_cdp_session(page)
        gate = cls(session, handler)
        session.on('Fetch.requestPaused', gate.paused)
        await session.send('Network.enable', {'maxTotalBufferSize':4*1024*1024,
            'maxResourceBufferSize':2*1024*1024, 'maxPostDataSize':1024*1024})
        await session.send('Fetch.enable', {'patterns':[{'urlPattern':origin+'/api/llm/*','requestStage':'Request'}]})
        return gate

    async def paused(self, event):
        route = None
        try:
            request = event['request']
            body = request.get('postData')
            if body is None and request.get('hasPostData') and event.get('networkId'):
                body = (await self.session.send('Network.getRequestPostData', {'requestId':event['networkId']})).get('postData')
            route = PausedRequest(self.session, event, body)
            await self.handler(route)
        except Exception:
            # Never log request data, headers or browser exception messages.
            pass
        finally:
            try:
                if route is None:
                    await self.session.send('Fetch.failRequest', {'requestId':event['requestId'],'errorReason':'BlockedByClient'})
                elif not route.settled:
                    await route.abort()
            except Exception:
                pass  # Page or browser already closed.
