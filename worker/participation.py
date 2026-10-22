"""Shared local participation policy. No network, model spawning or app closing.

The service owns ParticipationPolicy and all persistence. Agents use the
read-only ParticipationGate before admission and while streaming; False must
cancel work. The supervisor shares the service's policy instance.
"""

from __future__ import annotations

import json
import math
import os
import re
from pathlib import Path
import secrets
import stat
import subprocess
import tempfile
import threading
import time
from dataclasses import dataclass
from datetime import datetime
from zoneinfo import ZoneInfo

from .macos_sensors import _thermal_from_processinfo


def participation_sensors():
    """Bounded critical sampling only; power and raw idle are sampled last."""
    started = time.monotonic()
    sensor = {'metrics': {}}
    try:
        sensor['thermal_state'] = _thermal_from_processinfo()
    except Exception:
        sensor['thermal_state'] = 'critical'
    sensor['metrics'].pop('vm_pressure_level', None)
    try:
        value = subprocess.check_output(
            ['/usr/sbin/sysctl', '-n', 'kern.memorystatus_vm_pressure_level'],
            text=True, timeout=2).strip()
        sensor['metrics']['vm_pressure_level'] = int(value)
    except Exception:
        pass  # Missing pressure is denied by the policy.
    sensor['metrics'].pop('memory_available_mib', None)
    try:
        available_pct = int(subprocess.check_output(
            ['/usr/sbin/sysctl', '-n', 'kern.memorystatus_level'], text=True, timeout=2).strip())
        total = int(subprocess.check_output(
            ['/usr/sbin/sysctl', '-n', 'hw.memsize'], text=True, timeout=2).strip())
        if 0 <= available_pct <= 100 and total > 0:
            sensor['metrics']['memory_available_mib'] = total / 2**20 * available_pct / 100
    except Exception:
        pass
    try:
        power = subprocess.check_output(
            ['/usr/bin/pmset', '-g', 'batt'], text=True, timeout=1)
        sensor['on_ac'] = "'AC Power'" in power
        raw = subprocess.check_output(
            ['/usr/sbin/ioreg', '-c', 'IOHIDSystem'], text=True, timeout=1)
        match = re.search(r'"HIDIdleTime"\s*=\s*(\d+)', raw)
        idle = int(match.group(1)) / 1e9 if match else 0
        sensor.update(idle_seconds=idle, raw_idle_latest=idle, user_active=idle < 120)
    except Exception:
        sensor.update(on_ac=False, user_active=True)
    sensor['sampled_at'] = time.monotonic()
    if sensor['sampled_at'] - started > 8:
        return {}
    return sensor


@dataclass(frozen=True)
class Config:
    enabled: bool = False
    automatic_participation: bool = False
    start_hour: int = 17
    start_minute: int = 30
    end_hour: int = 8
    heartbeat_seconds: float = 15
    warning_seconds: float = 60
    cooldown_seconds: float = 60
    reclaim_allowlist: tuple = ()
    idle_seconds: float = 600
    memory_budget_mib: float | None = None
    memory_reserve_mib: float = 0
    # Pending/manual warning lifetime; healthy automatic runs do not reconfirm.
    confirmation_validity_seconds: float = 1800

    def __post_init__(self):
        if type(self.enabled) is not bool or type(self.automatic_participation) is not bool:
            raise ValueError('enrollment flags must be boolean')
        for value, upper in ((self.start_hour, 23), (self.end_hour, 23),
                             (self.start_minute, 59)):
            if type(value) is not int or not 0 <= value <= upper:
                raise ValueError('invalid window')
        if self.start_hour * 60 + self.start_minute <= self.end_hour * 60:
            raise ValueError('window must cross midnight')
        for value in (self.heartbeat_seconds, self.warning_seconds, self.cooldown_seconds,
                      self.confirmation_validity_seconds):
            if type(value) not in (float, int) or not math.isfinite(value) or not 1 <= value <= 3600:
                raise ValueError('invalid timing bound')
        for value in (self.idle_seconds, self.memory_reserve_mib):
            if type(value) not in (int, float) or not math.isfinite(value) or value < 0:
                raise ValueError('invalid idle or reserve bound')
        if self.memory_budget_mib is not None and (
                type(self.memory_budget_mib) not in (int, float) or
                not math.isfinite(self.memory_budget_mib) or self.memory_budget_mib <= 0):
            raise ValueError('invalid memory budget')
        if self.reclaim_allowlist:
            raise ValueError('application reclaim is not supported')


def boot_identity():
    """Stable across process restarts, different across boots; failure is closed."""
    import sys
    if sys.platform != 'darwin':
        raise RuntimeError('macOS required')
    value = subprocess.check_output(
        ['/usr/sbin/sysctl', '-n', 'kern.boottime'], timeout=2, text=True).strip()
    if not value or len(value) > 256:
        raise RuntimeError('invalid boot identity')
    return value


def trusted_directory_chain(path):
    """Reject symlinks/untrusted ancestors; sticky root temp parents are safe."""
    path = Path(path).absolute()
    for component in (*reversed(path.parents), path):
        info = component.lstat()
        sticky_root = info.st_uid == 0 and info.st_mode & stat.S_ISVTX
        if (not stat.S_ISDIR(info.st_mode) or info.st_uid not in (0, os.geteuid()) or
                (info.st_mode & 0o022 and not sticky_root)):
            raise PermissionError('unsafe directory ancestor')


def private_directory(path):
    """Existing parent must be private and owned; never follow a directory symlink."""
    path = Path(path)
    path.mkdir(mode=0o700, parents=True, exist_ok=True)
    trusted_directory_chain(path)
    info = path.lstat()
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.geteuid() or stat.S_IMODE(info.st_mode) != 0o700:
        raise PermissionError('state directory must be owned and mode 0700')
    return path


class ParticipationPolicy:
    def __init__(self, state_path, config=None, sensors=participation_sensors,
                 wall=time.time, monotonic=time.monotonic, boot=boot_identity):
        self.config = config or Config()
        self.state_path = Path(state_path)
        self.sensors = sensors
        self.wall = wall
        self.monotonic = monotonic
        self.lock = threading.RLock()
        self._boot = ''
        self._pause = None
        self._error = None
        self._heartbeat = None
        self._capable = False
        self._reset_warning()
        self._cooldown = 0
        self._model_running = False
        self._last_wall = None
        self._last_mono = None
        try:
            self._boot = boot()
            if not isinstance(self._boot, str) or not 1 <= len(self._boot) <= 256:
                raise ValueError('invalid boot identity')
            private_directory(self.state_path.parent)
            self._load()
        except Exception:
            self._error = 'state-error'

    def _now(self):
        wall, mono = self.wall(), self.monotonic()
        if any(type(v) not in (int, float) or not math.isfinite(v) or v < 0 for v in (wall, mono)):
            raise ValueError('invalid clock')
        if self._last_wall is not None:
            # Wall advances must keep pace with monotonic time (small NTP jitter allowed).
            if mono < self._last_mono or wall < self._last_wall - 2 or wall - self._last_wall < mono - self._last_mono - 2:
                self._error = 'clock-rollback'
                self._save(max(wall, self._last_wall))
        self._last_wall, self._last_mono = wall, mono
        return wall, mono

    def _load(self):
        wall, mono = self._now()
        try:
            fd = os.open(self.state_path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
        except FileNotFoundError:
            self._save(wall)
            return
        with os.fdopen(fd, 'r') as stream:
            info = os.fstat(stream.fileno())
            if not stat.S_ISREG(info.st_mode) or info.st_uid != os.geteuid() or stat.S_IMODE(info.st_mode) != 0o600 or info.st_size > 4096:
                raise ValueError('unsafe state file')
            data = json.loads(stream.read(4097))
        if not isinstance(data, dict) or set(data) != {'version', 'wall', 'pause', 'fault'} or data['version'] != 1:
            raise ValueError('invalid state')
        saved_wall = data['wall']
        if type(saved_wall) not in (int, float) or not math.isfinite(saved_wall) or saved_wall < 0:
            raise ValueError('invalid saved clock')
        if data['fault'] not in (None, 'clock-rollback'):
            raise ValueError('invalid clock fault')
        self._error = data['fault']
        if wall < saved_wall - 2:
            self._error = 'clock-rollback'
        pause = data['pause']
        if pause is not None:
            if not isinstance(pause, dict) or set(pause) != {'seconds', 'boot', 'until'}:
                raise ValueError('invalid pause')
            if type(pause['seconds']) is not int or pause['seconds'] not in (3600, 7200):
                raise ValueError('invalid duration')
            if not isinstance(pause['boot'], str) or not 1 <= len(pause['boot']) <= 256:
                raise ValueError('invalid saved boot')
            until = pause['until']
            if type(until) not in (int, float) or not math.isfinite(until) or until < 0:
                raise ValueError('invalid deadline')
            if pause['boot'] != self._boot:
                # Never infer elapsed pause from wall time after reboot. Restart the
                # full requested pause, then persist the new monotonic deadline.
                pause = dict(seconds=pause['seconds'], boot=self._boot, until=mono + pause['seconds'])
            elif until > mono + pause['seconds'] + 2:
                self._error = 'clock-rollback'
            self._pause = pause
        self._save(wall)

    def _save(self, wall):
        payload = dict(version=1, wall=wall, pause=self._pause,
                       fault='clock-rollback' if self._error == 'clock-rollback' else None)
        temporary = None
        try:
            private_directory(self.state_path.parent)
            fd, temporary = tempfile.mkstemp(prefix='.participation-', dir=self.state_path.parent)
            with os.fdopen(fd, 'w') as stream:
                os.fchmod(stream.fileno(), 0o600)
                json.dump(payload, stream, allow_nan=False)
                stream.flush()
                os.fsync(stream.fileno())
            os.replace(temporary, self.state_path)
            directory = os.open(self.state_path.parent, os.O_RDONLY)
            try:
                os.fsync(directory)
            finally:
                os.close(directory)
        except Exception:
            self._error = 'state-error'
            raise
        finally:
            if temporary and os.path.exists(temporary):
                os.unlink(temporary)

    def _reset_warning(self):
        self._warning = None
        self._warning_created = None
        self._activity_since = None
        self._ack = None
        self._submitted_at = None
        self._automatic_ready = False

    def _reason(self, wall, mono):
        if self._error:
            return self._error
        if not self.config.enabled:
            return 'opted-out'
        if self._pause:
            if mono < self._pause['until']:
                return 'paused'
            self._pause = None
            self._save(wall)
        if mono < self._cooldown:
            return 'cooldown'
        if self._heartbeat is None or not self._capable or not 0 <= mono - self._heartbeat <= self.config.heartbeat_seconds:
            return 'ui-unavailable'
        local = datetime.fromtimestamp(wall, ZoneInfo('Europe/Copenhagen'))
        minute = local.hour * 60 + local.minute
        if not (minute >= self.config.start_hour * 60 + self.config.start_minute or minute < self.config.end_hour * 60):
            return 'outside-window'
        sensor = self.sensors()
        # Sensor commands can take time. Recheck freshness/window after sampling,
        # including a clock fault discovered during that interval.
        wall, mono = self._now()
        if self._error:
            return self._error
        if not 0 <= mono - self._heartbeat <= self.config.heartbeat_seconds:
            return 'ui-unavailable'
        local = datetime.fromtimestamp(wall, ZoneInfo('Europe/Copenhagen'))
        minute = local.hour * 60 + local.minute
        if not (minute >= self.config.start_hour * 60 + self.config.start_minute or minute < self.config.end_hour * 60):
            return 'outside-window'
        if sensor.get('on_ac') is not True:
            return 'power-or-active'
        if sensor.get('thermal_state') not in ('nominal', 'fair'):
            return 'thermal'
        # Missing, malformed or unknown pressure is unsafe. Existing macOS sensor
        # exposes the kernel's normal/warn/critical enum under metrics.
        pressure = sensor.get('metrics', {}).get('vm_pressure_level')
        if type(pressure) is not int or pressure != 1:
            return 'memory-pressure'
        if self.config.memory_budget_mib is not None or self.config.memory_reserve_mib:
            available = sensor.get('metrics', {}).get('memory_available_mib')
            if (type(available) not in (int, float) or not math.isfinite(available) or
                    available < (0 if self._model_running else (self.config.memory_budget_mib or 0)) + self.config.memory_reserve_mib):
                return 'memory-budget'
        # Check every unsafe condition before treating activity as transient.
        if type(sensor.get('user_active')) is not bool:
            return 'power-or-active'
        idle = sensor.get('idle_seconds')
        if type(idle) not in (int, float) or not math.isfinite(idle) or idle < 0:
            return 'not-idle'
        if sensor['user_active']:
            return 'user-active'
        if idle < self.config.idle_seconds:
            return 'idle-wait'
        return None

    def status(self):
        with self.lock:
            try:
                wall, mono = self._now()
                reason = self._reason(wall, mono)
                mono = self._last_mono  # Includes time spent sampling sensors.
                if (self._warning is not None and
                        not (self._model_running and self._automatic_ready) and
                        mono - self._warning_created >= self.config.confirmation_validity_seconds):
                    self._reset_warning()
                if reason in ('user-active', 'idle-wait'):
                    # Automatic starts require uninterrupted idle; manual clicks
                    # retain the existing bounded acknowledgment behavior.
                    if self.config.automatic_participation and self._ack is None:
                        self._reset_warning()
                    # A menu click resets HID idle. Keep pending consent briefly
                    # across status/heartbeat races (one warning grace period),
                    # but never grant admission. Acknowledged consent lasts only
                    # until the absolute warning deadline above.
                    if self._warning is not None and self._ack is None:
                        if self._activity_since is None:
                            self._activity_since = mono
                        if mono - self._activity_since >= self.config.warning_seconds:
                            self._reset_warning()
                    reason = 'power-or-active' if reason == 'user-active' else 'not-idle'
                elif reason:
                    self._reset_warning()
                else:
                    if self._warning is None:
                        self._warning = secrets.token_hex(16)
                        self._warning_created = mono
                    self._automatic_ready = (
                        self.config.automatic_participation and
                        self._submitted_at is not None and
                        mono - self._submitted_at >= self.config.warning_seconds)
                    manual_ready = self._ack is not None and mono - self._ack >= self.config.warning_seconds
                    if not (self._automatic_ready or manual_ready):
                        reason = 'warning-grace'
                return dict(allowed=reason is None, reason=reason or 'eligible',
                            warning_id=self._warning, model_running=self._model_running,
                            warning_acknowledged=self._ack is not None,
                            warning_submitted_at=self._submitted_at,
                            warning_seconds=self.config.warning_seconds,
                            pause_remaining=max(0, math.ceil(self._pause['until'] - mono)) if self._pause else 0)
            except Exception:
                self._error = 'state-or-sensor-error'
                self._reset_warning()
                return dict(allowed=False, reason=self._error, warning_id=None,
                            model_running=self._model_running, warning_acknowledged=False,
                            warning_submitted_at=None,
                            warning_seconds=self.config.warning_seconds, pause_remaining=0)

    def allowed(self):
        return self.status()['allowed']

    def ui_heartbeat(self, notification_capable, warning_id=None):
        if type(notification_capable) is not bool or (warning_id is not None and
                (not isinstance(warning_id, str) or len(warning_id) != 32)):
            raise ValueError('invalid heartbeat')
        with self.lock:
            _, mono = self._now()
            # Expiry must invalidate old warnings BEFORE refreshing the heartbeat.
            if self._heartbeat is None or not 0 <= mono - self._heartbeat <= self.config.heartbeat_seconds:
                self._reset_warning()
            self._heartbeat = mono
            self._capable = notification_capable
            if not notification_capable:
                self._reset_warning()
            # Validate safety and warning lifetime before accepting an echo.
            # Submission is not visibility or consent: only explicit enrollment
            # permits an automatic start after the submission grace period.
            status = self.status()
            if (notification_capable and warning_id is not None and
                    status['warning_id'] == warning_id and
                    status['reason'] in ('warning-grace', 'eligible') and
                    self._submitted_at is None):
                self._submitted_at = self._last_mono
                status['warning_submitted_at'] = self._submitted_at
            return status

    def acknowledge_warning(self, warning_id):
        """UI-only explicit user confirmation, never notification submission."""
        if not isinstance(warning_id, str) or len(warning_id) != 32:
            raise ValueError('invalid warning id')
        with self.lock:
            status = self.status()  # invalidate expired/unsafe warnings first
            if status['warning_id'] != warning_id:
                raise ValueError('stale warning id')
            if self._ack is None:
                self._ack = self.monotonic()
            return self.status()

    def set_model_running(self, running):
        """Supervisor-only; serialize allocation state with admission/spawn."""
        if type(running) is not bool:
            raise ValueError('invalid model state')
        with self.lock:
            self._model_running = running

    def pause(self, hours):
        if type(hours) is not int or hours not in (1, 2):
            raise ValueError('pause must be 1 or 2 hours')
        with self.lock:
            wall, mono = self._now()
            self._pause = dict(seconds=hours * 3600, boot=self._boot, until=mono + hours * 3600)
            self._reset_warning()
            self._save(wall)
            return self.status()

    def resume(self):
        """Explicit user recovery also clears a clock fault, never a storage fault."""
        with self.lock:
            if self._error not in (None, 'clock-rollback'):
                return self.status()
            self._last_wall = self._last_mono = None
            wall, mono = self._now()
            self._error = None
            self._pause = None
            self._cooldown = mono + self.config.cooldown_seconds
            self._reset_warning()
            self._save(wall)
            return self.status()

    def stopped(self):
        """Supervisor records any unload/crash; no restart until a new warning."""
        with self.lock:
            _, mono = self._now()
            self._cooldown = mono + self.config.cooldown_seconds
            self._reset_warning()


class ParticipationGate:
    """Read-only live status callable; never loads or writes policy state.

    server_uid is the installed service owner (0 for a root service). Each call
    authenticates a new connection and requests freshly evaluated safety. Agents
    must check at admission and during work, cancelling on False. No cached allow
    decision survives an outage or restart.
    """

    def __init__(self, socket_path, server_uid, timeout=1):
        if type(server_uid) is not int or not 0 <= server_uid < 2**32:
            raise ValueError('invalid service uid')
        if type(timeout) not in (int, float) or not math.isfinite(timeout) or not 0 < timeout <= 5:
            raise ValueError('invalid gate timeout')
        self.socket_path = str(socket_path)
        self.server_uid = server_uid
        self.timeout = timeout

    def __call__(self):
        import socket
        from .participation_service import peer_uid

        try:
            deadline = time.monotonic() + self.timeout
            with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as connection:
                connection.settimeout(self.timeout)
                connection.connect(self.socket_path)
                if peer_uid(connection) != self.server_uid:
                    return False
                connection.sendall(b'{"op":"status"}\n')
                raw = bytearray()
                while b'\n' not in raw:
                    remaining = deadline - time.monotonic()
                    if remaining <= 0:
                        return False
                    connection.settimeout(remaining)
                    chunk = connection.recv(4097 - len(raw))
                    if not chunk:
                        return False
                    raw.extend(chunk)
                    if len(raw) > 4096:
                        return False
                if time.monotonic() > deadline or raw.count(b'\n') != 1 or not raw.endswith(b'\n'):
                    return False
                response = json.loads(raw)
                return (response.get('ok') is True and
                        response['status']['allowed'] is True and
                        response['status']['reason'] == 'eligible')
        except Exception:
            return False
