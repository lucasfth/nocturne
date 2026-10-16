import hashlib, json, secrets
from pathlib import Path
p=Path('data'); p.mkdir(exist_ok=True,mode=0o700); p.chmod(0o700)
def put(name,value):
    f=p/name; f.write_text(value); f.chmod(0o600)
def digest(s): return hashlib.sha256(s.encode()).hexdigest()
if not (p/'api_keys.json').exists() and not (p/'ledger.sqlite').exists():
    secret=secrets.token_urlsafe(32); put('demo-api-key.txt',secret+'\n')
    put('api_keys.json',json.dumps({'demo':{'key_id':'demo','hashed_secret':digest(secret),'credit_msat':1000000,'rate_limit_per_minute':120,'max_concurrent':4,'allow_thinking':False}},indent=2))
    admin=secrets.token_urlsafe(32); put('admin-secret.txt',admin+'\n'); put('admin-secret.sha256',digest(admin)+'\n')
    ws={}
    for i in range(1,13):
        secret=secrets.token_urlsafe(32); put(f'sim-{i}.secret',secret+'\n'); ws[f'sim-{i}']=digest(secret)
    put('worker_secrets.json',json.dumps(ws,indent=2)); put('prices.json',json.dumps({'input_msat_per_1k_tokens':500,'output_msat_per_1k_tokens':2000,'min_charge_msat_per_request':10000}))
