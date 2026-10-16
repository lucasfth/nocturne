# SPDX-License-Identifier: AGPL-3.0-or-later
"""Disposable real supervisor files; no installation or live service calls."""
import hashlib
import json
from pathlib import Path
import socket
import threading

import pytest
from worker import benchmark_service as s


class FakeLaunchctl:
    def __init__(self):
        self.states = {x: {'loaded': True, 'disabled': False} for x in s.LABELS}
        self.calls = []
        self.before = lambda: None

    def state(self):
        return json.loads(json.dumps(self.states))

    def loaded(self, label):
        return self.states[label]['loaded']

    def mutate(self, verb, label):
        self.before()
        assert label == s.LLAMA
        self.calls.append(verb)
        self.states[label]['disabled' if verb in ('enable', 'disable') else 'loaded'] = (
            verb in ('disable', 'bootstrap'))


@pytest.fixture
def setup(tmp_path, monkeypatch):
    # OS protection is separately tested: ordinary tests do not require root.
    monkeypatch.setattr(s, 'protected', lambda p, **kw: Path(p))
    monkeypatch.setattr(s, 'INSTALL_ROOT', tmp_path)
    monkeypatch.setattr(s, 'SUPERVISOR', tmp_path / 'bin/llama-supervisor.sh')
    monkeypatch.setattr(s, 'APPLY_BASELINE', tmp_path / 'original')
    monkeypatch.setattr(s, 'APPLY_CATALOG', tmp_path / 'catalog.json')
    (tmp_path / 'bin').mkdir()
    (tmp_path / 'models').mkdir()
    # Actual installer heredoc, including power/time policy and fixed model.
    script = Path('worker/install-services.sh').read_bytes()
    original = script.split(b"llama-supervisor.sh >/dev/null <<'EOF'\n", 1)[1].split(b'\nEOF', 1)[0] + b'\n'
    s.SUPERVISOR.write_bytes(original)
    s.APPLY_BASELINE.write_bytes(original)
    model = tmp_path / 'models/Ornith-1.5-9B-Q4_K_M.gguf'
    server = tmp_path / 'bin/llama-server'
    model.write_bytes(b'disposable-model')
    server.write_bytes(b'disposable-server')
    model.chmod(0o444)
    server.chmod(0o555)
    entry = {'model_sha256': s.file_digest(model), 'server_sha256': s.file_digest(server),
             'ctx_size': 8192, 'parallel': 4, 'cache_type_k': 'q4_0', 'cache_type_v': 'q8_0'}
    preset = hashlib.sha256(json.dumps(entry, sort_keys=True, separators=(',', ':')).encode()).hexdigest()
    s.durable(s.APPLY_CATALOG, {'version': 1, 'supervisor_sha256': hashlib.sha256(original).hexdigest(),
                              'presets': [entry]})
    cfg = s.InstallationConfig(1, 501, 600, s.Inventory(str(model.parent), str(server), 'a'*40),
                               True, 'b'*64, True, s.file_digest(s.APPLY_CATALOG))
    policy = s.ApplyPolicy(cfg)
    state = tmp_path / 'state'
    state.mkdir()
    lc = FakeLaunchctl()
    health = {'ok': True}
    def make():
        return s.ProductionApplyAdapter(state, lc, lambda: 'generation', policy=policy,
                    health=lambda: health['ok'], closed=lambda _: True, absent=lambda: True,
                    readiness_timeout=0.01, readiness_interval=0.001)
    a = make()
    a.peer = {'pid': 123, 'uid': 501, 'start_sec': 1, 'start_usec': 2}
    monkeypatch.setattr(s, 'runner_absent', lambda peer: True)
    monkeypatch.setattr(s, 'runner_peer', lambda conn: a.peer)
    return a, lc, preset, original, make, health, cfg


@pytest.mark.parametrize('changed', [False, True])
def test_pinned_peer_checks_kernel_and_commands_without_rehash(monkeypatch, changed):
    from types import SimpleNamespace
    pinned = SimpleNamespace(cfg=SimpleNamespace(enable_execution=True, worker_uid=501),
                             executable=Path('/verified/python'))
    identity = {'pid': 123, 'uid': 501, 'start_sec': 1, 'start_usec': 2}
    monkeypatch.setattr(s.sys, 'platform', 'darwin')
    monkeypatch.setattr(s, 'peer_uid', lambda conn: 501)
    identities = []
    def kernel(pid):
        identities.append(pid)
        return identity
    monkeypatch.setattr(s, 'process_identity', kernel)
    def forbidden(*args, **kwargs):
        pytest.fail('pinned peer performed expensive verification')
    monkeypatch.setattr(s, 'execution_capability', forbidden)
    monkeypatch.setattr(s.InstallationConfig, 'read', forbidden)
    monkeypatch.setattr(s, 'protected_runtime', forbidden)
    commands = []
    def ps(argv, **kwargs):
        commands.append(argv[-1])
        return 0, (('/wrong/python' if changed else '/verified/python')
                   if argv[-1] == 'comm=' else
                   str(s.RUNTIME_ROOTS[0] / 'bin/python') +
                   ' -I -m worker.benchmark_production').encode()
    monkeypatch.setattr(s, 'bounded', ps)
    conn = SimpleNamespace(getsockopt=lambda level, option: 123)
    if changed:
        with pytest.raises(s.Refused, match='runner_source_unverified'):
            s.runner_peer(conn, pinned=pinned)
    else:
        assert s.runner_peer(conn, pinned=pinned) == identity
        assert identities == [123, 123]
        assert commands == ['comm=', 'args=']


def test_startup_attestation_cached_through_transaction(setup, monkeypatch):
    a, _, preset, original, _, _, cfg = setup
    clock = [0.0]
    calls = []
    monkeypatch.setattr(s.time, 'monotonic', lambda: clock[0])

    def expensive(*, include_policy):
        assert include_policy
        calls.append('full')
        clock[0] += 720  # much longer than any RPC/handshake budget
        return cfg, 'generation', a.policy

    monkeypatch.setattr(s, 'verify_installation', expensive)
    monkeypatch.setattr(s, 'protected_runtime', lambda p: Path('/verified/python'))
    attestation = s.StartupAttestation()
    assert clock[0] == 720
    a.fingerprint = attestation.fingerprint

    def forbidden(*args, **kwargs):
        pytest.fail('expensive attestation inside RPC')

    monkeypatch.setattr(s, 'verify_installation', forbidden)
    monkeypatch.setattr(s, 'execution_digest', forbidden)
    monkeypatch.setattr(s, 'file_digest', forbidden)
    monkeypatch.setattr(s, 'ApplyPolicy', forbidden)
    monkeypatch.setattr(s, 'protected_runtime', forbidden)
    peers = []
    client, server = socket.socketpair()
    requests = [{'op': 'snapshot'}, {'op': 'prepare', 'preset': preset,
                'transaction_id': 'c'*64, 'generation': 1,
                'config_generation': hashlib.sha256(original).hexdigest()},
                {'op': 'apply'}, {'op': 'finalize', 'transaction_id': 'c'*64,
                                 'generation': 1, 'agreement': 'applied'}]
    for request in requests:
        s.send(client, request)
    s.session(server, a, 600, capability=lambda: True,
              peer_attestor=lambda conn: peers.append('peer') or a.peer)
    assert all(s.receive(client)['ok'] for _ in requests)
    client.close()
    assert calls == ['full'] and len(peers) >= 3
    assert a.record is None


@pytest.mark.parametrize('stage', ['prepare', 'apply'])
def test_expired_session_verifier_never_succeeds_and_recovers(setup, monkeypatch, stage):
    a, lc, preset, original, _, _, _ = setup
    clock = [0.0]
    monkeypatch.setattr(s.time, 'monotonic', lambda: clock[0])
    if stage == 'apply':
        prepare(a, preset, a.snapshot())
    calls = [0]

    def slow_generation():
        calls[0] += 1
        # Prepare's snapshot recheck or Apply's restore generation check.
        if calls[0] == (2 if stage == 'prepare' else 1):
            clock[0] += 101
        return 'generation'

    a.fingerprint = slow_generation
    client, server = socket.socketpair()
    if stage == 'prepare':
        s.send(client, {'op': 'snapshot'})
        s.send(client, {'op': 'prepare', 'preset': preset, 'transaction_id': 'c'*64,
                       'generation': 1, 'config_generation': hashlib.sha256(original).hexdigest()})
    else:
        s.send(client, {'op': 'apply'})
    with pytest.raises(s.Refused, match='lease_expired'):
        s.session(server, a, 600, peer_attestor=lambda conn: a.peer)
    if stage == 'prepare':
        assert s.receive(client)['ok']  # only the pre-expiry snapshot
        assert not lc.calls
    with pytest.raises(EOFError):
        s.receive(client)
    client.close()
    assert s.SUPERVISOR.read_bytes() == original
    if stage == 'apply':
        assert a.status()['phase'] == 'rolled_back'
        # Retry/finalize uses cached generation, not full model/runtime scans.
        a.finalize('c'*64, 1, 'rolled_back')
        assert a.record is None


def test_readiness_rejects_success_after_budget(setup, monkeypatch):
    a, _, _, _, _, _, _ = setup
    clock = [0.0]
    monkeypatch.setattr(s.time, 'monotonic', lambda: clock[0])
    prior = a.snapshot()
    def slow_verify(prior):
        clock[0] += 31
        return True
    a.readiness_timeout = 30
    monkeypatch.setattr(a, 'verify', slow_verify)
    assert not a.wait_verified(prior)
    a.health = lambda: slow_verify(prior)
    with pytest.raises(s.Refused, match='apply_health_required'):
        a.stable_current(hashlib.sha256(a.policy.current()).hexdigest())


def test_startup_timeout_precedes_socket_and_mutation(setup, monkeypatch):
    def timeout(**kwargs):
        s.signal.getsignal(s.signal.SIGALRM)(s.signal.SIGALRM, None)
    monkeypatch.setattr(s, 'verify_installation', timeout)
    monkeypatch.setattr(s.socket, 'socket', lambda *a: pytest.fail('socket before attestation'))
    with pytest.raises(s.Refused, match='startup_attestation_timeout'):
        s.serve()
    assert not setup[1].calls


def prepare(a, preset, prior):
    return a.prepare(preset, prior, 'c'*64, 1,
                     hashlib.sha256(a.policy.current()).hexdigest())


def test_real_config_apply_commit(setup):
    a, lc, preset, original, make, _, cfg = setup
    prior = a.snapshot()
    def journal_before_mutation():
        record = json.loads(a.journal.read_text())
        assert bytes.fromhex(record['apply']['original']) == original
        assert record['prior']['config'] == prior.config
    lc.before = journal_before_mutation
    assert prepare(a, preset, prior)['phase'] == 'prepared'
    a.apply()
    expected = original.replace(s.APPLY_FLAGS, b'--ctx-size 8192 --parallel 4 -ctk q4_0 -ctv q8_0')
    assert s.SUPERVISOR.read_bytes() == expected
    assert json.loads(a.fence_path.read_text())['active']
    assert s.ApplyPolicy(cfg).current() == expected
    a.finalize('c'*64, 1, 'applied')
    assert not json.loads(a.fence_path.read_text())['active']
    make().recover()
    assert s.SUPERVISOR.read_bytes() == expected


def test_health_failure_rolls_back_exact_bytes_and_states(setup):
    a, lc, preset, original, _, health, _ = setup
    lc.states[s.LLAMA]['disabled'] = True
    prior = a.snapshot()
    prepare(a, preset, prior)
    health['ok'] = False
    with pytest.raises(s.Refused, match='health'):
        a.apply()
    with pytest.raises(s.Refused):
        a.rollback(explicit=True)
    assert s.SUPERVISOR.read_bytes() == original
    assert a.record is not None
    assert json.loads(a.fence_path.read_text())['active']
    health['ok'] = True
    a.rollback(explicit=True)
    assert lc.state() == json.loads(prior.config)
    assert a.status()['phase'] == 'rolled_back'
    assert a.record is not None


@pytest.mark.parametrize('window', ['prepared', 'before_write', 'after_write', 'applied'])
def test_crash_windows(setup, monkeypatch, window):
    a, lc, preset, original, make, _, _ = setup
    prior = a.snapshot()
    prepare(a, preset, prior)
    if window in ('before_write', 'after_write'):
        write = a.policy.write
        def crash(raw):
            if window == 'after_write':
                write(raw)
            raise RuntimeError('simulated_crash')
        monkeypatch.setattr(a.policy, 'write', crash)
        with pytest.raises(RuntimeError):
            a.apply()
        monkeypatch.setattr(a.policy, 'write', write)
    elif window == 'applied':
        a.apply()
    restarted = make()
    restarted.recover()
    assert s.SUPERVISOR.read_bytes() == (a.policy.presets[preset] if window == 'applied' else original)
    assert lc.state() == json.loads(prior.config)
    assert restarted.record is not None
    assert restarted.status()['phase'] == ('applied' if window == 'applied' else 'rolled_back')
    assert json.loads(a.fence_path.read_text())['active']


def test_live_or_ambiguous_peer_retains_fence_and_journal(setup, monkeypatch):
    a, _, preset, _, make, _, _ = setup
    prepare(a, preset, a.snapshot())
    monkeypatch.setattr(s, 'runner_absent', lambda _: False)
    restarted = make()
    with pytest.raises(s.Refused, match='runner_still_alive'):
        restarted.recover()
    assert restarted.record
    assert json.loads(a.fence_path.read_text())['active']
    def ambiguous(_):
        raise s.Refused('peer_identity_changed')
    monkeypatch.setattr(s, 'runner_absent', ambiguous)
    with pytest.raises(s.Refused, match='identity'):
        make().recover()


@pytest.mark.parametrize('change', ['model', 'server', 'catalog', 'supervisor', 'writable'])
def test_binding_refusals(setup, change):
    _, _, _, _, _, _, cfg = setup
    if change == 'catalog':
        s.APPLY_CATALOG.write_bytes(b'{}')
    elif change == 'supervisor':
        s.SUPERVISOR.write_bytes(b'unreviewed shell')
    else:
        path = Path(cfg.inventory.server) if change == 'server' else Path(cfg.inventory.model_directory) / 'Ornith-1.5-9B-Q4_K_M.gguf'
        path.chmod(0o644)
        if change != 'writable':
            path.write_bytes(b'changed')
            path.chmod(0o444)
    with pytest.raises(s.Refused):
        s.ApplyPolicy(cfg)


def test_unknown_preset_has_no_effect(setup):
    a, lc, _, original, _, _, _ = setup
    with pytest.raises(s.Refused):
        prepare(a, '0'*64, a.snapshot())
    assert not lc.calls and a.record is None
    assert s.SUPERVISOR.read_bytes() == original


def test_rpc_rejects_client_paths(setup):
    a, _, _, _, _, _, _ = setup
    client, server = socket.socketpair()
    errors = []
    def run():
        try:
            s.session(server, a, 10, capability=lambda: True)
        except s.Refused as e:
            errors.append(str(e))
    t = threading.Thread(target=run)
    t.start()
    s.send(client, {'op': 'prepare', 'preset': 'a'*64, 'path': '/not/accepted'})
    assert client.recv(1) == b''
    t.join(2)
    client.close()
    assert errors == ['request_schema']


def test_original_journal_failure_prevents_all_mutation(setup, monkeypatch):
    a, lc, preset, original, _, _, _ = setup
    durable = s.durable
    def fail(path, value, mode=0o600):
        if path == a.journal:
            raise OSError('simulated_fsync_failure')
        durable(path, value, mode)
    monkeypatch.setattr(s, 'durable', fail)
    with pytest.raises(OSError):
        prepare(a, preset, a.snapshot())
    with pytest.raises(s.Refused, match='persistence_uncertain'):
        a.recover()
    assert not lc.calls
    assert json.loads(a.fence_path.read_text())['active']
    assert s.SUPERVISOR.read_bytes() == original


def test_rpc_apply_commit(setup):
    a, _, preset, original, _, _, _ = setup
    client, server = socket.socketpair()
    errors = []
    def run():
        try:
            s.session(server, a, 10, capability=lambda: True)
        except Exception as e:
            errors.append(type(e))
    t = threading.Thread(target=run)
    t.start()
    def call(op, **fields):
        s.send(client, {'op': op, **fields})
        reply = s.receive(client)
        assert reply['ok'] is True
        return reply['value']
    assert call('ping')['production_apply'] is True
    call('snapshot')
    assert call('prepare', preset=preset, transaction_id='c'*64, generation=1,
                config_generation=hashlib.sha256(original).hexdigest())['phase'] == 'prepared'
    assert call('apply')['phase'] == 'applied'
    assert call('status')['phase'] == 'applied'
    call('finalize', transaction_id='c'*64, generation=1, agreement='applied')
    t.join(2)
    client.close()
    assert not errors and not t.is_alive()
    assert s.SUPERVISOR.read_bytes() != original
    assert a.record is None


@pytest.mark.parametrize('unsafe', ['owner', 'mode', 'symlink', 'acl'])
def test_protected_root_boundary(monkeypatch, unsafe):
    # Simulate filesystem metadata, preserving the production component walk.
    import stat
    from types import SimpleNamespace
    target = Path('/trusted/supervisor')
    def meta(path):
        mode = stat.S_IFREG | 0o755 if path == target else stat.S_IFDIR | 0o755
        uid = 0
        if path == Path('/trusted'):
            if unsafe == 'owner':
                uid = 501
            elif unsafe == 'mode':
                mode |= 0o020
            elif unsafe == 'symlink':
                mode = stat.S_IFLNK | 0o777
        return SimpleNamespace(st_uid=uid, st_mode=mode)
    monkeypatch.setattr(Path, 'lstat', meta)
    monkeypatch.setattr(Path, 'stat', meta)
    def acl(path):
        if unsafe == 'acl' and path == Path('/trusted'):
            raise s.Refused('acl_not_allowed')
    monkeypatch.setattr(s, 'no_acl', acl)
    with pytest.raises(s.Refused):
        s.protected(target)


@pytest.mark.parametrize('field,value', [('transaction_id', 'bad'), ('generation', True),
                                        ('generation', 2), ('config_generation', '0'*64)])
def test_stale_prepare_no_mutations(setup, field, value):
    a, lc, preset, original, _, _, _ = setup
    fields = dict(transaction_id='c'*64, generation=1,
                  config_generation=hashlib.sha256(original).hexdigest())
    fields[field] = value
    with pytest.raises(s.Refused):
        a.prepare(preset, a.snapshot(), **fields)
    assert not lc.calls and a.record is None
    assert s.SUPERVISOR.read_bytes() == original


@pytest.mark.parametrize('boundary', ['receipt', 'latest', 'fence', 'journal'])
def test_finalize_crash_receipt_replay(setup, monkeypatch, boundary):
    a, _, preset, _, make, _, _ = setup
    prepare(a, preset, a.snapshot())
    a.apply()
    decision = a.status()
    durable = s.durable
    targets = {'receipt': a.receipts / ('c'*64 + '.json'),
               'latest': a.root / 'apply-latest.json', 'fence': a.fence_path,
               'journal': a.journal}
    def crash(path, value, mode=0o600):
        durable(path, value, mode)
        if path == targets[boundary]:
            raise OSError('crash_after_write')
    monkeypatch.setattr(s, 'durable', crash)
    with pytest.raises(OSError):
        a.finalize('c'*64, 1, 'applied')
    monkeypatch.setattr(s, 'durable', durable)
    restarted = make()
    restarted.recover()
    assert restarted.status() == decision
    assert restarted.finalize('c'*64, 1, 'applied') == decision
    assert make().status() == decision
    assert not json.loads(a.fence_path.read_text())['active']
    assert json.loads((a.receipts / ('c'*64 + '.json')).read_text()) == decision


def test_irreversible_and_agreement_validation(setup):
    a, lc, preset, _, make, _, _ = setup
    prepare(a, preset, a.snapshot())
    a.apply()
    calls = list(lc.calls)
    with pytest.raises(s.Refused, match='irreversible'):
        a.rollback(explicit=True)
    for tx, agreement in [('d'*64, 'applied'), ('c'*64, 'rolled_back')]:
        with pytest.raises(s.Refused, match='agreement'):
            a.finalize(tx, 1, agreement)
    make().recover()
    assert lc.calls == calls
    assert json.loads(a.fence_path.read_text())['active']


def test_takeover_requires_independent_absence(setup, monkeypatch):
    a, _, preset, _, _, _, _ = setup
    prepare(a, preset, a.snapshot())
    peer = dict(a.peer, pid=456)
    monkeypatch.setattr(s, 'runner_absent', lambda _: False)
    with pytest.raises(s.Refused):
        a.authorize(peer, takeover=True)
    monkeypatch.setattr(s, 'runner_absent', lambda _: True)
    a.authorize(peer, takeover=True)
    a.recover()
    assert a.status()['phase'] == 'rolled_back'
    a.finalize('c'*64, 1, 'rolled_back')


def test_real_model_selection_and_listing(setup):
    a, _, _, original, _, _, cfg = setup
    model = Path(cfg.inventory.model_directory) / 'Second-Model.gguf'
    model.write_bytes(b'second immutable model')
    model.chmod(0o444)
    catalog = json.loads(s.APPLY_CATALOG.read_text())
    entry = dict(catalog['presets'][0], model_sha256=s.file_digest(model))
    catalog['presets'].append(entry)
    s.durable(s.APPLY_CATALOG, catalog)
    from dataclasses import replace
    cfg = replace(cfg, apply_manifest=s.file_digest(s.APPLY_CATALOG))
    a.policy = s.ApplyPolicy(cfg)
    listing = a.presets()
    assert listing['generation'] == cfg.apply_manifest
    preset = next(k for k, v in listing['presets'].items() if v == entry)
    prepare(a, preset, a.snapshot())
    a.apply()
    expected = original.replace(b'Ornith-1.5-9B-Q4_K_M.gguf', b'Second-Model.gguf').replace(
        s.APPLY_FLAGS, b'--ctx-size 8192 --parallel 4 -ctk q4_0 -ctv q8_0')
    assert s.SUPERVISOR.read_bytes() == expected
    assert s.ApplyPolicy(cfg).current() == expected


@pytest.mark.parametrize('decision', ['applied', 'rolled_back'])
def test_crash_before_terminal_decision_write(setup, monkeypatch, decision):
    a, _, preset, original, make, _, _ = setup
    prepare(a, preset, a.snapshot())
    durable = s.durable
    def crash(path, value, mode=0o600):
        if path == a.journal and value.get('apply', {}).get('phase') == decision:
            raise OSError('before_decision_write')
        durable(path, value, mode)
    monkeypatch.setattr(s, 'durable', crash)
    with pytest.raises(OSError):
        if decision == 'applied':
            a.apply()
        else:
            a.rollback(explicit=True)
    monkeypatch.setattr(s, 'durable', durable)
    restarted = make()
    restarted.recover()
    assert restarted.status()['phase'] == 'rolled_back'
    assert s.SUPERVISOR.read_bytes() == original
    assert json.loads(a.fence_path.read_text())['active']


def test_status_connection_is_readonly(setup):
    a, lc, preset, _, _, _, _ = setup
    prepare(a, preset, a.snapshot())
    journal = a.journal.read_bytes()
    calls = list(lc.calls)
    client, server = socket.socketpair()
    def run():
        try:
            s.session(server, a, 10, capability=lambda: True)
        except EOFError:
            pass
    t = threading.Thread(target=run)
    t.start()
    for op in ('status', 'presets'):
        s.send(client, {'op': op})
        assert s.receive(client)['ok']
    client.close()
    t.join(2)
    assert not t.is_alive()
    assert lc.calls == calls and a.journal.read_bytes() == journal


def rpc(client, op, **fields):
    s.send(client, {'op': op, **fields})
    return s.receive(client)['value']


def serve_one(monkeypatch, a, cfg):
    """Real serve/session and socket IO; fake accept, disposable lock, no services."""
    client, conn = socket.socketpair()
    class Server:
        used = False
        def __enter__(self):
            return self
        def __exit__(self, *args):
            pass
        def bind(self, path):
            pass
        def listen(self, backlog):
            pass
        def accept(self):
            if self.used:
                raise EOFError('test_server_done')
            self.used = True
            return conn, None
    monkeypatch.setattr(s, 'STATE', a.root)
    monkeypatch.setattr(s, 'SOCKET', a.root / 'test.sock')
    monkeypatch.setattr(s, 'FENCE', a.fence_path)
    class Attestation:
        def __init__(self):
            self.cfg, self.policy = cfg, a.policy
        fingerprint = staticmethod(lambda: 'generation')
        peer = staticmethod(lambda conn: s.runner_peer(conn))
    monkeypatch.setattr(s, 'StartupAttestation', Attestation)
    monkeypatch.setattr(s, 'Launchctl', lambda: a.lc)
    monkeypatch.setattr(s, 'ApplyPolicy', lambda cfg: a.policy)
    original_class = s.ProductionApplyAdapter
    class Factory(type):
        def __call__(cls, *args, **kwargs):
            return a
        def __instancecheck__(cls, instance):
            return isinstance(instance, original_class)
    class Adapter(metaclass=Factory):
        pass
    monkeypatch.setattr(s, 'ProductionApplyAdapter', Adapter)
    monkeypatch.setattr(s.socket, 'socket', lambda *args: Server())
    monkeypatch.setattr(s.os, 'chown', lambda *args: None)
    monkeypatch.setattr(s.os, 'chmod', lambda *args: None)
    monkeypatch.setattr(s.pwd, 'getpwuid', lambda uid: type('User', (), {'pw_gid': 501})())
    monkeypatch.setattr(s, 'peer_uid', lambda conn: cfg.worker_uid)
    errors = []
    def run():
        try:
            s.serve()
        except (EOFError, s.Refused, OSError) as exc:
            errors.append(exc)
        finally:
            conn.close()
    thread = threading.Thread(target=run)
    thread.start()
    client.settimeout(2)
    return client, thread, errors


@pytest.mark.parametrize('boundary', ['idle', 'open'])
@pytest.mark.parametrize('after_write', [False, True])
def test_serve_exits_poisoned_finalize_even_without_record(setup, monkeypatch, boundary, after_write):
    a, _, preset, _, _, _, cfg = setup
    prepare(a, preset, a.snapshot())
    a.apply()
    durable = s.durable
    def fail(path, value, mode=0o600):
        hit = ((boundary == 'idle' and path == a.journal and value == {'phase': 'idle'}) or
               (boundary == 'open' and path == a.fence_path and value.get('active') is False))
        if hit and not after_write:
            raise OSError('test_io_failure')
        durable(path, value, mode)
        if hit:
            raise OSError('test_io_failure')
    monkeypatch.setattr(s, 'durable', fail)
    client, thread, errors = serve_one(monkeypatch, a, cfg)
    s.send(client, {'op': 'finalize', 'transaction_id': 'c'*64,
                    'generation': 1, 'agreement': 'applied'})
    assert client.recv(1) == b''
    client.close()
    thread.join(2)
    assert not thread.is_alive()
    assert len(errors) == 1 and str(errors[0]) == 'recovery_required'
    assert a.persistence_failed
    assert (a.record is not None) == (boundary == 'idle')
    assert json.loads(a.fence_path.read_text())['active']
    with pytest.raises(s.Refused, match='persistence_uncertain'):
        a.finalize('c'*64, 1, 'applied')


def test_delayed_rollback_startup_does_not_repeat_teardown(setup, monkeypatch):
    a, lc, preset, original, make, health, cfg = setup
    prepare(a, preset, a.snapshot())
    health['ok'] = False
    with pytest.raises(s.Refused, match='rollback_unverified'):
        a.rollback(explicit=True)
    assert a.status()['phase'] == 'rollback_restoring'
    calls = list(lc.calls)
    restarted = make()
    checks = []
    def delayed():
        checks.append(True)
        return len(checks) >= 4
    restarted.health = delayed
    restarted.readiness_timeout = 0.1
    client, thread, errors = serve_one(monkeypatch, restarted, cfg)
    assert rpc(client, 'status')['phase'] == 'rolled_back'
    assert lc.calls.count('bootout') == calls.count('bootout')
    assert lc.calls.count('bootstrap') == calls.count('bootstrap')
    assert len(checks) >= 4 and s.SUPERVISOR.read_bytes() == original
    rpc(client, 'finalize', transaction_id='c'*64, generation=1, agreement='rolled_back')
    client.close()
    thread.join(2)
    assert not thread.is_alive()
    assert len(errors) == 1 and isinstance(errors[0], EOFError)


def test_delayed_apply_health_session(setup):
    a, lc, preset, _, _, _, _ = setup
    prepare(a, preset, a.snapshot())
    checks = []
    def health():
        checks.append(True)
        return len(checks) >= 4
    a.health = health
    a.readiness_timeout = 0.1
    client, server = socket.socketpair()
    errors = []
    def run():
        try:
            s.session(server, a, 10)
        except Exception as exc:
            errors.append(exc)
    thread = threading.Thread(target=run)
    thread.start()
    client.settimeout(2)
    assert rpc(client, 'apply')['phase'] == 'applied'
    assert len(checks) >= 4 and lc.calls.count('bootstrap') == 1
    rpc(client, 'finalize', transaction_id='c'*64, generation=1, agreement='applied')
    client.close()
    thread.join(2)
    assert not errors and not thread.is_alive()


def test_abort_never_prepared_rpc(setup):
    a, lc, preset, original, make, _, _ = setup
    client, server = socket.socketpair()
    errors = []
    def run():
        try:
            s.session(server, a, 10)
        except Exception as exc:
            errors.append(exc)
    thread = threading.Thread(target=run)
    thread.start()
    client.settimeout(2)
    digest = hashlib.sha256(original).hexdigest()
    decision = rpc(client, 'abort', preset=preset, transaction_id='d'*64,
                   generation=1, config_generation=digest)
    assert decision == dict(transaction_id='d'*64, generation=1, preset=preset,
                            phase='rolled_back', original_config_digest=digest,
                            target_config_digest=hashlib.sha256(a.policy.presets[preset]).hexdigest())
    assert json.loads((a.receipts / ('d'*64 + '.json')).read_text()) == decision
    assert json.loads(a.fence_path.read_text())['active']
    assert not lc.calls and s.SUPERVISOR.read_bytes() == original
    rpc(client, 'finalize', transaction_id='d'*64, generation=1, agreement='rolled_back')
    client.close()
    thread.join(2)
    assert not errors and not thread.is_alive()
    assert make().finalize('d'*64, 1, 'rolled_back') == decision
    assert not json.loads(a.fence_path.read_text())['active']


@pytest.mark.parametrize('conflict', ['active', 'applied_receipt', 'rollback_receipt', 'stale', 'unstable', 'unhealthy'])
def test_abort_cannot_fabricate_rollback(setup, conflict):
    a, lc, preset, original, _, health, _ = setup
    digest = hashlib.sha256(original).hexdigest()
    tx = 'd'*64
    if conflict == 'active':
        prepare(a, preset, a.snapshot())
    elif conflict in ('applied_receipt', 'rollback_receipt'):
        prepare(a, preset, a.snapshot())
        if conflict == 'applied_receipt':
            a.apply()
            a.finalize('c'*64, 1, 'applied')
        else:
            a.rollback(explicit=True)
            a.finalize('c'*64, 1, 'rolled_back')
        tx = 'c'*64
        digest = hashlib.sha256(a.policy.current()).hexdigest()
        # A newer receipt must not hide the immutable history of the old tx.
        a.abort(preset, 'e'*64, 1, digest)
        a.finalize('e'*64, 1, 'rolled_back')
    elif conflict == 'stale':
        digest = '0'*64
    elif conflict == 'unstable':
        def changing_health():
            lc.states[s.LLAMA]['disabled'] = True
            return True
        a.health = changing_health
    else:
        health['ok'] = False
    calls = list(lc.calls)
    current = s.SUPERVISOR.read_bytes()
    with pytest.raises(s.Refused):
        a.abort(preset, tx, 1, digest)
    assert lc.calls == calls and s.SUPERVISOR.read_bytes() == current


@pytest.mark.parametrize('boundary', ['journal', 'fence', 'receipt'])
def test_abort_lost_response_and_persistence_restart(setup, monkeypatch, boundary):
    a, lc, preset, original, make, _, _ = setup
    durable = s.durable
    targets = {'journal': a.journal, 'fence': a.fence_path,
               'receipt': a.receipts / ('d'*64 + '.json')}
    def crash(path, value, mode=0o600):
        durable(path, value, mode)
        if path == targets[boundary]:
            raise OSError('lost_write_ack')
    monkeypatch.setattr(s, 'durable', crash)
    with pytest.raises(OSError):
        a.abort(preset, 'd'*64, 1, hashlib.sha256(original).hexdigest())
    assert a.persistence_failed
    monkeypatch.setattr(s, 'durable', durable)
    restarted = make()
    restarted.recover()
    assert restarted.status()['phase'] == 'rolled_back'
    assert json.loads(a.fence_path.read_text())['active']
    restarted.finalize('d'*64, 1, 'rolled_back')
    assert not lc.calls and s.SUPERVISOR.read_bytes() == original


def test_receipt_only_finalize_requires_health_and_current_digest(setup):
    a, _, preset, original, make, health, _ = setup
    prepare(a, preset, a.snapshot())
    a.apply()
    a.finalize('c'*64, 1, 'applied')
    restarted = make()
    health['ok'] = False
    with pytest.raises(s.Refused, match='health'):
        restarted.finalize('c'*64, 1, 'applied')
    assert json.loads(a.fence_path.read_text())['active']
    health['ok'] = True
    s.SUPERVISOR.write_bytes(original)
    with pytest.raises(s.Refused, match='stale_config'):
        restarted.finalize('c'*64, 1, 'applied')
