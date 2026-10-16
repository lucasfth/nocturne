"""Loopback phoenixd invoice adapter with signed, rechecked payment notifications."""
import hashlib
import hmac
import json
import re
import stat
import time
import uuid
from pathlib import Path
import httpx


class Phoenixd:
    name = 'phoenixd'

    def __init__(self, config, bill):
        self.config, self.bill = config, bill
        self.base_url = config.get('base_url', 'http://127.0.0.1:9740').rstrip('/')
        if self.base_url != 'http://127.0.0.1:9740':
            raise ValueError('phoenixd must use the private loopback endpoint')
        path = Path(config.get('conf_file', '~/.phoenix/phoenix.conf')).expanduser()
        if stat.S_IMODE(path.stat().st_mode) != 0o600:
            raise ValueError('phoenix.conf must have mode 0600')
        conf = dict(line.strip().split('=', 1) for line in path.read_text().splitlines()
                    if '=' in line and not line.lstrip().startswith('#'))
        # Prefer daemon-generated invoice/read-only credential over the spend credential.
        self.auth = ('', conf.get('http-password-limited-access', conf['http-password']))
        self.webhook_secret = conf['webhook-secret'].encode()

    def client(self):
        return httpx.AsyncClient(base_url=self.base_url, auth=self.auth, timeout=15,
                                 trust_env=False)

    async def create_invoice(self, amount_msat, memo, expiry=None):
        if (type(amount_msat) != int or amount_msat % 1000 or
                not self.config.get('min_msat', 1000) <= amount_msat <= self.config.get('max_msat', 100000000)):
            raise ValueError('invoice amount outside whole-satoshi limits')
        duration = self.config.get('expiry_s', 3600) if expiry is None else expiry
        if type(duration) != int or duration <= 0:
            raise ValueError('invalid expiry')
        invoice_id = uuid.uuid4().hex
        created = time.time()
        async with self.client() as c:
            r = await c.post('/createinvoice', data={'amountSat': amount_msat // 1000,
                'description': memo[:128], 'externalId': invoice_id, 'expirySeconds': duration})
            r.raise_for_status()
            d = r.json()
        if not re.fullmatch(r'[0-9a-f]{64}', d['paymentHash']) or not d['serialized'].startswith('lnbc'):
            raise ValueError('invalid mainnet invoice response')
        return dict(invoice_id=invoice_id, payment_hash=d['paymentHash'],
                    bolt11=d['serialized'], expires_at=created + duration)

    async def wallet(self):
        """Read-only wallet view (limited-access credential cannot spend)."""
        async with self.client() as c:
            b = (await c.get('/getbalance')).json()
            i = (await c.get('/getinfo')).json()
        chans = i.get('channels', []) or []
        return dict(balance_sat=b.get('balanceSat', 0), fee_credit_sat=b.get('feeCreditSat', 0),
                    channels=len(chans), inbound_liquidity_sat=sum(ch.get('inboundLiquiditySat', 0) for ch in chans),
                    node_id=i.get('nodeId'))

    async def check_invoice(self, invoice_id):
        row = self.bill.invoice(invoice_id)
        if not row or row['backend'] != self.name:
            return 'pending'
        return await self.check_paid(row['payment_hash'], row)

    async def check_paid(self, payment_hash, row=None):
        if not re.fullmatch(r'[0-9a-f]{64}', payment_hash):
            raise ValueError('invalid payment hash')
        async with self.client() as c:
            r = await c.get('/payments/incoming/' + payment_hash)
            if r.status_code == 204:
                return 'pending'
            r.raise_for_status()
            d = r.json()
        if d.get('paymentHash') != payment_hash:
            raise ValueError('payment hash mismatch')
        if row and (d.get('externalId') != row['invoice_id'] or
                    d.get('requestedSat') != row['amount_msat'] // 1000):
            raise ValueError('invoice metadata mismatch')
        return 'paid' if d.get('isPaid') is True else 'expired' if d.get('isExpired') is True else 'pending'

    async def webhook(self, body, signature):
        expected = hmac.new(self.webhook_secret, body, hashlib.sha256).hexdigest()
        if not hmac.compare_digest(expected, signature):
            raise PermissionError('invalid webhook signature')
        event = json.loads(body)
        if not isinstance(event, dict):
            raise ValueError('invalid event')
        if event.get('type') != 'payment_received':
            return False
        row = self.bill.invoice(event.get('externalId', ''))
        if not row or row['backend'] != self.name or row['state'] != 'pending':
            return False
        if event.get('paymentHash') != row['payment_hash']:
            raise ValueError('payment hash mismatch')
        # Never trust notification amount alone; lookup the exact stored invoice.
        state = await self.check_paid(row['payment_hash'], row)
        self.bill.invoice_state(row['invoice_id'], state)
        return self.bill.invoice(row['invoice_id'])['state'] == 'paid'
