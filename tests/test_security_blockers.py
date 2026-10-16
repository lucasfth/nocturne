import asyncio
import hashlib
import json
import os
import stat
import time

import httpx
import pytest

from test_gateway import c, clients, run
from gateway.machines import RegistryUnavailable


@pytest.mark.parametrize('operation', ['disable', 'delete', 'rotate'])
def test_registry_postrename_failure_fails_closed(c, monkeypatch, operation):
    original = os.fsync
    def fail_directory(fd):
        if stat.S_ISDIR(os.fstat(fd).st_mode): raise OSError('sensitive filesystem details')
        original(fd)
    monkeypatch.setattr(os, 'fsync', fail_directory)
    async def scenario():
        _, worker = clients(c)
        async with worker, httpx.AsyncClient(transport=httpx.ASGITransport(app=c.admin),base_url='http://test',headers={'Authorization':'Bearer admin'}) as admin:
            await worker.post('/worker/register',json={'worker_id':'w'})
            lease = asyncio.create_task(worker.post('/worker/lease',json={'worker_id':'w'}))
            await asyncio.sleep(.02)
            if operation == 'delete': response = await admin.delete('/admin/machines/w')
            else:
                changes = {'enabled':False} if operation == 'disable' else {'secret_sha256':hashlib.sha256(b'new').hexdigest()}
                response = await admin.patch('/admin/machines/w',json=changes)
            assert response.status_code == 503
            assert 'sensitive' not in response.text
            assert (await asyncio.wait_for(lease,1)).status_code == 503
            assert (await worker.post('/worker/register',json={'worker_id':'w'})).status_code == 503
            assert (await admin.patch('/admin/machines/w',json={'enabled':True})).status_code == 503
            assert not c.machines.enabled('w')
            with pytest.raises(RegistryUnavailable): c.machines.secret('w')
    run(scenario)


def test_registry_prerename_failure_retains_old_auth(c, monkeypatch):
    def fail(*args): raise OSError('private details')
    monkeypatch.setattr(os,'replace',fail)
    with pytest.raises(RegistryUnavailable): c.machines.update('w',{'enabled':False})
    assert not c.machines.uncertain
    assert c.machines.secret('w') == hashlib.sha256(b'worker').hexdigest()


def test_concurrent_invoice_reservations(c):
    async def scenario():
        entered = 0
        gate = asyncio.Event()
        original = c.lightning.create_invoice
        async def blocked(*args):
            nonlocal entered
            entered += 1
            await gate.wait()
            return await original(*args)
        c.lightning.create_invoice = blocked
        public,_ = clients(c)
        async with public:
            tasks = [asyncio.create_task(public.post('/v1/credits/invoice',json={'amount_msat':1000})) for _ in range(8)]
            for _ in range(100):
                if sum(t.done() for t in tasks)==3: break
                await asyncio.sleep(.001)
            assert entered == 5
            assert c.invoice_admissions == {'test':5}
            gate.set()
            statuses = [r.status_code for r in await asyncio.gather(*tasks)]
            assert statuses.count(200)==5 and statuses.count(429)==3
            assert len(c.bill.pending_invoices())==5
            assert not c.invoice_admissions
    run(scenario)


def test_invoice_reservation_released_on_failure_and_cancel(c):
    async def scenario():
        gate=asyncio.Event()
        async def blocked(*args): await gate.wait(); raise RuntimeError('backend secret')
        c.lightning.create_invoice=blocked
        public,_=clients(c)
        async with public:
            task=asyncio.create_task(public.post('/v1/credits/invoice',json={'amount_msat':1000}))
            for _ in range(100):
                if c.invoice_admissions: break
                await asyncio.sleep(.001)
            task.cancel()
            with pytest.raises(asyncio.CancelledError): await task
            assert not c.invoice_admissions
            gate.set()
            response=await public.post('/v1/credits/invoice',json={'amount_msat':1000})
            assert response.status_code==502 and 'secret' not in response.text
            assert not c.invoice_admissions
    run(scenario)


@pytest.mark.parametrize('worker', [True,False])
def test_private_ingress_chunked_and_stalled(c,worker):
    async def scenario():
        c.config['body_idle_timeout_s']=.03
        c.config['body_read_timeout_s']=.08
        app=c.worker if worker else c.admin
        path='/worker/lease' if worker else '/admin/config'
        auth='worker' if worker else 'admin'
        async with httpx.AsyncClient(transport=httpx.ASGITransport(app=app),base_url='http://test',headers={'Authorization':'Bearer '+auth,'Content-Type':'application/json'}) as client:
            async def chunks():
                yield b'x'*3000
                yield b'x'*3000
            r=await client.request('POST' if worker else 'PATCH',path,content=chunks())
            assert r.status_code==413
            async def stalled():
                yield b'{'
                await asyncio.sleep(10)
            t=asyncio.create_task(client.request('POST' if worker else 'PATCH',path,content=stalled()))
            public,_=clients(c)
            async with public:
                assert (await asyncio.wait_for(public.get('/healthz'),.5)).status_code==200
            assert (await asyncio.wait_for(t,.5)).status_code==408
    run(scenario)


def test_worker_missing_auth_does_not_read_body(c):
    async def scenario():
        async def unread(): raise AssertionError('body read before bearer shape'); yield b''
        async with httpx.AsyncClient(transport=httpx.ASGITransport(app=c.worker),base_url='http://test') as client:
            assert (await client.post('/worker/register',content=unread())).status_code==401
            assert (await client.post('/worker/register',json={'worker_id':['w']},headers={'Authorization':'Bearer worker'})).status_code==401
    run(scenario)


def test_admin_cross_origin_credit_rejected(c):
    async def scenario():
        async with httpx.AsyncClient(transport=httpx.ASGITransport(app=c.admin),base_url='http://test',headers={'Authorization':'Bearer admin'}) as client:
            response=await client.post('/admin/credit',content='{}',headers={'Origin':'https://other','Content-Type':'text/plain'})
            assert response.status_code==403
            assert (await client.post('/admin/credit',content='{}',headers={'Content-Type':'text/plain'})).status_code==415
    run(scenario)
