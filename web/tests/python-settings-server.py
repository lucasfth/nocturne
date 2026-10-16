"""Disposable real Coordinator, no maintenance loops, external payments or live data."""
import hashlib
import json
import os
from pathlib import Path
import sys
import tempfile

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))
import uvicorn
from gateway.app import Coordinator

os.umask(0o077)
with tempfile.TemporaryDirectory(prefix='nocturne-ui-test-') as directory:
    root = Path(directory)
    data = root / 'data'
    data.mkdir()
    (root / 'config.json').write_text(json.dumps(dict(model='test-model', service_name='Fixture inference', public_limit_concurrency=64, lightning={'backend': 'fake'}, energy={}, dkk_per_btc=100000)))
    (data / 'api_keys.json').write_text('{}')
    (data / 'prices.json').write_text('{"price_per_token_msat":1}')
    (data / 'machines.json').write_text('{}')
    (data / 'admin-secret.sha256').write_text(hashlib.sha256(b'browser-test').hexdigest())
    os.environ['NOCTURNE_AUTHORIZED_KEYS'] = str(root / 'absent-authorized-keys')
    c = Coordinator(root)
    uvicorn.run(c.admin, host='127.0.0.1', port=5200, access_log=False, log_level='warning')
