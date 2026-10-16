import asyncio, hashlib, json, time
import httpx, pytest
from gateway.app import Coordinator, ThinkStrip
from gateway.policy import eligible

@pytest.fixture
def c(tmp_path):
    d=tmp_path/'data'; d.mkdir()
    h=lambda s:hashlib.sha256(s.encode()).hexdigest()
    (tmp_path/'config.json').write_text(json.dumps(dict(model='Ornith-1.5-9B',max_input_chars=1000,max_tokens=100,max_context=1000,request_timeout_s=2,queue_depth=2)))
    (d/'api_keys.json').write_text(json.dumps({'test':dict(hashed_secret=h('api'),credit_msat=10000,rate_limit_per_minute=10,max_concurrent=10,allow_thinking=False)}))
    (d/'prices.json').write_text('{"price_per_token_msat":1}')
    (d/'worker_secrets.json').write_text(json.dumps({'w':h('worker')})); (d/'admin-secret.sha256').write_text(h('admin'))
    return Coordinator(tmp_path)

def run(fn): return asyncio.run(fn())
def body(stream=False): return dict(model='Ornith-1.5-9B',messages=[dict(role='user',content='hello')],max_tokens=20,stream=stream)
def clients(c): return httpx.AsyncClient(transport=httpx.ASGITransport(app=c.public),base_url='http://test',headers={'Authorization':'Bearer api'}),httpx.AsyncClient(transport=httpx.ASGITransport(app=c.worker),base_url='http://test',headers={'Authorization':'Bearer worker'})
async def register(w,**kw):
    await w.post('/worker/register',json={'worker_id':'w'})
    return await beat(w,**kw)
async def beat(w,**kw):
    return await w.post('/worker/heartbeat',json=dict(worker_id='w',model='Ornith-1.5-9B',ready=True,on_ac=True,battery_pct=90,thermal_state='nominal',user_active=False,in_flight=0,capacity=1,**kw)) if not kw else await w.post('/worker/heartbeat',json={**dict(worker_id='w',model='Ornith-1.5-9B',ready=True,on_ac=True,battery_pct=90,thermal_state='nominal',user_active=False,in_flight=0,capacity=1),**kw})
async def waitqueue(c,n=1):
    for _ in range(100):
        if c.pending()>=n: return
        await asyncio.sleep(.001)
    raise AssertionError('queue did not fill')
async def final(w,job,text='hello',**extra): return await w.post('/worker/result',json=dict(worker_id='w',request_id=job['request_id'],lease_id=job['lease_id'],chunk=text,final=True,usage=extra.pop('usage',{'prompt_tokens':2,'completion_tokens':3}),**extra))

def test_auth_and_success(c):
    async def scenario():
        p,w=clients(c)
        async with p,w:
            assert (await p.get('/v1/models',headers={'Authorization':'bad'})).status_code==401
            assert (await p.get('/v1/models')).status_code==200
            await register(w); t=asyncio.create_task(p.post('/v1/chat/completions',json=body())); await waitqueue(c)
            job=(await w.post('/worker/lease',json={'worker_id':'w'})).json(); await final(w,job)
            r=await t; assert r.status_code==200 and r.json()['choices'][0]['message']['content']=='hello'
    run(scenario)

@pytest.mark.parametrize('extra',[{'tools':[]},{'functions':[]},{'tool_choice':'auto'},{'logit_bias':{}},{'seed':1},{'n':2},{'unknown':True},{'model':'other'}])
def test_400(c,extra):
    async def s():
        p,_=clients(c)
        async with p: assert (await p.post('/v1/chat/completions',json={**body(),**extra})).status_code==400
    run(s)

def test_rate(c):
    c.rates['test']=__import__('collections').deque([time.monotonic()]*10)
    async def s():
        p,_=clients(c)
        async with p: assert (await p.post('/v1/chat/completions',json=body())).status_code==429
    run(s)

def test_concurrency(c):
    c.active['test']=10
    async def s():
        p,_=clients(c)
        async with p: assert (await p.post('/v1/chat/completions',json=body())).status_code==429
    run(s)

def test_queue_full(c):
    async def s():
        p,_=clients(c)
        async with p:
            tasks=[asyncio.create_task(p.post('/v1/chat/completions',json=body())) for _ in range(2)]; await waitqueue(c,2)
            assert (await p.post('/v1/chat/completions',json=body())).status_code==503
            for t in tasks: t.cancel()
            await asyncio.gather(*tasks,return_exceptions=True)
    run(s)

def test_credit(c):
    c.bill.db.execute("UPDATE keys SET balance_msat=0 WHERE key_id='test'")
    async def s():
        p,_=clients(c)
        async with p: assert (await p.post('/v1/chat/completions',json=body())).status_code==402
    run(s)

@pytest.mark.parametrize('state',[{'on_ac':False,'battery_pct':25},{'on_ac':False,'battery_pct':20},{'on_ac':True,'battery_pct':3}])
def test_admission_refusal(c,state):
    async def s():
        _,w=clients(c)
        async with w:
            r=await register(w,**state); assert not r.json()['eligible']; assert not eligible(c.workers['w'],time.monotonic())
    run(s)

def test_unplug_cancel_requeue(c):
    async def s():
        p,w=clients(c)
        async with p,w:
            await register(w); t=asyncio.create_task(p.post('/v1/chat/completions',json=body())); await waitqueue(c)
            j=(await w.post('/worker/lease',json={'worker_id':'w'})).json()
            await beat(w,on_ac=False,battery_pct=20); assert c.jobs[j['request_id']]['retries']==1; assert c.pending()==1
            assert not (await final(w,j)).json()['accepted']
            await beat(w); j2=(await w.post('/worker/lease',json={'worker_id':'w'})).json(); assert j2['lease_id']!=j['lease_id']
            await final(w,j2); assert (await t).status_code==200
    run(s)

def test_heartbeat_loss_stream(c):
    async def s():
        p,w=clients(c)
        async with p,w:
            await register(w); t=asyncio.create_task(p.post('/v1/chat/completions',json=body(True))); await waitqueue(c)
            j=(await w.post('/worker/lease',json={'worker_id':'w'})).json()
            c.workers['w']['seen']-=46; monitor=asyncio.create_task(c.maintenance()); r=await asyncio.wait_for(t,2); monitor.cancel(); await asyncio.gather(monitor,return_exceptions=True)
            assert 'worker_interrupted' in r.text and '[DONE]' in r.text
    run(s)

def test_billing_idempotency(c):
    before=c.bill.keys['test']['credit_msat']; assert c.bill.reserve('r','test',8,20)
    assert c.bill.keys['test']['credit_msat']==before-22
    c.bill.finish('r',2,3); c.bill.finish('r',2,3)
    assert c.bill.keys['test']['credit_msat']==before-5
    lines=c.bill.tail()
    assert [x['event'] for x in lines]==['reserve','settle','release']
    assert c.bill.reserve('cancel','test',4,20); c.bill.finish('cancel',cancel=True); assert c.bill.keys['test']['credit_msat']==before-5

def test_think_strip():
    s=ThinkStrip(); assert s.feed('Hello <thi')=='Hello '; assert s.feed('nk>secret</th')==''; assert s.feed('ink>world',True)=='world'
    s=ThinkStrip(True); assert s.feed('<think>secret</think>')=='<think>secret</think>'


def test_public_perimeter(c):
    async def s():
        p,_=clients(c)
        async with p:
            assert (await p.post('/v1/keys',content=b'x'*(256*1024+1))).status_code==413
            async def chunks():
                yield b'x'*200000
                yield b'x'*100000
            assert (await p.post('/v1/keys',content=chunks())).status_code==413
            for path in ['/worker/register','/admin/credit','/admin/invoices/x/mark-paid','/docs','/openapi.json','/v1/credits/webhook/fake']:
                assert (await p.post(path)).status_code==404
            assert (await p.get('/admin/workers')).status_code==404
            r=await p.get('/healthz'); assert r.headers['x-content-type-options']=='nosniff'
            assert 'server' not in r.headers and 'access-control-allow-origin' not in r.headers
    run(s)

def test_ip_limiter_and_proxy_trust(c):
    c.config['public_ip_rate_per_minute']=2
    async def s():
        async with httpx.AsyncClient(transport=httpx.ASGITransport(app=c.public,client=('127.0.0.1',10)),base_url='http://test') as p:
            assert (await p.get('/healthz',headers={'X-Forwarded-For':'1.2.3.4'})).status_code==200
            assert (await p.get('/healthz',headers={'X-Forwarded-For':'1.2.3.4'})).status_code==200
            assert (await p.get('/healthz',headers={'X-Forwarded-For':'1.2.3.4'})).status_code==429
            assert (await p.get('/healthz',headers={'X-Forwarded-For':'1.2.3.5'})).status_code==200
        async with httpx.AsyncClient(transport=httpx.ASGITransport(app=c.public,client=('8.8.8.8',10)),base_url='http://test') as p:
            for i in range(2): assert (await p.get('/healthz',headers={'X-Forwarded-For':f'2.3.4.{i}'})).status_code==200
            assert (await p.get('/healthz',headers={'X-Forwarded-For':'2.3.4.8'})).status_code==429
    run(s)

def test_concurrent_duplicate_finals(c):
    from concurrent.futures import ThreadPoolExecutor
    before=c.bill.keys['test']['credit_msat']
    with ThreadPoolExecutor(8) as pool:
        assert all(pool.map(lambda _:c.bill.reserve('duplicate','test',8,20),range(20)))
        list(pool.map(lambda _:c.bill.finish('duplicate',2,3),range(20)))
    assert c.bill.keys['test']['credit_msat']==before-5
    assert sum(r['event']=='settle' for r in c.bill.tail())==1

def test_invoice_flow_and_invite(c):
    c.config['signup_invite']='invite-test'
    async def s():
        p,_=clients(c)
        async with p,httpx.AsyncClient(transport=httpx.ASGITransport(app=c.admin),base_url='http://test',headers={'Authorization':'Bearer admin'}) as a:
            assert (await p.post('/v1/keys',json={})).status_code==403
            assert (await p.post('/v1/keys',json={'invite':'wrong'})).status_code==403
            key=(await p.post('/v1/keys',json={'invite':'invite-test'})).json()
            p.headers['Authorization']='Bearer '+key['api_key_plaintext_once']
            assert (await p.get('/v1/credits/balance')).json()['balance_msat']==0
            i=(await p.post('/v1/credits/invoice',json={'amount_msat':10000})).json()['invoice_id']
            assert (await p.get('/v1/credits/invoice/'+i)).json()['state']=='pending'
            for _ in range(2): assert (await a.post('/admin/invoices/'+i+'/mark-paid')).json()['state']=='paid'
            for _ in range(2): assert (await p.get('/v1/credits/invoice/'+i)).json()['state']=='paid'
            assert (await p.get('/v1/credits/balance')).json()['balance_msat']==10000
            i2=(await p.post('/v1/credits/invoice',json={'amount_msat':10000})).json()['invoice_id']
            c.bill.db.execute('UPDATE invoices SET expires_at=0 WHERE invoice_id=?',(i2,))
            assert (await a.post('/admin/invoices/'+i2+'/mark-paid')).json()['state']=='expired'
            assert (await p.get('/v1/credits/balance')).json()['balance_msat']==10000
    run(s)

def test_gateway_token_count_and_mismatch(c):
    async def s():
        p,w=clients(c)
        async with p,w:
            await register(w); task=asyncio.create_task(p.post('/v1/chat/completions',json=body())); await waitqueue(c)
            job=(await w.post('/worker/lease',json={'worker_id':'w'})).json()
            await final(w,job,usage={'prompt_tokens':999,'completion_tokens':999})
            usage=(await task).json()['usage']
            assert usage['prompt_tokens']==2 and usage['completion_tokens']==3
            assert any(r['event']=='token_mismatch' for r in c.bill.tail())
    run(s)


def test_admin_summary(c):
    async def s():
        p,_=clients(c)
        async with p,httpx.AsyncClient(transport=httpx.ASGITransport(app=c.admin),base_url='http://test') as a:
            assert (await a.get('/admin/summary')).status_code==401
            r=await a.get('/admin/summary',headers={'Authorization':'Bearer admin'})
            assert r.status_code==200
            d=r.json()
            assert {'wallet','energy','workers','days','keys','invoices','customer_credit_liability_msat','earned_msat','test_usage_msat','customers'}<=set(d)
            assert (await a.get('/admin')).status_code in (200,503)  # 503 = UI not built
            assert (await p.get('/admin/summary')).status_code==404
    run(s)


def test_battery_mode_serves_until_20(c):
    async def s():
        _,w=clients(c)
        async with w:
            assert (await register(w,on_ac=False,battery_pct=26)).json()['eligible']
            assert (await beat(w,on_ac=False,battery_pct=60)).json()['eligible']
            assert not (await beat(w,on_ac=False,battery_pct=20)).json()['eligible']
    run(s)

def test_reasoning_effort_mapping(c):
    c.config['max_tokens']=100
    b=c.backend_body(dict(body(),max_tokens=100),100)
    assert b['chat_template_kwargs']=={'enable_thinking':True} and b['thinking_budget_tokens']==50  # capped at half of max_tokens
    b=c.backend_body(dict(body(),reasoning_effort='none'),100)
    assert b['chat_template_kwargs']=={'enable_thinking':False} and 'thinking_budget_tokens' not in b and 'reasoning_effort' not in b
    b=c.backend_body(dict(body(),reasoning_effort='low'),2000)
    assert b['thinking_budget_tokens']==256
    async def s():
        p,_=clients(c)
        async with p: assert (await p.post('/v1/chat/completions',json=dict(body(),reasoning_effort='extreme'))).status_code==400
    run(s)

def test_reasoning_hidden_but_billed(c):
    async def s():
        p,w=clients(c)
        async with p,w:
            await register(w); t=asyncio.create_task(p.post('/v1/chat/completions',json=body())); await waitqueue(c)
            j=(await w.post('/worker/lease',json={'worker_id':'w'})).json()
            assert (await w.post('/worker/result',json=dict(worker_id='w',request_id=j['request_id'],lease_id=j['lease_id'],chunk='',reasoning='secret plan'))).json()['accepted']
            await final(w,j,text='answer',usage={'prompt_tokens':2,'completion_tokens':8})
            r=(await t).json(); m=r['choices'][0]['message']
            assert m['content']=='answer' and 'reasoning_content' not in m and r['usage']['completion_tokens']==8
    run(s)

def test_fx_reprice(c, tmp_path):
    from gateway.fx import Fx
    c.bill.dkk_prices=dict(input_dkk_per_million_tokens=0.5,output_dkk_per_million_tokens=2.0,min_charge_dkk_per_request=0.01)
    c.bill.reprice(500000)
    assert c.bill.output_price==400 and c.bill.input_price==100 and c.bill.minimum==2000
    async def bad(): return 5.0
    async def good(): return 555000.0
    fx=Fx({'dkk_per_btc':100000},tmp_path,provider=bad)
    assert not asyncio.run(fx.refresh()) and fx.rate==100000
    fx=Fx({'dkk_per_btc':100000},tmp_path,provider=good)
    assert asyncio.run(fx.refresh()) and fx.rate==555000 and fx.status()['source']=='coinbase'
    assert Fx({'dkk_per_btc':1},tmp_path).rate==555000  # cached on disk


def test_admin_tailnet_guard(c):
    c.config['admin_tailnet_logins']=['lucasfth@github']
    async def s():
        a=httpx.AsyncClient(transport=httpx.ASGITransport(app=c.admin),base_url='http://test')
        async with a:
            assert (await a.get('/admin/summary',headers={'tailscale-user-login':'someone@else'})).status_code==403
            assert (await a.get('/admin/summary',headers={'tailscale-user-login':'lucasfth@github'})).status_code==401  # still needs admin secret
            assert (await a.get('/admin',headers={'tailscale-user-login':'lucasfth@github'})).status_code in (200,503)
            assert (await a.post('/admin/lightning/webhook',headers={'tailscale-user-login':'lucasfth@github'})).status_code==404
    run(s)


def test_summary_excludes_test_money(c):
    c.config['internal_keys']=['test']
    assert c.bill.reserve('r1','test',4,20); c.bill.finish('r1',1,3)
    d=c.bill.summary(0,['test'],'fake')
    assert d['earned_msat']==0 and d['test_usage_msat']>0 and d['customers']==0 and d['keys'][0]['internal']


def test_open_signup_trial_and_limits(c):
    c.config.update(open_signup=True,trial_credit_msat=20000,signup_per_ip_per_day=2,trials_per_day=1)
    async def s():
        p,_=clients(c)
        async with p:
            k1=(await p.post('/v1/keys',json={})).json(); assert k1['trial_credit_msat']==20000
            k2=(await p.post('/v1/keys',json={})).json(); assert 'trial_credit_msat' not in k2  # daily trial budget used
            assert (await p.post('/v1/keys',json={})).status_code==429
            assert c.bill.keys[k1['key_id']]['credit_msat']==20000
            r=await p.get('/'); assert r.status_code==200 and 'Ornith' in r.text and "script-src 'unsafe-inline'" in r.headers['content-security-policy']
            assert "default-src 'none'" in (await p.get('/healthz')).headers['content-security-policy']
        d=c.bill.summary(0,[],'fake'); assert d['earned_msat']==0 and d['trial_users']==1
    run(s)


def test_public_docs(c):
    async def s():
        p,_=clients(c)
        async with p:
            for n in ('robots.txt','llms.txt','openapi.json'):
                r=await p.get('/'+n); assert r.status_code==200, n
            assert c.config['model'] in (await p.get('/openapi.json')).json()['paths']['/v1/chat/completions']['post']['requestBody']['content']['application/json']['schema']['properties']['model']['const']
            assert (await p.get('/docs')).status_code==404
    run(s)


def test_bulk_lane_yields_to_paid(c):
    """Own bulk work: lower priority than paid, never takes a worker's last slot."""
    c.config.update(bulk_keys=['test'],bulk_slots_per_worker=1,bulk_queue_depth=4)
    async def s():
        p,w=clients(c)
        async with p,w:
            await register(w,capacity=1)
            t=asyncio.create_task(p.post('/v1/chat/completions',json=body()))
            for _ in range(200):
                if c.pending(c.bulk): break
                await asyncio.sleep(.001)
            assert c.pending(c.bulk)==1 and c.pending()==0
            # capacity 1: bulk may not take the only slot
            assert (await w.post('/worker/lease',json={'worker_id':'w'})).json().get('job',1) is None
            await beat(w,capacity=2)
            job=(await w.post('/worker/lease',json={'worker_id':'w'})).json(); assert c.jobs[job['request_id']]['bulk']
            await final(w,job); r=await t; assert r.status_code==200
    run(s)


def test_heartbeat_metrics_and_mac_info(c):
    async def s():
        c.machines.update('w',{'meta':{'owner':'Test','host':'t.local','user':'u'}})
        p,w=clients(c)
        async with p,w,httpx.AsyncClient(transport=httpx.ASGITransport(app=c.admin),base_url='http://test') as a:
            assert (await register(w,metrics={"mem_pressure_pct":40,'cpu_pct':12.5,'gpu_pct':None})).status_code==200
            assert (await beat(w,metrics={'x':'bad'})).status_code==400
            assert (await beat(w,metrics=[1])).status_code==400
            d=(await a.get('/admin/summary',headers={'Authorization':'Bearer admin'})).json()
            x=[x for x in d['workers'] if x['worker_id']=='w'][0]
            assert x['metrics']['mem_pressure_pct']==40 and x['mac']['host']=='t.local'
    run(s)


H=lambda s:hashlib.sha256(s.encode()).hexdigest()
PUB='ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIM+qPbR64P1oIeDurt040Y6ZXJRC2DTnqD2RHVAsq2wu'
PUB2='ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEbdGkLkLVGxaPpCchQ65W1S1NJrbVxfk74UyyAFa06L'

def admin_client(c): return httpx.AsyncClient(transport=httpx.ASGITransport(app=c.admin),base_url='http://test',headers={'Authorization':'Bearer admin'})

def test_machine_registry_crud(c):
    async def s():
        _,w=clients(c)
        async with w,admin_client(c) as a:
            assert (await httpx.AsyncClient(transport=httpx.ASGITransport(app=c.admin),base_url='http://test').get('/admin/machines')).status_code==401
            assert [m['worker_id'] for m in (await a.get('/admin/machines')).json()]==['w']  # migrated from worker_secrets.json
            for bad in [dict(worker_id='Bad!',secret_sha256=H('x')),dict(worker_id='m2',secret_sha256='zz'),dict(worker_id='m2',secret_sha256=H('x'),tunnel_pubkey='ssh-rsa AAAA'),
                        dict(worker_id='m2',secret_sha256=H('x'),meta={'host':'a;rm -rf /'}),dict(worker_id='m2',secret_sha256=H('x'),meta={'evil':'1'}),dict(worker_id='m2',secret_sha256=H('worker'))]:
                assert (await a.post('/admin/machines',json=bad)).status_code==400,bad
            r=await a.post('/admin/machines',json=dict(worker_id='m2',secret_sha256=H('s2'),tunnel_pubkey=PUB+' comment',meta={'owner':'Anna','host':'mac-example.local','ip':'192.0.2.9','user':'example'}))
            assert r.status_code==201 and r.json()['tunnel_key_fingerprint'].startswith('SHA256:') and r.json()['meta']['owner']=='Anna'
            assert (await a.post('/admin/machines',json=dict(worker_id='m2',secret_sha256=H('s3')))).status_code==409
            # the new machine can connect without a restart
            hw={'Authorization':'Bearer s2'}
            assert (await w.post('/worker/register',json={'worker_id':'m2'},headers=hw)).status_code==200
            rows={x['worker_id']:x for x in (await a.get('/admin/summary')).json()['workers']}
            assert rows['m2']['online'] and rows['w']['online'] is False and rows['w']['battery_pct'] is None
            # disable kicks it out and blocks reconnect
            assert (await a.patch('/admin/machines/m2',json={'enabled':False})).json()['enabled'] is False
            assert 'm2' not in c.workers
            assert (await w.post('/worker/register',json={'worker_id':'m2'},headers=hw)).status_code==401
            assert (await a.patch('/admin/machines/m2',json={'enabled':True,'meta':{'owner':None,'agent':'Max'}})).json()['meta']=={'host':'mac-example.local','ip':'192.0.2.9','user':'example','agent':'Max'}
            assert (await a.delete('/admin/machines/m2')).json()=={'deleted':'m2'}
            assert (await a.delete('/admin/machines/m2')).status_code==404
            assert (await w.post('/worker/register',json={'worker_id':'m2'},headers=hw)).status_code==401
    run(s)
    import stat
    f=c.root/'data'/'machines.json'
    assert stat.S_IMODE(f.stat().st_mode)==0o600 and 's2' not in f.read_text()
    from gateway.app import Coordinator
    assert [m['worker_id'] for m in Coordinator(c.root).machines.list()]==['w']  # persisted

def test_tunnel_key_sync(tmp_path):
    import importlib.util, subprocess, sys
    m={'a':dict(secret_sha256=H('a'),enabled=True,tunnel_pubkey=PUB),'b':dict(secret_sha256=H('b'),enabled=False,tunnel_pubkey=PUB),'c':dict(secret_sha256=H('c'),enabled=True,tunnel_pubkey='ssh-ed25519 bad')}
    (tmp_path/'machines.json').write_text(json.dumps(m))
    ak=tmp_path/'authorized_keys'; ak.write_text('ssh-ed25519 AAAAuser me@laptop\nrestrict,permitopen="127.0.0.1:8701" ssh-ed25519 OLD nocturne-tunnel-a\nrestrict,permitopen="127.0.0.1:8701" '+PUB+' nocturne-tunnel-b\nrestrict,permitopen="127.0.0.1:8701" '+PUB2+' nocturne-tunnel-other\n')
    tool=c_root=__import__('pathlib').Path(__file__).parent.parent/'tools'/'sync-tunnel-keys.py'
    for _ in range(2): subprocess.run([sys.executable,str(tool),'--machines',str(tmp_path/'machines.json'),'--authorized-keys',str(ak)],check=True,capture_output=True)
    t=ak.read_text().splitlines()
    assert t[0]=='ssh-ed25519 AAAAuser me@laptop' and not any('OLD' in x for x in t)
    managed=[x for x in t if x.endswith('nocturne-tunnel-a')]
    assert len(managed)==1 and managed[0].startswith('restrict,port-forwarding,permitopen="127.0.0.1:8701",permitlisten="none",command=')
    assert not any('tunnel-b' in x or 'tunnel-c' in x for x in t)  # disabled b is revoked even though it was a legacy line
    assert any(x.endswith('nocturne-tunnel-other') for x in t)  # not in registry: untouched
    assert 'permitlisten="none"' in managed[0]

def test_migration_imports_tunnel_keys_and_refuses_bad_ids(tmp_path,monkeypatch):
    from gateway.machines import Machines, legacy_tunnel_keys
    ak=tmp_path/'ak'; ak.write_text('restrict,permitopen="127.0.0.1:8701" '+PUB+' nocturne-tunnel-mac-a\n')
    m=Machines(tmp_path,{'mac-a':H('a')},{'mac-a':{'owner':'A','ip':'fe80::1%eth0'}},legacy_tunnel_keys(ak))
    assert m.items['mac-a']['tunnel_pubkey']==PUB and m.items['mac-a']['meta']=={}  # bad meta dropped, machine kept
    with pytest.raises(SystemExit): Machines(tmp_path/'x',{'Bad_ID':H('b')})
    assert not (tmp_path/'x'/'machines.json').exists()

def test_lease_poll_rechecks_auth(c):
    async def s():
        _,w=clients(c)
        async with w,admin_client(c) as a:
            await register(w)
            t=asyncio.create_task(w.post('/worker/lease',json={'worker_id':'w'}))
            await asyncio.sleep(.05)
            assert (await a.patch('/admin/machines/w',json={'secret_sha256':H('new')})).status_code==200
            r=await t
            assert r.status_code in (200,401) and r.json().get('request_id') is None
    run(s)

def test_meta_rejects_newline_and_scope(c):
    from gateway.machines import clean_meta
    for bad in [{'user':'anna\n'},{'host':'a.local\n'},{'ip':'fe80::1%$(id)'}]:
        with pytest.raises(ValueError): clean_meta(bad)
