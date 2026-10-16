import asyncio, hashlib, hmac, json, logging, math, os, re, time, uuid
from collections import deque
from pathlib import Path
from fastapi import FastAPI, Request, HTTPException
from fastapi.responses import StreamingResponse
from .billing import Billing
from .energy import Energy
from .policy import eligible, unsafe
from .edge import PublicEdge, PrivateIngress
from .lightning import backend
from .fx import Fx
from .machines import Machines, RegistryUnavailable, ID, legacy_tunnel_keys
from .settings import install_settings, load_config

EFFORT={'none':0,'minimal':0,'low':256,'medium':1024,'high':4096}

log=logging.getLogger(__name__)


def valid_metrics(m):
    # Optional admin-only load numbers from the worker (memory/CPU/GPU); never used for scheduling.
    import math
    return isinstance(m,dict) and len(m)<=16 and all(isinstance(k,str) and len(k)<=32 and (v is None or (type(v) in (int,float) and math.isfinite(v) and abs(v)<1e7)) for k,v in m.items())

class ThinkStrip:
    def __init__(self,allow=False): self.buffer=''; self.hidden=False; self.allow=allow
    def feed(self,text,final=False):
        if self.allow: return text
        self.buffer+=text; out=''
        while self.buffer:
            marker='</think>' if self.hidden else '<think>'
            i=self.buffer.find(marker)
            if i>=0:
                if not self.hidden: out+=self.buffer[:i]
                self.buffer=self.buffer[i+len(marker):]; self.hidden=not self.hidden; continue
            keep=0
            if not final:
                for n in range(1,min(len(marker),len(self.buffer)+1)):
                    if self.buffer.endswith(marker[:n]): keep=n
            chunk=self.buffer[:-keep] if keep else self.buffer
            if not self.hidden: out+=chunk
            self.buffer=self.buffer[-keep:] if keep else ''; break
        return out

class Coordinator:
    def __init__(self,root):
        self.root=Path(root); self.config=load_config(self.root/'config.json'); self.bill=Billing(self.root/'data')
        self.energy=Energy(self.config)
        self.fx=Fx(self.config,self.root/'data'); self.bill.reprice(self.fx.rate); self.config['live_dkk_per_btc']=self.fx.rate
        self.bill.default_max_concurrent=self.config.get('per_key_max_concurrent',4)
        legacy=self.root/'data/worker_secrets.json'
        self.machines=Machines(self.root/'data',(lambda: json.loads(legacy.read_text()) if legacy.exists() else {}),self.config.get('macs',{}),legacy_tunnel_keys(Path(os.environ.get('NOCTURNE_AUTHORIZED_KEYS',Path.home()/'.ssh'/'authorized_keys'))) if not (self.root/'data'/'machines.json').exists() else {}); self.admin_hash=(self.root/'data/admin-secret.sha256').read_text().strip()
        self.invoice_admissions={}; self.signups={}; self.trials={}; self.workers={}; self.jobs={}; self.queue=deque(); self.bulk=deque(); self.rates={}; self.active={}; self.wake=asyncio.Event()
        self.lightning=backend(self.config.get('lightning',{}),self.bill)
        self.public=FastAPI(docs_url=None,redoc_url=None,openapi_url=None); self.worker=FastAPI(); self.admin=FastAPI(); self.routes()
        install_settings(self)
        self.public.add_middleware(PublicEdge,config=self.config)
        self.worker.add_middleware(PrivateIngress,config=self.config,worker=True)
        self.admin.add_middleware(PrivateIngress,config=self.config,worker=False)
        async def registry_unavailable(request,error):
            self.wake.set()
            from fastapi.responses import JSONResponse
            return JSONResponse({'detail':'machine registry unavailable; restart required'},status_code=503)
        self.worker.add_exception_handler(RegistryUnavailable,registry_unavailable)
        self.admin.add_exception_handler(RegistryUnavailable,registry_unavailable)
        @self.admin.middleware('http')
        async def tailnet_guard(request,call_next):
            # Requests proxied by `tailscale serve` carry Tailscale-User-Login (set by tailscaled, not the client).
            login=request.headers.get('tailscale-user-login')
            if login is not None:
                from fastapi.responses import JSONResponse
                if login not in self.config.get('admin_tailnet_logins',[]): return JSONResponse({'detail':'not allowed'},403)
                if request.url.path.startswith('/admin/lightning/'): return JSONResponse({'detail':'not found'},404)
            return await call_next(request)
    def client_ip(self,r):
        import ipaddress
        peer=r.client.host if r.client else 'unknown'
        try:
            if ipaddress.ip_address(peer).is_loopback and r.headers.get('x-forwarded-for'):
                return str(ipaddress.ip_address(r.headers['x-forwarded-for'].split(',')[0].strip()))
        except ValueError: pass
        return peer
    def digest(self,s): return hashlib.sha256(s.encode()).hexdigest()
    def bearer(self,r):
        v=r.headers.get('authorization',''); return v[7:] if v.startswith('Bearer ') else ''
    def auth(self,r):
        k=self.bill.key_for_digest(self.digest(self.bearer(r)))
        if k:
                v=self.bill.key(k)
                if r.url.path!='/v1/chat/completions':
                    now=time.monotonic(); rate=self.rates.setdefault('api:'+k,deque())
                    while rate and now-rate[0]>=60: rate.popleft()
                    if len(rate)>=v['rate_limit_per_minute']: raise HTTPException(429,'API key rate limit')
                    rate.append(now)
                return k
        raise HTTPException(401,'invalid API key')
    def worker_auth(self,r,wid):
        self.machines.check_available()
        want=self.machines.secret(wid) if isinstance(wid,str) and ID.fullmatch(wid) else None
        ok=hmac.compare_digest(self.digest(self.bearer(r)),want or '0'*64)
        if not want or not ok: raise HTTPException(401,'invalid worker credentials')
    def admin_auth(self,r):
        if not hmac.compare_digest(self.digest(self.bearer(r)),self.admin_hash): raise HTTPException(401,'invalid admin credentials')
    async def json_body(self,r):
        try: b=await r.json()
        except Exception: raise HTTPException(400,'invalid JSON')
        if not isinstance(b,dict): raise HTTPException(400,'invalid body')
        return b
    async def drop_worker(self,wid):
        await self.interrupt(wid); self.workers.pop(wid,None); self.wake.set()
    def machine_rows(self,now):
        rows=[]
        for wid in sorted(set(self.workers)|set(self.machines.items)):
            w=self.workers.get(wid)
            base={'worker_id':wid,'online':w is not None,'enabled':self.machines.enabled(wid),'mac':self.machines.meta(wid)}
            if w is None:
                rows.append({**base,'eligible':False,'heartbeat_age_s':None,'battery_pct':None,'on_ac':None,'thermal_state':None,'user_active':None,'tokens_per_sec':None,'busy':0,'capacity':None,'metrics':{}})
            else:
                rows.append({**base,'eligible':eligible(w,now),'heartbeat_age_s':round(now-w['seen'],1),'battery_pct':w.get('battery_pct'),'on_ac':w.get('on_ac'),'thermal_state':w.get('thermal_state'),'user_active':w.get('user_active'),'tokens_per_sec':w.get('tokens_per_sec'),'busy':len(w['jobs']),'capacity':w.get('capacity',1),'metrics':w.get('metrics') or {}})
        return rows
    def pending(self,q=None): return sum(self.jobs[r]['status']=='queued' for r in (self.queue if q is None else q) if r in self.jobs)
    def is_bulk(self,k): return k in set(self.config.get('bulk_keys',[]))
    def enqueue(self,rid):
        (self.bulk if self.jobs[rid].get('bulk') else self.queue).append(rid); self.wake.set()
    def sse(self,payload): return 'data: '+(payload if isinstance(payload,str) else json.dumps(payload))+'\n\n'
    def chunk(self,j,text='',finish=None):
        return dict(id=j['id'],object='chat.completion.chunk',created=j['created'],model=self.config['model'],choices=[dict(index=0,delta={'content':text} if text else {},finish_reason=finish)])
    def close(self,j,cancel=False):
        if j['closed']: return
        j['closed']=True; j['status']='cancelled' if cancel else 'done'
        self.bill.finish(j['id'],j['prompt_tokens'],j['completion_tokens'],cancel)
        self.active[j['key']]=max(0,self.active.get(j['key'],0)-1)
        if j.get('worker') in self.workers: self.workers[j['worker']]['jobs'].discard(j['id'])
        j['done'].set()
        log.info('request_id=%s key_id=%s worker_id=%s prompt_tokens=%s completion_tokens=%s latency_ms=%s',j['id'],j['key'],j.get('worker'),j['prompt_tokens'],j['completion_tokens'],int((time.monotonic()-j['start'])*1000))
    async def interrupt(self,wid):
        w=self.workers[wid]
        for rid in list(w['jobs']):
            j=self.jobs[rid]
            if j['closed']: continue
            w['jobs'].discard(rid); j['lease']=None
            if not j['stream'] and j['retries']==0:
                j['retries']=1; j['worker']=None; j['status']='queued'; j['text']=''; j['strip']=ThinkStrip(self.bill.key(j['key']).get('allow_thinking',False)); j['completion_tokens']=0; j['raw_chars']=0; j['reasoning']=''
                self.enqueue(rid)
                log.info('request_id=%s worker_id=%s event=cancel_requeue',rid,wid)
            else:
                j['error']={'type':'worker_interrupted'}; await j['chunks'].put(self.sse({'error':j['error']})); await j['chunks'].put(self.sse('[DONE]')); self.close(j,True)
    async def maintenance(self):
        while True:
            await self.energy.refresh()
            if await self.fx.refresh(): self.bill.reprice(self.fx.rate); self.config['live_dkk_per_btc']=self.fx.rate
            now=time.monotonic()
            opened, _, end=self.energy.window()
            grace=self.config.get("schedule",{}).get("grace_min",20)*60
            for wid,w in list(self.workers.items()):
                if now-w.get('seen',0)>=45 or unsafe(w) or (not opened and self.energy.clock().timestamp() >= end.timestamp()+grace): await self.interrupt(wid)
            for j in list(self.jobs.values()):
                if not j['closed'] and now-j['start']>self.config['request_timeout_s']:
                    j['error']={'type':'request_timeout'}; await j['chunks'].put(self.sse({'error':j['error']})); await j['chunks'].put(self.sse('[DONE]')); self.close(j,True)
                if j['closed'] and now-j['start']>self.config['request_timeout_s']+60: self.jobs.pop(j['id'],None)
            await asyncio.sleep(1)
    async def poll_invoice(self,i):
        row=self.bill.invoice(i)
        if row and row['state']=='pending':
            if row['backend']!=self.lightning.name: return row
            state='expired' if row['expires_at']<=time.time() else await self.lightning.check_invoice(i)
            self.bill.invoice_state(i,state)
        return self.bill.invoice(i)
    async def invoice_maintenance(self):
        while True:
            for row in self.bill.pending_invoices():
                try: await self.poll_invoice(row['invoice_id'])
                except Exception: log.warning('invoice_poll_failed invoice_id=%s',row['invoice_id'])
            await asyncio.sleep(30)
    def validate(self,b):
        allowed={'model','messages','max_tokens','stream','temperature','top_p','stop','presence_penalty','frequency_penalty','n','stream_options','reasoning_effort'}
        if not isinstance(b,dict) or set(b)-allowed: raise HTTPException(400,'unsupported parameters')
        if b.get('model')!=self.config['model'] or b.get('n',1)!=1: raise HTTPException(400,'unsupported model or n')
        if not isinstance(b.get('stream',False),bool): raise HTTPException(400,'invalid stream')
        msgs=b.get('messages')
        if not isinstance(msgs,list) or not msgs: raise HTTPException(400,'messages required')
        for m in msgs:
            if not isinstance(m,dict) or set(m)-{'role','content'} or m.get('role') not in {'system','user','assistant'} or not isinstance(m.get('content'),str): raise HTTPException(400,'text messages only')
        chars=sum(len(m['content']) for m in msgs)
        mt=b.get('max_tokens',self.config['max_tokens'])
        if type(mt)!=int or not 1<=mt<=self.config['max_tokens'] or chars>self.config['max_input_chars'] or math.ceil(chars/4)+mt>self.config['max_context']: raise HTTPException(400,'input or token limit exceeded')
        for field,low,high in [('temperature',0,2),('top_p',0,1),('presence_penalty',-2,2),('frequency_penalty',-2,2)]:
            if field in b and (type(b[field]) not in (int,float) or not low<=b[field]<=high): raise HTTPException(400,'invalid '+field)
        if 'reasoning_effort' in b and b['reasoning_effort'] not in EFFORT: raise HTTPException(400,'invalid reasoning_effort')
        if 'stream_options' in b and b['stream_options']!={'include_usage':True}: raise HTTPException(400,'unsupported stream_options')
        if 'stop' in b and not (isinstance(b['stop'],str) or isinstance(b['stop'],list) and len(b['stop'])<=4 and all(isinstance(x,str) for x in b['stop'])): raise HTTPException(400,'invalid stop')
        return chars,mt
    def price_info(self):
        r=self.fx.rate
        return {'input_msat_per_1k_tokens':self.bill.input_price,'output_msat_per_1k_tokens':self.bill.output_price,'min_charge_msat_per_request':self.bill.minimum,'input_approx_dkk_per_million_tokens':round(self.bill.input_price/1e8*r,3),'output_approx_dkk_per_million_tokens':round(self.bill.output_price/1e8*r,3),'dkk_per_btc':r,'thinking':'reasoning_effort none|low|medium|high (default ~512 thinking tokens); thinking tokens bill as output'}
    def backend_body(self,b,mt):
        body={x:v for x,v in b.items() if x!='reasoning_effort'}; body['max_tokens']=mt
        budget=EFFORT.get(b.get('reasoning_effort'),self.config.get('default_thinking_budget',512))
        budget=min(budget,mt//2)
        body['chat_template_kwargs']={'enable_thinking':budget>=32}
        if budget>=32: body['thinking_budget_tokens']=budget
        return body
    def routes(self):
        @self.public.get('/v1/status')
        async def status():
            await self.energy.refresh()
            return self.energy.state(self.workers)[0]
        @self.admin.get('/admin/energy')
        async def energy(r:Request):
            self.admin_auth(r)
            await self.energy.refresh()
            return {**self.energy.state(self.workers)[0], 'rows':self.energy.rows, 'last_success':self.energy.last_success}
        @self.public.get('/')
        async def landing():
            from fastapi.responses import HTMLResponse
            return HTMLResponse((Path(__file__).parent/'landing.html').read_text().replace('{{SERVICE}}',self.config.get('service_name','inference')).replace('{{MODEL}}',self.config['model']))
        def public_file(name,media):
            from fastapi.responses import Response
            async def h(r:Request):
                base='https://'+r.headers.get('host','') if r.headers.get('host') else ''
                body=(Path(__file__).parent/'public'/name).read_text().replace('{{BASE}}',self.config.get('public_base_url',base)).replace('{{MODEL}}',self.config['model'])
                return Response(body,media_type=media)
            self.public.add_api_route('/'+name,h,methods=['GET'])
        public_file('robots.txt','text/plain'); public_file('llms.txt','text/plain'); public_file('openapi.json','application/json')
        @self.public.get('/healthz')
        async def health(): return {'status':'ok','model':self.config['model'],'eligible_workers':sum(eligible(w,time.monotonic()) for w in self.workers.values())}
        @self.public.get('/v1/models')
        async def models(r:Request):
            self.auth(r); return {'object':'list','data':[{'id':self.config['model'],'object':'model','owned_by':self.config.get('service_name','inference')}]}
        @self.public.post('/v1/chat/completions')
        async def chat(r:Request):
            k=self.auth(r)
            try: b=await r.json()
            except Exception: raise HTTPException(400,'invalid JSON')
            chars,mt=self.validate(b)
            await self.energy.refresh()
            self.energy.admit(self.workers,self.bill.quote(chars,mt),math.ceil(chars/4),mt,bulk=self.is_bulk(k))
            key=self.bill.key(k); now=time.monotonic()
            rate=self.rates.setdefault(k,deque())
            while rate and now-rate[0]>=60: rate.popleft()
            if len(rate)>=key['rate_limit_per_minute'] or self.active.get(k,0)>=key['max_concurrent']: raise HTTPException(429,'rate or concurrency limit')
            bulk=self.is_bulk(k)
            if bulk and self.pending(self.bulk)>=self.config.get('bulk_queue_depth',16): raise HTTPException(503,'bulk queue full')
            if not bulk and self.pending()>=self.config['queue_depth']: raise HTTPException(503,'queue full')
            rid='chatcmpl-'+uuid.uuid4().hex
            if not self.bill.reserve(rid,k,chars,mt): raise HTTPException(402,'insufficient credit')
            rate.append(now); self.active[k]=self.active.get(k,0)+1
            j=dict(id=rid,key=k,created=int(time.time()),start=now,body=self.backend_body(b,mt),stream=b.get('stream',False),status='queued',worker=None,lease=None,retries=0,text='',prompt_tokens=math.ceil(chars/4),completion_tokens=0,strip=ThinkStrip(key.get('allow_thinking',False)),chunks=asyncio.Queue(),done=asyncio.Event(),closed=False,error=None,bulk=bulk)
            self.jobs[rid]=j; self.enqueue(rid)
            async def output():
                try:
                    while True:
                        item=await j['chunks'].get(); yield item
                        if item==self.sse('[DONE]'): break
                finally:
                    if not j['closed']: self.close(j,True)
            if j['stream']: return StreamingResponse(output(),media_type='text/event-stream')
            try:
                while not j['done'].is_set():
                    try: await asyncio.wait_for(j['done'].wait(),1)
                    except asyncio.TimeoutError:
                        if await r.is_disconnected(): self.close(j,True); raise HTTPException(499,'client disconnected')
                if j['error']: raise HTTPException(503,j['error'])
                return dict(id=rid,object='chat.completion',created=j['created'],model=self.config['model'],choices=[dict(index=0,message={'role':'assistant','content':j['text'],**({'reasoning_content':j['reasoning']} if j.get('reasoning') else {})},finish_reason='stop')],usage=dict(prompt_tokens=j['prompt_tokens'],completion_tokens=j['completion_tokens'],total_tokens=j['prompt_tokens']+j['completion_tokens']))
            except asyncio.CancelledError: self.close(j,True); raise
        @self.worker.post('/worker/register')
        async def register(r:Request):
            b=await self.json_body(r); wid=b.get('worker_id'); self.worker_auth(r,wid)
            if wid in self.workers: await self.interrupt(wid)
            self.workers[wid]=dict(worker_id=wid,jobs=set(),seen=time.monotonic(),ready=False)
            return {'registered':True}
        @self.worker.post('/worker/heartbeat')
        async def heartbeat(r:Request):
            b=await self.json_body(r); wid=b.get('worker_id'); self.worker_auth(r,wid)
            if wid not in self.workers: raise HTTPException(409,'register first')
            required={'worker_id','model','ready','on_ac','battery_pct','thermal_state','user_active','in_flight','capacity'}
            if 'metrics' in b and not valid_metrics(b['metrics']): raise HTTPException(400,'invalid heartbeat')
            if set(b)-{'tokens_per_sec','metrics'}!=required or ('tokens_per_sec' in b and (type(b['tokens_per_sec']) not in (int,float) or not 0 < b['tokens_per_sec'] <= 100000)) or b['model']!=self.config['model'] or any(type(b[x])!=bool for x in ['ready','on_ac','user_active']) or type(b['battery_pct']) not in (int,float) or not 0<=b['battery_pct']<=100 or b['thermal_state'] not in {'nominal','fair','serious','critical'} or type(b['capacity'])!=int or not 1<=b['capacity']<=4 or type(b['in_flight'])!=int or not 0<=b['in_flight']<=b['capacity']: raise HTTPException(400,'invalid heartbeat')
            w=self.workers[wid]; w.update(b,seen=time.monotonic())
            if unsafe(w): await self.interrupt(wid)
            self.wake.set()
            _, serving=self.energy.state(self.workers)
            return {'eligible':eligible(w,time.monotonic()),'serve':wid in serving,'active_requests':list(w['jobs'])}
        @self.worker.post('/worker/lease')
        async def lease(r:Request):
            b=await self.json_body(r); wid=b.get('worker_id'); self.worker_auth(r,wid); deadline=time.monotonic()+20
            reg=self.workers.get(wid)
            while True:
                # Secret rotation, disable or a re-register while this poll waited must not hand it work.
                self.worker_auth(r,wid)
                w=self.workers.get(wid)
                if w is None or w is not reg: return {'job':None}
                _, serving=self.energy.state(self.workers)
                if wid in serving and len(w['jobs'])<w.get('capacity',1):
                    # Paying traffic first. Bulk (our own) work only fills spare slots and is capped per worker,
                    # so a paying request always finds a free slot on every serving Mac.
                    bulk_here=sum(1 for x in w['jobs'] if self.jobs.get(x,{}).get('bulk'))
                    lanes=[self.queue]
                    if not self.pending() and bulk_here<min(self.config.get('bulk_slots_per_worker',1),w.get('capacity',1)-1): lanes.append(self.bulk)
                    for q in lanes:
                        for _ in range(len(q)):
                            rid=q.popleft(); j=self.jobs.get(rid)
                            if not j or j['status']!='queued': continue
                            j['status']='leased'; j['worker']=wid; j['lease']=uuid.uuid4().hex; w['jobs'].add(rid)
                            return {'request_id':rid,'lease_id':j['lease'],'body':j['body']}
                remaining=deadline-time.monotonic()
                if remaining<=0: return {'job':None}
                self.wake.clear()
                try: await asyncio.wait_for(self.wake.wait(),remaining)
                except asyncio.TimeoutError: return {'job':None}
        async def job_auth(r):
            b=await self.json_body(r); self.worker_auth(r,b.get('worker_id')); j=self.jobs.get(b.get('request_id'))
            if not j or j['closed'] or j['worker']!=b.get('worker_id') or j['lease']!=b.get('lease_id'): return b,None
            return b,j
        @self.worker.post('/worker/result')
        async def result(r:Request):
            b,j=await job_auth(r)
            if j is None: return {'accepted':False}
            raw=b.get('chunk',''); thought=b.get('reasoning','')
            if not isinstance(raw,str) or len(raw)>16384 or not isinstance(thought,str) or len(thought)>16384: raise HTTPException(400,'invalid chunk')
            if thought:
                j['raw_chars']=j.get('raw_chars',0)+len(thought)
                if j['strip'].allow:
                    j['reasoning']=j.get('reasoning','')+thought
                    if j['stream']: await j['chunks'].put(self.sse(dict(id=j['id'],object='chat.completion.chunk',created=j['created'],model=self.config['model'],choices=[dict(index=0,delta={'reasoning_content':thought},finish_reason=None)])))
            # Bound completion by both requested token estimate and a hard character cap.
            j['raw_chars']=j.get('raw_chars',0)+len(raw)
            if j['raw_chars']>j['body']['max_tokens']*16:
                j['error']={'type':'output_limit'}; await j['chunks'].put(self.sse({'error':j['error']})); await j['chunks'].put(self.sse('[DONE]')); self.close(j,True); return {'accepted':False}
            text=j['strip'].feed(raw,b.get('final',False)); j['text']+=text
            if text and j['stream']: await j['chunks'].put(self.sse(self.chunk(j,text)))
            if b.get('final'):
                usage=b.get('usage',{}); p=usage.get('prompt_tokens',j['prompt_tokens']); c=usage.get('completion_tokens',math.ceil(j['raw_chars']/4))
                if type(p)!=int or type(c)!=int or p<0 or c<0: raise HTTPException(400,'invalid usage')
                cap=min(j['body']['max_tokens'],math.ceil(j['raw_chars']/2))
                if c>cap: self.bill.log('token_mismatch',j['id'],j['key'],0,reported=c,cap=cap)
                p=j['prompt_tokens']; j['completion_tokens']=min(c,cap)
                if j['stream']:
                    await j['chunks'].put(self.sse(self.chunk(j,finish='stop')))
                    if j['body'].get('stream_options'): await j['chunks'].put(self.sse(dict(id=j['id'],object='chat.completion.chunk',model=self.config['model'],choices=[],usage=dict(prompt_tokens=p,completion_tokens=j['completion_tokens'],total_tokens=p+j['completion_tokens']))))
                    await j['chunks'].put(self.sse('[DONE]'))
                self.close(j)
            return {'accepted':True}
        @self.worker.post('/worker/fail')
        async def fail(r:Request):
            b,j=await job_auth(r)
            if j: await self.interrupt(b['worker_id'])
            return {'accepted':j is not None}
        @self.admin.post('/admin/credit')
        async def credit(r:Request):
            self.admin_auth(r); b=await r.json(); k=b.get('key_id'); amount=b.get('amount_msat')
            if not self.bill.key(k) or type(amount)!=int or amount<=0: raise HTTPException(400,'invalid credit')
            self.bill.credit(k,amount); return {'key_id':k,'credit_msat':self.bill.key(k)['credit_msat']}
        @self.admin.get('/admin/workers')
        async def workers(r:Request):
            self.admin_auth(r); return [{'worker_id':wid,'eligible':eligible(w,time.monotonic()),'heartbeat_age':time.monotonic()-w['seen'],**{k:v for k,v in w.items() if k not in {'jobs','seen','worker_id'}},'active_requests':list(w['jobs'])} for wid,w in self.workers.items()]
        @self.admin.get('/admin/summary')
        async def summary(r:Request):
            self.admin_auth(r)
            await self.energy.refresh()
            wallet=None
            if hasattr(self.lightning,'wallet'):
                try: wallet=await self.lightning.wallet()
                except Exception: wallet={'error':'wallet unavailable'}
            now=time.monotonic(); rate=self.fx.rate
            return dict(service=self.config.get('service_name','inference'),dkk_per_btc=rate,fx=self.fx.status(),pricing=self.price_info(),watts_per_worker=self.config.get('energy',{}).get('watts_per_worker',20),wallet=wallet,
                energy=self.energy.state(self.workers)[0],
                workers=self.machine_rows(now),
                queue=dict(pending=self.pending(),active=sum(len(w['jobs']) for w in self.workers.values())),
                **self.bill.summary(time.time()-30*86400,self.config.get('internal_keys',['demo']),self.lightning.name))
        @self.admin.get('/admin/machines')
        async def machines_list(r:Request):
            self.admin_auth(r); return self.machines.list()
        @self.admin.post('/admin/machines',status_code=201)
        async def machines_add(r:Request):
            self.admin_auth(r); b=await self.json_body(r)
            try: return self.machines.add(b)
            except KeyError: raise HTTPException(409,'machine already exists')
            except ValueError as e: raise HTTPException(400,str(e))
        @self.admin.patch('/admin/machines/{wid}')
        async def machines_update(wid:str,r:Request):
            self.admin_auth(r); b=await self.json_body(r)
            try: m=self.machines.update(wid,b)
            except LookupError: raise HTTPException(404,'not found')
            except ValueError as e: raise HTTPException(400,str(e))
            if wid in self.workers and (not m['enabled'] or 'secret_sha256' in b): await self.drop_worker(wid)
            return m
        @self.admin.delete('/admin/machines/{wid}')
        async def machines_delete(wid:str,r:Request):
            self.admin_auth(r)
            try: self.machines.delete(wid)
            except LookupError: raise HTTPException(404,'not found')
            if wid in self.workers: await self.drop_worker(wid)
            return {'deleted':wid}
        dist=Path(__file__).parent.parent/'web'/'dist'
        csp={'Cache-Control':'no-store','Content-Security-Policy':"default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'",'X-Content-Type-Options':'nosniff','Referrer-Policy':'no-referrer'}
        @self.admin.get('/admin')
        async def admin_page():
            from fastapi.responses import HTMLResponse
            index=dist/'index.html'
            if not index.exists(): return HTMLResponse('<h1>admin UI not built</h1><p>Run <code>cd web && bun install && bun run build</code>.</p>',503,headers=csp)
            return HTMLResponse(index.read_text(),headers=csp)
        @self.admin.get('/admin/assets/{name}')
        async def admin_asset(name:str):
            from fastapi.responses import FileResponse
            f=(dist/'assets'/name)
            if not re.fullmatch(r'[A-Za-z0-9_.-]{1,128}',name) or name.startswith('.') or not f.is_file(): raise HTTPException(404,'not found')
            media={'.js':'text/javascript','.css':'text/css','.svg':'image/svg+xml','.woff2':'font/woff2','.png':'image/png'}.get(f.suffix,'application/octet-stream')
            return FileResponse(f,media_type=media,headers={'Cache-Control':'public, max-age=31536000, immutable','X-Content-Type-Options':'nosniff'})
        @self.admin.get('/admin/ledger/tail')
        async def ledger(r:Request):
            self.admin_auth(r); return self.bill.tail()

        @self.public.post('/v1/keys')
        async def create_key(r:Request):
            try: b=await r.json()
            except Exception: raise HTTPException(400,'invalid JSON')
            if not isinstance(b,dict): raise HTTPException(400,'invalid body')
            invite=self.config.get('signup_invite','')
            invited=bool(invite) and isinstance(b.get('invite'),str) and hmac.compare_digest(b['invite'],invite)
            if not invited and not self.config.get('open_signup',False): raise HTTPException(403,'invite required')
            now=time.time(); ip=self.client_ip(r)
            hist=self.signups.setdefault(ip,deque())
            while hist and now-hist[0]>=86400: hist.popleft()
            if len(hist)>=self.config.get('signup_per_ip_per_day',3): raise HTTPException(429,'signup limit for this address; try tomorrow')
            hist.append(now)
            if len(self.signups)>20000: self.signups={k:v for k,v in self.signups.items() if v and now-v[-1]<86400}
            key=self.bill.create_key()
            trial=self.config.get('trial_credit_msat',0)
            day=time.strftime('%Y-%m-%d',time.gmtime(now))
            if trial and self.trials.get(day,0)<self.config.get('trials_per_day',100):
                self.trials={day:self.trials.get(day,0)+1}; self.bill.grant_trial(key['key_id'],trial); key['trial_credit_msat']=trial
            return key
        @self.public.get('/v1/pricing')
        async def pricing():
            return self.price_info()
        @self.public.get('/v1/credits/balance')
        async def balance(r:Request):
            k=self.auth(r); return {'key_id':k,'balance_msat':self.bill.key(k)['credit_msat']}
        @self.public.post('/v1/credits/invoice')
        async def create_invoice(r:Request):
            k=self.auth(r)
            try: b=await r.json()
            except Exception: raise HTTPException(400,'invalid JSON')
            amount=b.get('amount_msat') if isinstance(b,dict) else None
            cfg=self.config.get('lightning',{})
            if type(amount)!=int or not cfg.get('min_msat',1000)<=amount<=cfg.get('max_msat',100000000): raise HTTPException(400,'invoice amount outside limits')
            if self.lightning.name in {'lnbits','phoenixd'} and amount%1000: raise HTTPException(400,'whole satoshis required')
            # No await between counting and reserving: admission is atomic on this
            # coordinator's event loop, without holding a lock across network I/O.
            if sum(x['key_id']==k for x in self.bill.pending_invoices())+self.invoice_admissions.get(k,0)>=5: raise HTTPException(429,'too many pending invoices')
            self.invoice_admissions[k]=self.invoice_admissions.get(k,0)+1
            try:
                try: invoice=await self.lightning.create_invoice(amount,self.config.get('service_name','inference')+' prepaid credits')
                except Exception: raise HTTPException(502,'invoice backend unavailable')
                self.bill.add_invoice(k,amount,invoice,self.lightning.name)
            finally:
                self.invoice_admissions[k]-=1
                if not self.invoice_admissions[k]: self.invoice_admissions.pop(k)
            try:
                import segno; invoice=dict(invoice,qr_svg=segno.make('lightning:'+invoice['bolt11'].upper(),error='l').svg_data_uri(scale=4,border=2))
            except Exception: pass
            return invoice
        @self.public.get('/v1/credits/invoice/{invoice_id}')
        async def get_invoice(invoice_id:str,r:Request):
            k=self.auth(r); row=self.bill.invoice(invoice_id)
            if not row or row['key_id']!=k: raise HTTPException(404,'not found')
            try: row=await self.poll_invoice(invoice_id)
            except Exception: raise HTTPException(502,'invoice backend unavailable')
            return {field:row[field] for field in ('invoice_id','state','amount_msat','expires_at','paid_at')}
        @self.admin.post('/admin/lightning/webhook')
        async def lightning_webhook(r:Request):
            if self.lightning.name!='phoenixd': raise HTTPException(404,'not found')
            if not r.client or r.client.host not in {'127.0.0.1','::1'}: raise HTTPException(403,'loopback required')
            body=bytearray()
            async for chunk in r.stream():
                body.extend(chunk)
                if len(body)>16384: raise HTTPException(413,'webhook too large')
            try: credited=await self.lightning.webhook(bytes(body),r.headers.get('X-Phoenix-Signature',''))
            except PermissionError: raise HTTPException(401,'invalid webhook signature')
            except (ValueError,TypeError): raise HTTPException(400,'invalid webhook event')
            except Exception: raise HTTPException(502,'invoice backend unavailable')
            return {'ok':True,'credited':credited}
        @self.admin.post('/admin/invoices/{invoice_id}/mark-paid')
        async def mark_paid(invoice_id:str,r:Request):
            self.admin_auth(r)
            if self.lightning.name!='fake': raise HTTPException(404,'not found')
            row=self.bill.invoice(invoice_id)
            if not row or row['backend']!='fake': raise HTTPException(404,'not found')
            await self.lightning.mark_paid(invoice_id)
            return {'invoice_id':invoice_id,'state':self.bill.invoice(invoice_id)['state']}
