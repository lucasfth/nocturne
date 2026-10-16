"""Public ASGI perimeter: byte caps apply before JSON parsing, including chunked bodies."""
import asyncio
import ipaddress
import re
import time
from collections import deque
from starlette.responses import JSONResponse

LANDING_CSP=b"default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; img-src data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"


class PublicEdge:
    def __init__(self,app,config): self.app=app; self.config=config; self.rates={}
    async def __call__(self,scope,receive,send):
        if scope['type']!='http': return await self.app(scope,receive,send)
        headers={k.decode().lower():v.decode() for k,v in scope['headers']}
        async def secure_send(message):
            if message['type']=='http.response.start':
                csp=LANDING_CSP if scope['path']=='/' else b"default-src 'none'; frame-ancestors 'none'"
                message['headers']=[(k,v) for k,v in message.get('headers',[]) if k.lower() not in (b'server',b'access-control-allow-origin')]+[(b'x-content-type-options',b'nosniff'),(b'cache-control',b'no-store'),(b'content-security-policy',csp),(b'x-frame-options',b'DENY'),(b'strict-transport-security',b'max-age=31536000'),(b'referrer-policy',b'no-referrer')]
            await send(message)
        async def reject(code,detail): await JSONResponse({'detail':detail},status_code=code)(scope,receive,secure_send)
        path=scope['path']; method=scope['method']
        allowed={('GET','/'),('GET','/robots.txt'),('GET','/llms.txt'),('GET','/openapi.json'),('GET','/healthz'),('GET','/v1/status'),('GET','/v1/models'),('GET','/v1/pricing'),('POST','/v1/keys'),('POST','/v1/credits/invoice'),('GET','/v1/credits/balance'),('POST','/v1/chat/completions')}
        if (method,path) not in allowed and not (method=='GET' and re.fullmatch(r'/v1/credits/invoice/[a-zA-Z0-9_-]{1,128}',path)):
            return await reject(404,'not found')
        peer=(scope.get('client') or ('unknown',0))[0]; ip=peer
        try:
            if ipaddress.ip_address(peer).is_loopback and 'x-forwarded-for' in headers:
                ip=str(ipaddress.ip_address(headers['x-forwarded-for'].split(',')[0].strip()))
        except ValueError: pass
        now=time.monotonic()
        # Bound rate-limit storage; unexpired entries are never evicted to bypass limits.
        if len(self.rates)>4096:
            self.rates={k:v for k,v in self.rates.items() if v and now-v[-1]<60}
            if ip not in self.rates and len(self.rates)>4096: return await reject(429,'rate limiter capacity')
        rate=self.rates.setdefault(ip,deque())
        while rate and now-rate[0]>=60: rate.popleft()
        strict=path!='/v1/chat/completions'
        limit=self.config.get('public_ip_rate_per_minute',20) if strict else self.config.get('chat_ip_rate_per_minute',60)
        if len(rate)>=limit: return await reject(429,'IP rate limit')
        rate.append(now)
        maximum=self.config.get('max_body_bytes',256*1024)
        try:
            if 'content-length' in headers:
                n=int(headers['content-length'])
                if n<0: return await reject(400,'invalid content length')
                if n>maximum: return await reject(413,'body too large')
        except ValueError: return await reject(400,'invalid content length')
        body=bytearray(); deadline=time.monotonic()+self.config.get('body_read_timeout_s',15)
        while True:
            try:
                remaining=min(self.config.get('body_idle_timeout_s',5),deadline-time.monotonic())
                if remaining<=0: raise asyncio.TimeoutError
                message=await asyncio.wait_for(receive(),remaining)
            except asyncio.TimeoutError: return await reject(408,'body read timeout')
            if message['type']=='http.disconnect': return
            chunk=message.get('body',b'')
            if len(body)+len(chunk)>maximum: return await reject(413,'body too large')
            body.extend(chunk)
            if not message.get('more_body',False): break
        consumed=False
        async def replay():
            nonlocal consumed
            if not consumed:
                consumed=True
                return {'type':'http.request','body':bytes(body),'more_body':False}
            return await receive()
        await self.app(scope,replay,secure_send)


class PrivateIngress:
    """Bound private HTTP ingress before JSON parsing, not response duration.

    Timers end once the body is buffered, preserving lease polling and streams.
    Authorization here checks only syntax; the route still verifies the secret.
    """
    def __init__(self, app, config, worker=False):
        self.app, self.config, self.worker = app, config, worker

    async def __call__(self, scope, receive, send):
        if scope['type'] != 'http': return await self.app(scope, receive, send)
        headers = {k.decode().lower(): v.decode() for k,v in scope['headers']}
        async def reject(code, detail):
            await JSONResponse({'detail':detail},status_code=code,headers={'Connection':'close'})(scope,receive,send)
        path = scope['path']
        if self.worker:
            token = headers.get('authorization','')
            if not re.fullmatch(r'Bearer [\x21-\x7e]{1,512}', token):
                return await reject(401,'invalid worker credentials')
        elif scope['method'] in {'POST','PATCH','PUT','DELETE'} and path != '/admin/lightning/webhook':
            from .settings import browser_guard
            from starlette.requests import Request
            from fastapi import HTTPException
            try: browser_guard(Request(scope))
            except HTTPException as error: return await reject(error.status_code,error.detail)
            if (headers.get('content-type') or headers.get('origin') or headers.get('sec-fetch-site')) and headers.get('content-type','').split(';')[0].strip().lower() != 'application/json':
                return await reject(415,'application/json required')
        maximum = 262144 if path == '/worker/result' else 65536
        if path in {'/worker/register','/worker/lease','/worker/fail','/admin/config'}: maximum = 4096
        try:
            n = int(headers.get('content-length','0'))
            if n < 0: return await reject(400,'invalid content length')
            if n > maximum: return await reject(413,'body too large')
        except ValueError: return await reject(400,'invalid content length')
        body = bytearray()
        deadline = time.monotonic()+self.config.get('body_read_timeout_s',15)
        while True:
            remaining = min(self.config.get('body_idle_timeout_s',5),deadline-time.monotonic())
            try:
                if remaining <= 0: raise asyncio.TimeoutError
                message = await asyncio.wait_for(receive(),remaining)
            except asyncio.TimeoutError: return await reject(408,'body read timeout')
            if message['type'] == 'http.disconnect': return
            chunk = message.get('body',b'')
            if len(body)+len(chunk)>maximum: return await reject(413,'body too large')
            body.extend(chunk)
            if not message.get('more_body',False): break
        consumed = False
        async def replay():
            nonlocal consumed
            if not consumed:
                consumed = True
                return {'type':'http.request','body':bytes(body),'more_body':False}
            return await receive()
        await self.app(scope,replay,send)
