#!/usr/bin/env python3
"""Install worker tunnel keys from data/machines.json into ~/.ssh/authorized_keys.

Runs as a separate user service (see nocturne-tunnel-keys.path) so the gateway itself never
gets write access to ~/.ssh. Every key it writes gets the same fixed restriction: port
forwarding to 127.0.0.1:<worker port> only, no shell, no pty, no agent/X11 forwarding.
Lines outside the managed block are kept as they are, except old unmanaged
`nocturne-tunnel-<id>` lines for machines that are now in the registry.
"""
import argparse, base64, fcntl, json, os, re, shutil, sys, tempfile, time
from pathlib import Path

BEGIN, END = '# >>> nocturne tunnel keys (managed, do not edit) >>>', '# <<< nocturne tunnel keys <<<'
ID = re.compile(r'^[a-z0-9][a-z0-9-]{0,31}$')

def valid(pub):
    p = pub.split()
    if len(p) != 2 or p[0] != 'ssh-ed25519': return False
    try: blob = base64.b64decode(p[1], validate=True)
    except Exception: return False
    return blob[:15] == b'\x00\x00\x00\x0bssh-ed25519' and len(blob) == 51

def render(machines, port, false):
    out = []
    for wid, m in sorted(machines.items()):
        pub = m.get('tunnel_pubkey')
        if not ID.match(wid) or not m.get('enabled', True) or not pub: continue
        if not valid(pub): print(f'skip {wid}: invalid key', file=sys.stderr); continue
        out.append(f'restrict,port-forwarding,permitopen="127.0.0.1:{port}",permitlisten="none",command="{false}" {pub} nocturne-tunnel-{wid}')
    return out

def adopt_legacy(text, machines):
    """Return {id: pubkey} for old unmanaged tunnel lines whose machine has no key in the registry yet."""
    found = {}
    for line in text.splitlines():
        m = re.search(r'(ssh-ed25519 [A-Za-z0-9+/=]+)\s+nocturne-tunnel-([a-z0-9-]+)\s*$', line)
        if m and 'permitopen=' in line and m.group(2) in machines and not machines[m.group(2)].get('tunnel_pubkey') and valid(m.group(1)):
            found[m.group(2)] = m.group(1)
    return found

def merge(text, block, managed_ids):
    keep, inside = [], False
    for line in text.splitlines():
        if line == BEGIN: inside = True; continue
        if line == END: inside = False; continue
        if inside: continue
        m = re.search(r'\snocturne-tunnel-([a-z0-9-]+)\s*$', line)
        if m and m.group(1) in managed_ids and 'permitopen=' in line: continue  # legacy line now managed
        keep.append(line)
    while keep and not keep[-1].strip(): keep.pop()
    return '\n'.join(keep + ([''] if keep else []) + [BEGIN] + block + [END]) + '\n'

def stable_registry(path, quiet=0.2, timeout=2.0):
    """Read a quiescent complete snapshot; never publish keys from a torn input.

    Registry writes are atomic replacements. Including inode/mtime/size catches
    replacements even when the bytes match. A busy or malformed input fails
    without touching authorized_keys; the next path event can retry.
    """
    deadline = time.monotonic() + timeout
    previous = None
    while time.monotonic() < deadline:
        try:
            with path.open('rb') as f:
                before = os.fstat(f.fileno())
                raw = f.read()
                after = os.fstat(f.fileno())
            current = path.stat()
            signature = lambda st: (st.st_dev, st.st_ino, st.st_mtime_ns, st.st_size, st.st_ctime_ns)
            sample = (signature(after), raw)
            stable = signature(before) == signature(after) == signature(current)
            if stable and sample == previous:
                machines = json.loads(raw)
                if not isinstance(machines, dict) or any(
                    not ID.fullmatch(wid) or not isinstance(m, dict)
                    or ('enabled' in m and type(m['enabled']) is not bool)
                    or (m.get('tunnel_pubkey') is not None and not isinstance(m['tunnel_pubkey'], str))
                    for wid, m in machines.items()
                ):
                    raise ValueError('invalid registry shape')
                return machines
            previous = sample if stable else None
        except (OSError, ValueError):
            previous = None
        time.sleep(quiet)
    raise ValueError('machine registry not stable and valid; authorized_keys unchanged')

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--machines', default=str(Path.home() / 'nocturne/data/machines.json'))
    ap.add_argument('--authorized-keys', default=str(Path.home() / '.ssh/authorized_keys'))
    ap.add_argument('--port', type=int, default=8701)
    ap.add_argument('--dry-run', action='store_true')
    ap.add_argument('--settle-seconds', type=float, default=0, help='minimum coalescing delay (0..30 seconds)')
    a = ap.parse_args()
    if not 0 <= a.settle_seconds <= 30: ap.error('settle-seconds must be 0..30')
    time.sleep(a.settle_seconds)
    ak = Path(a.authorized_keys)
    # 8: one writer at a time (path unit can fire while a manual run is going).
    with open(ak.parent / '.nocturne-tunnel-keys.lock', 'w') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        machines = stable_registry(Path(a.machines))
        old = ak.read_text() if ak.exists() else ''
        legacy = adopt_legacy(old, machines)
        for wid, pub in legacy.items(): machines[wid] = dict(machines[wid], tunnel_pubkey=pub)
        false = shutil.which('false') or '/bin/false'
        block = render(machines, a.port, false)
        # Every registry machine owns its id, keyed or not, so a disabled/removed one is revoked.
        new = merge(old, block, set(machines))
        if a.dry_run: sys.stdout.write(new); return
        if legacy: print('adopted legacy tunnel keys: ' + ', '.join(sorted(legacy)) + '; the gateway imports these on first start', file=sys.stderr)
        if new == old: return
        fd, tmp = tempfile.mkstemp(dir=ak.parent, prefix='.authorized_keys.')
        try:
            with os.fdopen(fd, 'w') as f:
                os.fchmod(f.fileno(), 0o600); f.write(new); f.flush(); os.fsync(f.fileno())
            os.replace(tmp, ak)
            directory = os.open(ak.parent, os.O_RDONLY)
            try: os.fsync(directory)
            finally: os.close(directory)
        finally:
            try: os.unlink(tmp)
            except FileNotFoundError: pass
        print(f'installed {len(block)} tunnel keys')

if __name__ == '__main__':
    try: main()
    except (OSError, ValueError):
        print('tunnel key synchronization failed; check registry and filesystem', file=sys.stderr)
        sys.exit(1)
