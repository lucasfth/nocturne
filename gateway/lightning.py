"""Invoice-only Lightning adapters. No withdrawal credentials are required."""
import os
import stat
import time
import uuid
from pathlib import Path
import httpx


def private_secret(path):
    p=Path(path)
    if stat.S_IMODE(p.stat().st_mode)!=0o600: raise ValueError('Lightning secret file must have mode 0600')
    return p.read_text().strip()


class Fake:
    name='fake'
    def __init__(self,config,bill): self.config=config; self.bill=bill
    async def create_invoice(self,amount_msat,memo):
        i=uuid.uuid4().hex
        return dict(invoice_id=i,bolt11='fake-not-payable-'+i,payment_hash=i,expires_at=time.time()+self.config.get('expiry_s',3600))
    async def check_invoice(self,i):
        r=self.bill.invoice(i)
        return 'expired' if not r or r['expires_at']<=time.time() else r['state']
    async def mark_paid(self,i): self.bill.invoice_state(i,'paid')


class LNbits:
    name='lnbits'
    def __init__(self,config,bill):
        self.config=config
        if not config['base_url'].startswith('https://'): raise ValueError('LNbits requires HTTPS')
        self.headers={'X-Api-Key':private_secret(config['invoice_key_file'])}
    async def create_invoice(self,amount_msat,memo):
        if amount_msat%1000: raise ValueError('LNbits invoices require whole satoshis')
        async with httpx.AsyncClient(timeout=15) as c:
            r=await c.post(self.config['base_url'].rstrip('/')+'/api/v1/payments',headers=self.headers,json={'out':False,'amount':amount_msat//1000,'memo':memo,'expiry':self.config.get('expiry_s',3600)})
            r.raise_for_status(); d=r.json()
        return dict(invoice_id=d['payment_hash'],payment_hash=d['payment_hash'],bolt11=d['payment_request'],expires_at=time.time()+self.config.get('expiry_s',3600))
    async def check_invoice(self,i):
        async with httpx.AsyncClient(timeout=15) as c:
            r=await c.get(self.config['base_url'].rstrip('/')+'/api/v1/payments/'+i,headers=self.headers); r.raise_for_status()
            return 'paid' if r.json().get('paid') else 'pending'


class LND:
    name='lnd'
    def __init__(self,config,bill):
        self.config=config
        if not config['base_url'].startswith('https://'): raise ValueError('LND requires HTTPS')
        self.headers={'Grpc-Metadata-macaroon':private_secret(config['macaroon_file'])}
    def client(self): return httpx.AsyncClient(timeout=15,verify=self.config['tls_cert_path'])
    async def create_invoice(self,amount_msat,memo):
        import base64
        async with self.client() as c:
            r=await c.post(self.config['base_url'].rstrip('/')+'/v1/invoices',headers=self.headers,json={'value_msat':str(amount_msat),'memo':memo,'expiry':str(self.config.get('expiry_s',3600))}); r.raise_for_status(); d=r.json()
        h=base64.b64decode(d['r_hash']).hex()
        return dict(invoice_id=h,payment_hash=h,bolt11=d['payment_request'],expires_at=time.time()+self.config.get('expiry_s',3600))
    async def check_invoice(self,i):
        async with self.client() as c:
            r=await c.get(self.config['base_url'].rstrip('/')+'/v1/invoice/'+i,headers=self.headers); r.raise_for_status(); d=r.json()
        return 'paid' if d.get('settled') or d.get('state')=='SETTLED' else 'expired' if d.get('state')=='CANCELED' else 'pending'


def backend(config,bill):
    from .lightning_phoenixd import Phoenixd
    return {'fake':Fake,'lnbits':LNbits,'lnd':LND,'phoenixd':Phoenixd}[config.get('backend','fake')](config,bill)
