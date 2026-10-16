# SPDX-License-Identifier: AGPL-3.0-or-later
"""No installation, live launchctl, privileged subprocess or inference in tests."""
from dataclasses import asdict
import json
from pathlib import Path
import socket
import sys
import threading

import pytest

from worker import benchmark_service as s


class FakeLaunchctl:
    def __init__(self):
        self.states = {label: {'loaded': True, 'disabled': False} for label in s.LABELS}
        self.calls = []
        self.before = lambda: None
        self.fail = None

    def state(self):
        return json.loads(json.dumps(self.states))

    def loaded(self, label):
        return self.states[label]['loaded']

    def mutate(self, verb, label):
        self.before()
        self.calls.append((verb, label))
        if self.fail == (verb, label):
            raise s.Refused('injected')
        if verb in ('disable', 'enable'):
            self.states[label]['disabled'] = verb == 'disable'
        else:
            self.states[label]['loaded'] = verb == 'bootstrap'


REAL_RUNNER_PEER = s.runner_peer
REAL_RUNNER_ABSENT = s.runner_absent
PEER = {'pid': 12345, 'uid': 501, 'start_sec': 100, 'start_usec': 42}


@pytest.fixture(autouse=True)
def peer_lifecycle(monkeypatch):
    # Inject lifecycle observations only in tests; production always uses Darwin.
    state = {'absent': True}
    monkeypatch.setattr(s, 'runner_peer', lambda conn: dict(PEER))
    monkeypatch.setattr(s, 'runner_absent', lambda peer: state['absent'])
    return state


def adapter(tmp_path, **kwargs):
    lc = FakeLaunchctl()
    a = s.ServiceAdapter(tmp_path, lc, lambda: 'generation', health=lambda: True,
                         closed=lambda port: True, absent=lambda: True, **kwargs)
    a.peer = dict(PEER)
    return a, lc


def test_journal_precedes_every_mutation_and_exact_restoration(tmp_path):
    a, lc = adapter(tmp_path)
    lc.states[s.LABELS[0]] = {'loaded': False, 'disabled': True}
    lc.states[s.LABELS[2]] = {'loaded': True, 'disabled': True}
    original = lc.state()
    prior = a.snapshot()
    def before():
        record = json.loads(a.journal.read_text())
        assert record['prior'] == asdict(prior)
        assert json.loads(record['prior']['config']) == original
    lc.before = before
    a.fence()
    assert lc.calls == [('disable', s.LLAMA), ('bootout', s.LLAMA)]
    assert json.loads(a.fence_path.read_text())['active'] is True
    assert a.drain() == s.DrainAck('generation', 0, True)
    a.restore(prior)
    assert a.verify(prior)
    assert lc.state() == original
    a.release()
    assert json.loads(a.journal.read_text()) == {'phase': 'idle'}
    assert a.journal.stat().st_mode & 0o777 == 0o600
    assert a.fence_path.stat().st_mode & 0o777 == 0o644
    assert json.loads(a.fence_path.read_text())['active'] is False
    assert all(label == s.LLAMA for _, label in lc.calls)


def test_partial_fence_recovers_after_restart(tmp_path):
    a, lc = adapter(tmp_path)
    prior = a.snapshot()
    lc.fail = ('bootout', s.LABELS[1])
    with pytest.raises(s.Refused):
        a.fence()
    lc.fail = None
    restarted = s.ServiceAdapter(tmp_path, lc, lambda: 'generation', health=lambda: True,
                                 closed=lambda _: True, absent=lambda: True)
    restarted.recover()
    assert lc.state() == json.loads(prior.config)
    assert restarted.record is None


def test_drain_and_restore_fail_closed_for_lingering_process(tmp_path):
    a, _ = adapter(tmp_path)
    prior = a.snapshot()
    with pytest.raises(s.Refused, match='not_fenced'):
        a.drain()
    a.fence()
    a.absent = lambda: False
    with pytest.raises(s.Refused, match='not_drained'):
        a.drain()
    with pytest.raises(s.Refused, match='inference_still_running'):
        a.restore(prior)
    assert a.record


def test_generation_and_health_required(tmp_path):
    a, _ = adapter(tmp_path)
    prior = a.snapshot()
    a.fence()
    a.fingerprint = lambda: 'changed'
    with pytest.raises(s.Refused, match='installation_changed'):
        a.restore(prior)
    a.fingerprint = lambda: prior.generation
    a.restore(prior)
    a.health = lambda: False
    assert not a.verify(prior)
    with pytest.raises(s.Refused, match='restoration_unverified'):
        a.release()
    assert a.record


@pytest.mark.parametrize('payload', [{'op': 'execute'}, {'op': 'fence', 'path': '/tmp/x'},
                                     {'op': ['snapshot']}, {'op': 'restore'}])
def test_rpc_rejects_untrusted_operations(tmp_path, payload):
    a, lc = adapter(tmp_path)
    left, right = socket.socketpair()
    with left, right:
        s.send(left, payload)
        with pytest.raises(s.Refused):
            s.session(right, a, 10)
    assert not lc.calls


def test_rpc_eof_restores(tmp_path):
    a, lc = adapter(tmp_path)
    original = lc.state()
    left, right = socket.socketpair()
    errors = []
    def run():
        with right:
            try:
                s.session(right, a, 10)
            except EOFError:
                pass
            except Exception as e:
                errors.append(e)
    t = threading.Thread(target=run)
    t.start()
    with left:
        for op in ('snapshot', 'fence', 'drain'):
            s.send(left, {'op': op})
            assert s.receive(left)['ok']
    t.join(3)
    assert not t.is_alive() and not errors
    assert lc.state() == original and a.record is None


def test_deadline_cannot_be_extended_by_bytes(tmp_path):
    a, _ = adapter(tmp_path)
    left, right = socket.socketpair()
    with left, right:
        left.sendall(b'{')
        with pytest.raises((TimeoutError, s.Refused)):
            s.session(right, a, .02)


def test_launchctl_allowlist_and_parser():
    calls = []
    def run(argv):
        calls.append(argv)
        if argv[1] == 'print-disabled':
            return 0, b'disabled services = {\n "io.nocturne.llama" => true\n}\n'
        return 113, b'Could not find service "io.nocturne.llama" in domain for system'
    lc = s.Launchctl(run)
    assert lc.disabled()['io.nocturne.llama'] is True
    assert lc.disabled()['io.nocturne.worker'] is False
    assert lc.loaded('io.nocturne.llama') is False
    with pytest.raises(s.Refused):
        lc.call('bootstrap', 'evil')
    with pytest.raises(s.Refused):
        lc.call('kickstart', s.LABELS[0])
    assert all(c[0] == '/bin/launchctl' for c in calls)
    lc = s.Launchctl(lambda argv: (1, b'permission denied'))
    with pytest.raises(s.Refused):
        lc.loaded(s.LABELS[0])
    with pytest.raises(s.Refused):
        lc.disabled()


def test_bounded_harmless_children():
    assert s.bounded([sys.executable, '-I', '-c', 'print("ok")'])[1] == b'ok\n'
    with pytest.raises(s.Refused, match='command_timeout'):
        s.bounded([sys.executable, '-I', '-c', 'import time; time.sleep(3)'], timeout=.05)
    with pytest.raises(s.Refused, match='command_output_limit'):
        s.bounded([sys.executable, '-I', '-c', 'print("x" * 10000)'], limit=100)


@pytest.mark.parametrize('raw', [b'{"op":1,"op":2}', b'{"v":NaN}', b'x' * 4097])
def test_strict_wire(raw):
    with pytest.raises(s.Refused):
        s.decode(raw)


def test_typed_config(tmp_path, monkeypatch):
    path = tmp_path / 'config'
    monkeypatch.setattr(s, 'protected', lambda p, **kw: Path(p))
    monkeypatch.setattr(s.pwd, 'getpwnam', lambda _: type('User', (), {'pw_uid': 501})())
    inventory = {'model_directory': '/models', 'server': '/bin/llama-server',
                 'code_revision': 'a' * 40}
    monkeypatch.setattr(s.Inventory, 'parse', classmethod(lambda cls, obj: cls(**obj)))
    path.write_text(json.dumps({'version': 1, 'worker_uid': 501, 'lease_seconds': 600,
                                'inventory': inventory}))
    assert s.InstallationConfig.read(path).worker_uid == 501
    for obj in ({'version': True, 'worker_uid': 501, 'lease_seconds': 600},
                {'version': 1, 'worker_uid': 0, 'lease_seconds': 600},
                {'version': 1, 'worker_uid': 501, 'lease_seconds': 601},
                {'version': 1, 'worker_uid': 501, 'lease_seconds': 600, 'command': 'evil'}):
        path.write_text(json.dumps({**obj, 'inventory': inventory}))
        with pytest.raises(s.Refused):
            s.InstallationConfig.read(path)


def test_acl_detection(monkeypatch):
    monkeypatch.setattr(s, 'bounded', lambda argv: (0, b'drwxr-xr-x+ 2 root wheel 64 Jan 1 x\n 0: user:write allow\n'))
    with pytest.raises(s.Refused, match='acl_not_allowed'):
        s.no_acl('/not-used')


def test_protected_rejects_links_writable_parents_and_acl(monkeypatch):
    # Simulated metadata only: never chmod/chown system directories.
    class Metadata:
        st_uid = 0
        st_mode = 0o40755
    def metadata(path):
        m = Metadata()
        if str(path) == '/safe/code':
            m.st_mode = 0o100644
        if str(path) == '/safe':
            m.st_mode = bad_mode
        return m
    monkeypatch.setattr(Path, 'lstat', metadata)
    monkeypatch.setattr(Path, 'stat', metadata)
    monkeypatch.setattr(s, 'no_acl', lambda _: None)
    for bad_mode in (0o40777, 0o120755):
        with pytest.raises(s.Refused):
            s.protected('/safe/code')
    bad_mode = 0o40755
    assert s.protected('/safe/code') == Path('/safe/code')


def test_readiness_real_http_contract_without_network(monkeypatch):
    paths = []
    class Response:
        status = 200
        def read(self, n):
            return b'{"status":"ok"}' if paths[-1] == '/health' else b'[{"is_processing":false}]'
    class HTTP:
        def __init__(self, host, port, timeout):
            assert (host, port) == ('127.0.0.1', 8080)
        def request(self, method, path):
            assert method == 'GET'
            paths.append(path)
        def getresponse(self):
            return Response()
        def close(self):
            pass
    monkeypatch.setattr(s.http.client, 'HTTPConnection', HTTP)
    assert s.readiness()
    assert paths == ['/health', '/slots']
    Response.status = 503
    assert not s.readiness()


@pytest.mark.parametrize('health,slots', [(b'{"status":"loading"}', b'[]'),
    (b'{"status":"ok"}', b'[]'), (b'{"status":"ok"}', b'[{"is_processing":true}]'),
    (b'{"status":"ok"}', b'[{"is_processing":0}]'),
    (b'{"status":"ok"}', b'[{}]'), (b'{"status":"ok"}', b'x' * 4097)])
def test_readiness_refuses_unhealthy_busy_or_malformed(monkeypatch, health, slots):
    class HTTP:
        status = 200
        def __init__(self, *args, **kwargs):
            pass
        def request(self, method, path):
            self.path = path
        def getresponse(self):
            return self
        def read(self, limit):
            assert limit == s.MAX_MESSAGE + 1
            return health if self.path == '/health' else slots
        def close(self):
            pass
    monkeypatch.setattr(s.http.client, 'HTTPConnection', HTTP)
    assert not s.readiness()


def test_shared_fence_fail_closed(tmp_path, monkeypatch):
    path = tmp_path / 'admission.json'
    monkeypatch.setattr(s, 'FENCE', path)
    monkeypatch.setattr(s, 'protected', lambda p, **kw: Path(p))
    assert s.ServiceClient.admission_fenced()
    for obj in ({'version': 1, 'active': True}, {'version': True, 'active': False},
                {'version': 1, 'active': 0}, {'active': False}, []):
        path.write_text(json.dumps(obj))
        assert s.ServiceClient.admission_fenced()
    s.durable(path, {'version': 1, 'active': False}, mode=0o644)
    assert not s.ServiceClient.admission_fenced()
    def unsafe(*args, **kwargs):
        raise s.Refused('untrusted_path')
    monkeypatch.setattr(s, 'protected', unsafe)
    assert s.ServiceClient.admission_fenced()


def test_fence_precedes_llama_mutation_and_survives_health_retry(tmp_path):
    a, lc = adapter(tmp_path)
    prior = a.snapshot()
    def before():
        assert json.loads(a.fence_path.read_text())['active'] is True
    lc.before = before
    a.fence()
    a.health = lambda: False
    with pytest.raises(s.Refused, match='restoration_unverified'):
        a.recover()
    assert json.loads(a.fence_path.read_text())['active'] is True
    a.health = lambda: True
    a.recover()
    assert not json.loads(a.fence_path.read_text())['active']
    assert lc.state() == json.loads(prior.config)
    assert all(label == s.LLAMA for _, label in lc.calls)


def test_disconnect_with_child_retains_fence_without_bootstrap(tmp_path):
    a, lc = adapter(tmp_path)
    a.fence()
    a.absent = lambda: False
    left, right = socket.socketpair()
    left.close()
    with right, pytest.raises(s.Refused, match='inference_still_running'):
        s.session(right, a, 10)
    assert a.record
    assert json.loads(a.fence_path.read_text())['active']
    assert not any(verb == 'bootstrap' for verb, _ in lc.calls)


def test_changed_worker_state_is_not_repaired_or_released(tmp_path):
    a, lc = adapter(tmp_path)
    a.fence()
    lc.states[s.LABELS[0]]['loaded'] = False
    with pytest.raises(s.Refused, match='restoration_unverified'):
        a.recover()
    assert all(label == s.LLAMA for _, label in lc.calls)
    assert json.loads(a.fence_path.read_text())['active']


def test_unloaded_prior_requires_absent_children_at_release(tmp_path):
    a, lc = adapter(tmp_path)
    lc.states[s.LLAMA]['loaded'] = False
    prior = a.snapshot()
    a.fence()
    a.restore(prior)
    a.absent = lambda: False
    with pytest.raises(s.Refused, match='restoration_unverified'):
        a.release()
    assert json.loads(a.fence_path.read_text())['active']


def test_mutations_cannot_target_worker_or_tunnel():
    lc = s.Launchctl(lambda argv: pytest.fail('must not execute'))
    for label in (s.LABELS[0], s.LABELS[2]):
        for verb in ('disable', 'enable', 'bootstrap', 'bootout'):
            with pytest.raises(s.Refused, match='command_not_allowed'):
                lc.call(verb, label)


def test_inventory_validates_operator_paths(tmp_path, monkeypatch):
    server = tmp_path / 'llama-server'
    server.write_text('not executed')
    server.chmod(0o755)
    seen = []
    def protected(path, **kwargs):
        seen.append((path, kwargs))
        return Path(path)
    monkeypatch.setattr(s, 'protected', protected)
    obj = {'model_directory': str(tmp_path), 'server': str(server), 'code_revision': 'a' * 40}
    assert asdict(s.Inventory.parse(obj)) == obj
    assert seen == [(str(tmp_path), {'directory': True}), (str(server), {})]
    for bad in ({**obj, 'code_revision': 'main'}, {**obj, 'argv': []},
                {**obj, 'server': None}):
        with pytest.raises(s.Refused):
            s.Inventory.parse(bad)
    server.chmod(0o644)
    with pytest.raises(s.Refused, match='inventory_server'):
        s.Inventory.parse(obj)


@pytest.mark.parametrize('enabled', [False, True])
def test_readonly_inventory_and_execution_capability(tmp_path, enabled):
    a, lc = adapter(tmp_path)
    inventory = s.Inventory('/trusted/models', '/trusted/bin/llama-server', 'a' * 40)
    left, right = socket.socketpair()
    errors = []
    def run():
        with right:
            try:
                s.session(right, a, 10, inventory, lambda: enabled)
            except EOFError:
                pass
            except Exception as exc:
                errors.append(exc)
    thread = threading.Thread(target=run)
    thread.start()
    with left:
        s.send(left, {'op': 'inventory'})
        assert s.receive(left)['value'] == asdict(inventory)
        s.send(left, {'op': 'ping'})
        assert s.receive(left)['value'] == {'service_control': True, 'benchmark_execution': enabled}
    thread.join(3)
    assert not thread.is_alive() and not errors and not lc.calls


def test_fresh_startup_health_required_before_admission(tmp_path):
    a, lc = adapter(tmp_path)
    a.health = lambda: False
    with pytest.raises(s.Refused, match='startup_health_required'):
        a.recover()
    assert json.loads(a.fence_path.read_text())['active']
    assert not lc.calls
    a.health = lambda: True
    a.recover()
    assert not json.loads(a.fence_path.read_text())['active']
    assert not lc.calls


def test_fresh_unloaded_baseline_requires_absence(tmp_path):
    a, lc = adapter(tmp_path)
    lc.states[s.LLAMA]['loaded'] = False
    a.absent = lambda: False
    with pytest.raises(s.Refused, match='startup_health_required'):
        a.recover()
    assert json.loads(a.fence_path.read_text())['active']


def test_execution_optin_and_manifest_rechecked(monkeypatch):
    inv = s.Inventory('/models', '/bin/llama-server', 'a' * 40)
    cfg = s.InstallationConfig(1, 501, 600, inv)
    monkeypatch.setattr(s, 'execution_digest', lambda _: pytest.fail('disabled must not inspect'))
    assert not s.execution_capability(cfg)
    cfg = s.InstallationConfig(1, 501, 600, inv, True, 'a' * 64)
    monkeypatch.setattr(s, 'execution_digest', lambda _: 'a' * 64)
    assert s.execution_capability(cfg)
    monkeypatch.setattr(s, 'execution_digest', lambda _: 'b' * 64)
    assert not s.execution_capability(cfg)


def test_pth_explicit_provision_and_content_hash(tmp_path, monkeypatch):
    module = tmp_path / 'runner.py'
    module.write_text('# reviewed')
    pth = tmp_path / s.PTH_NAME
    monkeypatch.setattr(s, 'execution_files', lambda _: [module])
    monkeypatch.setattr(s, 'execution_pth', lambda: pth)
    monkeypatch.setattr(s, 'protected', lambda p, **kw: Path(p))
    with pytest.raises(FileNotFoundError):
        s.execution_digest(None)
    assert not pth.exists()
    digest = s.execution_digest(None, provision=True)
    assert pth.read_bytes() == b'/opt/nocturne\n'
    assert digest == s.execution_digest(None)
    module.write_text('# changed')
    assert digest != s.execution_digest(None)
    pth.write_text('import evil\n')
    with pytest.raises(s.Refused, match='runtime_pth'):
        s.execution_digest(None, provision=True)
    assert pth.read_text() == 'import evil\n'


def test_no_pth_write_before_verification(tmp_path, monkeypatch):
    def refuse(_):
        raise s.Refused('unsafe')
    monkeypatch.setattr(s, 'execution_files', refuse)
    monkeypatch.setattr(s, 'execution_pth', lambda: pytest.fail('must verify first'))
    with pytest.raises(s.Refused, match='unsafe'):
        s.execution_digest(None, provision=True)


def test_runtime_checks_intermediate_symlink_owner(monkeypatch):
    import stat
    paths = []
    class Meta:
        st_uid = 0
        st_mode = stat.S_IFDIR | 0o755
    def meta(path):
        paths.append(str(path))
        m = Meta()
        if str(path) == '/opt/nocturne/.venv/bin/python':
            m.st_mode = stat.S_IFLNK | 0o777
        if str(path) == '/opt/nocturne/python/hop':
            m.st_mode = stat.S_IFLNK | 0o777
            m.st_uid = 501
        return m
    monkeypatch.setattr(Path, 'lstat', meta)
    monkeypatch.setattr(s, 'no_acl', lambda _: None)
    monkeypatch.setattr(s, 'protected', lambda p, **kw: Path(p))
    monkeypatch.setattr(s.os, 'readlink', lambda _: '/opt/nocturne/python/hop')
    with pytest.raises(s.Refused, match='runtime_permissions'):
        s.protected_runtime('/opt/nocturne/.venv/bin/python')
    assert '/opt/nocturne/python/hop' in paths


def test_runtime_link_boundary_and_loop(monkeypatch):
    import stat
    class Meta:
        st_uid = 0
        st_mode = stat.S_IFDIR | 0o755
    def meta(path):
        m = Meta()
        if path.name == 'python' and str(path).startswith('/opt/nocturne/.venv/bin'):
            m.st_mode = stat.S_IFLNK | 0o777
        return m
    monkeypatch.setattr(Path, 'lstat', meta)
    monkeypatch.setattr(s, 'protected', lambda p, **kw: Path(p))
    monkeypatch.setattr(s, 'no_acl', lambda _: None)
    monkeypatch.setattr(s.os, 'readlink', lambda _: '/tmp/python')
    with pytest.raises(s.Refused, match='runtime_boundary'):
        s.protected_runtime('/opt/nocturne/.venv/bin/python')
    monkeypatch.setattr(s.os, 'readlink', lambda _: '/opt/nocturne/.venv/bin/python')
    with pytest.raises(s.Refused, match='runtime_link'):
        s.protected_runtime('/opt/nocturne/.venv/bin/python')


@pytest.mark.parametrize('enabled,manifest', [(1, None), (True, None),
                         (True, 'not-a-digest'), (False, 'a' * 64)])
def test_execution_config_types(tmp_path, monkeypatch, enabled, manifest):
    monkeypatch.setattr(s, 'protected', lambda p, **kw: Path(p))
    monkeypatch.setattr(s.Inventory, 'parse', classmethod(lambda cls, obj: cls(**obj)))
    obj = asdict(s.InstallationConfig(1, 501, 600,
                 s.Inventory('/models', '/bin/llama-server', 'a' * 40)))
    obj.update(enable_execution=enabled, execution_manifest=manifest)
    path = tmp_path / 'config'
    path.write_text(json.dumps(obj))
    with pytest.raises(s.Refused, match='execution_schema'):
        s.InstallationConfig.read(path)


@pytest.mark.parametrize('failure', [None, 'pid', 'executable', 'missing', 'start', 'busy',
                                     'fence', 'loaded', 'enabled', 'changed_after'])
def test_admission_attests_only_stable_managed_worker(tmp_path, monkeypatch, failure):
    a, lc = adapter(tmp_path)
    with pytest.raises(s.Refused, match='not_fenced'):
        a.admission()
    a.fence()
    executable = tmp_path / 'python'
    executable.write_text('never executed')
    executable.chmod(0o755)
    monkeypatch.setattr(s, 'protected_runtime', lambda p: executable)
    pids = iter([123, 124 if failure == 'pid' else 123])
    lc.worker_pid = lambda: next(pids)
    calls = []
    start = 'Mon Jan  5 12:34:56 2026'
    def run(argv, **kwargs):
        calls.append(argv)
        assert kwargs == {'timeout': 2, 'limit': 4096}
        if failure == 'missing':
            return 1, b''
        value = str(executable) if argv[-1] == 'comm=' else start
        if failure == 'executable' and argv[-1] == 'comm=':
            value = '/tmp/python'
        if failure == 'start' and argv[-1] == 'lstart=':
            value = ''
        if failure == 'changed_after' and argv[-1] == 'lstart=':
            lc.states[s.LLAMA]['loaded'] = True
        return 0, (value + '\n').encode()
    monkeypatch.setattr(s, 'bounded', run)
    if failure == 'busy':
        a.absent = lambda: False
        a.closed = lambda _: False
    if failure == 'fence':
        a.publish_fence(False)
    if failure == 'loaded':
        lc.states[s.LLAMA]['loaded'] = True
    if failure == 'enabled':
        lc.states[s.LLAMA]['disabled'] = False
    mutations = list(lc.calls)
    if failure and failure != 'busy':
        with pytest.raises(s.Refused):
            a.admission()
    else:
        assert a.admission() == {'worker_pid': 123, 'worker_executable': str(executable),
                                 'worker_start': start}
        assert calls == [['/bin/ps', '-p', '123', '-o', field]
                         for field in ('comm=', 'lstart=')]
    assert lc.calls == mutations


@pytest.mark.parametrize('rc,out', [(113, b'Could not find service'), (0, b'pid = 0\n'),
                         (0, b'pid = 12\npid = 13\n'), (0, b'pid = 99999999999\n')])
def test_worker_pid_refuses_unloaded_or_ambiguous(rc, out):
    with pytest.raises(s.Refused):
        s.Launchctl(lambda argv: (rc, out)).worker_pid()


def test_worker_pid_fixed_launchctl():
    def run(argv):
        assert argv == ['/bin/launchctl', 'print', 'system/io.nocturne.worker']
        return 0, b'system/io.nocturne.worker = {\n\tpid = 123\n}\n'
    assert s.Launchctl(run).worker_pid() == 123


def test_admission_rpc_and_client(tmp_path):
    a, _ = adapter(tmp_path)
    attestation = {'worker_pid': 123, 'worker_executable': '/protected/python',
                   'worker_start': 'Mon Jan  5 12:34:56 2026'}
    def admission():
        a.drain()
        return attestation
    a.admission = admission
    left, right = socket.socketpair()
    errors = []
    def run():
        with right:
            try:
                s.session(right, a, 10, capability=lambda: True)
            except EOFError:
                pass
            except Exception as exc:
                errors.append(exc)
    thread = threading.Thread(target=run)
    thread.start()
    client = s.ServiceClient.__new__(s.ServiceClient)
    client.conn = left
    try:
        client.snapshot()
        client.fence()
        client.drain()
        assert client.admission() == attestation
    finally:
        client.close()
        thread.join(3)
    assert not thread.is_alive() and not errors


def test_ping_attests_once_per_session(tmp_path):
    a, _ = adapter(tmp_path)
    calls = []
    left, right = socket.socketpair()
    with left, right:
        for _ in range(3):
            s.send(left, {'op': 'ping'})
        left.shutdown(socket.SHUT_WR)
        with pytest.raises(EOFError):
            s.session(right, a, 10, capability=lambda: calls.append(True) or True)
        for _ in range(3):
            assert s.receive(left)['value']['benchmark_execution'] is True
    assert calls == [True]


@pytest.mark.parametrize('payload', [{'op': 'admission', 'pid': 123},
                                    {'op': 'admission', 'path': '/tmp/python'},
                                    {'op': 'admission'}])
def test_admission_rpc_requires_transaction_and_no_parameters(tmp_path, payload):
    a, _ = adapter(tmp_path)
    left, right = socket.socketpair()
    with left, right:
        s.send(left, payload)
        with pytest.raises(s.Refused):
            s.session(right, a, 10, capability=lambda: True)


def test_verify_is_read_only(monkeypatch):
    monkeypatch.setattr(s, 'verify_installation', lambda: (None, 'generation'))
    monkeypatch.setattr(s, 'bounded', lambda *a, **k: pytest.fail('unexpected subprocess'))
    monkeypatch.setattr(sys, 'argv', ['benchmark_service.py', 'verify'])
    from types import SimpleNamespace
    monkeypatch.setattr(sys, 'flags', SimpleNamespace(isolated=1, no_site=1))
    assert s.main() == 0


@pytest.fixture
def rpc_service(tmp_path):
    a, lc = adapter(tmp_path)
    left, right = socket.socketpair()
    errors = []
    def run():
        with right:
            try:
                s.session(right, a, 10, s.Inventory('/models', '/server', 'revision'),
                          capability=lambda: True)
            except EOFError:
                pass
            except Exception as exc:
                errors.append(exc)
    thread = threading.Thread(target=run)
    thread.start()
    client = s.ServiceClient.__new__(s.ServiceClient)
    client.conn = left
    client.prior = None
    try:
        yield client, a, lc, errors
    finally:
        client.close()
        thread.join(3)
        assert not thread.is_alive()


def test_production_prefence_failure_completes_unchanged(tmp_path, monkeypatch, rpc_service):
    import asyncio
    from worker import benchmark_production as p
    from worker.benchmark_executor import Job
    client, a, lc, errors = rpc_service
    runner = tmp_path / 'runner'
    runner.mkdir(mode=0o700)
    monkeypatch.setattr(p, 'journal_root', lambda: runner)
    job = Job(id='job', machine='worker', options={},
              provenance={'config_generation': 'stale'})
    p.atomic(runner / 'journal.json', {'binding': p.binding(job)})
    saves = []
    original = a.save
    def save():
        original()
        saves.append(json.loads(a.journal.read_text()))
    a.save = save
    assert asyncio.run(p.transaction(job, client)) == {'restored': True, 'summary': None}
    assert not lc.calls and not errors
    receipt = saves[0]
    assert receipt['completion'] == 'unchanged'
    assert receipt['verified_prior'] == receipt['prior'] == asdict(client.prior)
    assert json.loads((runner / 'journal.json').read_text())['verified_prior'] == receipt['prior']
    assert json.loads(a.fence_path.read_text())['active'] is False


@pytest.mark.parametrize('change', ['state', 'generation', 'health', 'during_health'])
def test_no_mutation_completion_refuses_changed_snapshot(rpc_service, change):
    client, a, lc, errors = rpc_service
    client.snapshot()
    if change == 'state':
        lc.states[s.LABELS[2]]['disabled'] = True
    elif change == 'generation':
        a.fingerprint = lambda: 'new'
    elif change == 'health':
        a.health = lambda: False
    else:
        def health():
            lc.states[s.LABELS[2]]['disabled'] = True
            return True
        a.health = health
    with pytest.raises((EOFError, OSError, s.Refused)):
        client.restore(client.prior)
    assert not lc.calls
    assert not a.journal.exists()


def test_no_mutation_rpc_cannot_supply_old_prior(rpc_service):
    client, a, lc, errors = rpc_service
    prior = client.snapshot()
    s.send(client.conn, {'op': 'restore', 'prior': asdict(prior)})
    with pytest.raises(EOFError):
        s.receive(client.conn)
    assert not lc.calls and not a.journal.exists()


@pytest.mark.parametrize('changed', [False, True])
def test_unchanged_receipt_recovery_never_mutates_services(tmp_path, changed):
    a, lc = adapter(tmp_path)
    prior = a.snapshot()
    a.complete_unchanged(prior)
    restarted = s.ServiceAdapter(tmp_path, lc, lambda: 'generation', health=lambda: True)
    if changed:
        lc.states[s.LLAMA]['disabled'] = True
        with pytest.raises(s.Refused, match='restoration_unverified'):
            restarted.recover()
        assert restarted.record['verified_prior'] == asdict(prior)
    else:
        restarted.recover()
        assert restarted.record is None
    assert not lc.calls


@pytest.mark.parametrize('closed,absent', [(True, True), (False, True), (True, False)])
def test_unchanged_unloaded_prior_requires_absence(tmp_path, closed, absent):
    a, lc = adapter(tmp_path)
    lc.states[s.LLAMA]['loaded'] = False
    prior = a.snapshot()
    a.health = lambda: pytest.fail('unloaded service has no health endpoint')
    a.closed = lambda _: closed
    a.absent = lambda: absent
    if closed and absent:
        a.complete_unchanged(prior)
        assert a.verify(prior)
        a.release()
    else:
        with pytest.raises(s.Refused, match='unchanged_health_required'):
            a.complete_unchanged(prior)
        assert not a.journal.exists()
    assert not lc.calls


@pytest.mark.parametrize('isolated,no_site', [(0, 1), (1, 0)])
def test_root_entry_refuses_unsafe_flags(monkeypatch, isolated, no_site):
    from types import SimpleNamespace
    monkeypatch.setattr(sys, 'argv', ['benchmark_service.py', 'verify'])
    monkeypatch.setattr(sys, 'flags', SimpleNamespace(isolated=isolated, no_site=no_site))
    monkeypatch.setattr(s, 'verify_installation', lambda: pytest.fail('unsafe runtime'))
    assert s.main() == 1


def test_root_argv_disables_site():
    assert s.daemon_plist()['ProgramArguments'] == [
        '/usr/bin/python3', '-I', '-S', str(s.CODE), 'serve']


# Reuse the synthetic macOS sensors, but NOT its mocked root admission.
from test_benchmark_installed_probe import host


def test_owned_live_child_composes_with_root_admission(host, tmp_path, monkeypatch):
    import asyncio
    import subprocess
    from worker import model_benchmark as b
    state, probe = host
    a, lc = adapter(tmp_path)
    prior = a.snapshot()
    a.fence()
    a.drain()  # Initial absence remains mandatory.
    executable = tmp_path / 'python'
    executable.write_text('not executed')
    executable.chmod(0o755)
    monkeypatch.setattr(s, 'protected_runtime', lambda _: executable)
    lc.worker_pid = lambda: 42
    monkeypatch.setattr(s, 'bounded', lambda argv, **kw: (
        0, (str(executable) if argv[-1] == 'comm=' else 'Mon Jan 1 12:00:00 2026').encode()))
    # The probe's independently observed identity must match root's attestation.
    original_command = b.command
    async def command(*args, **kwargs):
        text = await original_command(*args, **kwargs)
        text = text.replace('/opt/nocturne/python', str(executable))
        if '99 1024 /other/llama-server' in state['extra'] and 'lstart' in args[-1]:
            text = text.replace('99 Mon Jan 1 12:00:00 2026 /usr/bin/python3',
                                '99 Mon Jan 1 12:00:00 2026 /other/llama-server')
        return text
    monkeypatch.setattr(b, 'command', command)
    probe.service.call = lambda op: a.admission()
    child = subprocess.Popen([sys.executable, '-I', '-S', '-c',
                              'import time; time.sleep(60)'])
    try:
        assert child.poll() is None
        a.absent = lambda: child.poll() is not None
        a.closed = lambda _: child.poll() is not None
        state['extra'] = f'{child.pid} 1024 /protected/llama-server\n'
        snapshot = asyncio.run(probe(str(tmp_path), owned_pid=child.pid))
        assert snapshot['blockers'] == []
        with pytest.raises(s.Refused, match='not_drained'):
            a.drain()
        state['extra'] += '99 1024 /other/llama-server\n'
        assert 'processes_present' in asyncio.run(
            probe(str(tmp_path), owned_pid=child.pid))['blockers']
        with pytest.raises(s.Refused, match='inference_still_running'):
            a.restore(prior)
    finally:
        child.terminate()
        child.wait(timeout=5)
    a.restore(prior)
    assert a.verify(prior)
    a.release()


@pytest.mark.parametrize('ending', ['deadline', 'eof'])
def test_delayed_spawn_retains_fence_through_restart(tmp_path, peer_lifecycle, ending):
    import subprocess
    peer_lifecycle['absent'] = False
    a, lc = adapter(tmp_path)
    left, right = socket.socketpair()
    errors = []
    def run():
        with right:
            try:
                s.session(right, a, .15 if ending == 'deadline' else 10)
            except (s.Refused, EOFError, OSError) as exc:
                errors.append(type(exc))
    thread = threading.Thread(target=run)
    thread.start()
    child = None
    try:
        for op in ('snapshot', 'fence', 'drain'):
            s.send(left, {'op': op})
            assert s.receive(left)['ok']
        # Successful final pre-spawn observation, then delayed OS spawn. The
        # root connection expires/closes while there is still no child at all.
        if ending == 'eof':
            left.close()
        thread.join(3)
        assert not thread.is_alive() and errors
        assert a.record['phase'] == 'restore_pending'
        assert a.record['runner_session'] == 'active'
        assert a.record['peer'] == PEER
        def restart():
            return s.ServiceAdapter(tmp_path, lc, lambda: 'generation',
                                    health=lambda: True, closed=lambda _: True,
                                    absent=lambda: child is None or child.poll() is not None)
        restarted = restart()
        with pytest.raises(s.Refused, match='runner_still_alive'):
            restarted.recover()
        assert lc.states[s.LLAMA] == {'loaded': False, 'disabled': True}
        assert json.loads(a.fence_path.read_text())['active'] is True
        # A delayed spawn can still complete after heartbeat cancellation. The
        # interlock, not cancellation timing, must protect admission.
        child = subprocess.Popen([sys.executable, '-I', '-S', '-c',
                                  'import time; time.sleep(60)'])
        assert child.poll() is None
        assert lc.states[s.LLAMA] == {'loaded': False, 'disabled': True}
        peer_lifecycle['absent'] = True  # killed runner with orphan child
        with pytest.raises(s.Refused, match='inference_still_running'):
            restart().recover()
        assert json.loads(a.fence_path.read_text())['active'] is True
        child.terminate()
        child.wait(timeout=5)
        restart().recover()
        assert lc.states[s.LLAMA] == {'loaded': True, 'disabled': False}
        assert json.loads(a.fence_path.read_text())['active'] is False
    finally:
        left.close()
        thread.join(3)
        if child is not None and child.poll() is None:
            child.terminate()
            child.wait(timeout=5)


def test_deadline_during_cancelled_backend_spawn(tmp_path, peer_lifecycle, monkeypatch):
    import asyncio
    from worker import model_benchmark as b
    peer_lifecycle['absent'] = False
    a, lc = adapter(tmp_path)
    left, right = socket.socketpair()
    def root():
        with right:
            try:
                s.session(right, a, .15)
            except (s.Refused, OSError, EOFError):
                pass
    thread = threading.Thread(target=root)
    thread.start()
    async def exercise():
        entered, resume = asyncio.Event(), asyncio.Event()
        spawn = asyncio.create_subprocess_exec
        async def delayed(*args, **kwargs):
            entered.set()
            await resume.wait()
            return await spawn(sys.executable, '-I', '-S', '-c',
                               'import time; time.sleep(60)', **kwargs)
        monkeypatch.setattr(asyncio, 'create_subprocess_exec', delayed)
        backend = b.Backend({'server': '/not-executed'}, {'path': '/not-read'},
                            {'context': 32, 'parallel': 1, 'spec_n': 0}, str(tmp_path))
        for op in ('snapshot', 'fence', 'drain'):
            s.send(left, {'op': op})
            assert s.receive(left)['ok']
        task = asyncio.create_task(backend.start())
        try:
            await entered.wait()
            await asyncio.to_thread(thread.join, 3)
            assert not thread.is_alive()
            assert a.record['phase'] == 'restore_pending'
            task.cancel()  # heartbeat detects the closed root socket
            await asyncio.sleep(0)
            resume.set()  # cancellation cannot undo an already pending spawn
            with pytest.raises(asyncio.CancelledError):
                await task
            assert backend.process.returncode is None
            assert not lc.loaded(s.LLAMA)
            assert json.loads(a.fence_path.read_text())['active'] is True
        finally:
            resume.set()
            await backend.stop()
        peer_lifecycle['absent'] = True
        a.recover()
        assert lc.loaded(s.LLAMA)
    try:
        asyncio.run(exercise())
    finally:
        left.close()
        thread.join(3)


def test_explicit_restore_closes_live_runner_session(tmp_path, peer_lifecycle):
    peer_lifecycle['absent'] = False
    a, lc = adapter(tmp_path)
    left, right = socket.socketpair()
    errors = []
    def run():
        with right:
            try:
                s.session(right, a, 10)
            except Exception as exc:
                errors.append(type(exc))
    thread = threading.Thread(target=run)
    thread.start()
    with left:
        for op in ('snapshot', 'fence', 'drain', 'restore', 'verify', 'release'):
            s.send(left, {'op': op})
            assert s.receive(left)['ok']
    thread.join(3)
    assert not thread.is_alive() and not errors
    assert a.record is None and lc.loaded(s.LLAMA)


def test_kernel_peer_binding_and_source_attestation(tmp_path, monkeypatch):
    from types import SimpleNamespace
    monkeypatch.setattr(s.sys, 'platform', 'darwin')
    monkeypatch.setattr(s.InstallationConfig, 'read', lambda: SimpleNamespace(worker_uid=501))
    monkeypatch.setattr(s, 'execution_capability', lambda cfg: True)
    monkeypatch.setattr(s, 'peer_uid', lambda conn: 501)
    monkeypatch.setattr(s, 'process_identity', lambda pid: dict(PEER))
    monkeypatch.setattr(s, 'protected_runtime', lambda path: Path('/trusted/python'))
    outputs = {'comm=': b'/trusted/python',
               'args=': b'/opt/nocturne/.venv/bin/python -I -m worker.benchmark_production'}
    monkeypatch.setattr(s, 'bounded', lambda argv, **kw: (0, outputs[argv[-1]]))
    class Conn:
        def getsockopt(self, level, option):
            assert (level, option) == (0, 2)
            return PEER['pid']
    assert REAL_RUNNER_PEER(Conn()) == PEER
    outputs['args='] = b'/opt/nocturne/.venv/bin/python -m worker.agent'
    with pytest.raises(s.Refused, match='runner_source_unverified'):
        REAL_RUNNER_PEER(Conn())
    monkeypatch.setattr(s, 'peer_uid', lambda conn: 0)
    with pytest.raises(s.Refused, match='peer_identity_unverified'):
        REAL_RUNNER_PEER(Conn())


def test_snapshot_only_live_agent_needs_no_exit(tmp_path, peer_lifecycle):
    peer_lifecycle['absent'] = False
    a, lc = adapter(tmp_path)
    left, right = socket.socketpair()
    with left, right:
        s.send(left, {'op': 'snapshot'})
        left.shutdown(socket.SHUT_WR)
        with pytest.raises(EOFError):
            s.session(right, a, 10)
    assert a.record is None and not lc.calls
    assert json.loads(a.fence_path.read_text())['active'] is False


def test_persisted_peer_reuse_and_query_failure_fail_closed(tmp_path, monkeypatch):
    # Undo only the test absence observer to exercise the real comparison.
    monkeypatch.setattr(s, 'runner_absent', REAL_RUNNER_ABSENT)
    a, lc = adapter(tmp_path)
    a.fence()
    monkeypatch.setattr(s, 'process_identity', lambda pid: {**PEER, 'start_usec': 43})
    with pytest.raises(s.Refused, match='peer_identity_changed'):
        a.recover()
    assert not lc.loaded(s.LLAMA)
    def failed(pid):
        raise s.Refused('peer_identity_unverified')
    monkeypatch.setattr(s, 'process_identity', failed)
    with pytest.raises(s.Refused, match='peer_identity_unverified'):
        a.recover()
    assert json.loads(a.fence_path.read_text())['active'] is True
