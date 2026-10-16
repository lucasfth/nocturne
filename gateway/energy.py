"""Operating window and cached public day-ahead prices (injectable offline provider)."""
import asyncio
import math
import time
from datetime import datetime, timedelta, timezone
from zoneinfo import ZoneInfo
import httpx


async def public_prices(area):
    async with httpx.AsyncClient(timeout=15, trust_env=False) as client:
        for dataset, stamp, price in [('DayAheadPrices', 'TimeDK', 'DayAheadPriceDKK'), ('Elspotprices', 'HourDK', 'SpotPriceDKK')]:
            try:
                response = await client.get('https://api.energidataservice.dk/dataset/' + dataset,
                    params={'filter': '{"PriceArea":["' + area + '"]}', 'sort': stamp + ' DESC', 'limit': 400})
                response.raise_for_status()
                rows = response.json()['records']
                if rows and stamp in rows[0] and price in rows[0]:
                    return [{'time': r[stamp], 'spot_dkk_mwh': r[price], 'dataset': dataset} for r in rows if r[price] is not None]
            except (httpx.HTTPError, KeyError, ValueError):
                continue
    raise RuntimeError('price provider unavailable')


class Energy:
    def __init__(self, config, provider=public_prices, clock=None):
        self.config = config
        self.provider = provider
        self.clock = clock or (lambda: datetime.now(timezone.utc))
        self.rows = []
        self.last_success = None
        self.last_attempt = None
        self.last_price = None
        self.lock = asyncio.Lock()

    def window(self):
        cfg = self.config.get('schedule')
        now = self.clock()
        if not cfg: return True, now, now
        local = now.astimezone(ZoneInfo(cfg['tz']))
        sh, sm = map(int, cfg['start'].split(':')); eh, em = map(int, cfg['end'].split(':'))
        start = local.replace(hour=sh, minute=sm, second=0, microsecond=0)
        end = local.replace(hour=eh, minute=em, second=0, microsecond=0)
        if end <= start:
            if local < end: start -= timedelta(days=1)
            else: end += timedelta(days=1)
        opened = start <= local < end
        next_start = start if local < start else start + timedelta(days=1)
        if not opened and local < start: end -= timedelta(days=1)
        return opened, next_start, end

    async def refresh(self):
        if not self.config.get('energy'): return
        now = self.clock().timestamp()
        async with self.lock:
            if self.last_attempt is not None and now-self.last_attempt < self.config['energy'].get('refresh_min', 30)*60: return
            self.last_attempt = now
            try:
                rows = await self.provider(self.config['energy']['price_area'])
                self.rows = rows
                price = self.current_row_price()
                if price is None: raise ValueError('no current price')
                self.last_price = price
                self.last_success = now
            except Exception:
                pass  # Never log external response bodies or credentials.

    def current_row_price(self):
        now = self.clock().astimezone(ZoneInfo('Europe/Copenhagen'))
        matches = []
        for r in self.rows:
            stamp = datetime.fromisoformat(r['time'])
            if stamp.tzinfo is None: stamp = stamp.replace(tzinfo=ZoneInfo('Europe/Copenhagen'))
            resolution = 15 if r.get('dataset') == 'DayAheadPrices' else 60
            if stamp <= now < stamp + timedelta(minutes=resolution): matches.append(r)
        return float(matches[-1]['spot_dkk_mwh'])/1000 + self.config['energy'].get('tariff_dkk_per_kwh', 1.2) if matches else None

    def state(self, workers):
        from .policy import eligible
        cfg = self.config.get('energy', {})
        opened, next_start, _ = self.window()
        price = self.current_row_price() if cfg else None
        if price is None and self.last_success is not None and self.clock().timestamp()-self.last_success <= 21600: price = self.last_price
        if self.last_success is not None and self.clock().timestamp()-self.last_success > 21600: price = None
        tiers = cfg.get('tiers', {})
        tier = 'normal' if price is None else ('cheap' if price <= tiers.get('cheap_max_dkk_kwh', 1) else 'normal' if price <= tiers.get('normal_max_dkk_kwh', 2.5) else 'expensive')
        ids = sorted(wid for wid, w in workers.items() if eligible(w, time.monotonic()))
        serving = ids if opened else []
        # All eligible workers serve in parallel; idle power (~18 W each) is negligible, admission margin is enforced per request.
        return dict(window_open=opened, next_window_start=next_start.isoformat(), price_area=cfg.get('price_area', 'DK2'), dkk_per_kwh=price, tier=tier, eligible_workers=len(ids), serving_workers=len(serving)), serving

    def admit(self, workers, reserved_msat, input_tokens, output_tokens=0, bulk=False):
        from fastapi import HTTPException
        status, _ = self.state(workers)
        if not status['window_open']:
            retry = max(1, math.ceil((datetime.fromisoformat(status['next_window_start'])-self.clock()).total_seconds()))
            raise HTTPException(503, 'operating window closed', headers={'Retry-After': str(retry)})
        if status['tier'] != 'expensive': return
        cfg = self.config.get('energy', {})
        price = status['dkk_per_kwh']
        if bulk:
            # Own internal jobs earn nothing; they only pause on a real price spike.
            if price is not None and price > cfg.get('bulk_max_dkk_kwh', 4.0):
                raise HTTPException(503, 'energy price above bulk ceiling', headers={'X-Reason': 'energy-price'})
            return
        speeds = [w.get('tokens_per_sec', 20) for w in workers.values() if w.get('tokens_per_sec', 20) > 0]
        speed = min(speeds, default=20)  # Conservative for the slowest worker.
        cost_per_token = (cfg.get('watts_per_worker', 30)/1000 * price)/(speed*3600)
        # Prompt processing is many times faster than generation; weight input tokens accordingly.
        work = output_tokens + input_tokens*cfg.get('input_cost_weight', 0.1)
        revenue = reserved_msat/1e11*self.config.get('live_dkk_per_btc', self.config['dkk_per_btc'])
        if revenue < cfg.get('reserve_margin', 1.5)*cost_per_token*max(1, work):
            raise HTTPException(503, 'energy price exceeds reserve margin', headers={'X-Reason': 'energy-price'})
