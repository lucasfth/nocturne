"""Shared client for nocturne's low-priority bulk lane. Python 3.9+, stdlib only.

Our own overnight jobs call the public gateway with a bulk key. The gateway only leases bulk
work when no paying request waits, and never on a Mac's last free slot, so customers come first.
"""
import json, os, re, sys, time, threading, urllib.request, urllib.error
from datetime import datetime, timedelta
from concurrent.futures import ThreadPoolExecutor
from zoneinfo import ZoneInfo

BASE = os.environ.get('NOCTURNE_URL', 'http://127.0.0.1:8700')
MODEL = 'Ornith-1.5-9B'
TZ = ZoneInfo('Europe/Copenhagen')
_opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))  # talk to the gateway directly, ignore HTTP(S)_PROXY


def night_date():
    """Date label for the current night (evening start date in Copenhagen)."""
    now = datetime.now(TZ)
    return (now - timedelta(hours=12)).strftime('%Y-%m-%d')


def deadline():
    """07:30 Copenhagen of the morning ending this night (gateway closes 08:00)."""
    now = datetime.now(TZ)
    d = now.replace(hour=7, minute=30, second=0, microsecond=0)
    if now >= d: d += timedelta(days=1)
    return d.timestamp()


def log(event, **kw):
    """One JSON line per event to stdout (cron appends it to <job>.log). Never put prompts or outputs here."""
    rec = {'ts': datetime.now(TZ).isoformat(timespec='seconds'), 'event': event}; rec.update(kw)
    print(json.dumps(rec, ensure_ascii=False), flush=True)


class Stats:
    def __init__(self):
        self.lock = threading.Lock(); self.d = {}

    def add(self, job, **kw):
        with self.lock:
            s = self.d.setdefault(job, {'requests': 0, 'ok': 0, 'failed': 0, 'prompt_tokens': 0, 'completion_tokens': 0, 'seconds': 0.0, 'items': 0, 'notes': []})
            for k, v in kw.items():
                if k == 'note': s['notes'].append(v)
                else: s[k] = s.get(k, 0) + v


class Client:
    def __init__(self, key_file, stats, until=None):
        self.key = open(os.path.expanduser(key_file)).read().strip()
        self.stats = stats; self.until = until or deadline(); self.tl = threading.local()  # per-thread last call info

    def chat(self, job, messages, max_tokens=1200, effort='none', temperature=0.2):
        body = {'model': MODEL, 'messages': messages, 'max_tokens': max_tokens, 'temperature': temperature, 'reasoning_effort': effort}
        delay = 20; self.tl.usage = (0, 0, 0); self.tl.answered = False
        while time.time() < self.until:
            req = urllib.request.Request(BASE + '/v1/chat/completions', data=json.dumps(body).encode(),
                                         headers={'Content-Type': 'application/json', 'Authorization': 'Bearer ' + self.key})
            t = time.time()
            try:
                d = json.load(_opener.open(req, timeout=900))
                u = d.get('usage', {})
                self.stats.add(job, requests=1, ok=1, prompt_tokens=u.get('prompt_tokens', 0), completion_tokens=u.get('completion_tokens', 0), seconds=time.time() - t)
                self.tl.answered = True; self.tl.usage = (u.get('prompt_tokens', 0), u.get('completion_tokens', 0), round(time.time() - t, 1))
                return d['choices'][0]['message'].get('content') or ''
            except urllib.error.HTTPError as e:
                if e.code in (429, 499, 502, 503, 504):  # busy, closed or worker lost: wait and retry
                    log('retry', job=job, http=e.code)
                    retry = e.headers.get('Retry-After')
                    wait = min(int(retry), 1800) if retry and retry.isdigit() else delay
                    time.sleep(max(5, min(wait, self.until - time.time()))); delay = min(delay * 2, 600); continue
                log('error', job=job, http=e.code); self.stats.add(job, requests=1, failed=1, note='HTTP %s' % e.code); return None
            except Exception as e:
                log('error', job=job, exc=type(e).__name__); self.stats.add(job, requests=1, failed=1, note=type(e).__name__); time.sleep(delay); delay = min(delay * 2, 600)
        log('deadline', job=job); self.stats.add(job, note='deadline reached'); return None

    def ask_json(self, job, messages, **kw):
        txt = self.chat(job, messages, **kw)
        if txt is None: return None
        return parse_json(txt)


def parse_json(txt):
    txt = re.sub(r'^```(?:json)?\s*|\s*```$', '', txt.strip(), flags=re.M)
    for opener, closer in (('{', '}'), ('[', ']')):
        i, j = txt.find(opener), txt.rfind(closer)
        if i >= 0 and j > i:
            try: return json.loads(txt[i:j + 1])
            except ValueError: pass
    return None


def run_parallel(fn, items, workers):
    with ThreadPoolExecutor(max_workers=workers) as ex:
        return list(ex.map(fn, items))


def online_macs():
    """Macs the gateway can use right now (public /v1/status). None if the gateway can't be reached."""
    try:
        d = json.load(_opener.open(BASE + '/v1/status', timeout=15))
        return int(d.get('eligible_workers', 0))
    except Exception:
        return None


def run_adaptive(fn, items, per_mac=1, cap=64, refresh=60, job='bulk'):
    """Run fn over items with as many requests in flight as there are Macs online.

    Re-reads the Mac count every `refresh` seconds, so Macs that join or leave during the night
    change the parallelism (no fixed limit of six). Keeps at least 1 in flight so a closed window
    is waited out by the normal retry/backoff instead of ending the run.
    """
    items = list(items); cond = threading.Condition(); state = {'running': 0, 'target': 1, 'checked': 0.0}

    def target():
        now = time.time()
        if now - state['checked'] >= refresh:
            n = online_macs(); state['checked'] = now
            if n is not None:
                t = max(1, min(cap, n * per_mac))
                if t != state['target']: log('parallelism', job=job, macs_online=n, in_flight_max=t)
                state['target'] = t
        return state['target']

    def wrap(item):
        try: fn(item)
        finally:
            with cond: state['running'] -= 1; cond.notify_all()

    with ThreadPoolExecutor(max_workers=cap) as ex:
        for item in items:
            with cond:
                while state['running'] >= target(): cond.wait(timeout=refresh)
                state['running'] += 1
            ex.submit(wrap, item)


def write_summary(path, stats, started, extra=None):
    out = {'night': night_date(), 'started': started, 'finished': datetime.now(TZ).isoformat(timespec='seconds'), 'jobs': stats.d}
    if extra: out.update(extra)
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, 'w') as f: json.dump(out, f, indent=1, ensure_ascii=False)
    return out


REDACT = [(re.compile(r'\+?\d[\d \-]{7,}\d'), '<num>'), (re.compile(r'[A-Za-z0-9_\-]{32,}'), '<token>'),
          (re.compile(r'[\w.+-]+@[\w-]+\.[\w.]+'), '<email>')]


def redact(s):
    for rx, rep in REDACT: s = rx.sub(rep, s)
    return s
