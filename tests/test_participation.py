import json
import os
from pathlib import Path
import socket
import stat
import tempfile
import unittest
from datetime import datetime, timezone
from unittest.mock import Mock, patch

from worker.participation import Config, ParticipationGate, ParticipationPolicy, participation_sensors
from worker.participation_service import ParticipationService, peer_uid


class Clock:
    def __init__(self):
        self.wall = datetime(2026, 1, 10, 20, tzinfo=timezone.utc).timestamp()
        self.mono = 10000.0

    def advance(self, seconds):
        self.wall += seconds
        self.mono += seconds


class PolicyTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.temp.name = str(Path(self.temp.name).resolve())
        self.path = Path(self.temp.name) / 'state' / 'participation.json'
        self.clock = Clock()
        self.sensor = dict(on_ac=True, user_active=False, idle_seconds=600, thermal_state='nominal',
                           metrics={'vm_pressure_level': 1})
        self.boot = 'boot-one'
        self.policy = self.make()

    def make(self, config=None):
        return ParticipationPolicy(self.path, config or Config(enabled=True, warning_seconds=3,
                                   cooldown_seconds=2), sensors=lambda: self.sensor,
                                   wall=lambda: self.clock.wall, monotonic=lambda: self.clock.mono,
                                   boot=lambda: self.boot)

    def ready(self, policy=None):
        policy = policy or self.policy
        status = policy.ui_heartbeat(True)
        self.assertEqual(status['reason'], 'warning-grace')
        self.assertIsNotNone(status['warning_id'])
        policy.acknowledge_warning(status['warning_id'])
        self.clock.advance(3)
        self.assertTrue(policy.allowed())

    def automatic_policy(self, **kwargs):
        return self.make(Config(enabled=True, automatic_participation=True,
                                warning_seconds=3, **kwargs))

    def test_automatic_requires_both_enrollments_and_submission(self):
        self.assertFalse(Config().automatic_participation)
        with self.assertRaises(ValueError):
            Config(automatic_participation=1)
        for enabled, automatic in ((False, False), (False, True), (True, False), (True, True)):
            with self.subTest(enabled=enabled, automatic=automatic):
                policy = self.make(Config(enabled=enabled, automatic_participation=automatic,
                                          warning_seconds=3))
                warning = policy.ui_heartbeat(True)['warning_id']
                self.clock.advance(3)
                self.assertFalse(policy.allowed())  # Silence is never submission.
                policy.ui_heartbeat(True, 'x' * 32)
                self.assertIsNone(policy.status()['warning_submitted_at'])
                status = policy.ui_heartbeat(True, warning)
                submitted = status['warning_submitted_at']
                self.assertFalse(status['allowed'])
                self.assertFalse(status['warning_acknowledged'])
                self.clock.advance(2)
                self.assertFalse(policy.ui_heartbeat(True, warning)['allowed'])
                self.assertEqual(policy.status()['warning_submitted_at'], submitted)
                self.clock.advance(1)
                self.assertEqual(policy.allowed(), enabled and automatic)

    def test_automatic_missing_denied_stale_ui_and_failed_submission(self):
        policy = self.automatic_policy()
        self.assertEqual(policy.status()['reason'], 'ui-unavailable')
        self.assertIsNone(policy.ui_heartbeat(False)['warning_id'])
        old = policy.ui_heartbeat(True)['warning_id']
        policy.ui_heartbeat(True, old)
        self.clock.advance(3)
        self.assertTrue(policy.allowed())
        status = policy.ui_heartbeat(False, old)  # Failed submission/denied alerts.
        self.assertFalse(status['allowed'])
        self.assertIsNone(status['warning_submitted_at'])
        status = policy.ui_heartbeat(True, old)
        self.assertNotEqual(status['warning_id'], old)
        self.assertIsNone(status['warning_submitted_at'])
        old = status['warning_id']
        policy.ui_heartbeat(True, old)
        self.clock.advance(16)
        self.assertFalse(policy.allowed())
        status = policy.ui_heartbeat(True, old)
        self.assertNotEqual(status['warning_id'], old)
        self.assertIsNone(status['warning_submitted_at'])

    def test_automatic_activity_and_unsafe_conditions_reset_submission(self):
        for changes in (dict(user_active=True, idle_seconds=0), dict(idle_seconds=599),
                        dict(on_ac=False), dict(thermal_state='critical'),
                        dict(metrics={'vm_pressure_level': 2})):
            with self.subTest(changes=changes):
                original = self.sensor.copy()
                policy = self.automatic_policy()
                old = policy.ui_heartbeat(True)['warning_id']
                policy.ui_heartbeat(True, old)
                self.clock.advance(3)
                self.assertTrue(policy.allowed())
                self.sensor.update(changes)
                status = policy.status()
                self.assertFalse(status['allowed'])
                self.assertIsNone(status['warning_submitted_at'])
                self.sensor = original
                status = policy.ui_heartbeat(True, old)
                self.assertNotEqual(status['warning_id'], old)
                self.assertFalse(status['allowed'])

    def test_automatic_pause_expiry_requires_new_submitted_warning(self):
        for hours in (1, 2):
            with self.subTest(hours=hours):
                policy = self.automatic_policy()
                old = policy.ui_heartbeat(True)['warning_id']
                policy.ui_heartbeat(True, old)
                policy.pause(hours)
                self.clock.advance(hours * 3600 - 1)
                self.assertEqual(policy.ui_heartbeat(True, old)['reason'], 'paused')
                self.clock.advance(1)
                status = policy.ui_heartbeat(True, old)
                self.assertNotEqual(status['warning_id'], old)
                self.assertIsNone(status['warning_submitted_at'])
                self.assertFalse(status['allowed'])
                policy.ui_heartbeat(True, status['warning_id'])
                self.clock.advance(3)
                self.assertTrue(policy.allowed())

    def test_automatic_pending_expiry_and_clock_rollback(self):
        policy = self.automatic_policy(confirmation_validity_seconds=10)
        old = policy.ui_heartbeat(True)['warning_id']
        policy.ui_heartbeat(True, old)
        self.clock.advance(10)
        status = policy.ui_heartbeat(True, old)
        self.assertNotEqual(status['warning_id'], old)
        self.assertIsNone(status['warning_submitted_at'])
        policy.ui_heartbeat(True, status['warning_id'])
        self.clock.advance(3)
        self.assertTrue(policy.allowed())
        self.clock.wall -= 10
        status = policy.status()
        self.assertEqual(status['reason'], 'clock-rollback')
        self.assertIsNone(status['warning_submitted_at'])

    def test_supervisor_allocation_does_not_oscillate(self):
        from worker.model_supervisor import ModelSupervisor
        policy = self.make(Config(enabled=True, warning_seconds=3,
                                  memory_budget_mib=8192, memory_reserve_mib=1024))
        self.sensor['metrics']['memory_available_mib'] = 10240
        self.ready(policy)
        child = Mock()
        child.poll.return_value = None
        def allocate(*args, **kwargs):
            self.sensor['metrics']['memory_available_mib'] = 2048
            return child
        spawn = Mock(side_effect=allocate)
        supervisor = ModelSupervisor(policy, ['/unused/model'], spawn=spawn,
                                     monotonic=lambda: self.clock.mono, settle_seconds=1)
        supervisor.step()
        self.clock.advance(1)
        supervisor.step()
        for _ in range(5):
            supervisor.step()
        self.assertTrue(policy.status()['model_running'])
        spawn.assert_called_once()
        child.terminate.assert_not_called()
        child.wait.side_effect = lambda **kwargs: setattr(child.poll, 'return_value', 0)
        self.sensor['metrics']['memory_available_mib'] = 1000
        supervisor.step()
        child.terminate.assert_called_once()
        self.assertFalse(policy.status()['model_running'])

    def test_allocation_reserve_and_reloading_budget(self):
        policy = self.make(Config(enabled=True, warning_seconds=3,
                                  memory_budget_mib=8192, memory_reserve_mib=1024))
        self.sensor['metrics']['memory_available_mib'] = 10240
        self.ready(policy)
        policy.set_model_running(True)
        self.sensor['metrics']['memory_available_mib'] = 2048
        for _ in range(5):
            self.assertTrue(policy.allowed())
        self.sensor['metrics']['vm_pressure_level'] = 2
        self.assertEqual(policy.status()['reason'], 'memory-pressure')
        self.sensor['metrics']['vm_pressure_level'] = 1
        self.sensor['metrics']['memory_available_mib'] = 1023
        self.assertEqual(policy.status()['reason'], 'memory-budget')
        policy.set_model_running(False)
        self.sensor['metrics']['memory_available_mib'] = 2048
        self.assertEqual(policy.status()['reason'], 'memory-budget')

    def test_stale_and_duplicate_explicit_confirmation(self):
        warning = self.policy.ui_heartbeat(True)['warning_id']
        with self.assertRaises(ValueError):
            self.policy.acknowledge_warning('a' * 32)
        self.policy.acknowledge_warning(warning)
        ack = self.policy._ack
        self.clock.advance(1)
        self.policy.acknowledge_warning(warning)
        self.assertEqual(self.policy._ack, ack)
        self.policy.pause(1)
        with self.assertRaises(ValueError):
            self.policy.acknowledge_warning(warning)

    @patch('worker.participation._thermal_from_processinfo', return_value='nominal')
    @patch('worker.participation.subprocess.check_output')
    def test_active_click_status_and_heartbeat_then_idle(self, command, thermal):
        def sample(command, **kwargs):
            if command[0] == '/usr/bin/pmset':
                return "'AC Power'"
            if command[0] == '/usr/sbin/ioreg':
                return f'"HIDIdleTime"={int(self.sensor["idle_seconds"] * 1e9)}'
            return {'kern.memorystatus_vm_pressure_level': '1',
                    'kern.memorystatus_level': '25',
                    'hw.memsize': str(8 * 2**30)}[command[-1]]
        command.side_effect = sample
        self.policy.sensors = participation_sensors
        warning = self.policy.ui_heartbeat(True)['warning_id']
        self.sensor.update(user_active=True, idle_seconds=0)
        for status in (self.policy.status(), self.policy.ui_heartbeat(True)):
            self.assertEqual(status['warning_id'], warning)
            self.assertFalse(status['allowed'])
        status = self.policy.acknowledge_warning(warning)
        self.assertTrue(status['warning_acknowledged'])
        self.assertFalse(status['allowed'])
        # Keep the GUI alive throughout the real ten-minute idle threshold.
        for elapsed in range(10, 601, 10):
            self.clock.advance(10)
            self.sensor.update(user_active=elapsed < 120, idle_seconds=elapsed)
            status = self.policy.ui_heartbeat(True)
            self.assertEqual(status['warning_id'], warning)
            self.assertEqual(status['allowed'], elapsed == 600)
        self.sensor.update(user_active=True, idle_seconds=0)
        self.assertFalse(self.policy.allowed())

    def test_pending_click_retention_is_brief_and_not_extended_by_heartbeat(self):
        warning = self.policy.ui_heartbeat(True)['warning_id']
        self.sensor.update(user_active=True, idle_seconds=0)
        self.policy.status()
        self.clock.advance(2)
        self.assertEqual(self.policy.ui_heartbeat(True)['warning_id'], warning)
        self.clock.advance(1)
        self.assertIsNone(self.policy.ui_heartbeat(True)['warning_id'])
        with self.assertRaises(ValueError):
            self.policy.acknowledge_warning(warning)

    def test_warning_validity_bounds_pending_and_acknowledged_consent(self):
        for acknowledged in (False, True):
            with self.subTest(acknowledged=acknowledged):
                policy = self.make(Config(enabled=True, warning_seconds=3,
                                          confirmation_validity_seconds=10))
                warning = policy.ui_heartbeat(True)['warning_id']
                if acknowledged:
                    policy.acknowledge_warning(warning)
                self.clock.advance(9)
                self.assertEqual(policy.ui_heartbeat(True)['warning_id'], warning)
                if acknowledged:
                    policy.acknowledge_warning(warning)  # Cannot extend validity.
                self.clock.advance(1)
                status = policy.ui_heartbeat(True)
                self.assertNotEqual(status['warning_id'], warning)
                self.assertFalse(status['allowed'])
                self.assertFalse(status['warning_acknowledged'])
                with self.assertRaises(ValueError):
                    policy.acknowledge_warning(warning)

    def test_sensor_sampling_cannot_extend_confirmation_deadline(self):
        policy = self.make(Config(enabled=True, warning_seconds=3,
                                  confirmation_validity_seconds=10))
        warning = policy.ui_heartbeat(True)['warning_id']
        policy.acknowledge_warning(warning)
        self.clock.advance(9)
        def slow_sensor():
            self.clock.advance(1)
            return self.sensor
        policy.sensors = slow_sensor
        status = policy.status()
        self.assertFalse(status['allowed'])
        self.assertNotEqual(status['warning_id'], warning)
        self.assertFalse(status['warning_acknowledged'])

    def test_unsafe_conditions_revoke_even_during_click(self):
        for acknowledged in (False, True):
            for fault in ('power', 'thermal', 'pressure', 'budget', 'pause',
                          'ui', 'heartbeat', 'window', 'clock', 'stop', 'timeout'):
                with self.subTest(acknowledged=acknowledged, fault=fault):
                    self.clock = Clock()
                    self.sensor = dict(on_ac=True, user_active=False, idle_seconds=600,
                                       thermal_state='nominal', metrics={
                                           'vm_pressure_level': 1, 'memory_available_mib': 2048})
                    policy = self.make(Config(enabled=True, warning_seconds=3,
                                              memory_budget_mib=1024,
                                              confirmation_validity_seconds=10))
                    warning = policy.ui_heartbeat(True)['warning_id']
                    self.sensor.update(user_active=True, idle_seconds=0)
                    if acknowledged:
                        policy.acknowledge_warning(warning)
                    if fault == 'power':
                        self.sensor['on_ac'] = False
                    elif fault == 'thermal':
                        self.sensor['thermal_state'] = 'critical'
                    elif fault == 'pressure':
                        self.sensor['metrics']['vm_pressure_level'] = 2
                    elif fault == 'budget':
                        self.sensor['metrics']['memory_available_mib'] = 512
                    elif fault == 'pause':
                        policy.pause(1)
                    elif fault == 'ui':
                        policy.ui_heartbeat(False)
                    elif fault == 'heartbeat':
                        self.clock.advance(16)
                    elif fault == 'window':
                        self.clock.wall += 12 * 3600
                    elif fault == 'clock':
                        self.clock.wall -= 10
                    elif fault == 'stop':
                        policy.stopped()
                    elif fault == 'timeout':
                        self.clock.advance(10)
                    status = policy.status()
                    self.assertFalse(status['allowed'])
                    self.assertIsNone(status['warning_id'])
                    self.assertFalse(status['warning_acknowledged'])
                    with self.assertRaises(ValueError):
                        policy.acknowledge_warning(warning)
                    self.path.unlink()  # Isolate persistent clock faults.

    def test_default_opt_out_and_config_bounds(self):
        policy = self.make(Config())
        self.assertFalse(policy.allowed())
        self.assertEqual(policy.status()['reason'], 'opted-out')
        for kwargs in ({'enabled': 1}, {'warning_seconds': float('nan')},
                       {'heartbeat_seconds': 0}, {'start_hour': 3},
                       {'confirmation_validity_seconds': 0},
                       {'confirmation_validity_seconds': True},
                       {'confirmation_validity_seconds': float('inf')},
                       {'confirmation_validity_seconds': 3601},
                       {'reclaim_allowlist': ('anything',)}):
            with self.subTest(kwargs=kwargs), self.assertRaises(ValueError):
                Config(**kwargs)

    def test_warning_requires_ack_and_fresh_gui(self):
        status = self.policy.ui_heartbeat(True)
        self.clock.advance(4)
        self.assertFalse(self.policy.allowed())
        self.policy.ui_heartbeat(True, 'a' * 32)
        self.assertFalse(self.policy.allowed())
        self.policy.ui_heartbeat(True, status['warning_id'])
        self.clock.advance(3)
        self.assertFalse(self.policy.allowed())
        self.policy.acknowledge_warning(status['warning_id'])
        self.clock.advance(3)
        self.assertTrue(self.policy.allowed())
        self.clock.advance(16)
        self.assertFalse(self.policy.allowed())
        refreshed = self.policy.ui_heartbeat(True, status['warning_id'])
        self.assertNotEqual(refreshed['warning_id'], status['warning_id'])
        self.assertFalse(refreshed['allowed'])
        self.assertFalse(self.policy.ui_heartbeat(False)['allowed'])

    def test_heartbeat_refresh_cannot_resurrect_expired_warning(self):
        self.ready()
        old = self.policy.status()['warning_id']
        self.clock.advance(16)
        status = self.policy.ui_heartbeat(True, old)
        self.assertFalse(status['allowed'])
        self.assertNotEqual(status['warning_id'], old)

    def test_pause_expiry_and_atomic_permissions(self):
        self.ready()
        self.assertEqual(self.policy.pause(1)['pause_remaining'], 3600)
        self.assertEqual(stat.S_IMODE(self.path.stat().st_mode), 0o600)
        self.assertEqual(stat.S_IMODE(self.path.parent.stat().st_mode), 0o700)
        self.clock.advance(3599)
        self.assertEqual(self.policy.status()['reason'], 'paused')
        self.clock.advance(1)
        self.assertEqual(self.policy.status()['reason'], 'ui-unavailable')
        self.ready()
        self.assertIsNone(json.loads(self.path.read_text())['pause'])
        self.assertEqual(list(self.path.parent.glob('.participation-*')), [])

    def test_restart_and_reboot_pause(self):
        self.policy.pause(2)
        self.clock.advance(60)
        restarted = self.make()
        self.assertEqual(restarted.status()['pause_remaining'], 7140)
        self.boot = 'boot-two'
        self.clock.mono = 10
        self.clock.wall += 50000
        rebooted = self.make()
        self.assertEqual(rebooted.status()['pause_remaining'], 7200)
        self.clock.advance(60)
        self.assertEqual(self.make().status()['pause_remaining'], 7140)

    def test_wall_jump_cannot_expire_pause(self):
        self.policy.pause(1)
        self.clock.wall += 100000
        self.assertEqual(self.policy.status()['pause_remaining'], 3600)

    def test_clock_rollback_persists_until_explicit_resume(self):
        self.ready()
        self.clock.wall -= 10
        self.assertEqual(self.policy.status()['reason'], 'clock-rollback')
        restarted = self.make()
        self.assertEqual(restarted.status()['reason'], 'clock-rollback')
        self.assertEqual(restarted.resume()['reason'], 'cooldown')
        self.clock.advance(2)
        self.ready(restarted)

    def test_restart_rollback_and_monotonic_rollback(self):
        self.policy.pause(1)
        self.clock.wall -= 10
        self.assertEqual(self.make().status()['reason'], 'clock-rollback')
        self.policy.resume()
        self.clock.mono -= 5
        self.assertFalse(self.policy.allowed())
        self.assertEqual(self.policy.status()['reason'], 'clock-rollback')

    def test_resume_cooldown_and_invalid_pause(self):
        for hours in (0, 3, -1, True, 1.0, '1', None):
            with self.subTest(hours=hours), self.assertRaises(ValueError):
                self.policy.pause(hours)
        self.policy.pause(2)
        self.assertEqual(self.policy.resume()['reason'], 'cooldown')
        self.clock.advance(2)
        self.ready()
        self.policy.stopped()
        self.assertEqual(self.policy.status()['reason'], 'cooldown')

    def test_night_window_winter_summer_boundaries(self):
        for month, hour, minute, expected in ((1, 16, 29, False), (1, 16, 30, True),
                (1, 6, 59, True), (1, 7, 0, False), (7, 15, 30, True), (7, 6, 0, False)):
            with self.subTest(month=month, hour=hour, minute=minute):
                self.clock.wall = datetime(2026, month, 10, hour, minute, tzinfo=timezone.utc).timestamp()
                policy = self.make()
                policy.resume()  # explicit reset for independently chosen clock values
                self.clock.advance(2)
                status = policy.ui_heartbeat(True)
                self.assertEqual(status['reason'], 'warning-grace' if expected else 'outside-window')

    def test_all_safety_gates_reset_warning(self):
        for field, bad_values in (('on_ac', (False, None, 1)),
                                  ('user_active', (None, 0)),
                                  ('thermal_state', ('serious', 'critical', None)),
                                  ('metrics', ({}, {'vm_pressure_level': 2},
                                               {'vm_pressure_level': 4}, {'vm_pressure_level': True}))):
            for bad in bad_values:
                with self.subTest(field=field, bad=bad):
                    self.ready()
                    previous = self.sensor[field]
                    self.sensor[field] = bad
                    self.assertFalse(self.policy.allowed())
                    self.sensor[field] = previous
                    self.assertEqual(self.policy.status()['reason'], 'warning-grace')

    def test_idle_and_memory_bounds_fail_closed(self):
        for idle in (None, True, -1, 599.9, float('nan'), float('inf')):
            self.sensor['idle_seconds'] = idle
            self.assertEqual(self.policy.ui_heartbeat(True)['reason'], 'not-idle')
        self.sensor['idle_seconds'] = 900
        policy = self.make(Config(enabled=True, idle_seconds=900,
                                  memory_budget_mib=1024, memory_reserve_mib=256))
        for available in (None, True, -1, 1279, float('nan'), float('inf')):
            self.sensor['metrics']['memory_available_mib'] = available
            self.assertEqual(policy.ui_heartbeat(True)['reason'], 'memory-budget')
        self.sensor['metrics']['memory_available_mib'] = 1280
        self.assertEqual(policy.status()['reason'], 'warning-grace')
        for kwargs in ({'idle_seconds': -1}, {'idle_seconds': True},
                       {'memory_budget_mib': 0}, {'memory_budget_mib': float('inf')},
                       {'memory_reserve_mib': -1}, {'memory_reserve_mib': True}):
            with self.subTest(kwargs=kwargs), self.assertRaises(ValueError):
                Config(**kwargs)

    def test_slow_sensor_read_invalidates_heartbeat(self):
        self.ready()
        def slow_sensor():
            self.clock.advance(16)
            return self.sensor
        self.policy.sensors = slow_sensor
        self.assertEqual(self.policy.status()['reason'], 'ui-unavailable')

    def test_slow_sensor_read_rechecks_window(self):
        self.clock.wall = datetime(2026, 1, 11, 6, 59, 55, tzinfo=timezone.utc).timestamp()
        self.ready()
        def slow_sensor():
            self.clock.advance(3)
            return self.sensor
        self.policy.sensors = slow_sensor
        self.assertEqual(self.policy.status()['reason'], 'outside-window')

    def test_sensor_exception_fails_closed(self):
        self.ready()
        self.policy.sensors = lambda: (_ for _ in ()).throw(OSError())
        self.assertFalse(self.policy.allowed())

    def test_invalid_state_is_not_overwritten_or_resumed(self):
        for content in ('{', '[]', '{"version": 2}', 'x' * 5000):
            with self.subTest(content=content[:30]):
                self.path.write_text(content)
                policy = self.make()
                self.assertFalse(policy.allowed())
                self.assertFalse(policy.resume()['allowed'])
                self.assertEqual(self.path.read_text(), content)

    def test_unsafe_state_mode_symlink_and_write_failure(self):
        self.path.chmod(0o644)
        self.assertFalse(self.make().allowed())
        self.path.chmod(0o600)
        other = self.path.with_name('other')
        other.write_text('secret')
        self.path.unlink()
        self.path.symlink_to(other)
        self.assertFalse(self.make().allowed())
        self.assertEqual(other.read_text(), 'secret')
        self.path.unlink()
        policy = self.make()
        with patch('worker.participation.os.replace', side_effect=OSError('disk full')):
            with self.assertRaises(OSError):
                policy.pause(1)
        self.assertFalse(policy.allowed())
        self.assertEqual(list(self.path.parent.glob('.participation-*')), [])


class ServiceTests(PolicyTests):
    # Inherited policy checks also exercise independent fresh state per service test.
    def setUp(self):
        super().setUp()
        self.uid = os.getuid() or 501
        self.service = ParticipationService(self.policy, Path(self.temp.name) / 'ipc' / 'control.sock',
                                            self.uid, worker_uid=self.uid,
                                            status_socket=Path(self.temp.name) / 'ipc' / 'status.sock',
                                            peer=lambda _: self.uid, console=lambda: self.uid)

    def request(self, raw, role='ui'):
        client, server = socket.socketpair()
        with client, server:
            client.sendall(raw)
            client.shutdown(socket.SHUT_WR)
            self.service.handle(server, role)
            return json.loads(client.recv(4096))

    def test_submission_echo_requires_authorized_ui(self):
        self.policy = self.automatic_policy()
        self.service.policy = self.policy
        warning = self.policy.ui_heartbeat(True)['warning_id']
        raw = json.dumps(dict(op='ui_heartbeat', notification_capable=True,
                              warning_id=warning)).encode() + b'\n'
        self.assertFalse(self.request(raw, 'worker')['ok'])
        self.service.peer = lambda _: self.uid + 1
        self.assertFalse(self.request(raw)['ok'])
        self.service.peer = lambda _: self.uid
        self.service.console = lambda: self.uid + 1
        self.assertFalse(self.request(raw)['ok'])
        self.assertIsNone(self.policy.status()['warning_submitted_at'])
        self.service.console = lambda: self.uid
        status = self.request(raw)['status']
        self.assertEqual(status['warning_submitted_at'], self.clock.mono)
        self.assertFalse(status['warning_acknowledged'])
        self.clock.advance(3)
        self.assertTrue(self.policy.allowed())

    def test_explicit_ack_is_ui_only(self):
        warning = self.policy.ui_heartbeat(True)['warning_id']
        raw = json.dumps(dict(op='acknowledge_warning', warning_id=warning)).encode() + b'\n'
        self.assertFalse(self.request(raw, 'worker')['ok'])
        self.assertIsNone(self.policy._ack)
        self.assertTrue(self.request(raw)['ok'])
        self.assertIsNotNone(self.policy._ack)

    def test_instance_lock_symlink_is_preserved_and_denied(self):
        self.service.open()
        self.service.close()
        lock = self.service.path.with_name(self.service.path.name + '.lock')
        lock.unlink()
        target = lock.with_name('target')
        target.write_text('preserve')
        lock.symlink_to(target)
        with self.assertRaises(OSError):
            self.service.open()
        self.assertTrue(lock.is_symlink())
        self.assertEqual(target.read_text(), 'preserve')

    def test_stale_socket_recovery_and_unsafe_types(self):
        self.service.open()
        # Simulate crash: descriptors/locks close, pathnames survive.
        for listener in self.service.listeners:
            listener.close()
        self.service.listeners.clear()
        self.service._socket_identities.clear()
        for fd in self.service._instance_locks:
            os.close(fd)
        self.service._instance_locks.clear()
        self.service.open()
        self.service.close()
        target = self.service.path.with_name('target')
        target.write_text('preserve')
        self.service.path.symlink_to(target)
        with self.assertRaises(PermissionError):
            self.service.open()
        self.assertTrue(self.service.path.is_symlink())
        self.assertEqual(target.read_text(), 'preserve')

    def test_protocol_operations(self):
        for payload in (dict(op='status'), dict(op='ui_heartbeat', notification_capable=True),
                        dict(op='pause', hours=1), dict(op='pause', hours=2), dict(op='resume')):
            self.assertTrue(self.request(json.dumps(payload).encode() + b'\n')['ok'])

    def test_worker_role_status_only_even_with_same_ui_uid(self):
        self.assertTrue(self.request(b'{"op":"status"}\n', 'worker')['ok'])
        for request in (dict(op='pause', hours=1), dict(op='resume'),
                        dict(op='ui_heartbeat', notification_capable=True)):
            self.assertFalse(self.request(json.dumps(request).encode() + b'\n', 'worker')['ok'])
        self.assertIsNone(self.policy._pause)
        self.assertIsNone(self.policy._heartbeat)
        self.assertEqual(self.policy._cooldown, 0)
        with self.assertRaises(AttributeError):
            self.service.worker_uid = 0
        self.service.console = lambda: self.uid + 1
        self.assertTrue(self.request(b'{"op":"status"}\n', 'worker')['ok'])
        self.assertFalse(self.request(b'{"op":"status"}\n', 'ui')['ok'])
        for uid in (0, self.uid + 1):
            self.service.peer = lambda _, uid=uid: uid
            self.assertFalse(self.request(b'{"op":"status"}\n', 'worker')['ok'])

    def test_distinct_worker_and_explicit_root(self):
        for worker in (0, self.uid + 1):
            service = ParticipationService(self.policy, self.service.path, self.uid,
                worker_uid=worker, status_socket=self.service.status_path,
                peer=lambda _: worker, console=lambda: self.uid)
            original, self.service = self.service, service
            try:
                self.assertTrue(self.request(b'{"op":"status"}\n', 'worker')['ok'])
                self.assertFalse(self.request(b'{"op":"resume"}\n', 'ui')['ok'])
                self.assertFalse(self.request(b'{"op":"resume"}\n', 'worker')['ok'])
            finally:
                self.service = original

    def test_second_socket_collision_cleans_only_our_control_socket(self):
        self.service.status_path.parent.mkdir(mode=0o711 if os.geteuid() == 0 else 0o700)
        self.service.status_path.write_text('existing')
        with self.assertRaises(OSError):
            self.service.open()
        self.assertFalse(self.service.path.exists())
        self.assertEqual(self.service.status_path.read_text(), 'existing')

    def test_root_assigns_each_socket_to_its_role_owner(self):
        service = ParticipationService(self.policy, self.service.path, self.uid,
            worker_uid=self.uid + 1, status_socket=self.service.status_path)
        # Exercise root ownership operations without changing host privileges.
        service.path.parent.mkdir(mode=0o711)
        service.path.parent.chmod(0o711)
        original_lstat = Path.lstat
        def root_directory(path):
            info = original_lstat(path)
            if stat.S_ISDIR(info.st_mode):
                fields = list(info)
                fields[4] = 0
                return os.stat_result(fields)
            return info
        original_fstat = os.fstat
        def root_fstat(fd):
            fields = list(original_fstat(fd))
            fields[4] = 0
            return os.stat_result(fields)
        with patch('worker.participation_service.os.geteuid', return_value=0), \
                patch('worker.participation_service.os.fstat', side_effect=root_fstat), \
                patch('worker.participation_service.os.chown') as chown, \
                patch.object(Path, 'lstat', root_directory):
            try:
                service.open()
                self.assertEqual([call.args for call in chown.call_args_list],
                    [(service.path, self.uid, -1), (service.status_path, self.uid + 1, -1)])
                self.assertEqual(stat.S_IMODE(service.status_path.parent.stat().st_mode), 0o711)
            finally:
                service.close()

    def test_service_identity_restricted(self):
        with patch('worker.participation_service.os.geteuid', return_value=self.uid + 1):
            with self.assertRaises(PermissionError):
                self.service.open()
        self.assertFalse(self.service.path.exists())

    def test_invalid_protocol(self):
        for raw in (b'{}\n', b'[]\n', b'null\n', b'{\n', b'\xff\n', b'{}', b'x' * 2049,
                    b'{"op":"pause","hours":true}\n', b'{"op":"pause","hours":1.0}\n',
                    b'{"op":"pause","hours":3}\n', b'{"op":"kill","pid":123}\n',
                    b'{"op":"status","uid":1}\n', b'{"op":"status","op":"resume"}\n',
                    b'{"op":"status"}\n{}\n', b'{"op":"pause","hours":NaN}\n',
                    b'{"op":"ui_heartbeat","notification_capable":1}\n'):
            with self.subTest(raw=raw[:80]):
                self.assertFalse(self.request(raw)['ok'])

    def test_authentication_and_console_race(self):
        self.service.peer = lambda _: self.uid + 1
        self.assertFalse(self.request(b'{"op":"pause","hours":1}\n')['ok'])
        self.assertIsNone(self.policy._pause)
        self.service.peer = lambda _: self.uid
        values = iter((self.uid, self.uid + 1))
        self.service.console = lambda: next(values)
        self.assertFalse(self.request(b'{"op":"pause","hours":1}\n')['ok'])
        self.assertIsNone(self.policy._pause)
        with self.assertRaises(AttributeError):
            self.service.permitted_uid = self.uid + 1

    def test_peer_platform_guard_and_macos_getpeereid(self):
        with patch('worker.participation_service.sys.platform', 'linux'):
            with self.assertRaises(PermissionError):
                peer_uid(None)
        class Peer:
            def getpeereid(inner):
                return (501, 20)
        with patch('worker.participation_service.sys.platform', 'darwin'):
            self.assertEqual(peer_uid(Peer()), 501)

    def test_socket_permissions_collision_cleanup_and_separation(self):
        self.service.open()
        try:
            self.assertEqual(stat.S_IMODE(self.service.path.stat().st_mode), 0o600)
            self.assertEqual(stat.S_IMODE(self.service.status_path.stat().st_mode), 0o600)
            self.assertEqual(self.service.status_path.stat().st_uid, self.uid)
            self.assertEqual(stat.S_IMODE(self.service.path.parent.stat().st_mode),
                             0o711 if os.geteuid() == 0 else 0o700)
            collision = ParticipationService(self.policy, self.service.path, self.uid,
                worker_uid=self.uid, status_socket=self.service.status_path)
            with self.assertRaises(OSError):
                collision.open()
            self.assertTrue(self.service.path.exists())
        finally:
            self.service.close()
        self.assertFalse(self.service.path.exists())
        self.assertFalse(self.service.status_path.exists())
        unsafe = ParticipationService(self.policy, self.path.parent / 'sock', self.uid,
            worker_uid=self.uid, status_socket=self.service.status_path)
        with self.assertRaises(ValueError):
            unsafe.open()


class GateTests(unittest.TestCase):
    def test_live_read_only_status_and_no_cached_allow(self):
        connection = Mock()
        connection.__enter__ = Mock(return_value=connection)
        connection.__exit__ = Mock(return_value=False)
        connection.recv.side_effect = [b'{"ok":true,"status":{"allowed":true,"reason":"eligible"}}\n',
                                       b'{"ok":true,"status":{"allowed":false,"reason":"paused"}}\n']
        gate = ParticipationGate('/unused/control.sock', server_uid=501)
        with patch('socket.socket', return_value=connection) as factory, \
                patch('worker.participation_service.peer_uid', return_value=501):
            self.assertTrue(gate())
            self.assertFalse(gate())
            self.assertEqual(factory.call_count, 2)
            self.assertEqual([call.args for call in connection.sendall.call_args_list],
                             [(b'{"op":"status"}\n',), (b'{"op":"status"}\n',)])

    def test_gate_denies_untrusted_missing_malformed_and_stale_responses(self):
        gate = ParticipationGate('/unused/control.sock', server_uid=501)
        for response in (b'', b'[]\n', b'{}\n', b'x' * 4097,
                         b'{"ok":true,"status":{"allowed":1,"reason":"eligible"}}\n',
                         b'{"ok":true,"status":{"allowed":true,"reason":"paused"}}\n',
                         b'{}\n{}\n'):
            with self.subTest(response=response[:60]):
                connection = Mock()
                connection.__enter__ = Mock(return_value=connection)
                connection.__exit__ = Mock(return_value=False)
                connection.recv.return_value = response
                with patch('socket.socket', return_value=connection), \
                        patch('worker.participation_service.peer_uid', return_value=501):
                    self.assertFalse(gate())
        with patch('socket.socket', side_effect=OSError('missing')):
            self.assertFalse(gate())
        with patch('socket.socket', return_value=connection), \
                patch('worker.participation_service.peer_uid', return_value=502):
            connection.sendall.reset_mock()
            self.assertFalse(gate())
            connection.sendall.assert_not_called()
        with patch('socket.socket', return_value=connection), \
                patch('worker.participation_service.peer_uid', return_value=501), \
                patch('worker.participation.time.monotonic', side_effect=[10, 12]):
            self.assertFalse(gate())

    def test_safety_sample_order_and_latest_raw_idle(self):
        with patch('worker.participation._thermal_from_processinfo', return_value='nominal'), \
                patch('worker.participation.subprocess.check_output', side_effect=[
                    '1', '25', str(8 * 2**30), "'AC Power'", '"HIDIdleTime"=0']) as command:
            sample = participation_sensors()
        self.assertTrue(sample['user_active'])
        self.assertEqual(sample['raw_idle_latest'], 0)
        self.assertEqual([c.args[0][0] for c in command.call_args_list],
                         ['/usr/sbin/sysctl'] * 3 + ['/usr/bin/pmset', '/usr/sbin/ioreg'])
        self.assertTrue(all(c.kwargs['timeout'] <= 2 for c in command.call_args_list))
        with patch('worker.participation._thermal_from_processinfo', return_value='nominal'), \
                patch('worker.participation.subprocess.check_output', side_effect=OSError()), \
                patch('worker.participation.time.monotonic', side_effect=[10, 19]):
            self.assertEqual(participation_sensors(), {})

    def test_memory_sample_is_measured_and_never_cached(self):
        cached = {'metrics': {'memory_available_mib': 99999}}
        with patch('worker.participation._thermal_from_processinfo', return_value='nominal'), \
                patch('worker.participation.subprocess.check_output',
                      side_effect=['1', '25', str(8 * 2**30), "'AC Power'", '"HIDIdleTime"=900000000000']):
            self.assertEqual(participation_sensors()['metrics']['memory_available_mib'], 2048)
        with patch('worker.participation._thermal_from_processinfo', return_value='nominal'), \
                patch('worker.participation.subprocess.check_output', side_effect=OSError()):
            self.assertNotIn('memory_available_mib', participation_sensors()['metrics'])
        self.assertEqual(cached['metrics']['memory_available_mib'], 99999)

    def test_pressure_sample_does_not_reuse_cached_safe_value(self):
        cached = {'metrics': {'vm_pressure_level': 1}}
        with patch('worker.participation._thermal_from_processinfo', return_value='nominal'), \
                patch('worker.participation.subprocess.check_output', return_value='4\n'):
            self.assertEqual(participation_sensors()['metrics']['vm_pressure_level'], 4)
        with patch('worker.participation._thermal_from_processinfo', return_value='nominal'), \
                patch('worker.participation.subprocess.check_output', side_effect=OSError()):
            self.assertNotIn('vm_pressure_level', participation_sensors()['metrics'])


if __name__ == '__main__':
    unittest.main()
