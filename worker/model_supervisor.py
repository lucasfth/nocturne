"""Exclusive owner of exactly one model child, never external PIDs/process groups."""

import math
import os
from pathlib import Path
import stat
import subprocess
import threading
import time


def trusted_executable(executable):
    """Privileged launch requires an absolute, root-controlled, non-symlink chain."""
    path = Path(executable)
    if not path.is_absolute() or '..' in path.parts:
        raise PermissionError('absolute trusted model executable required')
    for component in (*reversed(path.parents), path):
        info = component.lstat()
        expected = stat.S_ISREG if component == path else stat.S_ISDIR
        if (not expected(info.st_mode) or info.st_uid != 0 or
                info.st_mode & 0o022):
            raise PermissionError('unsafe model executable or ancestor')
    if not info.st_mode & 0o111:
        raise PermissionError('model is not executable')


class ModelSupervisor:
    def __init__(self, policy, argv, spawn=subprocess.Popen, monotonic=time.monotonic,
                 settle_seconds=5, terminate_seconds=5, kill_seconds=2,
                 worker_uid=None, worker_gid=None):
        if not isinstance(argv, (list, tuple)) or not argv or any(
                not isinstance(arg, str) or not arg or '\0' in arg for arg in argv):
            raise ValueError('model argv must be a nonempty string sequence')
        for value in (settle_seconds, terminate_seconds, kill_seconds):
            if type(value) not in (int, float) or not math.isfinite(value) or not 0 < value <= 60:
                raise ValueError('invalid supervisor timing')
        if worker_uid is None:
            worker_uid = os.geteuid()
        if worker_gid is None and os.geteuid() != 0:
            worker_gid = os.getegid()
        if (type(worker_uid) is not int or not 1 <= worker_uid < 2**32 or
                type(worker_gid) is not int or not 1 <= worker_gid < 2**32):
            raise ValueError('explicit non-root worker uid/gid required')
        if os.geteuid() != 0 and (worker_uid != os.geteuid() or worker_gid != os.getegid()):
            raise PermissionError('cannot assume worker identity')
        self.worker_uid, self.worker_gid = worker_uid, worker_gid
        if os.geteuid() == 0:
            trusted_executable(argv[0])
        self.policy = policy
        self.argv = tuple(argv)
        self.spawn = spawn
        self.monotonic = monotonic
        self.settle_seconds = settle_seconds
        self.terminate_seconds = terminate_seconds
        self.kill_seconds = kill_seconds
        self.child = None
        self._eligible_since = None
        self._closed = False
        self._lock = threading.RLock()

    def _stop(self):
        self._eligible_since = None
        if self.child is None:
            return
        child = self.child
        try:
            if child.poll() is None:
                try:
                    child.terminate()
                except ProcessLookupError:
                    pass
                try:
                    child.wait(timeout=self.terminate_seconds)
                except subprocess.TimeoutExpired:
                    try:
                        child.kill()
                    except ProcessLookupError:
                        pass
                    child.wait(timeout=self.kill_seconds)
            else:
                child.wait(timeout=self.kill_seconds)
        finally:
            # Keep ownership if the OS cannot reap it; never start a replacement.
            if child.poll() is not None:
                self.child = None
                self.policy.set_model_running(False)
            self.policy.stopped()

    def step(self):
        with self._lock:
            if self._closed:
                return
            try:
                if self.child is not None and self.child.poll() is not None:
                    self._stop()
                    return
                if not self.policy.allowed():
                    self._eligible_since = None
                    self._stop()
                    return
                if self.child is not None:
                    return
                now = self.monotonic()
                if self._eligible_since is None or now < self._eligible_since:
                    self._eligible_since = now
                    return
                if now - self._eligible_since < self.settle_seconds:
                    return
                # Serialize pause/resume with the final recheck + spawn. Sensors
                # are freshly sampled here, not the earlier eligibility snapshot.
                with self.policy.lock:
                    if not self.policy.allowed():
                        self._eligible_since = None
                        return
                    identity = {}
                    if os.geteuid() == 0:
                        trusted_executable(self.argv[0])
                        identity = dict(user=self.worker_uid, group=self.worker_gid, extra_groups=[])
                    self.child = self.spawn(list(self.argv), shell=False,
                                            stdin=subprocess.DEVNULL, close_fds=True, **identity)
                    self.policy.set_model_running(True)
                    # Catch a safety change during process creation immediately.
                    if not self.policy.allowed():
                        self._stop()
            except Exception:
                self._eligible_since = None
                self._stop()
                raise

    def close(self):
        with self._lock:
            self._closed = True
            self._stop()

    def run(self, stop, interval=1):
        try:
            while not stop.is_set():
                self.step()
                stop.wait(interval)
        finally:
            stop.set()  # A supervisor failure also shuts down its service owner.
            self.close()
