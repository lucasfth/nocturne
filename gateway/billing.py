import hashlib
import json
import math
import secrets
import sqlite3
import threading
import time
from contextlib import contextmanager
from pathlib import Path


class Billing:
    def __init__(self, data):
        self.data = Path(data)
        prices = json.loads((self.data / 'prices.json').read_text())
        self.input_price = prices.get('input_msat_per_1k_tokens', prices.get('price_per_token_msat', 1)*1000)
        self.output_price = prices.get('output_msat_per_1k_tokens', prices.get('price_per_token_msat', 1)*1000)
        self.minimum = prices.get('min_charge_msat_per_request', 0)
        # Optional DKK-denominated prices; converted to msat with the live BTC rate via reprice().
        self.dkk_prices = {k: prices[k] for k in ('input_dkk_per_million_tokens','output_dkk_per_million_tokens','min_charge_dkk_per_request') if k in prices}
        self.default_max_concurrent = 4
        self.lock = threading.RLock()
        self.db = sqlite3.connect(self.data / 'ledger.sqlite', check_same_thread=False, isolation_level=None)
        (self.data / 'ledger.sqlite').chmod(0o600)
        self.db.row_factory = sqlite3.Row
        self.db.execute('PRAGMA journal_mode=WAL')
        self.db.execute('PRAGMA synchronous=FULL')
        self.db.execute('PRAGMA foreign_keys=ON')
        self.db.executescript('''
        CREATE TABLE IF NOT EXISTS keys(key_id TEXT PRIMARY KEY, secret_sha256 TEXT UNIQUE NOT NULL, balance_msat INTEGER NOT NULL CHECK(balance_msat>=0), rate_limit INTEGER NOT NULL, max_concurrent INTEGER NOT NULL, allow_thinking INTEGER NOT NULL, created REAL NOT NULL);
        CREATE TABLE IF NOT EXISTS reservations(request_id TEXT PRIMARY KEY, key_id TEXT REFERENCES keys(key_id), reserved_msat INTEGER NOT NULL, settled_msat INTEGER, state TEXT NOT NULL, created REAL NOT NULL, updated REAL NOT NULL);
        CREATE TABLE IF NOT EXISTS ledger(id INTEGER PRIMARY KEY, ts REAL NOT NULL, event TEXT NOT NULL, request_id TEXT, key_id TEXT, amount_msat INTEGER NOT NULL, meta_json TEXT NOT NULL);
        CREATE TABLE IF NOT EXISTS invoices(invoice_id TEXT PRIMARY KEY, key_id TEXT REFERENCES keys(key_id), amount_msat INTEGER NOT NULL, bolt11 TEXT NOT NULL, payment_hash TEXT NOT NULL, state TEXT NOT NULL, created REAL NOT NULL, paid_at REAL, backend TEXT NOT NULL, expires_at REAL NOT NULL);
        ''')
        p = self.data / 'api_keys.json'
        if p.exists():
            with self.transaction():
                for k, v in json.loads(p.read_text()).items():
                    self.db.execute('INSERT OR IGNORE INTO keys VALUES(?,?,?,?,?,?,?)', (k,v['hashed_secret'],v['credit_msat'],v['rate_limit_per_minute'],v['max_concurrent'],int(v.get('allow_thinking',False)),time.time()))
                # Recover legacy outstanding reservations before retiring its ledger.
                if not self.db.execute('SELECT 1 FROM ledger LIMIT 1').fetchone():
                    old = self.data / 'ledger.jsonl'
                    pending = {}
                    if old.exists():
                        for line in old.read_text().splitlines():
                            r = json.loads(line)
                            if r['event']=='reserve': pending[r['request_id']]=r
                            else: pending.pop(r['request_id'],None)
                    for rid,r in pending.items():
                        self.db.execute('UPDATE keys SET balance_msat=balance_msat+? WHERE key_id=?',(r['amount_msat'],r['key_id']))
                        self._log('legacy_release',rid,r['key_id'],r['amount_msat'])
            p.chmod(0o400)
            p.rename(self.data / 'api_keys.json.migrated')
        # Jobs cannot survive a coordinator restart; release atomically.
        with self.transaction():
            for r in self.db.execute("SELECT * FROM reservations WHERE state='reserved'").fetchall():
                self._finish(r,0,'release')

    @contextmanager
    def transaction(self):
        with self.lock:
            self.db.execute('BEGIN IMMEDIATE')
            try:
                yield
                self.db.execute('COMMIT')
            except BaseException:
                self.db.execute('ROLLBACK')
                raise

    def key_for_digest(self, digest):
        # Indexed lookup (secret_sha256 is UNIQUE); the digest itself is a SHA-256 of a 256-bit secret,
        # so an index lookup leaks nothing a constant-time scan would protect.
        with self.lock:
            r = self.db.execute('SELECT key_id FROM keys WHERE secret_sha256=?', (digest,)).fetchone()
            return r[0] if r else None

    def key(self, k):
        with self.lock:
            r = self.db.execute('SELECT * FROM keys WHERE key_id=?', (k,)).fetchone()
            return dict(hashed_secret=r['secret_sha256'],credit_msat=r['balance_msat'],rate_limit_per_minute=r['rate_limit'],max_concurrent=r['max_concurrent'],allow_thinking=bool(r['allow_thinking'])) if r else None

    @property
    def keys(self):
        with self.lock:
            return {r['key_id']:dict(hashed_secret=r['secret_sha256'],credit_msat=r['balance_msat'],rate_limit_per_minute=r['rate_limit'],max_concurrent=r['max_concurrent'],allow_thinking=bool(r['allow_thinking'])) for r in self.db.execute('SELECT * FROM keys')}

    def _log(self,event,rid,k,amount,**meta):
        self.db.execute('INSERT INTO ledger(ts,event,request_id,key_id,amount_msat,meta_json) VALUES(?,?,?,?,?,?)',(time.time(),event,rid,k,amount,json.dumps(meta)))

    def log(self,event,rid,k,amount,**meta):
        with self.transaction(): self._log(event,rid,k,amount,**meta)

    def reprice(self, dkk_per_btc):
        if len(self.dkk_prices) != 3 or not dkk_per_btc > 0: return
        msat = lambda dkk: max(1, round(dkk*1e11/dkk_per_btc))  # 1 BTC = 1e11 msat
        self.input_price = msat(self.dkk_prices['input_dkk_per_million_tokens']/1000)
        self.output_price = msat(self.dkk_prices['output_dkk_per_million_tokens']/1000)
        self.minimum = msat(self.dkk_prices['min_charge_dkk_per_request'])

    def tail(self):
        with self.lock: return [dict(r) for r in self.db.execute('SELECT * FROM ledger ORDER BY id DESC LIMIT 50')][::-1]

    def summary(self,since,internal=(),real_backend=None):
        """Split real customer money from internal/test traffic so the admin view never overstates earnings."""
        with self.lock:
            q=lambda sql,*a: self.db.execute(sql,a).fetchall()
            internal=set(internal)
            # Keys funded only by test (fake-backend) invoices count as internal too.
            internal|={r[0] for r in q("SELECT DISTINCT key_id FROM invoices WHERE backend!=?",real_backend or '')}
            days=[dict(r) for r in q("SELECT date(ts,'unixepoch') day, key_id, COUNT(*) requests, SUM(amount_msat) revenue_msat, SUM(json_extract(meta_json,'$.prompt_tokens')) prompt_tokens, SUM(json_extract(meta_json,'$.completion_tokens')) completion_tokens FROM ledger WHERE event='settle' AND ts>=? GROUP BY day,key_id ORDER BY day DESC",since)]
            agg={}
            for d in days:
                a=agg.setdefault(d['day'],dict(day=d['day'],requests=0,test_requests=0,revenue_msat=0,test_msat=0,prompt_tokens=0,completion_tokens=0))
                t=d['key_id'] in internal
                a['test_requests' if t else 'requests']+=d['requests']; a['test_msat' if t else 'revenue_msat']+=d['revenue_msat'] or 0
                a['prompt_tokens']+=d['prompt_tokens'] or 0; a['completion_tokens']+=d['completion_tokens'] or 0
            keys=[dict(r) for r in q("""SELECT k.key_id, k.balance_msat, k.created, COALESCE(SUM(CASE WHEN l.event='settle' THEN l.amount_msat END),0) spent_msat, COALESCE(SUM(CASE WHEN l.event IN ('admin_credit','trial_credit') THEN l.amount_msat END),0) free_credit_msat, COUNT(CASE WHEN l.event='settle' THEN 1 END) requests, MAX(CASE WHEN l.event='settle' THEN l.ts END) last_used FROM keys k LEFT JOIN ledger l ON l.key_id=k.key_id GROUP BY k.key_id ORDER BY spent_msat DESC""")]
            paid={r[0]:r[1] for r in q("SELECT key_id, SUM(amount_msat) FROM invoices WHERE state='paid' AND backend=? GROUP BY key_id",real_backend or '')}
            for k in keys:
                k['internal']=k['key_id'] in internal; k['paid_in_msat']=paid.get(k['key_id'],0)
                # Conservative: free/trial credit is consumed first; only spend beyond it counts as earned.
                k['earned_msat']=0 if k['internal'] else max(0,min(k['spent_msat']-k['free_credit_msat'],k['paid_in_msat']))
            inv=dict(q("SELECT COALESCE(SUM(CASE WHEN state='paid' THEN amount_msat END),0) paid_msat, COUNT(CASE WHEN state='paid' THEN 1 END) paid, COUNT(CASE WHEN state='pending' THEN 1 END) pending FROM invoices WHERE backend=?",real_backend or '')[0])
            real=[k for k in keys if not k['internal']]
            return dict(days=list(agg.values()),keys=keys,invoices=inv,
                        earned_msat=sum(k['earned_msat'] for k in real),
                        trial_users=sum(1 for k in real if k['free_credit_msat']>0 and k['paid_in_msat']==0),
                        test_usage_msat=sum(a['test_msat'] for a in agg.values()),
                        customers=sum(1 for k in real if k['paid_in_msat']>0), customer_credit_liability_msat=sum(k['balance_msat'] for k in real))

    def quote(self,chars,max_tokens):
        return max(self.minimum,math.ceil((math.ceil(chars/4)*self.input_price+max_tokens*self.output_price)/1000))

    def reserve(self,rid,k,chars,max_tokens):
        amount=self.quote(chars,max_tokens)
        with self.transaction():
            if self.db.execute('SELECT 1 FROM reservations WHERE request_id=?',(rid,)).fetchone(): return True
            cur=self.db.execute('UPDATE keys SET balance_msat=balance_msat-? WHERE key_id=? AND balance_msat>=?',(amount,k,amount))
            if not cur.rowcount: return False
            self.db.execute('INSERT INTO reservations VALUES(?,?,?,?,?,?,?)',(rid,k,amount,None,'reserved',time.time(),time.time()))
            self._log('reserve',rid,k,amount)
            return True

    def _finish(self,r,actual,state,**meta):
        self.db.execute('UPDATE keys SET balance_msat=balance_msat+? WHERE key_id=?',(r['reserved_msat']-actual,r['key_id']))
        self.db.execute('UPDATE reservations SET settled_msat=?, state=?, updated=? WHERE request_id=?',(actual,state,time.time(),r['request_id']))
        self._log(state,r['request_id'],r['key_id'],actual,**meta)
        self._log('release',r['request_id'],r['key_id'],r['reserved_msat']-actual)

    def finish(self,rid,prompt=0,completion=0,cancel=False):
        with self.transaction():
            r=self.db.execute("SELECT * FROM reservations WHERE request_id=? AND state='reserved'",(rid,)).fetchone()
            if r: self._finish(r,0 if cancel else min(r['reserved_msat'],max(self.minimum,math.ceil((prompt*self.input_price+completion*self.output_price)/1000))),'cancel' if cancel else 'settle',prompt_tokens=prompt,completion_tokens=completion)

    def credit(self,k,amount):
        with self.transaction():
            self.db.execute('UPDATE keys SET balance_msat=balance_msat+? WHERE key_id=?',(amount,k))
            self._log('admin_credit',None,k,amount)

    def grant_trial(self,k,amount):
        with self.transaction():
            self.db.execute('UPDATE keys SET balance_msat=balance_msat+? WHERE key_id=?',(amount,k))
            self._log('trial_credit',None,k,amount)

    def create_key(self):
        k='key-'+secrets.token_hex(12); secret=secrets.token_urlsafe(32)
        with self.transaction(): self.db.execute('INSERT INTO keys VALUES(?,?,?,?,?,?,?)',(k,hashlib.sha256(secret.encode()).hexdigest(),0,30,self.default_max_concurrent,0,time.time()))
        return {'key_id':k,'api_key_plaintext_once':secret}

    def add_invoice(self,k,amount,invoice,backend):
        with self.transaction(): self.db.execute('INSERT INTO invoices VALUES(?,?,?,?,?,?,?,?,?,?)',(invoice['invoice_id'],k,amount,invoice['bolt11'],invoice['payment_hash'],'pending',time.time(),None,backend,invoice['expires_at']))

    def invoice(self,i):
        with self.lock:
            r=self.db.execute('SELECT * FROM invoices WHERE invoice_id=?',(i,)).fetchone()
            return dict(r) if r else None

    def pending_invoices(self):
        with self.lock: return [dict(r) for r in self.db.execute("SELECT * FROM invoices WHERE state='pending'")]

    def invoice_state(self,i,state):
        with self.transaction():
            r=self.db.execute('SELECT * FROM invoices WHERE invoice_id=?',(i,)).fetchone()
            if not r or r['state']!='pending': return
            if r['expires_at']<=time.time(): state='expired'
            if state=='paid':
                self.db.execute('UPDATE keys SET balance_msat=balance_msat+? WHERE key_id=?',(r['amount_msat'],r['key_id']))
                self._log('invoice_paid',i,r['key_id'],r['amount_msat'])
                self.db.execute("UPDATE invoices SET state='paid',paid_at=? WHERE invoice_id=?",(time.time(),i))
            elif state=='expired': self.db.execute("UPDATE invoices SET state='expired' WHERE invoice_id=?",(i,))
