"""Black-box Python/Go comparison; every byte of mutable state is disposable."""
import asyncio
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import sys
import tempfile
from datetime import datetime, timezone

import httpx
import uvicorn

REPO = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(REPO))
FIXED = 1792083600  # 2026-10-15T17:00:00Z


def digest(s):
    return hashlib.sha256(s.encode()).hexdigest()


def fixture(root):
    d = root / 'data'
    d.mkdir(parents=True)
    config = dict(model='shadow-model', service_name='shadow', max_input_chars=1000,
                  max_tokens=100, max_context=1000, request_timeout_s=10, queue_depth=8,
                  dkk_per_btc=100000, signup_invite='shadow-invite',
                  signup_per_ip_per_day=20, public_ip_rate_per_minute=10000,
                  chat_ip_rate_per_minute=10000, public_base_url='https://shadow.invalid',
                  lightning={'backend': 'fake'}, energy={'price_area': 'DK2'})
    (root / 'config.json').write_text(json.dumps(config))
    (root / 'config.json').chmod(0o600)
    (d / 'api_keys.json').write_text(json.dumps({'test': dict(hashed_secret=digest('api'), credit_msat=100000,
        rate_limit_per_minute=10000, max_concurrent=10, allow_thinking=False)}))
    (d / 'prices.json').write_text('{"price_per_token_msat":1}')
    (d / 'machines.json').write_text(json.dumps({'w': dict(secret_sha256=digest('worker'), enabled=True,
        added=FIXED, meta={}, tunnel_pubkey=None)}))
    (d / 'admin-secret.sha256').write_text(digest('admin'))
    (d / 'admin-secret.sha256').chmod(0o600)
    shutil.copytree(REPO / 'gateway', root / 'gateway', ignore=shutil.ignore_patterns('__pycache__'))
    assets = root / 'web/dist/assets'
    assets.mkdir(parents=True)
    (assets.parent / 'index.html').write_text('<html>shadow admin fixture</html>')
    (assets / 'shadow.js').write_text('console.log("shadow fixture");')


class Compare:
    def __init__(self):
        self.results = []
        self.intended = 0
        self.ids = [{}, {}]

    def normalize(self, value, side, field=''):
        # Explicit nondeterminism only. Preserve nulls, types, all other fields and ordering.
        if isinstance(value, dict):
            return {k: self.normalize(v, side, k) for k, v in value.items()}
        if isinstance(value, list):
            return [self.normalize(v, side, field) for v in value]
        if field in {'created', 'expires_at', 'paid_at', 'added', 'ts', 'heartbeat_age', 'heartbeat_age_s', 'last_used'} and isinstance(value, (int, float)):
            assert not isinstance(value, bool) and value >= 0, (field, value)
            return '<timestamp>'
        if isinstance(value, str):
            for original, replacement in self.ids[side].items():
                value = value.replace(original, replacement)
        return value

    def bind(self, a, b, label):
        assert isinstance(a, str) and isinstance(b, str) and a and b
        self.ids[0][a] = label
        self.ids[1][b] = label

    def record(self, name, responses, mode='json', intended=None):
        values = []
        for side, r in enumerate(responses):
            if mode == 'sse':
                data = [line[6:] for line in r.text.splitlines() if line.startswith('data: ')]
                value = [json.loads(x) if x != '[DONE]' else x for x in data]
            elif mode == 'text':
                value = r.text
            else:
                try:
                    value = r.json()
                except ValueError:
                    value = {'NON_JSON': r.text}
            values.append((r.status_code, self.normalize(value, side)))
        ok = values[0] == values[1]
        self.results.append((name, ok))
        print(('PASS ' if ok else 'DIFF ') + name, flush=True)
        if not ok:
            print('  python=' + json.dumps(values[0], ensure_ascii=False, sort_keys=True))
            print('  go=    ' + json.dumps(values[1], ensure_ascii=False, sort_keys=True))
        if not ok and intended:
            expected = intended[:2]
            if ([r.status_code for r in responses] == list(expected)
                    and responses[0].text == 'Internal Server Error'
                    and responses[1].json() == {'detail': 'invalid JSON' if name.endswith('{') else 'invalid body'}):
                self.intended += 1
                print('  INTENDED ' + intended[2])
            else:
                print('  UNEXPECTED status for documented deviation')
        return responses


async def main():
    with tempfile.TemporaryDirectory(prefix='nocturne-shadow-') as tmp:
        tmp = Path(tmp)
        roots = [tmp / 'python', tmp / 'go']
        for root in roots:
            fixture(root)
        binary = tmp / 'shadow-server'
        subprocess.run([os.environ.get('GO', 'go'), 'build', '-o', str(binary), './tools/shadow-server'], cwd=REPO / 'go', check=True)
        # Deny external sockets, including all Python provider traffic. Loopback only.
        original_connect = socket.socket.connect
        def connect(sock, address):
            if isinstance(address, tuple) and not ipaddress.ip_address(address[0]).is_loopback:
                raise RuntimeError('shadow forbids external sockets: ' + str(address))
            return original_connect(sock, address)
        socket.socket.connect = connect
        from gateway.app import Coordinator
        import gateway.app as app_module
        # Python routes resolve __file__ at request time; copied assets keep source checkout untouched.
        app_module.__file__ = str(roots[0] / 'gateway/app.py')
        c = Coordinator(roots[0])
        async def fx():
            return 100000
        async def energy(area):
            return [dict(time='2026-10-15T19:00:00+02:00', spot_dkk_mwh=100.0, dataset='DayAheadPrices')]
        c.fx.provider = fx
        c.fx.clock = lambda: FIXED
        c.energy.provider = energy
        c.energy.clock = lambda: datetime.fromtimestamp(FIXED, timezone.utc)
        await c.fx.refresh()
        await c.energy.refresh()
        servers, tasks, addresses = [], [], []
        for app in [c.public, c.worker, c.admin]:
            sock = socket.socket()
            sock.bind(('127.0.0.1', 0))
            sock.listen()
            addresses.append('http://127.0.0.1:' + str(sock.getsockname()[1]))
            server = uvicorn.Server(uvicorn.Config(app, log_level='critical', access_log=False, server_header=False, proxy_headers=False))
            servers.append(server)
            tasks.append(asyncio.create_task(server.serve(sockets=[sock])))
        stderr = open(tmp / 'go-stderr.log', 'w+')
        proc = subprocess.Popen([str(binary), str(roots[1])], stdout=subprocess.PIPE, stderr=stderr, text=True,
            env={**os.environ, 'NOCTURNE_AUTHORIZED_KEYS': str(tmp / 'nonexistent-authorized-keys')})
        clients = []
        try:
            line = await asyncio.wait_for(asyncio.to_thread(proc.stdout.readline), 30)
            if not line:
                stderr.seek(0)
                raise RuntimeError(stderr.read())
            go_addresses = json.loads(line)
            while not all(s.started for s in servers):
                await asyncio.sleep(.01)
            for urls in [addresses, go_addresses]:
                clients.append([httpx.AsyncClient(base_url=url, timeout=30, trust_env=False) for url in urls])
            comp = Compare()
            async def req(name, lane, method, path, token=None, data=None, raw=None, mode='json', headers=None, intended=None):
                paths = path if isinstance(path, list) else [path, path]
                hs = dict(headers or {})
                if token:
                    hs['Authorization'] = 'Bearer ' + token
                rs = await asyncio.gather(*(clients[i][lane].request(method, paths[i], headers=hs,
                    **({'content': raw} if raw is not None else {'json': data} if data is not None else {})) for i in range(2)))
                return comp.record(name, rs, mode, intended)
            # Settings use per-process opaque revisions and an explicit runtime label.
            # Normalize only those two fields; compare metadata, errors and effects verbatim.
            async def settings(name, method='GET', changes=None, bodies=None, headers=None, token='admin'):
                hs = {'Content-Type': 'application/json', **(headers or {})}
                if token:
                    hs['Authorization'] = 'Bearer ' + token
                if bodies is None:
                    bodies = [json.dumps({'revision': rev, 'changes': changes}) if changes is not None else '' for rev in revisions]
                rs = await asyncio.gather(*(clients[i][2].request(method, '/admin/config', headers=hs, content=bodies[i]) for i in range(2)))
                for i, r in enumerate(rs):
                    if r.status_code == 200:
                        v = r.json()
                        assert v['runtime'] == ['python', 'go'][i]
                        assert len(v['revision']) == 32
                        assert 'shadow-invite' not in r.text and 'signup_invite' not in r.text and 'lightning' not in r.text
                        assert r.headers['cache-control'] == 'no-store'
                        v['runtime'] = '<runtime>'
                        v['revision'] = '<revision>'
                        rs[i] = httpx.Response(r.status_code, json=v)
                comp.record('settings ' + name, rs)
                return rs
            revisions = ['', '']
            initial = await asyncio.gather(*(clients[i][2].get('/admin/config', headers={'Authorization': 'Bearer admin'}) for i in range(2)))
            revisions = [r.json()['revision'] for r in initial]
            await settings('snapshot')
            await settings('unauthorized', token=None)
            for headers in [{'Origin': 'https://evil.invalid'}, {'Sec-Fetch-Site': 'same-site'}, {'Tailscale-User-Login': 'outsider'}]:
                await settings('browser/tailnet denied ' + next(iter(headers)), headers=headers)
            before = [(root / 'config.json').read_bytes() for root in roots]
            for changes in [{'service_name': None}, {'service_name': '<script>'}, {'service_name': '\u00a0'}, {'public_limit_concurrency': True}, {'public_limit_concurrency': 1.0}, {'public_limit_concurrency': 0}, {'service_name': 'Valid', 'lightning': {}}, {}]:
                await settings('invalid ' + json.dumps(changes), 'PATCH', changes)
                assert [(root / 'config.json').read_bytes() for root in roots] == before
            for raw in ['null', '[]', '{', '{"revision":1,"revision":2}', ' ' * 4097]:
                await settings('malformed ' + raw[:40], 'PATCH', bodies=[raw, raw])
            await settings('wrong media type', 'PATCH', {'service_name': 'New'}, headers={'Content-Type': 'text/plain'})
            changes = {'service_name': 'Portable inference', 'public_limit_concurrency': 20}
            stale = [json.dumps({'revision': rev, 'changes': changes}) for rev in revisions]
            await settings('durable live/restart update', 'PATCH', changes)
            await settings('stale revision', 'PATCH', bodies=stale)
            for root in roots:
                saved = json.loads((root / 'config.json').read_text())
                assert saved['signup_invite'] == 'shadow-invite' and saved['lightning'] == {'backend': 'fake'}
                assert (root / 'config.json').stat().st_mode & 0o777 == 0o600
            await settings('updated snapshot')
            for side in range(2):
                models = await clients[side][0].get('/v1/models', headers={'Authorization': 'Bearer api'})
                assert models.json()['data'][0]['owned_by'] == 'Portable inference'
                summary = await clients[side][2].get('/admin/summary', headers={'Authorization': 'Bearer admin'})
                assert summary.json()['service'] == 'Portable inference'
                landing = await clients[side][0].get('/')
                assert 'Portable inference' in landing.text

            for path in ['/healthz', '/v1/status', '/v1/pricing', '/openapi.json']:
                await req('public ' + path, 0, 'GET', path)
            for path in ['/', '/robots.txt', '/llms.txt']:
                await req('public ' + path, 0, 'GET', path, mode='text')
            for path in ['/v1/models', '/v1/credits/balance', '/v1/credits/invoice/missing']:
                await req('unauth ' + path, 0, 'GET', path)
                await req('auth ' + path, 0, 'GET', path, 'api')
            for path in ['/admin/energy', '/admin/workers', '/admin/summary', '/admin/machines', '/admin/ledger/tail']:
                await req('unauth ' + path, 2, 'GET', path)
                await req('auth ' + path, 2, 'GET', path, 'admin')
            for path in ['/admin', '/admin/assets/shadow.js', '/admin/assets/missing.js']:
                await req('SPA ' + path, 2, 'GET', path, mode='text' if 'missing' not in path else 'json')
            await req('tailnet denied', 2, 'GET', '/admin/summary', 'admin', headers={'tailscale-user-login': 'not-allowed'})
            await req('fake webhook unavailable', 2, 'POST', '/admin/lightning/webhook', data={})
            for path in ['/admin/workers', '/worker/register', '/not-a-route']:
                await req('public isolation ' + path, 0, 'GET', path)
            for raw in ['{', '[]', '{}', '{"invite":"wrong"}']:
                await req('signup rejection ' + raw, 0, 'POST', '/v1/keys', raw=raw)
            # Bind generated values BEFORE comparison, including secret returned once (never print raw).
            rs = await asyncio.gather(*(cc[0].post('/v1/keys', json={'invite': 'shadow-invite'}) for cc in clients))
            for field in ['key_id', 'api_key_plaintext_once']:
                comp.bind(rs[0].json()[field], rs[1].json()[field], '<signup-' + field + '>')
            comp.record('signup create', rs)
            for amount in [0, True, 999, 100000001]:
                await req('invoice invalid ' + str(amount), 0, 'POST', '/v1/credits/invoice', 'api', {'amount_msat': amount})
            rs = await asyncio.gather(*(cc[0].post('/v1/credits/invoice', headers={'Authorization': 'Bearer api'}, json={'amount_msat': 10000}) for cc in clients))
            invoices = [r.json()['invoice_id'] for r in rs]
            comp.bind(*invoices, '<invoice>')
            comp.record('invoice create fake', rs)
            await req('invoice pending', 0, 'GET', ['/v1/credits/invoice/' + i for i in invoices], 'api')
            for n in range(2):
                await req('invoice mark paid ' + str(n), 2, 'POST', ['/admin/invoices/' + i + '/mark-paid' for i in invoices], 'admin')
            await req('invoice paid', 0, 'GET', ['/v1/credits/invoice/' + i for i in invoices], 'api')
            await req('balance after invoice', 0, 'GET', '/v1/credits/balance', 'api')
            await req('manual credit invalid', 2, 'POST', '/admin/credit', 'admin', {'key_id': 'test', 'amount_msat': -1})
            await req('manual credit', 2, 'POST', '/admin/credit', 'admin', {'key_id': 'test', 'amount_msat': 42})
            for path in ['/worker/register', '/worker/heartbeat', '/worker/lease', '/worker/result', '/worker/fail']:
                for raw in ['{', '[]']:
                    await req('documented worker malformed ' + path + ' ' + raw, 1, 'POST', path, 'worker', raw=raw,
                        intended=(500, 400, 'go/internal/app/doc.go: worker malformed bodies fail closed with 400 instead of Python uncaught 500'))
            for raw in ['{', '[]']:
                await req('documented credit malformed ' + raw, 2, 'POST', '/admin/credit', 'admin', raw=raw,
                    intended=(500, 400, 'go/internal/app/doc.go: malformed admin credit fails closed with 400 instead of Python uncaught 500'))
            for path in ['/worker/register', '/worker/heartbeat', '/worker/lease', '/worker/result', '/worker/fail']:
                await req('worker unauth ' + path, 1, 'POST', path, data={'worker_id': 'w'})
            await req('heartbeat before register', 1, 'POST', '/worker/heartbeat', 'worker', {'worker_id': 'w'})
            await req('worker register', 1, 'POST', '/worker/register', 'worker', {'worker_id': 'w'})
            await req('heartbeat invalid', 1, 'POST', '/worker/heartbeat', 'worker', {'worker_id': 'w'})
            beat = dict(worker_id='w', model='shadow-model', ready=True, on_ac=True, battery_pct=90,
                thermal_state='nominal', user_active=False, in_flight=0, capacity=1, tokens_per_sec=25, metrics={'cpu': 1.5})
            await req('heartbeat ready', 1, 'POST', '/worker/heartbeat', 'worker', beat)
            chat = dict(model='shadow-model', messages=[dict(role='user', content='hello')], max_tokens=20, stream=False)
            for extra in [{'unknown': True}, {'model': 'wrong'}, {'messages': []}, {'max_tokens': True}, {'stream': 'yes'}, {'temperature': 3}, {'reasoning_effort': 'bad'}, {'stop': [1]}]:
                await req('chat invalid ' + json.dumps(extra), 0, 'POST', '/v1/chat/completions', 'api', {**chat, **extra})
            for stream in [False, True]:
                body = {**chat, 'stream': stream}
                if stream:
                    body['stream_options'] = {'include_usage': True}
                pending = [asyncio.create_task(cc[0].post('/v1/chat/completions', headers={'Authorization': 'Bearer api'}, json=body)) for cc in clients]
                leases = await asyncio.gather(*(cc[1].post('/worker/lease', headers={'Authorization': 'Bearer worker'}, json={'worker_id': 'w'}) for cc in clients))
                jobs = [r.json() for r in leases]
                for key in ['request_id', 'lease_id']:
                    comp.bind(jobs[0][key], jobs[1][key], f'<{key}-{stream}>')
                comp.record(f'lease stream={stream}', leases)
                for chunk, final in [('<think>hidden</think>Hello ', False), ('world', True)]:
                    results = await asyncio.gather(*(clients[i][1].post('/worker/result', headers={'Authorization': 'Bearer worker'},
                        json=dict(worker_id='w', request_id=jobs[i]['request_id'], lease_id=jobs[i]['lease_id'], chunk=chunk, final=final,
                                  usage={'prompt_tokens': 2, 'completion_tokens': 4})) for i in range(2)))
                    comp.record(f'result stream={stream} final={final}', results)
                comp.record(f'chat stream={stream}', await asyncio.gather(*pending), 'sse' if stream else 'json')
            # Exercise a genuine in-flight worker failure, not only a stale request.
            pending = [asyncio.create_task(cc[0].post('/v1/chat/completions', headers={'Authorization': 'Bearer api'},
                json={**chat, 'stream': True})) for cc in clients]
            leases = await asyncio.gather(*(cc[1].post('/worker/lease', headers={'Authorization': 'Bearer worker'},
                json={'worker_id': 'w'}) for cc in clients))
            jobs = [r.json() for r in leases]
            for key in ['request_id', 'lease_id']:
                comp.bind(jobs[0][key], jobs[1][key], '<failed-' + key + '>')
            comp.record('lease before failure', leases)
            results = await asyncio.gather(*(clients[i][1].post('/worker/fail', headers={'Authorization': 'Bearer worker'},
                json=dict(worker_id='w', request_id=jobs[i]['request_id'], lease_id=jobs[i]['lease_id'])) for i in range(2)))
            comp.record('worker active fail', results)
            comp.record('chat interrupted SSE', await asyncio.gather(*pending), 'sse')
            await req('balance after refund', 0, 'GET', '/v1/credits/balance', 'api')
            for path in ['/worker/result', '/worker/fail']:
                await req('stale ' + path, 1, 'POST', path, 'worker', {'worker_id': 'w', 'request_id': 'stale', 'lease_id': 'stale'})
            await req('workers online', 2, 'GET', '/admin/workers', 'admin')
            await req('summary settled', 2, 'GET', '/admin/summary', 'admin')
            await req('ledger settled', 2, 'GET', '/admin/ledger/tail', 'admin')
            machine = dict(worker_id='new-worker', secret_sha256=digest('new-worker'), meta={'owner': 'Shadow'})
            await req('machine invalid', 2, 'POST', '/admin/machines', 'admin', {'worker_id': 'BAD'})
            await req('machine add', 2, 'POST', '/admin/machines', 'admin', machine)
            await req('machine duplicate', 2, 'POST', '/admin/machines', 'admin', machine)
            await req('machine update', 2, 'PATCH', '/admin/machines/new-worker', 'admin', {'meta': {'owner': 'Updated'}})
            await req('machine list', 2, 'GET', '/admin/machines', 'admin')
            await req('machine disable online', 2, 'PATCH', '/admin/machines/w', 'admin', {'enabled': False})
            await req('disabled worker auth', 1, 'POST', '/worker/register', 'worker', {'worker_id': 'w'})
            await req('machine delete', 2, 'DELETE', '/admin/machines/new-worker', 'admin')
            await req('machine delete missing', 2, 'DELETE', '/admin/machines/new-worker', 'admin')
            await req('machine patch missing', 2, 'PATCH', '/admin/machines/missing', 'admin', {'enabled': True})
            failures = sum(not ok for _, ok in comp.results)
            print(f'SHADOW cases={len(comp.results)} equal={len(comp.results)-failures} differences={failures} intended={comp.intended} unexpected={failures-comp.intended}')
            return bool(failures - comp.intended)
        finally:
            for group in clients:
                for client in group:
                    await client.aclose()
            for server in servers:
                server.should_exit = True
            await asyncio.gather(*tasks, return_exceptions=True)
            proc.terminate()
            await asyncio.to_thread(proc.wait, 10)
            stderr.close()


if __name__ == '__main__':
    raise SystemExit(asyncio.run(main()))
