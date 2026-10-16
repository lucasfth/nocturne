import asyncio
import hashlib
import hmac
import json
import time
import httpx
import pytest
from test_gateway import c
from gateway.lightning_phoenixd import Phoenixd

HASH = 'ab' * 32

@pytest.fixture
def phoenix(c, tmp_path, monkeypatch):
    conf=tmp_path/'phoenix.conf'
    conf.write_text('http-password=test-full\nhttp-password-limited-access=test-read\nwebhook-secret=test-signing\n'); conf.chmod(0o600)
    cfg=dict(conf_file=str(conf), min_msat=1000,max_msat=100000000,expiry_s=3600)
    p=Phoenixd(cfg,c.bill); c.lightning=p; c.config['lightning']=cfg
    replies=[]; calls=[]
    def handle(request):
        calls.append(request)
        status, body=replies.pop(0)
        return httpx.Response(status,json=body) if body is not None else httpx.Response(status)
    transport=httpx.MockTransport(handle)
    monkeypatch.setattr(p,'client',lambda:httpx.AsyncClient(base_url=p.base_url,auth=p.auth,transport=transport))
    return c,p,replies,calls

def insert(c):
    c.bill.add_invoice('test',10000,dict(invoice_id='invoice-test',payment_hash=HASH,bolt11='lnbc-test',expires_at=time.time()+3600),'phoenixd')

def paid(): return dict(paymentHash=HASH,externalId='invoice-test',requestedSat=10,isPaid=True,isExpired=False)

def event(): return json.dumps(dict(type='payment_received',externalId='invoice-test',paymentHash=HASH,amountSat=10)).encode()

def signature(body): return hmac.new(b'test-signing',body,hashlib.sha256).hexdigest()

def test_create(phoenix):
    c,p,replies,calls=phoenix
    replies.append((200,dict(paymentHash=HASH,serialized='lnbc10-test')))
    result=asyncio.run(p.create_invoice(10000,'memo'))
    assert result['payment_hash']==HASH and result['bolt11']=='lnbc10-test'
    assert result['expires_at']>time.time()+3500
    from urllib.parse import parse_qs
    data=parse_qs(calls[0].content.decode())
    assert data['amountSat']==['10'] and data['externalId']==[result['invoice_id']]
    assert data['expirySeconds']==['3600'] and calls[0].url.path=='/createinvoice'
    assert calls[0].headers['authorization']=='Basic '+__import__('base64').b64encode(b':test-read').decode()

@pytest.mark.parametrize('amount',[0,999,1001,-1000,100001000,True])
def test_bounds(phoenix,amount):
    _,p,replies,calls=phoenix
    with pytest.raises(ValueError): asyncio.run(p.create_invoice(amount,'memo'))
    assert not calls

def test_webhook_paid_duplicate(phoenix):
    c,p,replies,calls=phoenix; insert(c); before=c.bill.keys['test']['credit_msat']
    replies.append((200,paid()))
    async def scenario():
        async with httpx.AsyncClient(transport=httpx.ASGITransport(app=c.admin,client=('127.0.0.1',1)),base_url='http://test') as a:
            for expected in (True,False):
                r=await a.post('/admin/lightning/webhook',content=event(),headers={'X-Phoenix-Signature':signature(event())})
                assert r.status_code==200 and r.json()['credited']==expected
        async with httpx.AsyncClient(transport=httpx.ASGITransport(app=c.public,client=('127.0.0.1',1)),base_url='http://test') as a:
            assert (await a.post('/admin/lightning/webhook',content=event())).status_code==404
    asyncio.run(scenario())
    assert len(calls)==1 and c.bill.keys['test']['credit_msat']==before+10000
    assert sum(r['event']=='invoice_paid' for r in c.bill.tail())==1

def test_webhook_invalid_and_remote(phoenix):
    c,p,replies,calls=phoenix; insert(c)
    async def scenario():
        for host,status in [('127.0.0.1',401),('8.8.8.8',403)]:
            async with httpx.AsyncClient(transport=httpx.ASGITransport(app=c.admin,client=(host,1)),base_url='http://test') as a:
                assert (await a.post('/admin/lightning/webhook',content=event())).status_code==status
    asyncio.run(scenario()); assert not calls

def test_poller_paid(phoenix):
    c,p,replies,calls=phoenix; insert(c); replies.append((200,paid()))
    async def scenario():
        task=asyncio.create_task(c.invoice_maintenance())
        for _ in range(100):
            if c.bill.invoice('invoice-test')['state']=='paid': break
            await asyncio.sleep(.001)
        task.cancel(); await asyncio.gather(task,return_exceptions=True)
        assert c.bill.invoice('invoice-test')['state']=='paid'
    asyncio.run(scenario()); assert calls[0].url.path=='/payments/incoming/'+HASH

def test_pending_no_content(phoenix):
    c,p,replies,calls=phoenix; insert(c); replies.append((204,None))
    assert asyncio.run(c.poll_invoice('invoice-test'))['state']=='pending'

def test_lookup_metadata_mismatch(phoenix):
    c,p,replies,calls=phoenix; insert(c); replies.append((200,{**paid(),'requestedSat':1}))
    with pytest.raises(ValueError): asyncio.run(c.poll_invoice('invoice-test'))
    assert c.bill.invoice('invoice-test')['state']=='pending'

def test_expired_does_not_credit(phoenix):
    c,p,replies,calls=phoenix; insert(c)
    c.bill.db.execute("UPDATE invoices SET expires_at=0 WHERE invoice_id='invoice-test'")
    before=c.bill.keys['test']['credit_msat']; replies.append((200,paid()))
    assert not asyncio.run(p.webhook(event(),signature(event())))
    assert c.bill.keys['test']['credit_msat']==before
