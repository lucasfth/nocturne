"""Live BTC->DKK rate with on-disk cache; fallback to config constant."""
import json, time
from pathlib import Path
import httpx

async def coinbase():
    async with httpx.AsyncClient(timeout=10, trust_env=False) as c:
        r = await c.get('https://api.coinbase.com/v2/prices/BTC-DKK/spot'); r.raise_for_status()
        return float(r.json()['data']['amount'])

class Fx:
    def __init__(self, config, data, provider=coinbase, clock=time.time):
        self.config, self.path, self.provider, self.clock = config, Path(data)/'fx.json', provider, clock
        self.rate, self.updated, self.source, self.last_attempt = float(config.get('dkk_per_btc', 100000)), None, 'config', None
        try:
            d = json.loads(self.path.read_text())
            if 1e4 < d['dkk_per_btc'] < 1e8: self.rate, self.updated, self.source = d['dkk_per_btc'], d['updated'], 'cache'
        except Exception: pass
    async def refresh(self, every=600):
        now = self.clock()
        if self.last_attempt is not None and now-self.last_attempt < every: return False
        self.last_attempt = now
        try:
            rate = float(await self.provider())
            if not 1e4 < rate < 1e8: raise ValueError('implausible rate')
            # Ignore a single wild jump (>30%) vs. the previous live value.
            if self.source != 'config' and abs(rate/self.rate-1) > .3 and now-(self.updated or 0) < 3600: raise ValueError('rate jump')
        except Exception: return False
        self.rate, self.updated, self.source = rate, now, 'coinbase'
        self.path.write_text(json.dumps({'dkk_per_btc': rate, 'updated': now})); self.path.chmod(0o600)
        return True
    def status(self):
        return {'dkk_per_btc': self.rate, 'source': self.source, 'age_s': None if self.updated is None else round(self.clock()-self.updated)}
