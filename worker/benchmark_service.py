# SPDX-License-Identifier: AGPL-3.0-or-later
"""Explicitly installed macOS service fence. Standalone root code: stdlib only.

No job-supplied executable, environment, path, PID, or import is accepted.
This controls services and opt-in fixed presets, not benchmark children or
 distributed coordinator commits.
See docs/production-benchmark-install.md before enabling an integration.
"""
from __future__ import annotations

import argparse
from contextvars import ContextVar
from dataclasses import asdict, dataclass
import errno
import fcntl
import hashlib
import http.client
import json
import os
from pathlib import Path
import plistlib
import pwd
import re
import selectors
import signal
import socket
import stat
import subprocess
import sys
import tempfile
import time
import threading

LABELS = ('io.nocturne.worker', 'io.nocturne.llama', 'io.nocturne.tunnel')
PLISTS = Path('/Library/LaunchDaemons')
ROOT = Path('/Library/NocturneBenchmark')
CONFIG = ROOT / 'installation.json'
CODE = ROOT / 'benchmark_service.py'
STATE = ROOT / 'state'
SOCKET_DIR = Path('/var/run/nocturne-benchmark')
SOCKET = SOCKET_DIR / 'control.sock'
FENCE = ROOT / 'admission.json'
LLAMA = 'io.nocturne.llama'
DAEMON_LABEL = 'io.nocturne.benchmark-service'
DAEMON_PLIST = PLISTS / (DAEMON_LABEL + '.plist')
INSTALL_ROOT = Path('/opt/nocturne')
RUNTIME_ROOTS = (INSTALL_ROOT / '.venv', INSTALL_ROOT / 'python')
RUNNER_MODULES = ('__init__', 'agent', 'benchmark_production', 'benchmark_service',
                  'benchmark_executor', 'benchmark_artifacts', 'benchmark_discovery',
                  'model_benchmark', 'macos_sensors')
PTH_NAME = 'nocturne-production.pth'
APPLY_CATALOG = INSTALL_ROOT / 'production-apply-presets.json'
SUPERVISOR = INSTALL_ROOT / 'bin/llama-supervisor.sh'
APPLY_BASELINE = ROOT / 'supervisor-original'
APPLY_FLAGS = b'--ctx-size 65536 --parallel 2 -ctk q8_0 -ctv q8_0'
MAX_MESSAGE = 4096
ENV = {'PATH': '/usr/bin:/bin:/usr/sbin:/sbin', 'HOME': '/var/root', 'LANG': 'C'}


class Refused(ValueError):
    """Static, nonsecret error only."""


_deadline = ContextVar('service_deadline', default=None)


def check_deadline():
    deadline = _deadline.get()
    if deadline is not None and time.monotonic() >= deadline:
        raise Refused('lease_expired')


def remaining_timeout(timeout):
    check_deadline()
    deadline = _deadline.get()
    return timeout if deadline is None else min(timeout, deadline - time.monotonic())


def bounded(argv, timeout=5, limit=262144):
    """No shell; deadline and combined output cap, including pipe-holding children."""
    timeout = remaining_timeout(timeout)
    p = subprocess.Popen(argv, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                         stderr=subprocess.STDOUT, env=ENV, cwd='/', start_new_session=True)
    output = bytearray()
    deadline = time.monotonic() + timeout
    completed = False
    try:
        with selectors.DefaultSelector() as sel:
            sel.register(p.stdout, selectors.EVENT_READ)
            while sel.get_map():
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise Refused('command_timeout')
                for key, _ in sel.select(remaining):
                    chunk = os.read(key.fd, 8192)
                    if not chunk:
                        sel.unregister(key.fd)
                    else:
                        output.extend(chunk)
                        if len(output) > limit:
                            raise Refused('command_output_limit')
        rc = p.wait(timeout=max(.001, deadline-time.monotonic()))
        completed = True
        return rc, bytes(output)
    except subprocess.TimeoutExpired:
        raise Refused('command_timeout') from None
    finally:
        if not completed:
            import signal
            try:
                os.killpg(p.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
        p.wait()
        p.stdout.close()


def no_acl(path):
    rc, out = bounded(['/bin/ls', '-lde', str(path)])
    if rc or len(out.splitlines()) != 1 or b'+' in out.split(None, 1)[0]:
        raise Refused('acl_not_allowed')


def protected(path, *, private=False, directory=False):
    """Reject links at EVERY component, writable parents and any macOS ACL.

    /var is an Apple symlink: callers use /private/var for filesystem validation.
    Concurrent root modification is outside the trust model.
    """
    path = Path(path)
    if not path.is_absolute() or '..' in path.parts:
        raise Refused('absolute_path_required')
    for p in [*reversed(path.parents), path]:
        s = p.lstat()
        if s.st_uid != 0 or s.st_mode & 0o022 or stat.S_ISLNK(s.st_mode):
            raise Refused('untrusted_path')
        if p != path and not stat.S_ISDIR(s.st_mode):
            raise Refused('untrusted_parent')
        no_acl(p)
    s = path.stat()
    if not (stat.S_ISDIR(s.st_mode) if directory else stat.S_ISREG(s.st_mode)):
        raise Refused('wrong_file_type')
    if private and s.st_mode & 0o077:
        raise Refused('private_state_required')
    return path


def protected_runtime(path):
    """Installer-equivalent link walk; inspect every hop without executing Python."""
    hops = 0

    def within(p, root):
        return p == root or root in p.parents

    def resolve(raw, roots=None):
        nonlocal hops
        current = Path('/')
        protected(current, directory=True)
        for part in str(raw).split('/')[1:]:
            if part in ('', '.'):
                continue
            current = current.parent if part == '..' else current / part
            if roots and not any(within(current, r) or (part != '..' and within(r, current))
                                 for r in roots):
                raise Refused('runtime_boundary')
            meta = current.lstat()
            no_acl(current)
            if meta.st_uid != 0 or (not stat.S_ISLNK(meta.st_mode) and meta.st_mode & 0o022):
                raise Refused('runtime_permissions')
            if stat.S_ISLNK(meta.st_mode):
                allowed = (RUNTIME_ROOTS if within(current, RUNTIME_ROOTS[0]) else
                           (RUNTIME_ROOTS[1],) if within(current, RUNTIME_ROOTS[1]) else ())
                if roots:
                    allowed = tuple(r for r in allowed if r in roots)
                hops += 1
                if not allowed or hops > 40:
                    raise Refused('runtime_link')
                target = os.readlink(current)
                current = resolve(target if os.path.isabs(target) else
                                  str(current.parent) + '/' + target, allowed)
        if roots and not any(within(current, r) for r in roots):
            raise Refused('runtime_boundary')
        return current

    path = Path(path)
    if not path.is_absolute() or '..' in path.parts:
        raise Refused('runtime_path')
    return resolve(str(path))


def execution_files(inventory):
    """Fixed installed code only. Root reads bytes; never imports worker modules."""
    files = set()
    for name in RUNNER_MODULES:
        files.add(protected(INSTALL_ROOT / 'worker' / (name + '.py')))
    agent = (INSTALL_ROOT / 'worker/agent.py').read_text()
    if ('from .benchmark_production import admission_allowed, installation_present, polling' not in agent
            or agent.count('admission_allowed()') < 5):
        raise Refused('fence_aware_agent_required')
    # Full runtime, dependencies, startup hooks and package tree are attested.
    for root in (*RUNTIME_ROOTS, INSTALL_ROOT / 'worker'):
        protected(root, directory=True)
        for base, dirs, names in os.walk(root, followlinks=False):
            for name in sorted(dirs + names):
                path = Path(base) / name
                target = protected_runtime(path) if root in RUNTIME_ROOTS else protected(
                    path, directory=path.is_dir() and not path.is_symlink())
                if target.is_file():
                    files.add(path)
                elif not target.is_dir():
                    raise Refused('runtime_file_type')
    python = protected_runtime(RUNTIME_ROOTS[0] / 'bin/python')
    if not python.is_file() or not python.stat().st_mode & 0o111:
        raise Refused('runtime_interpreter')
    server = protected(inventory.server)
    if server.stat().st_mode & 0o222:
        raise Refused('immutable_server_required')
    models = protected(inventory.model_directory, directory=True)
    count = total = 0
    for index, path in enumerate(models.iterdir(), 1):
        if index > 1024:
            raise Refused('inventory_limit')
        if path.name.lower().endswith('.gguf'):
            protected(path)
            if path.stat().st_mode & 0o222:
                raise Refused('immutable_model_required')
            count += 1
            total += path.stat().st_size
    if not 1 <= count <= 64 or total > 512 * 2**30:
        raise Refused('inventory_limit')
    return sorted(files)


def execution_pth():
    sites = list((RUNTIME_ROOTS[0] / 'lib').glob('python3.*/site-packages'))
    if len(sites) != 1:
        raise Refused('runtime_site_packages')
    site = protected(sites[0], directory=True)
    if any(os.path.lexists(site / name) for name in ('worker', 'worker.py', 'worker.pyc')):
        raise Refused('runtime_package_shadow')
    return site / PTH_NAME


def execution_digest(inventory, *, provision=False):
    files = execution_files(inventory)  # verify before ANY write
    pth = execution_pth()
    expected = b'/opt/nocturne\n'
    if provision and not os.path.lexists(pth):
        fd = os.open(pth, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o644)
        with os.fdopen(fd, 'wb') as f:
            f.write(expected)
            f.flush()
            os.fsync(f.fileno())
        fd = os.open(pth.parent, os.O_RDONLY)
        try:
            os.fsync(fd)
        finally:
            os.close(fd)
    if protected(pth).read_bytes() != expected:
        raise Refused('runtime_pth')
    files = sorted(set(files) | {pth})
    digest = hashlib.sha256()
    for path in files:
        digest.update(str(path).encode() + b'\0')
        if path.is_symlink():
            digest.update(os.readlink(path).encode() + b'\0')
        h = hashlib.sha256()
        with path.open('rb') as f:
            for chunk in iter(lambda: f.read(1024 * 1024), b''):
                h.update(chunk)
        digest.update(h.digest())
    return digest.hexdigest()


def execution_capability(cfg):
    return (cfg.enable_execution and
            execution_digest(cfg.inventory) == cfg.execution_manifest)


def decode(raw, limit=MAX_MESSAGE):
    def pairs(items):
        result = {}
        for k, v in items:
            if k in result:
                raise Refused('duplicate_key')
            result[k] = v
        return result
    if len(raw) > limit:
        raise Refused('message_limit')
    try:
        return json.loads(raw, object_pairs_hook=pairs,
                          parse_constant=lambda _: (_ for _ in ()).throw(Refused('invalid_number')))
    except (ValueError, UnicodeError):
        raise Refused('invalid_json') from None


def durable(path, value, mode=0o600):
    """Caller owns protected private parent; atomic replacement + both fsyncs."""
    check_deadline()
    path = Path(path)
    fd, tmp = tempfile.mkstemp(dir=path.parent)
    try:
        with os.fdopen(fd, 'wb') as f:
            os.fchmod(f.fileno(), mode)
            f.write(value if isinstance(value, bytes) else
                    json.dumps(value, separators=(',', ':'), allow_nan=False).encode())
            f.flush()
            os.fsync(f.fileno())
        check_deadline()
        os.replace(tmp, path)
        fd = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(fd)
        finally:
            os.close(fd)
    finally:
        if os.path.exists(tmp):
            os.unlink(tmp)


@dataclass(frozen=True)
class Inventory:
    model_directory: str
    server: str
    code_revision: str

    @classmethod
    def parse(cls, obj):
        if (not isinstance(obj, dict) or set(obj) != {'model_directory', 'server', 'code_revision'}
                or any(type(v) is not str for v in obj.values())):
            raise Refused('inventory_schema')
        inventory = cls(**obj)
        if not re.fullmatch(r'[0-9a-f]{40,64}', inventory.code_revision):
            raise Refused('inventory_revision')
        protected(inventory.model_directory, directory=True)
        server = protected(inventory.server)
        if server.name != 'llama-server' or not server.stat().st_mode & 0o111:
            raise Refused('inventory_server')
        return inventory


@dataclass(frozen=True)
class InstallationConfig:
    version: int
    worker_uid: int
    lease_seconds: int
    inventory: Inventory
    enable_execution: bool = False
    execution_manifest: str | None = None
    enable_apply: bool = False
    apply_manifest: str | None = None

    @classmethod
    def read(cls, path=CONFIG):
        protected(path, private=True)
        obj = decode(Path(path).read_bytes())
        required = {'version', 'worker_uid', 'lease_seconds', 'inventory'}
        if (not isinstance(obj, dict) or not required <= set(obj)
                or set(obj) - required - {'enable_execution', 'execution_manifest',
                                            'enable_apply', 'apply_manifest'}):
            raise Refused('installation_schema')
        if any(type(obj[k]) is not int for k in ('version', 'worker_uid', 'lease_seconds')):
            raise Refused('installation_types')
        cfg = cls(**{**obj, 'inventory': Inventory.parse(obj['inventory'])})
        if (type(cfg.enable_execution) is not bool or
                (cfg.enable_execution and (type(cfg.execution_manifest) is not str or
                 not re.fullmatch(r'[0-9a-f]{64}', cfg.execution_manifest))) or
                (not cfg.enable_execution and cfg.execution_manifest is not None)):
            raise Refused('execution_schema')
        if (type(cfg.enable_apply) is not bool or
                (cfg.enable_apply and (not cfg.enable_execution or
                 type(cfg.apply_manifest) is not str or
                 not re.fullmatch(r'[0-9a-f]{64}', cfg.apply_manifest))) or
                (not cfg.enable_apply and cfg.apply_manifest is not None)):
            raise Refused('apply_schema')
        if cfg.version != 1 or cfg.worker_uid <= 0 or not 10 <= cfg.lease_seconds <= 600:
            raise Refused('installation_bounds')
        if pwd.getpwnam('nocturne-worker').pw_uid != cfg.worker_uid:
            raise Refused('worker_identity')
        return cfg


def verify_services(supervisor_original=None):
    """Only existing installer labels; none of their code may execute as root."""
    content = bytearray()
    for label in LABELS:
        path = protected(PLISTS / (label + '.plist'))
        raw = path.read_bytes()
        if len(raw) > 65536:
            raise Refused('plist_limit')
        job = plistlib.loads(raw)
        if (job.get('Label') != label or job.get('UserName') != 'nocturne-worker'
                or job.get('AbandonProcessGroup', False) or job.get('RootDirectory')
                or 'Program' in job):
            raise Refused('unsupported_service')
        args = job.get('ProgramArguments')
        expected = {'io.nocturne.worker': ['/opt/nocturne/.venv/bin/python', '-m', 'worker.agent'],
                    'io.nocturne.llama': ['/bin/bash', '/opt/nocturne/bin/llama-supervisor.sh']}
        if label in expected and args != expected[label]:
            raise Refused('unsupported_service_command')
        if label == 'io.nocturne.tunnel' and (not isinstance(args, list) or
                not all(isinstance(x, str) for x in args) or args[:2] != ['/usr/bin/ssh', '-N']):
            raise Refused('unsupported_tunnel')
        content.extend(raw)
    # These are not imported by root. Interpreter symlinks in the existing venv
    # are deliberately not traversed here: the services have mandatory nonroot UID.
    for path in ('/opt/nocturne/worker/agent.py', '/opt/nocturne/bin/llama-supervisor.sh'):
        raw = protected(path).read_bytes()
        content.extend(supervisor_original if supervisor_original is not None and
                       path.endswith('/llama-supervisor.sh') else raw)
    return hashlib.sha256(content).hexdigest()


def verify_installation(*, include_policy=False):
    if sys.platform != 'darwin' or os.geteuid() != 0:
        raise Refused('macos_root_required')
    cfg = InstallationConfig.read()
    protected(CODE)
    if Path(__file__).absolute() != CODE:
        raise Refused('installed_code_required')
    protected(STATE, private=True, directory=True)
    protected('/private/var/run/nocturne-benchmark', directory=True)
    job = plistlib.loads(protected(DAEMON_PLIST).read_bytes())
    if job != daemon_plist():
        raise Refused('daemon_plist_mismatch')
    if cfg.enable_execution and not execution_capability(cfg):
        raise Refused('execution_manifest_changed')
    policy = ApplyPolicy(cfg) if cfg.enable_apply else None
    generation = hashlib.sha256(verify_services(policy.original if policy else None).encode()
                                + CODE.read_bytes() + CONFIG.read_bytes()).hexdigest()
    return (cfg, generation, policy) if include_policy else (cfg, generation)


class StartupAttestation:
    """Cached only under the protected, restart-only installation trust boundary."""
    def __init__(self):
        # Daemon entrypoint is single-threaded. A separate startup budget is not
        # borrowed from any runner handshake, lease, or readiness deadline.
        def expired(signum, frame):
            raise Refused('startup_attestation_timeout')

        previous = signal.signal(signal.SIGALRM, expired)
        signal.setitimer(signal.ITIMER_REAL, 3600)
        try:
            self.cfg, self.generation, self.policy = verify_installation(include_policy=True)
            self.executable = (protected_runtime(RUNTIME_ROOTS[0] / 'bin/python')
                               if self.cfg.enable_execution else None)
        finally:
            signal.setitimer(signal.ITIMER_REAL, 0)
            signal.signal(signal.SIGALRM, previous)

    def fingerprint(self):
        check_deadline()
        return self.generation

    def peer(self, conn):
        check_deadline()
        return runner_peer(conn, pinned=self)


class Launchctl:
    def __init__(self, run=bounded):
        self.run = run

    def call(self, verb, label=None):
        if verb == 'print-disabled' and label is None:
            args = [verb, 'system']
        elif (label in LABELS and verb == 'print') or (label == LLAMA and verb in ('disable', 'enable', 'bootout')):
            args = [verb, 'system/' + label]
        elif label == LLAMA and verb == 'bootstrap':
            args = [verb, 'system', str(PLISTS / (label + '.plist'))]
        else:
            raise Refused('command_not_allowed')
        return self.run(['/bin/launchctl', *args])

    def loaded(self, label):
        rc, out = self.call('print', label)
        if rc == 0:
            return True
        if rc == 113 and b'Could not find service' in out:
            return False
        raise Refused('launchctl_print_failed')

    def worker_pid(self):
        rc, out = self.call('print', LABELS[0])
        if rc:
            raise Refused('worker_not_loaded')
        matches = re.findall(rb'^\s*pid = ([1-9][0-9]*)\s*$', out, re.MULTILINE)
        if len(matches) != 1 or len(matches[0]) > 10:
            raise Refused('worker_pid_unverified')
        pid = int(matches[0])
        if pid > 2147483647:
            raise Refused('worker_pid_unverified')
        return pid

    def disabled(self):
        rc, out = self.call('print-disabled')
        if rc:
            raise Refused('launchctl_disabled_failed')
        text = out.decode('utf-8', errors='strict')
        if not text.strip().startswith('disabled services = {') or not text.strip().endswith('}'):
            raise Refused('launchctl_disabled_format')
        result = {}
        for label in LABELS:
            matches = re.findall(r'"' + re.escape(label) + r'"\s*=>\s*(true|false)', text)
            if len(matches) > 1 or (label in text and not matches):
                raise Refused('launchctl_disabled_format')
            result[label] = bool(matches and matches[0] == 'true')
        return result

    def state(self):
        disabled = self.disabled()
        return {label: {'loaded': self.loaded(label), 'disabled': disabled[label]} for label in LABELS}

    def mutate(self, verb, label):
        check_deadline()
        rc, _ = self.call(verb, label)
        if rc:
            raise Refused('launchctl_mutation_failed')


def port_closed(port):
    try:
        with socket.create_connection(('127.0.0.1', port), timeout=.5):
            return False
    except OSError as e:
        if e.errno == errno.ECONNREFUSED:
            return True
        raise Refused('endpoint_unverifiable') from None


def inference_absent():
    """Conservative system-wide executable snapshot, not arbitrary PID signaling.

    Runner must use the fixed llama-server basename and exclusive local admission;
    renamed executables are outside this service-control contract.
    """
    rc, out = bounded(['/bin/ps', '-axo', 'comm='])
    if rc:
        raise Refused('process_snapshot_failed')
    return not any(Path(line.strip()).name in ('llama-server', 'llama-supervisor.sh')
                   for line in out.decode('utf-8', errors='strict').splitlines())


def readiness():
    """llama.cpp's actual health and idle-slot endpoints; no proxy or redirects.

    Existing worker has no localhost health HTTP listener. Its loaded state is
    checked separately, NOT misrepresented as coordinator readiness.
    """
    deadline = time.monotonic() + remaining_timeout(4)
    for path in ('/health', '/slots'):
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            return False
        c = http.client.HTTPConnection('127.0.0.1', 8080, timeout=min(2, remaining))
        timer = None
        try:
            c.request('GET', path)
            sock = getattr(c, 'sock', None)

            def interrupt():
                try:
                    sock.shutdown(socket.SHUT_RDWR)
                except (OSError, AttributeError):
                    pass

            # Absolute probe bound, including a peer trickling HTTP headers/body.
            timer = threading.Timer(max(0, deadline - time.monotonic()), interrupt)
            timer.start()
            r = c.getresponse()
            raw = r.read(MAX_MESSAGE + 1)
            if r.status != 200:
                return False
            obj = decode(raw)
            if path == '/health' and (not isinstance(obj, dict) or obj.get('status') != 'ok'):
                return False
            if path == '/slots' and (not isinstance(obj, list) or not obj or
                    any(not isinstance(s, dict) or s.get('is_processing') is not False for s in obj)):
                return False
        except (OSError, http.client.HTTPException, Refused):
            return False
        finally:
            if timer is not None:
                timer.cancel()
                timer.join()
            c.close()
    check_deadline()
    return time.monotonic() < deadline


@dataclass(frozen=True)
class PriorState:
    generation: str
    config: str
    service: bool


@dataclass(frozen=True)
class DrainAck:
    generation: str
    active: int
    service_stopped: bool



def process_identity(pid):
    """Kernel start timestamp (microseconds), not ps's second-resolution lstart.

    Only ESRCH proves absence. Query errors and PID reuse remain fail closed;
    this helper never signals a PID. libproc is part of the trusted macOS OS.
    """
    if sys.platform != 'darwin' or type(pid) is not int or not 0 < pid <= 2147483647:
        raise Refused('peer_identity_unverified')
    import ctypes as c
    class BSDInfo(c.Structure):
        _fields_ = ([(n, c.c_uint32) for n in
                     ('flags', 'status', 'xstatus', 'pid', 'ppid', 'uid', 'gid',
                      'ruid', 'rgid', 'svuid', 'svgid', 'rfu')] +
                    [('comm', c.c_char * 16), ('name', c.c_char * 32)] +
                    [(n, c.c_uint32) for n in
                     ('nfiles', 'pgid', 'pjobc', 'tdev', 'tpgid', 'nice')] +
                    [('start_sec', c.c_uint64), ('start_usec', c.c_uint64)])
    lib = c.CDLL('/usr/lib/libproc.dylib', use_errno=True)
    fn = lib.proc_pidinfo
    fn.argtypes = [c.c_int, c.c_int, c.c_uint64, c.c_void_p, c.c_int]
    fn.restype = c.c_int
    info = BSDInfo()
    c.set_errno(0)
    size = fn(pid, 3, 0, c.byref(info), c.sizeof(info))  # PROC_PIDTBSDINFO
    if size == 0 and c.get_errno() == errno.ESRCH:
        return None
    if size != c.sizeof(info) or info.pid != pid or not info.start_sec:
        raise Refused('peer_identity_unverified')
    return {'pid': pid, 'uid': info.uid, 'start_sec': info.start_sec,
            'start_usec': info.start_usec}


def runner_peer(conn, *, pinned=None):
    # Darwin SOL_LOCAL=0, LOCAL_PEERPID=2. Never accept a JSON PID.
    if sys.platform != 'darwin':
        raise Refused('macos_required')
    cfg = pinned.cfg if pinned is not None else InstallationConfig.read()
    if not (cfg.enable_execution if pinned is not None else execution_capability(cfg)):
        raise Refused('runner_source_unverified')
    pid = conn.getsockopt(0, 2)
    uid = peer_uid(conn)
    if uid != cfg.worker_uid or uid <= 0:
        raise Refused('peer_identity_unverified')
    identity = process_identity(pid)
    if identity is None or identity['uid'] != uid:
        raise Refused('peer_identity_unverified')
    expected = pinned.executable if pinned is not None else protected_runtime(RUNTIME_ROOTS[0] / 'bin/python')
    for field, wanted in (
            ('comm=', str(expected)),
            ('args=', str(RUNTIME_ROOTS[0] / 'bin/python') +
             ' -I -m worker.benchmark_production')):
        rc, out = bounded(['/bin/ps', '-p', str(pid), '-o', field], timeout=2, limit=4096)
        if rc or out.decode('utf-8', errors='strict').strip() != wanted:
            raise Refused('runner_source_unverified')
    if process_identity(pid) != identity:
        raise Refused('peer_identity_changed')
    return identity


def runner_absent(peer):
    if (not isinstance(peer, dict) or
            set(peer) != {'pid', 'uid', 'start_sec', 'start_usec'} or
            any(type(v) is not int for v in peer.values()) or peer['uid'] <= 0):
        raise Refused('peer_identity_unverified')
    current = process_identity(peer['pid'])
    if current is None:
        return True
    if current != peer:
        raise Refused('peer_identity_changed')
    return False


class ServiceAdapter:
    """Daemon-side transaction. Dependencies injectable ONLY by trusted local code.

    An exclusive daemon lock serializes admission. Journal is persisted before
    the first disable, including *all* labels' prior loaded/disabled booleans.
    """
    def __init__(self, state, launchctl, fingerprint, *, health=readiness, closed=port_closed,
                 absent=inference_absent, fence_path=None):
        self.root = Path(state)
        self.journal = self.root / 'service-journal.json'
        self.fence_path = Path(fence_path) if fence_path is not None else self.root / 'admission.json'
        self.lc, self.fingerprint = launchctl, fingerprint
        self.health, self.closed, self.absent = health, closed, absent
        self.peer = None
        self.record = None
        if self.journal.exists():
            self.record = decode(self.journal.read_bytes(), limit=262144)
            if self.record.get('phase') == 'idle':
                self.record = None

    def mutate(self, verb, label):
        check_deadline()
        self.lc.mutate(verb, label)

    def save(self):
        check_deadline()
        durable(self.journal, self.record or {'phase': 'idle'})

    def publish_fence(self, active):
        durable(self.fence_path, {'version': 1, 'active': active}, mode=0o644)

    def snapshot(self):
        if self.record:
            return PriorState(**self.record['prior'])
        states = self.lc.state()
        config = json.dumps(states, sort_keys=True, separators=(',', ':'))
        return PriorState(self.fingerprint(), config, states['io.nocturne.llama']['loaded'])

    def fence(self):
        if self.record is None:
            if self.peer is None:
                raise Refused('runner_binding_required')
            prior = self.snapshot()
            self.record = {'phase': 'fencing', 'prior': asdict(prior),
                           'peer': self.peer, 'runner_session': 'active'}
            self.save()  # includes ALL prior state before ANY mutation
        self.publish_fence(True)  # durable before stopping inference; worker stays alive
        self.mutate('disable', LLAMA)
        if self.lc.loaded(LLAMA):
            self.mutate('bootout', LLAMA)
        state = self.lc.state()[LLAMA]
        if state['loaded'] or not state['disabled']:
            raise Refused('fence_unverified')
        self.record['phase'] = 'fenced'
        self.save()

    def attest_fence(self):
        if not self.record or self.record['phase'] != 'fenced':
            raise Refused('not_fenced')
        state = self.lc.state()[LLAMA]
        if state['loaded'] or not state['disabled']:
            raise Refused('not_fenced')
        if decode(self.fence_path.read_bytes()) != {'version': 1, 'active': True}:
            raise Refused('not_fenced')

    def drain(self):
        self.attest_fence()
        if not self.closed(8080) or not self.absent():
            raise Refused('not_drained')
        return DrainAck(self.record['prior']['generation'], 0, True)

    def admission(self):
        """Read-only observation, not permission to ignore other competitors."""
        self.attest_fence()
        expected = protected_runtime(RUNTIME_ROOTS[0] / 'bin/python')
        if not expected.is_file() or not expected.stat().st_mode & 0o111:
            raise Refused('runtime_interpreter')
        pid = self.lc.worker_pid()
        values = []
        for field in ('comm=', 'lstart='):
            rc, out = bounded(['/bin/ps', '-p', str(pid), '-o', field], timeout=2, limit=4096)
            lines = out.decode('utf-8', errors='strict').splitlines()
            if rc or len(lines) != 1 or not lines[0].strip():
                raise Refused('worker_process_unverified')
            values.append(lines[0].strip())
        executable, start = values
        if executable != str(expected):
            raise Refused('worker_executable_mismatch')
        if not re.fullmatch(r'[A-Z][a-z]{2} [A-Z][a-z]{2} +[0-9]{1,2} [0-9]{2}:[0-9]{2}:[0-9]{2} [0-9]{4}', start):
            raise Refused('worker_start_unverified')
        if self.lc.worker_pid() != pid:
            raise Refused('worker_pid_changed')
        self.attest_fence()
        return {'worker_pid': pid, 'worker_executable': executable, 'worker_start': start}

    def complete_unchanged(self, prior):
        """RPC-only completion using this connection's server-held snapshot.

        No service mutation occurred. Persist checked evidence before acknowledging
        restore; never accept a caller-selected historical prior on the wire.
        """
        if self.record is not None or self.snapshot() != prior:
            raise Refused('snapshot_changed')
        healthy = self.health() if prior.service else self.closed(8080) and self.absent()
        if not healthy or self.snapshot() != prior:
            raise Refused('unchanged_health_required')
        self.record = {'phase': 'restored', 'prior': asdict(prior),
                       'verified_prior': asdict(prior), 'completion': 'unchanged'}
        self.save()

    def restore(self, prior):
        if not self.record or asdict(prior) != self.record['prior']:
            raise Refused('prior_mismatch')
        if self.fingerprint() != prior.generation:
            raise Refused('installation_changed')
        if self.record.get('completion') == 'unchanged':
            if not self.verify(prior):
                raise Refused('restoration_unverified')
            return
        self.publish_fence(True)
        if self.record.get('runner_session') != 'closed':
            self.record['phase'] = 'restore_pending'
            self.save()
            # Keep the managed service disabled even on partial fence failure.
            self.mutate('disable', LLAMA)
            if self.lc.loaded(LLAMA):
                self.mutate('bootout', LLAMA)
            if not runner_absent(self.record.get('peer')):
                raise Refused('runner_still_alive')
            self.record['runner_session'] = 'closed'
            self.save()
        # Before the first bootstrap, absence is mandatory even if an unexpected
        # service has appeared. Retries after our bootstrap use health instead.
        if self.record['phase'] in ('fenced', 'fencing', 'restore_pending'):
            if self.lc.loaded(LLAMA):
                self.mutate('disable', LLAMA)
                self.mutate('bootout', LLAMA)
        if not self.lc.loaded(LLAMA) and (not self.closed(8080) or not self.absent()):
            raise Refused('inference_still_running')
        self.record['phase'] = 'restoring'
        self.save()
        states = json.loads(prior.config)
        # Worker and tunnel are observed, NEVER mutated.
        for label in (LLAMA,):
            target = states[label]
            if target['loaded'] and not self.lc.loaded(label):
                self.mutate('enable', label)
                self.mutate('bootstrap', label)
            elif not target['loaded'] and self.lc.loaded(label):
                self.mutate('bootout', label)
            self.mutate('disable' if target['disabled'] else 'enable', label)
        self.record['phase'] = 'restored'
        self.save()

    def verify(self, prior):
        if not self.record or asdict(prior) != self.record['prior']:
            return False
        return (self.record['phase'] == 'restored' and self.fingerprint() == prior.generation
                and self.lc.state() == json.loads(prior.config)
                and (self.health() if prior.service else (self.closed(8080) and self.absent())))

    def release(self):
        if not self.record or not self.verify(PriorState(**self.record['prior'])):
            raise Refused('restoration_unverified')
        self.record = None
        self.save()
        self.publish_fence(False)

    def recover(self):
        if self.record:
            prior = PriorState(**self.record['prior'])
            self.restore(prior)
            self.release()
        else:
            # Fresh/idle startup is not evidence of health. Keep admission closed
            # until a stable baseline has been observed without service mutations.
            self.publish_fence(True)
            generation = self.fingerprint()
            states = self.lc.state()
            healthy = (self.health() if states[LLAMA]['loaded'] else
                       self.closed(8080) and self.absent())
            if not healthy or self.lc.state() != states or self.fingerprint() != generation:
                raise Refused('startup_health_required')
            self.publish_fence(False)


def file_digest(path):
    h = hashlib.sha256()
    with protected(path).open('rb') as f:
        for chunk in iter(lambda: f.read(1024 * 1024), b''):
            h.update(chunk)
    return h.hexdigest()


class ApplyPolicy:
    """Operator-only fixed paths and fields; never interpret caller configuration."""
    def __init__(self, cfg, *, installing=False):
        raw = protected(APPLY_CATALOG).read_bytes()
        self.manifest = hashlib.sha256(raw).hexdigest()
        if not installing and self.manifest != cfg.apply_manifest:
            raise Refused('apply_manifest_changed')
        obj = decode(raw, limit=16384)
        if (type(obj) is not dict or set(obj) != {'version', 'supervisor_sha256', 'presets'}
                or type(obj['version']) is not int or obj['version'] != 1
                or type(obj['presets']) is not list or not 1 <= len(obj['presets']) <= 16):
            raise Refused('apply_catalog_schema')
        self.original = protected(SUPERVISOR if installing else APPLY_BASELINE).read_bytes()
        if (len(self.original) > 65536 or self.original.count(APPLY_FLAGS) != 1 or
                hashlib.sha256(self.original).hexdigest() != obj['supervisor_sha256']):
            raise Refused('apply_supervisor_binding')
        models = INSTALL_ROOT / 'models'
        server = INSTALL_ROOT / 'bin/llama-server'
        if (Path(cfg.inventory.model_directory) != models or
                Path(cfg.inventory.server) != server):
            raise Refused('apply_inventory_binding')
        protected(models, directory=True)
        inventory = {}
        for index, path in enumerate(sorted(models.iterdir()), 1):
            if index > 1024:
                raise Refused('inventory_limit')
            if path.suffix.lower() != '.gguf':
                continue
            if (not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.-]{0,127}\.gguf', path.name)
                    or protected(path).stat().st_mode & 0o222):
                raise Refused('apply_immutable_binding')
            digest = file_digest(path)
            if digest in inventory:
                raise Refused('apply_ambiguous_model')
            inventory[digest] = path.name
        if not 1 <= len(inventory) <= 64 or protected(server).stat().st_mode & 0o222:
            raise Refused('apply_immutable_binding')
        server_hash = file_digest(server)
        assignment = re.findall(rb'M=\$R/models/([A-Za-z0-9][A-Za-z0-9_.-]{0,127}\.gguf);',
                                self.original)
        if len(assignment) != 1 or assignment[0].decode() not in inventory.values():
            raise Refused('apply_supervisor_binding')
        literal = b'M=$R/models/' + assignment[0] + b';'
        self.presets = {}
        self.bindings = {}
        fields = {'model_sha256', 'server_sha256', 'ctx_size', 'parallel',
                  'cache_type_k', 'cache_type_v'}
        for entry in obj['presets']:
            if (type(entry) is not dict or set(entry) != fields or
                    type(entry['model_sha256']) is not str or entry['model_sha256'] not in inventory or entry['server_sha256'] != server_hash or
                    type(entry['ctx_size']) is not int or not 512 <= entry['ctx_size'] <= 262144 or
                    type(entry['parallel']) is not int or not 1 <= entry['parallel'] <= 16 or
                    entry['ctx_size'] % entry['parallel'] or
                    entry['cache_type_k'] not in ('f16', 'q8_0', 'q4_0') or
                    entry['cache_type_v'] not in ('f16', 'q8_0', 'q4_0')):
                raise Refused('apply_preset_schema')
            key = hashlib.sha256(json.dumps(entry, sort_keys=True,
                                           separators=(',', ':')).encode()).hexdigest()
            flags = ('--ctx-size %d --parallel %d -ctk %s -ctv %s' %
                     (entry['ctx_size'], entry['parallel'], entry['cache_type_k'],
                      entry['cache_type_v'])).encode()
            self.presets[key] = self.original.replace(APPLY_FLAGS, flags).replace(
                literal, b'M=$R/models/' + inventory[entry['model_sha256']].encode() + b';')
            self.bindings[key] = dict(entry)
        self.current()

    def current(self):
        raw = protected(SUPERVISOR).read_bytes()
        if raw != self.original and raw not in self.presets.values():
            raise Refused('apply_config_changed')
        return raw

    def write(self, raw):
        self.current()
        check_deadline()
        if raw != self.original and raw not in self.presets.values():
            raise Refused('apply_config_untrusted')
        durable(SUPERVISOR, raw, mode=0o755)


class ProductionApplyAdapter(ServiceAdapter):
    def __init__(self, *args, policy, readiness_timeout=30, readiness_interval=0.25, **kwargs):
        self.readiness_timeout = readiness_timeout
        self.readiness_interval = readiness_interval
        self.policy = policy
        self.persistence_failed = False
        super().__init__(*args, **kwargs)
        self.receipts = self.root / 'apply-receipts'
        self.receipts.mkdir(mode=0o700, exist_ok=True)
        protected(self.receipts, private=True, directory=True)

    def save(self):
        try:
            super().save()
        except OSError:
            self.persistence_failed = True
            raise

    def persist(self, path, value, mode=0o600):
        try:
            durable(path, value, mode)
        except OSError:
            self.persistence_failed = True
            raise

    def publish_fence(self, active):
        self.persist(self.fence_path, {'version': 1, 'active': active}, mode=0o644)

    def require_persistence(self):
        if self.persistence_failed:
            raise Refused('apply_persistence_uncertain')

    def wait_verified(self, prior):
        deadline = time.monotonic() + self.readiness_timeout
        while True:
            verified = self.verify(prior)
            check_deadline()
            if time.monotonic() >= deadline:
                return False
            if verified:
                return True
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                return False
            time.sleep(min(self.readiness_interval, remaining))

    def stable_current(self, config_generation):
        prior = self.snapshot()
        original = self.policy.current()
        if hashlib.sha256(original).hexdigest() != config_generation:
            raise Refused('apply_stale_config')
        deadline = time.monotonic() + self.readiness_timeout
        while not (self.health() if prior.service else self.closed(8080) and self.absent()):
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise Refused('apply_health_required')
            time.sleep(min(self.readiness_interval, remaining))
        check_deadline()
        if time.monotonic() >= deadline:
            raise Refused('apply_health_required')
        if self.snapshot() != prior or self.policy.current() != original:
            raise Refused('snapshot_changed')
        return prior, original

    def abort(self, preset, transaction_id, generation, config_generation):
        self.require_persistence()
        self.validate_binding(preset, transaction_id, generation, config_generation)
        if self.record is not None or self.peer is None:
            raise Refused('apply_state')
        prior, original = self.stable_current(config_generation)
        check_deadline()
        self.record = {'phase': 'restored', 'prior': asdict(prior), 'peer': self.peer,
                       'runner_session': 'closed', 'apply': {
                           'preset': preset, 'transaction_id': transaction_id,
                           'generation': generation, 'original': original.hex(),
                           'original_config_digest': config_generation,
                           'target_config_digest': hashlib.sha256(self.policy.presets[preset]).hexdigest(),
                           'phase': 'rolled_back', 'no_op': True}}
        self.save()
        self.publish_fence(True)
        self.persist(self.receipts / (transaction_id + '.json'), self.status(), mode=0o400)
        return self.status()

    def status(self):
        a = (self.record or {}).get('apply')
        if a:
            return {k: a[k] for k in ('transaction_id', 'generation', 'preset', 'phase',
                                      'original_config_digest', 'target_config_digest')}
        latest = self.root / 'apply-latest.json'
        return decode(protected(latest, private=True).read_bytes()) if latest.exists() else None

    def presets(self):
        return {'generation': self.policy.manifest, 'presets': self.policy.bindings,
                'config_generation': hashlib.sha256(self.policy.current()).hexdigest()}

    def validate_binding(self, preset, transaction_id, generation, config_generation):
        if (type(transaction_id) is not str or not re.fullmatch(r'[0-9a-f]{64}', transaction_id)
                or type(generation) is not int or generation != 1
                or type(config_generation) is not str
                or not re.fullmatch(r'[0-9a-f]{64}', config_generation)):
            raise Refused('apply_binding_schema')
        if (self.receipts / (transaction_id + '.json')).exists():
            raise Refused('apply_transaction_reused')
        if type(preset) is not str or preset not in self.policy.presets:
            raise Refused('apply_preset_unknown')
    def prepare(self, preset, prior, transaction_id, generation, config_generation):
        self.require_persistence()
        self.validate_binding(preset, transaction_id, generation, config_generation)
        if self.record or self.snapshot() != prior or self.peer is None:
            raise Refused('apply_state')
        original = self.policy.current()
        if hashlib.sha256(original).hexdigest() != config_generation:
            raise Refused('apply_stale_config')
        check_deadline()
        self.record = {'phase': 'fencing', 'prior': asdict(prior), 'peer': self.peer,
                       'runner_session': 'active', 'apply': {'preset': preset,
                       'transaction_id': transaction_id, 'generation': generation,
                       'original_config_digest': config_generation,
                       'target_config_digest': hashlib.sha256(self.policy.presets[preset]).hexdigest(),
                       'phase': 'prepared', 'original': original.hex()}}
        self.save()
        self.fence()
        self.drain()
        return self.status()

    def authorize(self, peer, takeover=False):
        if peer != self.record.get('peer'):
            if not takeover or not runner_absent(self.record.get('peer')):
                raise Refused('runner_still_alive')
        # Retain original peer for independent recovery absence checks.

    def apply(self):
        self.require_persistence()
        a = self.record['apply']
        if a['phase'] != 'prepared':
            raise Refused('apply_state')
        self.drain()
        a['phase'] = 'mutating'
        self.save()
        self.policy.write(self.policy.presets[a['preset']])
        # Same attested runner contract as restore: no more child spawning.
        self.record['runner_session'] = 'closed'
        self.save()
        prior = PriorState(**self.record['prior'])
        super().restore(prior)
        if not self.wait_verified(prior):
            raise Refused('apply_health_required')
        a['phase'] = 'applied'
        self.save()
        return self.status()

    def verify(self, prior):
        a = (self.record or {}).get('apply')
        if a:
            expected = (bytes.fromhex(a['original']) if a['phase'] in ('rolling_back', 'rollback_restoring', 'rolled_back')
                        else self.policy.presets[a['preset']])
            if self.policy.current() != expected:
                return False
        return super().verify(prior)

    def finalize(self, transaction_id, generation, agreement):
        self.require_persistence()
        decision = self.status()
        if (not decision or type(generation) is not int or generation != 1 or
                transaction_id != decision['transaction_id'] or
                generation != decision['generation'] or
                agreement not in ('applied', 'rolled_back') or agreement != decision['phase']):
            raise Refused('apply_agreement_mismatch')
        receipt = self.receipts / (transaction_id + '.json')
        if not self.record:
            if not receipt.exists() or decode(protected(receipt, private=True).read_bytes()) != decision:
                raise Refused('apply_receipt_mismatch')
            digest = decision['target_config_digest'] if agreement == 'applied' else decision['original_config_digest']
            self.publish_fence(True)
            self.stable_current(digest)
            self.publish_fence(False)
            return decision
        if not self.wait_verified(PriorState(**self.record['prior'])):
            raise Refused('apply_finalize_unverified')
        if receipt.exists():
            if decode(protected(receipt, private=True).read_bytes()) != decision:
                raise Refused('apply_receipt_mismatch')
        else:
            self.persist(receipt, decision, mode=0o400)
        self.persist(self.root / 'apply-latest.json', decision)
        self.record['finalizing'] = True
        self.save()
        self.persist(self.journal, {'phase': 'idle'})
        self.record = None
        self.publish_fence(False)
        return decision

    def release(self):
        if self.record and 'apply' in self.record:
            raise Refused('apply_finalize_required')
        super().release()

    def rollback(self, *, explicit=False):
        self.require_persistence()
        a = self.record['apply']
        if a['phase'] == 'rolled_back':
            if not self.verify(PriorState(**self.record['prior'])):
                raise Refused('apply_rollback_unverified')
            return self.status()
        if a['phase'] == 'applied':
            raise Refused('apply_irreversible')
        self.publish_fence(True)
        if a['phase'] == 'rollback_restoring':
            return self.finish_rollback()
        # Keep managed inference stopped on ambiguous/live-peer recovery too.
        # The original journal already precedes these service mutations.
        self.mutate('disable', LLAMA)
        if self.lc.loaded(LLAMA):
            self.mutate('bootout', LLAMA)
        # Even apply's health stage is not proof of runner absence after EOF.
        if not explicit and not runner_absent(self.record.get('peer')):
            raise Refused('runner_still_alive')
        a['phase'] = 'rolling_back'
        self.save()
        if not self.closed(8080) or not self.absent():
            raise Refused('inference_still_running')
        self.policy.write(bytes.fromhex(a['original']))
        self.record['runner_session'] = 'closed'
        self.record['phase'] = 'restoring'
        a['phase'] = 'rollback_restoring'
        self.save()  # durable progress before bootstrap; retries must not bootout
        return self.finish_rollback()

    def finish_rollback(self):
        super().restore(PriorState(**self.record['prior']))
        if not self.wait_verified(PriorState(**self.record['prior'])):
            raise Refused('apply_rollback_unverified')
        self.record['apply']['phase'] = 'rolled_back'
        self.save()
        return self.status()

    def recover(self):
        if self.persistence_failed:
            self.publish_fence(True)
            raise Refused('apply_persistence_uncertain')
        if self.record and 'apply' in self.record:
            self.publish_fence(True)
            if self.record['apply']['phase'] in ('applied', 'rolled_back'):
                if not self.wait_verified(PriorState(**self.record['prior'])):
                    raise Refused('apply_recovery_unverified')
            else:
                self.rollback()
        else:
            super().recover()


def receive(conn, deadline=None, limit=MAX_MESSAGE):
    # One byte at a time: never consume a subsequent request or allocate unbounded input.
    raw = bytearray()
    while len(raw) <= limit:
        if deadline is not None:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise Refused('lease_expired')
            conn.settimeout(min(remaining, 5))
        b = conn.recv(1)
        if not b:
            raise EOFError
        if b == b'\n':
            return decode(bytes(raw), limit=limit)
        raw.extend(b)
    raise Refused('message_limit')


def send(conn, value):
    raw = json.dumps(value, separators=(',', ':'), allow_nan=False).encode()
    if len(raw) > 16384:
        raise Refused('message_limit')
    conn.sendall(raw + b'\n')


def session(conn, adapter, lease_seconds, inventory=None, capability=lambda: False,
            peer_attestor=None):
    """EOF/deadline revoke the connection, NOT a live runner's spawn ability."""
    started = time.monotonic()
    deadline = started + lease_seconds
    token = _deadline.set(deadline)
    attest_peer = peer_attestor or runner_peer
    prior = None
    session_closed = False
    touched = False
    try:
        # Protected installation is immutable to the peer. Attest once per
        # connection, not on the one-second watchdog heartbeat.
        execution_enabled = capability() is True
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise Refused('lease_expired')
            conn.settimeout(min(remaining, 5))
            request = receive(conn, deadline)
            if (not isinstance(request, dict) or
                    set(request) != ({'op', 'preset', 'transaction_id', 'generation', 'config_generation'}
                                     if request.get('op') in ('prepare', 'abort') else
                                     {'op', 'transaction_id', 'generation', 'agreement'}
                                     if request.get('op') == 'finalize' else {'op'})):
                raise Refused('request_schema')
            op = request['op']
            if isinstance(adapter, ProductionApplyAdapter) and op in (
                    'prepare', 'abort', 'apply', 'rollback', 'recover', 'finalize'):
                deadline = min(deadline, started + 100)
                _deadline.set(deadline)
            check_deadline()
            if ((peer_attestor is not None or isinstance(adapter, ProductionApplyAdapter))
                    and op not in ('status', 'presets', 'ping', 'inventory', 'snapshot',
                                   'verify', 'admission', 'drain')):
                attest_peer(conn)
                check_deadline()
            if isinstance(adapter, ProductionApplyAdapter):
                adapter.require_persistence()
            applying = adapter.record is not None and 'apply' in adapter.record
            if applying and op not in ('apply', 'finalize', 'recover', 'rollback', 'status', 'presets', 'ping'):
                raise Refused('apply_operation_required')
            if op not in ('status', 'presets', 'ping', 'inventory', 'snapshot'):
                touched = True
            if op == 'presets' and isinstance(adapter, ProductionApplyAdapter):
                value = adapter.presets()
            elif op == 'abort' and isinstance(adapter, ProductionApplyAdapter):
                adapter.peer = attest_peer(conn)
                value = adapter.abort(request['preset'], request['transaction_id'],
                                      request['generation'], request['config_generation'])
            elif op == 'finalize' and isinstance(adapter, ProductionApplyAdapter):
                peer = attest_peer(conn)
                if applying:
                    adapter.authorize(peer, takeover=True)
                value = adapter.finalize(request['transaction_id'], request['generation'],
                                         request['agreement'])
                check_deadline()
                send(conn, {'ok': True, 'value': value})
                return
            elif op == 'recover' and applying:
                adapter.authorize(attest_peer(conn), takeover=True)
                adapter.recover()
                value = adapter.status()
            elif op == 'status' and isinstance(adapter, ProductionApplyAdapter):
                value = adapter.status()
            elif op == 'prepare' and isinstance(adapter, ProductionApplyAdapter) and prior is not None and not session_closed:
                adapter.peer = attest_peer(conn)
                value = adapter.prepare(request['preset'], prior, request['transaction_id'],
                                        request['generation'], request['config_generation'])
            elif op in ('apply', 'rollback') and applying:
                adapter.authorize(attest_peer(conn))
                value = adapter.apply() if op == 'apply' else adapter.rollback(explicit=True)
                session_closed = True
            elif op == 'snapshot' and prior is None:
                prior = adapter.snapshot()
                value = asdict(prior)
            elif op == 'fence' and prior is not None and not session_closed:
                # No client-supplied prior; refuse changes since snapshot.
                if adapter.snapshot() != prior:
                    raise Refused('snapshot_changed')
                adapter.peer = attest_peer(conn)
                adapter.fence()
                value = None
            elif op == 'drain' and prior is not None:
                value = asdict(adapter.drain())
            elif op == 'admission' and prior is not None and execution_enabled:
                value = adapter.admission()
            elif op == 'restore' and prior is not None:
                if adapter.record is None:
                    adapter.complete_unchanged(prior)
                else:
                    if attest_peer(conn) != adapter.record.get('peer'):
                        raise Refused('peer_identity_changed')
                    # Attested trusted runner promises child teardown complete and
                    # no future spawn on this session before sending restore.
                    adapter.record['runner_session'] = 'closed'
                    adapter.save()
                    adapter.restore(prior)
                session_closed = True
                value = None
            elif op == 'verify' and prior is not None:
                value = adapter.verify(prior)
            elif op == 'release' and prior is not None:
                adapter.release()
                check_deadline()
                send(conn, {'ok': True, 'value': None})
                return
            elif op == 'inventory' and inventory is not None:
                value = asdict(inventory)
            elif op == 'ping':
                value = {'service_control': True, 'benchmark_execution': execution_enabled}
                if isinstance(adapter, ProductionApplyAdapter):
                    value['production_apply'] = True
            else:
                raise Refused('operation_not_allowed')
            check_deadline()
            send(conn, {'ok': True, 'value': value})
    finally:
        _deadline.reset(token)
        conn.close()  # heartbeat cancellation; never wait for exit with socket open
        if touched or not isinstance(adapter, ProductionApplyAdapter):
            # Cleanup is not an extension of the expired RPC and cannot ack it.
            # Recovery retains its journal/fence on failure for a later retry.
            recovery_token = _deadline.set(time.monotonic() + 100)
            try:
                adapter.recover()  # readonly Apply connections never mutate
            finally:
                _deadline.reset(recovery_token)


def peer_uid(conn):
    if sys.platform != 'darwin':
        raise Refused('macos_required')
    # CPython builds do not consistently expose socket.getpeereid(). Use the
    # fixed system libc API; credentials are supplied by the kernel.
    import ctypes
    libc = ctypes.CDLL('/usr/lib/libSystem.B.dylib', use_errno=True)
    fn = libc.getpeereid
    fn.argtypes = [ctypes.c_int, ctypes.POINTER(ctypes.c_uint), ctypes.POINTER(ctypes.c_uint)]
    fn.restype = ctypes.c_int
    uid, gid = ctypes.c_uint(), ctypes.c_uint()
    if fn(conn.fileno(), ctypes.byref(uid), ctypes.byref(gid)) != 0:
        raise Refused('peer_credentials_failed')
    return uid.value


def serve():
    # No socket or inference mutation exists until full attestation completes.
    attestation = StartupAttestation()
    cfg = attestation.cfg
    fingerprint = attestation.fingerprint
    lock = os.open(STATE / 'daemon.lock', os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    try:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        journal = STATE / 'service-journal.json'
        if journal.exists() or journal.is_symlink():
            protected(journal, private=True)
        adapter = (ProductionApplyAdapter(STATE, Launchctl(), fingerprint,
                   fence_path=FENCE, policy=attestation.policy) if cfg.enable_apply else
                   ServiceAdapter(STATE, Launchctl(), fingerprint, fence_path=FENCE))
        adapter.recover()  # crash/reboot recovery before exposing capability
        if SOCKET.exists() or SOCKET.is_symlink():
            s = SOCKET.lstat()
            if not stat.S_ISSOCK(s.st_mode) or s.st_uid != 0:
                raise Refused('unsafe_socket')
            SOCKET.unlink()
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as server:
            server.bind(str(SOCKET))
            os.chown(SOCKET, 0, pwd.getpwuid(cfg.worker_uid).pw_gid)
            os.chmod(SOCKET, 0o660)
            server.listen(4)
            while True:
                conn, _ = server.accept()
                with conn:
                    if peer_uid(conn) != cfg.worker_uid:
                        continue
                    fingerprint()
                    try:
                        session(conn, adapter, cfg.lease_seconds, cfg.inventory,
                                lambda: cfg.enable_execution, peer_attestor=attestation.peer)
                    except (OSError, EOFError, Refused):
                        # Recovery failure is fail-closed and launchd retries on restart.
                        if adapter.record or getattr(adapter, 'persistence_failed', False):
                            raise Refused('recovery_required') from None
    finally:
        os.close(lock)


class ServiceClient:
    """Unprivileged TrustedAdapter-shaped client; one connection owns one lease.

    Keep socket open throughout benchmark. No root permission needed to import.
    Local runner's own journal/root is separate; this object does not expose it.
    """
    def __init__(self, socket_path=SOCKET):
        self.conn = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.conn.settimeout(120)
        self.conn.connect(str(socket_path))
        if peer_uid(self.conn) != 0:
            self.conn.close()
            raise Refused('untrusted_daemon')
        self.prior = None

    @staticmethod
    def admission_fenced():
        """Local, connection-independent check before lease AND execution.

        Missing, unsafe or malformed installation state denies admission. This
        check does not attest benchmark execution or replace coordinator drain.
        """
        try:
            path = protected(FENCE)
            with path.open('rb') as f:
                obj = decode(f.read(MAX_MESSAGE + 1))
            return not (type(obj) is dict and set(obj) == {'version', 'active'}
                        and type(obj['version']) is int and obj['version'] == 1
                        and obj['active'] is False)
        except (OSError, Refused):
            return True

    def inventory(self):
        """Read-only operator inventory; no request-supplied paths accepted."""
        return Inventory(**self.call('inventory'))

    def call(self, op, **fields):
        send(self.conn, {'op': op, **fields})
        reply = receive(self.conn, limit=16384)
        if not isinstance(reply, dict) or reply.get('ok') is not True:
            raise Refused('service_refused')
        return reply['value']

    def prepare(self, preset, transaction_id, generation, config_generation):
        return self.call('prepare', preset=preset, transaction_id=transaction_id,
                         generation=generation, config_generation=config_generation)

    def abort(self, preset, transaction_id, generation, config_generation):
        return self.call('abort', preset=preset, transaction_id=transaction_id,
                         generation=generation, config_generation=config_generation)

    def apply(self):
        return self.call('apply')

    def finalize(self, transaction_id, generation, agreement):
        return self.call('finalize', transaction_id=transaction_id,
                         generation=generation, agreement=agreement)

    def presets(self):
        return self.call('presets')

    def rollback(self):
        return self.call('rollback')

    def status(self):
        return self.call('status')

    def snapshot(self):
        self.prior = PriorState(**self.call('snapshot'))
        return self.prior

    def ping(self):
        """Call at least every 3 seconds while idle; never extends lease deadline."""
        return self.call('ping')

    def admission(self):
        """Attest the managed worker after fence/drain; accepts no PID or path."""
        return self.call('admission')

    def fence(self):
        self.call('fence')

    def drain(self):
        return DrainAck(**self.call('drain'))

    def restore(self, prior):
        if asdict(prior) != asdict(self.prior):
            raise Refused('prior_mismatch')
        self.call('restore')

    def verify(self, prior):
        return asdict(prior) == asdict(self.prior) and self.call('verify') is True

    def release(self):
        self.call('release')

    def close(self):
        self.conn.close()

    def __enter__(self):
        return self

    def __exit__(self, *args):
        self.close()


def daemon_plist():
    return {'Label': DAEMON_LABEL, 'ProgramArguments': ['/usr/bin/python3', '-I', '-S', str(CODE), 'serve'],
            'UserName': 'root', 'WorkingDirectory': '/', 'RunAtLoad': True,
            'KeepAlive': True, 'ThrottleInterval': 10, 'Umask': 0o077,
            'EnvironmentVariables': ENV}


def install(lease_seconds, inventory, enable_execution=False, enable_apply=False):
    """Fresh-only explicit operator installation. Never starts or stops launchd."""
    if sys.platform != 'darwin' or os.geteuid() != 0:
        raise Refused('macos_root_required')
    source = protected(Path(__file__).absolute())
    protected(ROOT.parent, directory=True)
    protected(PLISTS, directory=True)
    protected('/private/var/run', directory=True)
    verify_services()
    inventory = Inventory.parse(asdict(inventory))
    uid = pwd.getpwnam('nocturne-worker').pw_uid
    if uid <= 0 or not 10 <= lease_seconds <= 600:
        raise Refused('installation_bounds')
    if any(p.exists() or p.is_symlink() for p in (ROOT, SOCKET_DIR, DAEMON_PLIST)):
        raise Refused('fresh_install_required')
    if type(enable_execution) is not bool:
        raise Refused('execution_schema')
    if type(enable_apply) is not bool or (enable_apply and not enable_execution):
        raise Refused('apply_requires_execution')
    policy = (ApplyPolicy(InstallationConfig(1, uid, lease_seconds, inventory), installing=True)
              if enable_apply else None)
    manifest = execution_digest(inventory, provision=True) if enable_execution else None
    ROOT.mkdir(mode=0o755)
    STATE.mkdir(mode=0o700)
    SOCKET_DIR.mkdir(mode=0o755)
    # Source is independently provisioned protected code, never a worker checkout.
    for path, data, mode in ((CODE, source.read_bytes(), 0o644),
                             (DAEMON_PLIST, plistlib.dumps(daemon_plist()), 0o644)):
        fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, mode)
        with os.fdopen(fd, 'wb') as f:
            f.write(data)
            f.flush()
            os.fsync(f.fileno())
    if policy:
        durable(APPLY_BASELINE, policy.original)
    durable(CONFIG, asdict(InstallationConfig(1, uid, lease_seconds, inventory,
                                            enable_execution, manifest, enable_apply,
                                            policy.manifest if policy else None)))
    durable(FENCE, {'version': 1, 'active': True}, mode=0o644)
    for path in (ROOT, STATE, ROOT.parent, SOCKET_DIR.parent, PLISTS):
        fd = os.open(path, os.O_RDONLY)
        try:
            os.fsync(fd)
        finally:
            os.close(fd)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=('verify', 'install', 'serve'))
    parser.add_argument('--lease-seconds', type=int, default=600)
    parser.add_argument('--model-directory')
    parser.add_argument('--server')
    parser.add_argument('--code-revision')
    parser.add_argument('--enable-execution', action='store_true')
    parser.add_argument('--enable-apply', action='store_true')
    args = parser.parse_args()
    try:
        # Root entry point must not load global site/.pth hooks before validation.
        # Apple's system interpreter and stdlib remain an OS trust boundary.
        if not sys.flags.isolated or not sys.flags.no_site or sys.version_info < (3, 9):
            raise Refused('isolated_system_runtime_required')
        if args.action == 'install':
            install(args.lease_seconds, Inventory(args.model_directory, args.server, args.code_revision),
                    args.enable_execution, args.enable_apply)
        elif args.action == 'verify':
            verify_installation()  # read-only: NO launchctl invocation
        else:
            serve()
        return 0
    except Exception:
        # Never expose config, process output, paths or credential-bearing exceptions.
        print('benchmark_service_refused', file=sys.stderr)
        return 1


if __name__ == '__main__':
    raise SystemExit(main())
