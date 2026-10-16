"""Offline watcher regressions with disposable registry/SSH files only."""
import base64
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import threading
import time

import pytest

TOOL = Path(__file__).resolve().parents[1] / 'tools/sync-tunnel-keys.py'
spec = importlib.util.spec_from_file_location('tunnel_sync', TOOL)
sync = importlib.util.module_from_spec(spec)
spec.loader.exec_module(sync)
PUB = 'ssh-ed25519 ' + base64.b64encode(b'\0\0\0\x0bssh-ed25519\0\0\0\x20' + b'x'*32).decode()


def atomic(path, value):
    tmp = path.with_suffix('.tmp')
    tmp.write_text(json.dumps(value))
    os.replace(tmp, path)


def run_tool(registry, keys, delay='0'):
    return subprocess.run([sys.executable, str(TOOL), '--machines', str(registry),
                           '--authorized-keys', str(keys), '--settle-seconds', delay],
                          capture_output=True, timeout=8)


def test_burst_atomic_replace_final_state_and_no_self_trigger(tmp_path):
    registry, keys = tmp_path/'machines.json', tmp_path/'authorized_keys'
    keys.write_text('operator-key untouched\n')
    atomic(registry, {})
    def burst():
        for i in range(30):
            atomic(registry, {'test': {'enabled': i != 29, 'tunnel_pubkey': PUB}})
            time.sleep(.005)
    writer = threading.Thread(target=burst)
    writer.start()
    started = time.monotonic()
    result = run_tool(registry, keys, '.3')
    writer.join()
    assert result.returncode == 0, result.stderr
    assert .3 <= time.monotonic()-started < 3
    assert 'nocturne-tunnel-test' not in keys.read_text()
    assert keys.read_text().startswith('operator-key untouched\n')
    before = (registry.stat().st_ino, registry.stat().st_mtime_ns, keys.stat().st_ino, keys.stat().st_mtime_ns)
    assert run_tool(registry, keys).returncode == 0
    assert before == (registry.stat().st_ino, registry.stat().st_mtime_ns, keys.stat().st_ino, keys.stat().st_mtime_ns)
    assert keys.stat().st_mode & 0o777 == 0o600
    assert not list(tmp_path.glob('.authorized_keys.*'))


@pytest.mark.parametrize('bad', ['{', '[]', '{"a": []}', '{"a": {"enabled": "false"}}', '{"a": {"tunnel_pubkey": 2}}'])
def test_bad_input_fails_closed(tmp_path, bad):
    registry, keys = tmp_path/'machines.json', tmp_path/'authorized_keys'
    registry.write_text(bad)
    keys.write_text('keep exactly\n')
    original = keys.stat().st_ino
    r = run_tool(registry, keys)
    assert r.returncode == 1
    assert keys.read_text() == 'keep exactly\n' and keys.stat().st_ino == original
    assert b'Traceback' not in r.stderr


def test_partial_write_then_atomic_recovery(tmp_path):
    registry, keys = tmp_path/'machines.json', tmp_path/'authorized_keys'
    registry.write_text('{')
    keys.write_text('operator\n')
    writer = threading.Timer(.35, atomic, args=(registry, {'test': {'enabled': True, 'tunnel_pubkey': PUB}}))
    writer.start()
    r = run_tool(registry, keys)
    writer.join()
    assert r.returncode == 0, r.stderr
    assert 'nocturne-tunnel-test' in keys.read_text()
    assert 'permitlisten="none"' in keys.read_text()
    atomic(registry, {})
    assert run_tool(registry, keys).returncode == 0
    assert 'nocturne-tunnel-test' not in keys.read_text()


def test_continuous_atomic_replacements_are_bounded(tmp_path):
    registry = tmp_path/'machines.json'
    atomic(registry, {})
    stop = threading.Event()
    def churn():
        while not stop.is_set():
            atomic(registry, {})
            time.sleep(.005)
    writer = threading.Thread(target=churn)
    writer.start()
    try:
        started = time.monotonic()
        with pytest.raises(ValueError): sync.stable_registry(registry, quiet=.02, timeout=.15)
        assert time.monotonic()-started < .5
    finally:
        stop.set(); writer.join()
    assert sync.stable_registry(registry, quiet=.02) == {}


def test_service_limits_exceed_bounded_activation_rate():
    service = (TOOL.parents[1]/'systemd/nocturne-tunnel-keys.service').read_text()
    assert '--settle-seconds 3' in service
    assert 'StartLimitIntervalSec=30' in service and 'StartLimitBurst=15' in service
