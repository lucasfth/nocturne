import argparse, asyncio, json, os, time
from pathlib import Path
import httpx
from .macos_sensors import read_sensors
from .benchmark_production import admission_allowed, installation_present, polling

MODEL='Ornith-1.5-9B'

CAPACITY=int(os.getenv('WORKER_CAPACITY','2'))

def safe(s):
    # On AC: always fine. On battery: keep working until 20% (gateway stops new work at <=25%).
    batt=s.get('battery_pct',0); power=batt>=5 if s.get('on_ac') else batt>20
    return bool(power and not s.get('user_active',True) and s.get('thermal_state') in {'nominal','fair'})

class Agent:
    def __init__(self,url,wid,secret,sensors=None,tokens_per_s=None):
        self.url=url.rstrip('/'); self.wid=wid; self.secret=secret; self.sensors=sensors or read_sensors; self.fake=tokens_per_s; self.tasks={}; self.state={}; self.start=time.monotonic(); self.serving=False; self.tokens_per_sec=tokens_per_s or 20
    async def post(self,path,body):
        r=await self.client.post(self.url+path,json={'worker_id':self.wid,**body}); r.raise_for_status(); return r.json()
    async def backend_ok(self):
        if self.fake is not None: return True
        try:
            async with httpx.AsyncClient(timeout=3,trust_env=False) as c:
                return (await c.get('http://127.0.0.1:8080/health')).status_code==200
        except Exception: return False
    async def heartbeat(self):
        while True:
            self.state=self.sensors(time.monotonic()-self.start) if self.fake is not None else self.sensors()
            ready=admission_allowed() and safe(self.state) and await self.backend_ok()
            if not ready:
                for task in list(self.tasks.values()): task.cancel()
            try:
                reply=await self.post('/worker/heartbeat',dict(model=MODEL,ready=ready,in_flight=len(self.tasks),capacity=CAPACITY,tokens_per_sec=self.tokens_per_sec,**self.state))
                self.serving=reply.get('serve',False)
                for rid,task in list(self.tasks.items()):
                    if rid not in reply['active_requests']: task.cancel()
            except httpx.HTTPStatusError as e:
                for task in list(self.tasks.values()): task.cancel()
                if e.response.status_code==409:  # gateway restarted and forgot us
                    try: await self.post("/worker/register",{})
                    except Exception: pass
            except Exception:
                for task in list(self.tasks.values()): task.cancel()
            await asyncio.sleep(2)
    async def execute(self,job):
        started=time.monotonic()
        base={k:job[k] for k in ['request_id','lease_id']}
        try:
            if not admission_allowed(): raise asyncio.CancelledError()
            if self.fake is not None:
                total=min(job['body']['max_tokens'],80)
                for i in range(total):
                    if not admission_allowed() or not safe(self.state): raise asyncio.CancelledError()
                    await asyncio.sleep(1/self.fake)
                    result=await self.post('/worker/result',{**base,'chunk':'lorem '})
                    if not result['accepted']: return
                await self.post('/worker/result',{**base,'final':True,'usage':{'prompt_tokens':sum(len(m['content']) for m in job['body']['messages'])//4+1,'completion_tokens':total}})
            else:
                async with httpx.AsyncClient(timeout=httpx.Timeout(120,connect=5),trust_env=False) as backend:
                    body={**job['body'],'stream':True,'stream_options':{'include_usage':True}}
                    usage={}
                    async with backend.stream('POST','http://127.0.0.1:8080/v1/chat/completions',json=body) as response:
                        response.raise_for_status()
                        async for line in response.aiter_lines():
                            if not admission_allowed() or not safe(self.state): raise asyncio.CancelledError()
                            if not line.startswith('data: ') or line[6:]=='[DONE]': continue
                            chunk=json.loads(line[6:]); usage=chunk.get('usage') or usage
                            for choice in chunk.get('choices',[]):
                                delta=choice.get('delta',{}); text=delta.get('content') or ''; thought=delta.get('reasoning_content') or ''
                                if text or thought:
                                    result=await self.post('/worker/result',{**base,'chunk':text,**({'reasoning':thought} if thought else {})})
                                    if not result['accepted']: return
                    if usage.get('completion_tokens',0)>0: self.tokens_per_sec=usage['completion_tokens']/max(.001,time.monotonic()-started)
                    await self.post('/worker/result',{**base,'final':True,'usage':usage})
        except (Exception,asyncio.CancelledError):
            try: await self.post('/worker/fail',base)
            except Exception: pass
        finally: self.tasks.pop(job['request_id'],None)
    async def run(self):
        async with httpx.AsyncClient(headers={'Authorization':'Bearer '+self.secret},timeout=25,trust_env=False) as client:
            self.client=client; await self.post('/worker/register',{})
            hb=asyncio.create_task(self.heartbeat())
            bp=asyncio.create_task(polling(self.url,self.wid,self.secret)) if installation_present() else None
            try:
                while True:
                    if admission_allowed() and safe(self.state) and self.serving and len(self.tasks)<CAPACITY:
                        try: job=await self.post('/worker/lease',{})
                        except Exception: await asyncio.sleep(1); continue
                        if 'request_id' in job and admission_allowed() and safe(self.state): self.tasks[job['request_id']]=asyncio.create_task(self.execute(job))
                        elif 'request_id' in job: await self.post('/worker/fail',{k:job[k] for k in ['request_id','lease_id']})
                    else: await asyncio.sleep(.25)
            finally:
                hb.cancel()
                if bp: bp.cancel()
                for t in list(self.tasks.values()): t.cancel()
                await asyncio.gather(hb,*([bp] if bp else []),*list(self.tasks.values()),return_exceptions=True)

def main():
    p=argparse.ArgumentParser(); p.add_argument('--coordinator-url',default=os.getenv('COORDINATOR_URL','http://127.0.0.1:8701')); p.add_argument('--worker-id',default=os.getenv('WORKER_ID','mac-1')); p.add_argument('--secret-file',default=os.getenv('WORKER_SECRET_FILE')); p.add_argument('--simulate',action='store_true'); a=p.parse_args()
    secret=Path(a.secret_file).read_text().strip() if a.secret_file else os.environ['WORKER_SECRET']
    sensors=(lambda t:dict(on_ac=True,battery_pct=90,thermal_state='nominal',user_active=False)) if a.simulate else None
    asyncio.run(Agent(a.coordinator_url,a.worker_id,secret,sensors,8 if a.simulate else None).run())

if __name__=='__main__': main()
