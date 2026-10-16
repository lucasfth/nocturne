import asyncio
from datetime import datetime, timezone, timedelta
import httpx
import pytest
from gateway.energy import Energy
from test_gateway import c, clients, body, run, register

CFG = dict(schedule=dict(tz='Europe/Copenhagen',start='17:30',end='08:00',grace_min=20),energy=dict(price_area='DK2',refresh_min=30,watts_per_worker=30,tariff_dkk_per_kwh=1.2,tiers=dict(cheap_max_dkk_kwh=1,normal_max_dkk_kwh=2.5)),dkk_per_btc=100000)

def energy(c, hour=19, price=500):
    c.config.update(CFG)
    now=datetime(2026,10,10,hour,0,tzinfo=timezone.utc)
    async def fake(area): return [dict(time=now.isoformat(),spot_dkk_mwh=price,dataset='Elspotprices')]
    c.energy=Energy(c.config,provider=fake,clock=lambda:now)
    return now


def test_closed_503_retry_and_heartbeat(c):
    energy(c,10)
    async def s():
        p,w=clients(c)
        async with p,w:
            r=await p.post('/v1/chat/completions',json=body())
            assert r.status_code==503 and int(r.headers['retry-after'])==19800
            assert not (await register(w)).json()['serve']
            assert not (await p.get('/v1/status')).json()['window_open']
    run(s)


def test_expensive_refusal(c):
    energy(c,19,1000000)
    async def s():
        p,_=clients(c)
        async with p:
            r=await p.post('/v1/chat/completions',json=body())
            assert r.status_code==503 and r.headers['x-reason']=='energy-price'
    run(s)


def test_normal_cap(c):
    energy(c)
    import time
    for i in range(5): c.workers[str(i)]=dict(on_ac=True,battery_pct=90,thermal_state='nominal',user_active=False,seen=time.monotonic(),ready=True)
    async def s():
        await c.energy.refresh()
        status, serving=c.energy.state(c.workers)
        assert status['tier']=='normal' and status['serving_workers']==5 and len(serving)==5
    run(s)


def test_provider_failure_fallback(c):
    now=energy(c,19,2000)
    async def fail(area): raise RuntimeError('offline')
    async def s():
        await c.energy.refresh()
        assert c.energy.state({})[0]['tier']=='expensive'
        c.energy.provider=fail
        c.energy.clock=lambda:now+timedelta(hours=5)
        await c.energy.refresh()
        assert c.energy.state({})[0]['dkk_per_kwh']==3.2
        c.energy.clock=lambda:now+timedelta(hours=7)
        await c.energy.refresh()
        status,_=c.energy.state({})
        assert status['tier']=='normal' and status['dkk_per_kwh'] is None
    run(s)


def test_minimum_reserve_settle_cancel(c):
    c.bill.input_price=500; c.bill.output_price=2000; c.bill.minimum=10000
    c.bill.credit('test',20000)
    before=c.bill.keys['test']['credit_msat']
    assert c.bill.reserve('floor','test',4,1)
    assert c.bill.keys['test']['credit_msat']==before-10000
    c.bill.finish('floor',1,1)
    assert c.bill.keys['test']['credit_msat']==before-10000
    assert c.bill.reserve('cancel-floor','test',4,1)
    c.bill.finish('cancel-floor',cancel=True)
    assert c.bill.keys['test']['credit_msat']==before-10000
    assert c.bill.quote(40000,10000)==25000


def test_http_neutral_and_pricing(c):
    async def s():
        p,_=clients(c)
        async with p:
            for path in ['/healthz','/v1/models','/v1/status','/v1/pricing','/admin/workers','/v1/credits/balance']:
                r=await p.get(path)
                assert 'eco'+'ray' not in (r.text+str(dict(r.headers))).lower()
                assert 'server' not in r.headers
    run(s)


def test_window_boundary_and_grace(c):
    energy(c,6)
    assert not c.energy.window()[0]  # 08:00 local, exclusive end
    energy(c,15)
    assert not c.energy.window()[0]  # 17:00 local
    c.energy.clock=lambda:datetime(2026,10,10,15,30,tzinfo=timezone.utc)
    assert c.energy.window()[0]
    energy(c,23)
    assert c.energy.window()[0]


def test_bulk_skips_reserve_margin_below_ceiling(c):
    energy(c,19,2200)  # 2.2 spot + 1.2 tariff = 3.4 DKK/kWh, expensive
    c.config.update(bulk_keys=['test'])
    async def s():
        await c.energy.refresh()
        assert c.energy.state({})[0]['tier']=='expensive'
        c.energy.admit({},1,1000,4096,bulk=True)  # tiny revenue, still admitted
        import pytest as _p
        from fastapi import HTTPException
        with _p.raises(HTTPException): c.energy.admit({},1,1000,4096,bulk=False)
        c.config['energy']['bulk_max_dkk_kwh']=3.0
        with _p.raises(HTTPException) as e: c.energy.admit({},1,1000,4096,bulk=True)
        assert e.value.headers['X-Reason']=='energy-price'
    run(s)


def test_paid_admitted_at_evening_price_refused_on_spike(c):
    # Real prices: 0.5/2.0 DKK per 1M in/out at 100000 DKK/BTC config rate.
    msat=lambda i,o: round((i*0.5+o*2.0)/1e6*1e11/100000)
    energy(c,19,2200)  # 3.4 DKK/kWh
    async def s():
        await c.energy.refresh()
        c.energy.admit({},msat(8000,4096),8000,4096)  # long prompt still OK
        energy(c,19,6000)  # 7.2 DKK/kWh spike
        await c.energy.refresh()
        from fastapi import HTTPException
        import pytest as _p
        with _p.raises(HTTPException): c.energy.admit({},msat(8000,4096),8000,4096)
    run(s)
