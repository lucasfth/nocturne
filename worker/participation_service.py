"""Bounded JSON-lines local control protocol; no command execution.

Run with python -m worker.participation_service --help. The socket directory
must be dedicated to IPC, NEVER a secrets/state directory. UID is startup-only.
"""

import argparse
import fcntl
import json
import os
from pathlib import Path
import signal
import select
import socket
import stat
import sys
import threading
import time

from .participation import Config, ParticipationPolicy, participation_sensors, trusted_directory_chain
from .model_supervisor import ModelSupervisor


MAX_REQUEST = 2048


def peer_uid(connection):
    """Darwin getpeereid(3), not Linux SO_PEERCRED or a guessed struct ABI."""
    if sys.platform != 'darwin':
        raise PermissionError('peer authentication requires macOS')
    if hasattr(connection, 'getpeereid'):
        return connection.getpeereid()[0]
    import ctypes
    libc = ctypes.CDLL('/usr/lib/libSystem.B.dylib', use_errno=True)
    getpeereid = libc.getpeereid
    getpeereid.argtypes = [ctypes.c_int, ctypes.POINTER(ctypes.c_uint), ctypes.POINTER(ctypes.c_uint)]
    getpeereid.restype = ctypes.c_int
    uid, gid = ctypes.c_uint(), ctypes.c_uint()
    if getpeereid(connection.fileno(), ctypes.byref(uid), ctypes.byref(gid)) != 0:
        raise PermissionError('getpeereid failed')
    return uid.value


def console_uid():
    if sys.platform != 'darwin':
        raise PermissionError('macOS console required')
    return os.stat('/dev/console').st_uid


class ParticipationService:
    def __init__(self, policy, socket_path, permitted_uid, *, status_socket, worker_uid,
                 peer=peer_uid, console=console_uid):
        if type(permitted_uid) is not int or not 1 <= permitted_uid < 2**32:
            raise ValueError('a non-root configured console uid is required')
        if type(worker_uid) is not int or not 0 <= worker_uid < 2**32:
            raise ValueError('an explicit worker uid is required')
        if Path(socket_path).resolve() == Path(status_socket).resolve():
            raise ValueError('separate sockets required')
        self._worker_uid = worker_uid
        self.status_path = Path(status_socket)
        self.policy = policy
        self.path = Path(socket_path)
        self._permitted_uid = permitted_uid
        self.peer = peer
        self.console = console
        self.listeners = {}
        self._socket_identities = {}
        self._instance_locks = []

    @property
    def permitted_uid(self):
        return self._permitted_uid

    @property
    def worker_uid(self):
        return self._worker_uid

    def _authorize(self, connection, role):
        uid = self.peer(connection)
        if role == 'worker':
            if uid != self._worker_uid:
                raise PermissionError('not the configured worker')
        elif role != 'ui' or uid != self._permitted_uid or self.console() != self._permitted_uid:
            raise PermissionError('not the configured active console user')

    def dispatch(self, request, role='ui'):
        if not isinstance(request, dict) or type(request.get('op')) is not str:
            raise ValueError('invalid request')
        op = request['op']
        if role not in ('ui', 'worker') or (role == 'worker' and op != 'status'):
            raise PermissionError('operation not allowed for role')
        if op in ('status', 'resume') and set(request) == {'op'}:
            return getattr(self.policy, op)()
        if op == 'acknowledge_warning' and set(request) == {'op', 'warning_id'}:
            return self.policy.acknowledge_warning(request['warning_id'])
        if op == 'pause' and set(request) == {'op', 'hours'}:
            return self.policy.pause(request['hours'])
        if op == 'ui_heartbeat' and set(request) in ({'op', 'notification_capable'}, {'op', 'notification_capable', 'warning_id'}):
            return self.policy.ui_heartbeat(request['notification_capable'], request.get('warning_id'))
        raise ValueError('unknown operation or fields')

    def handle(self, connection, role='ui'):
        try:
            connection.settimeout(1)
            self._authorize(connection, role)
            raw = bytearray()
            deadline = time.monotonic() + 1
            while b'\n' not in raw:
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise ValueError('request timed out')
                connection.settimeout(remaining)
                chunk = connection.recv(min(512, MAX_REQUEST + 1 - len(raw)))
                if not chunk:
                    raise ValueError('incomplete request')
                raw.extend(chunk)
                if len(raw) > MAX_REQUEST:
                    raise ValueError('request too large')
            if raw.count(b'\n') != 1 or not raw.endswith(b'\n'):
                raise ValueError('one request per connection')
            def unique_pairs(pairs):
                result = {}
                for key, value in pairs:
                    if key in result:
                        raise ValueError('duplicate field')
                    result[key] = value
                return result
            request = json.loads(raw, object_pairs_hook=unique_pairs,
                                 parse_constant=lambda value: (_ for _ in ()).throw(ValueError('nonfinite JSON')))
            # Console may change while a client slowly sends a request.
            self._authorize(connection, role)
            response = dict(ok=True, status=self.dispatch(request, role))
        except Exception:
            response = dict(ok=False, error='request denied')
        try:
            connection.sendall(json.dumps(response, allow_nan=False).encode() + b'\n')
        except OSError:
            pass

    def open(self):
        if os.geteuid() != 0 and not os.geteuid() == self.permitted_uid == self.worker_uid:
            raise PermissionError('service must be root or both configured uids')
        try:
            self._open_socket(self.path, self.permitted_uid, 'ui')
            self._open_socket(self.status_path, self.worker_uid, 'worker')
        except Exception:
            self.close()
            raise

    def _open_socket(self, path, owner, role):
        parent = path.parent
        # Root can expose traversal only, with socket access restricted to the
        # configured UID. A per-user daemon keeps its directory entirely private.
        mode = 0o711 if os.geteuid() == 0 else 0o700
        if os.geteuid() not in (0, self._permitted_uid):
            raise PermissionError('service must run as root or permitted uid')
        state_parent = self.policy.state_path.parent.resolve()
        if (parent.resolve() == state_parent or state_parent.is_relative_to(parent.resolve()) or
                parent.resolve().is_relative_to(state_parent)):
            raise ValueError('IPC directory must be separate from state/secrets')
        parent.mkdir(mode=mode, parents=True, exist_ok=True)
        trusted_directory_chain(parent)
        info = parent.lstat()
        if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.geteuid() or stat.S_IMODE(info.st_mode) != mode:
            raise PermissionError('unsafe IPC directory')
        # Per-path locks cover partially overlapping configurations as well.
        # Lock files are permanent: unlinking a flock file permits split ownership.
        lock_path = path.with_name(path.name + '.lock')
        fd = os.open(lock_path, os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW | os.O_NONBLOCK, 0o600)
        try:
            info = os.fstat(fd)
            if (not stat.S_ISREG(info.st_mode) or info.st_uid != os.geteuid() or
                    stat.S_IMODE(info.st_mode) != 0o600 or info.st_nlink != 1):
                raise PermissionError('unsafe instance lock')
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
            current = lock_path.lstat()
            if (current.st_dev, current.st_ino) != (info.st_dev, info.st_ino):
                raise PermissionError('instance lock replaced')
        except Exception:
            os.close(fd)
            raise
        self._instance_locks.append(fd)
        try:
            stale = path.lstat()
        except FileNotFoundError:
            stale = None
        if stale is not None:
            if not stat.S_ISSOCK(stale.st_mode) or stale.st_uid != owner or stale.st_nlink != 1:
                raise PermissionError('unsafe stale socket')
            current = path.lstat()
            if (current.st_dev, current.st_ino) != (stale.st_dev, stale.st_ino):
                raise PermissionError('stale socket replaced')
            path.unlink()
        listener = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        try:
            listener.bind(str(path))
            info = path.lstat()
            self._socket_identities[path] = (info.st_dev, info.st_ino)
            os.chmod(path, 0o600)
            if os.geteuid() == 0:
                os.chown(path, owner, -1)
            listener.listen(4)
            listener.settimeout(0.5)
            self.listeners[listener] = role
        except Exception:
            listener.close()
            self.close()
            raise

    def serve(self, stop):
        """Caller owns open/close; retain instance locks until model is reaped."""
        while not stop.is_set():
            readable, _, _ = select.select(list(self.listeners), [], [], 0.5)
            for listener in readable:
                connection, _ = listener.accept()
                with connection:
                    self.handle(connection, self.listeners[listener])

    def close(self):
        for listener in self.listeners:
            listener.close()
        self.listeners.clear()
        for path, identity in self._socket_identities.items():
            try:
                info = path.lstat()
                if (info.st_dev, info.st_ino) == identity and stat.S_ISSOCK(info.st_mode):
                    path.unlink()
            except FileNotFoundError:
                pass
        self._socket_identities.clear()
        for fd in self._instance_locks:
            os.close(fd)
        self._instance_locks.clear()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--state-path', required=True)
    parser.add_argument('--socket-path', required=True)
    parser.add_argument('--permitted-uid', type=int, required=True)
    parser.add_argument('--status-socket', required=True)
    parser.add_argument('--worker-uid', type=int, required=True)
    parser.add_argument('--worker-gid', type=int, required=True)
    parser.add_argument('--idle-seconds', type=float, default=600)
    parser.add_argument('--memory-budget-mib', type=float)
    parser.add_argument('--memory-reserve-mib', type=float, default=0)
    parser.add_argument('--enabled', action='store_true', help='explicit participation opt-in')
    parser.add_argument('--automatic-participation', action='store_true',
                        help='enroll in automatic starts after submitted warning grace (requires --enabled)')
    parser.add_argument('--model-argv', nargs=argparse.REMAINDER, default=[])
    args = parser.parse_args()
    if sys.platform != 'darwin':
        parser.error('macOS required')
    stop = threading.Event()
    for signum in (signal.SIGTERM, signal.SIGINT):
        signal.signal(signum, lambda *_: stop.set())
    def sensors():
        # A user switch also withdraws supervisor permission, not only IPC access.
        if console_uid() != args.permitted_uid:
            return {}
        sample = participation_sensors()
        return sample if console_uid() == args.permitted_uid else {}

    policy = ParticipationPolicy(args.state_path, Config(enabled=args.enabled,
        automatic_participation=args.automatic_participation,
        idle_seconds=args.idle_seconds, memory_budget_mib=args.memory_budget_mib,
        memory_reserve_mib=args.memory_reserve_mib), sensors=sensors)
    service = ParticipationService(policy, args.socket_path, args.permitted_uid,
                                   status_socket=args.status_socket, worker_uid=args.worker_uid)
    supervisor = ModelSupervisor(policy, args.model_argv, worker_uid=args.worker_uid,
                                 worker_gid=args.worker_gid) if args.model_argv else None
    thread = None
    try:
        service.open()  # Own instance locks before any model can be started.
        if supervisor:
            thread = threading.Thread(target=supervisor.run, args=(stop,), daemon=False)
            thread.start()
        service.serve(stop)
    finally:
        stop.set()
        if thread:
            thread.join()
        service.close()


if __name__ == '__main__':
    main()
