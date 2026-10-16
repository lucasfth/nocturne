"""Registry of worker Macs, editable from the admin view.

Stored in data/machines.json (0600, atomic writes). The gateway is the only writer.
Only SHA-256 digests of worker secrets are stored; the secret itself never leaves the Mac.
"""
import base64, hashlib, ipaddress, json, os, re, tempfile, threading, time
from pathlib import Path

ID = re.compile(r'^[a-z0-9][a-z0-9-]{0,31}$')
HEX = re.compile(r'^[0-9a-f]{64}$')
# Values that end up in copy-paste shell commands are validated strictly.
STRICT = {
    'user': re.compile(r'^[a-z_][a-z0-9_.-]{0,31}$'),
    'admin_user': re.compile(r'^[a-z_][a-z0-9_.-]{0,31}$'),
    'host': re.compile(r'^(?=.{1,253}$)[A-Za-z0-9]([A-Za-z0-9-]{0,62})(\.[A-Za-z0-9]([A-Za-z0-9-]{0,62}))*\.?$'),
    'jump': re.compile(r'^[a-z_][a-z0-9_.-]{0,31}@[A-Za-z0-9.-]{1,253}$'),
}
FREE = {'owner', 'agent', 'model'}
META = FREE | set(STRICT) | {'ip'}


def _clean_free(v):
    if not isinstance(v, str) or len(v) > 128 or any(ord(c) < 32 or ord(c) == 127 for c in v): raise ValueError('invalid text')
    return v.strip()


def clean_meta(meta):
    if not isinstance(meta, dict) or set(meta) - META: raise ValueError(f'meta keys allowed: {", ".join(sorted(META))}')
    out = {}
    for k, v in meta.items():
        if v is None or v == '':
            out[k] = None; continue
        if k in FREE: out[k] = _clean_free(v)
        elif k == 'ip':
            try: ip = ipaddress.ip_address(v)
            except (ValueError, TypeError): raise ValueError('invalid ip')
            if getattr(ip, 'scope_id', None): raise ValueError('invalid ip (no scope ids)')
            out[k] = str(ip)
        elif not isinstance(v, str) or not STRICT[k].fullmatch(v): raise ValueError(f'invalid {k}')
        else: out[k] = v
    return out


def parse_pubkey(s):
    """Accept only ssh-ed25519 keys; return the normalized 'ssh-ed25519 <base64>' (comment dropped)."""
    if not isinstance(s, str): raise ValueError('invalid tunnel_pubkey')
    parts = s.strip().split()
    if len(parts) < 2 or parts[0] != 'ssh-ed25519' or len(parts[1]) > 128: raise ValueError('tunnel_pubkey must be ssh-ed25519')
    try: blob = base64.b64decode(parts[1], validate=True)
    except Exception: raise ValueError('invalid tunnel_pubkey')
    n = int.from_bytes(blob[:4], 'big')
    if blob[4:4 + n] != b'ssh-ed25519' or int.from_bytes(blob[4 + n:8 + n], 'big') != 32 or len(blob) != 8 + n + 32:
        raise ValueError('invalid tunnel_pubkey')
    return 'ssh-ed25519 ' + parts[1]


def fingerprint(pub):
    blob = base64.b64decode(pub.split()[1])
    return 'SHA256:' + base64.b64encode(hashlib.sha256(blob).digest()).decode().rstrip('=')


class RegistryUnavailable(RuntimeError):
    pass


class Machines:
    def __init__(self, data, legacy_secrets=None, legacy_meta=None, legacy_keys=None):
        self.path = Path(data) / 'machines.json'; self.lock = threading.RLock(); self.uncertain = False
        if self.path.exists():
            self.items = json.loads(self.path.read_text())
        else:
            # One-time migration from worker_secrets.json + config['macs']. Refuse rather than drop anyone.
            self.items = {}
            legacy = (legacy_secrets() if callable(legacy_secrets) else legacy_secrets) or {}
            bad = [w for w, d in legacy.items() if not (isinstance(w, str) and ID.fullmatch(w) and isinstance(d, str) and HEX.fullmatch(d))]
            if bad: raise SystemExit(f'machines: cannot migrate worker ids {bad}; rename them in worker_secrets.json (ids: {ID.pattern}, digests: 64 lowercase hex)')
            for wid, digest in legacy.items():
                if True:
                    meta = {k: v for k, v in (legacy_meta or {}).get(wid, {}).items() if k in META}
                    try: meta = {k: v for k, v in clean_meta(meta).items() if v is not None}
                    except ValueError: meta = {}
                    pub = (legacy_keys or {}).get(wid)
                    try: pub = parse_pubkey(pub) if pub else None
                    except ValueError: pub = None
                    self.items[wid] = dict(secret_sha256=digest, enabled=True, added=time.time(), meta=meta, tunnel_pubkey=pub)
            self._save()
        self._index()

    def _index(self):
        self.secrets = {wid: m['secret_sha256'] for wid, m in self.items.items() if m.get('enabled', True)}

    def _commit(self, items):
        """Write the candidate first; only publish it (and the auth index) once it is on disk."""
        self.check_available()
        self._save(items); self.items = items; self._index()

    def _save(self, items=None):
        items = self.items if items is None else items
        self.path.parent.mkdir(parents=True, exist_ok=True)
        fd, tmp = tempfile.mkstemp(dir=self.path.parent, prefix='.machines.')
        renamed = False
        try:
            with os.fdopen(fd, 'w') as f:
                os.fchmod(f.fileno(), 0o600); json.dump(items, f, indent=1, sort_keys=True); f.flush(); os.fsync(f.fileno())
            os.replace(tmp, self.path)
            renamed = True
            d = os.open(self.path.parent, os.O_RDONLY)
            try: os.fsync(d)
            finally: os.close(d)
        except BaseException as error:
            if renamed:
                self.uncertain = True
                self.secrets = {}
            try: os.unlink(tmp)
            except FileNotFoundError: pass
            if isinstance(error, OSError):
                raise RegistryUnavailable("machine registry unavailable; restart required") from None
            raise

    def check_available(self):
        if self.uncertain: raise RegistryUnavailable("machine registry unavailable; restart required")

    def secret(self, wid):
        with self.lock:
            self.check_available()
            return self.secrets.get(wid)

    def meta(self, wid): return dict((self.items.get(wid) or {}).get('meta') or {})

    def enabled(self, wid): return not self.uncertain and wid in self.items and self.items[wid].get('enabled', True)

    def public(self, wid):
        m = self.items[wid]
        return dict(worker_id=wid, enabled=m.get('enabled', True), added=m.get('added'), meta=dict(m.get('meta') or {}),
                    tunnel_key_fingerprint=fingerprint(m['tunnel_pubkey']) if m.get('tunnel_pubkey') else None)

    def list(self): return [self.public(w) for w in sorted(self.items)]

    def add(self, b):
        if not isinstance(b, dict) or set(b) - {'worker_id', 'secret_sha256', 'tunnel_pubkey', 'meta', 'enabled'}: raise ValueError('unknown field')
        wid = b.get('worker_id')
        if not isinstance(wid, str) or not ID.fullmatch(wid): raise ValueError('worker_id must match ^[a-z0-9][a-z0-9-]{0,31}$')
        digest = b.get('secret_sha256')
        if not isinstance(digest, str) or not HEX.fullmatch(digest.lower()): raise ValueError('secret_sha256 must be 64 hex characters')
        pub = parse_pubkey(b['tunnel_pubkey']) if b.get('tunnel_pubkey') else None
        meta = {k: v for k, v in clean_meta(b.get('meta') or {}).items() if v is not None}
        enabled = b.get('enabled', True)
        if type(enabled) is not bool: raise ValueError('enabled must be boolean')
        with self.lock:
            self.check_available()
            if wid in self.items: raise KeyError(wid)
            self._check_unique(wid, digest.lower(), pub)
            self._commit({**self.items, wid: dict(secret_sha256=digest.lower(), enabled=enabled, added=time.time(), meta=meta, tunnel_pubkey=pub)})
        return self.public(wid)

    def _check_unique(self, wid, digest, pub):
        for other, m in self.items.items():
            if other == wid: continue
            if m['secret_sha256'] == digest: raise ValueError(f'secret already used by {other}')
            if pub and m.get('tunnel_pubkey') == pub: raise ValueError(f'tunnel key already used by {other}')

    def update(self, wid, b):
        if not isinstance(b, dict) or not b or set(b) - {'secret_sha256', 'tunnel_pubkey', 'meta', 'enabled'}: raise ValueError('nothing to update')
        with self.lock:
            self.check_available()
            if wid not in self.items: raise LookupError(wid)
            m = dict(self.items[wid]); m['meta'] = dict(m.get('meta') or {})
            if 'enabled' in b:
                if type(b['enabled']) is not bool: raise ValueError('enabled must be boolean')
                m['enabled'] = b['enabled']
            if 'secret_sha256' in b:
                if not isinstance(b['secret_sha256'], str) or not HEX.fullmatch(b['secret_sha256'].lower()): raise ValueError('secret_sha256 must be 64 hex characters')
                m['secret_sha256'] = b['secret_sha256'].lower()
            if 'tunnel_pubkey' in b: m['tunnel_pubkey'] = parse_pubkey(b['tunnel_pubkey']) if b['tunnel_pubkey'] else None
            if 'meta' in b:
                for k, v in clean_meta(b['meta']).items():
                    if v is None: m['meta'].pop(k, None)
                    else: m['meta'][k] = v
            self._check_unique(wid, m['secret_sha256'], m.get('tunnel_pubkey'))
            self._commit({**self.items, wid: m})
        return self.public(wid)

    def delete(self, wid):
        with self.lock:
            self.check_available()
            if wid not in self.items: raise LookupError(wid)
            self._commit({k: v for k, v in self.items.items() if k != wid})


def legacy_tunnel_keys(path):
    """{worker_id: 'ssh-ed25519 ...'} from existing `... nocturne-tunnel-<id>` lines in authorized_keys."""
    out = {}
    try: text = Path(path).read_text()
    except OSError: return out
    for line in text.splitlines():
        m = re.search(r'(ssh-ed25519 [A-Za-z0-9+/=]+)\s+nocturne-tunnel-([a-z0-9-]+)\s*$', line)
        if m and 'permitopen=' in line: out[m.group(2)] = m.group(1)
    return out
