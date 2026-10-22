import os
from pathlib import Path
import stat
import subprocess
import threading
import tempfile
import unittest
from unittest.mock import Mock, patch

from worker.model_supervisor import ModelSupervisor, trusted_executable


class ModelSupervisorTests(unittest.TestCase):
    def setUp(self):
        self.now = 10.0
        self.policy = Mock()
        self.policy.lock = threading.RLock()
        self.policy.allowed.return_value = True
        self.child = Mock()
        self.child.poll.return_value = None
        self.child.wait.side_effect = self.reaped
        self.spawn = Mock(return_value=self.child)
        self.supervisor = ModelSupervisor(self.policy, ['/configured/model', '--serve'],
                                          spawn=self.spawn, monotonic=lambda: self.now,
                                          settle_seconds=2, terminate_seconds=3, kill_seconds=1)

    def reaped(self, timeout):
        self.child.poll.return_value = 0
        return 0

    def start(self):
        self.supervisor.step()
        self.spawn.assert_not_called()
        self.now += 2
        self.supervisor.step()
        self.assertIs(self.supervisor.child, self.child)

    def test_healthy_automatic_run_does_not_expire_but_safety_still_stops(self):
        from worker.participation import Config, ParticipationPolicy

        with tempfile.TemporaryDirectory() as directory:
            sensor = dict(on_ac=True, user_active=False, idle_seconds=600,
                          thermal_state='nominal', metrics={'vm_pressure_level': 1})
            policy = ParticipationPolicy(Path(directory).resolve() / 'state' / 'policy.json',
                Config(enabled=True, automatic_participation=True, warning_seconds=3,
                       confirmation_validity_seconds=10), sensors=lambda: sensor,
                wall=lambda: 1768075200 + self.now, monotonic=lambda: self.now,
                boot=lambda: 'test-boot')
            self.supervisor.policy = policy
            warning = policy.ui_heartbeat(True)['warning_id']
            policy.ui_heartbeat(True, warning)
            self.now += 3
            self.start()
            for _ in range(12):
                self.now += 5
                status = policy.ui_heartbeat(True, warning)
                self.assertTrue(status['allowed'])
                self.assertEqual(status['warning_id'], warning)
                self.assertFalse(status['warning_acknowledged'])
                self.supervisor.step()
            self.spawn.assert_called_once()
            self.child.terminate.assert_not_called()
            sensor['on_ac'] = False
            self.supervisor.step()
            self.child.terminate.assert_called_once()
            self.assertIsNone(policy.status()['warning_submitted_at'])

    def test_settle_spawn_and_cancel_owned_child(self):
        self.start()
        self.spawn.assert_called_once_with(['/configured/model', '--serve'], shell=False,
                                          stdin=subprocess.DEVNULL, close_fds=True)
        self.policy.allowed.return_value = False
        self.supervisor.step()
        self.child.terminate.assert_called_once_with()
        self.child.wait.assert_called_once_with(timeout=3)
        self.child.kill.assert_not_called()
        self.assertIsNone(self.supervisor.child)
        self.policy.stopped.assert_called_once_with()

    def test_final_gate_recheck_prevents_spawn(self):
        self.supervisor.step()
        self.now += 2
        self.policy.allowed.side_effect = [True, False]
        self.supervisor.step()
        self.spawn.assert_not_called()

    def test_change_during_spawn_unloads_immediately(self):
        self.supervisor.step()
        self.now += 2
        self.policy.allowed.side_effect = [True, True, False]
        self.supervisor.step()
        self.child.terminate.assert_called_once_with()
        self.assertIsNone(self.supervisor.child)

    def test_terminate_timeout_kills_and_reaps_only_child(self):
        self.start()
        self.child.wait.side_effect = [subprocess.TimeoutExpired('owned', 3), 0]
        self.child.kill.side_effect = lambda: setattr(self.child.poll, 'return_value', -9)
        self.supervisor.close()
        self.child.terminate.assert_called_once_with()
        self.child.kill.assert_called_once_with()
        self.assertEqual([call.kwargs for call in self.child.wait.call_args_list],
                         [{'timeout': 3}, {'timeout': 1}])
        self.assertIsNone(self.supervisor.child)
        self.supervisor.step()
        self.spawn.assert_called_once()

    def test_unreapable_child_keeps_ownership_and_close_can_retry(self):
        self.start()
        self.child.wait.side_effect = subprocess.TimeoutExpired('owned', 1)
        with self.assertRaises(subprocess.TimeoutExpired):
            self.supervisor.close()
        self.assertIs(self.supervisor.child, self.child)
        self.supervisor.step()
        self.spawn.assert_called_once()
        self.child.wait.side_effect = self.reaped
        self.supervisor.close()
        self.assertIsNone(self.supervisor.child)

    def test_exited_child_reaped_without_signal(self):
        self.start()
        self.child.poll.return_value = 1
        self.supervisor.step()
        self.child.wait.assert_called_once_with(timeout=1)
        self.child.terminate.assert_not_called()
        self.policy.stopped.assert_called_once_with()

    def test_gate_error_unloads_and_propagates(self):
        self.start()
        self.policy.allowed.side_effect = OSError('unavailable')
        with self.assertRaises(OSError):
            self.supervisor.step()
        self.child.terminate.assert_called_once_with()
        self.assertIsNone(self.supervisor.child)

    def test_run_cancellation_unloads(self):
        self.start()
        stop = threading.Event()
        stop.set()
        self.supervisor.run(stop)
        self.child.terminate.assert_called_once_with()
        self.assertIsNone(self.supervisor.child)

    def test_spawn_failure_stops_service_owner(self):
        self.supervisor.step()
        self.now += 2
        self.spawn.side_effect = OSError('cannot spawn')
        stop = threading.Event()
        with self.assertRaises(OSError):
            self.supervisor.run(stop)
        self.assertTrue(stop.is_set())
        self.assertIsNone(self.supervisor.child)

    def test_disabled_and_interrupted_settle_never_spawn(self):
        self.policy.allowed.return_value = False
        self.supervisor.step()
        self.now += 100
        self.supervisor.step()
        self.spawn.assert_not_called()
        self.policy.allowed.return_value = True
        self.supervisor.step()
        self.now -= 1
        self.supervisor.step()
        self.spawn.assert_not_called()

    def test_no_child_stop_does_not_start_or_extend_cooldown(self):
        self.policy.allowed.return_value = False
        for _ in range(3):
            self.supervisor.step()
            self.now += 10
        self.supervisor.close()
        self.supervisor.close()
        self.policy.stopped.assert_not_called()
        self.spawn.assert_not_called()

    def test_root_launch_drops_all_identity_and_never_shell(self):
        with patch('worker.model_supervisor.os.geteuid', return_value=0), \
                patch('worker.model_supervisor.trusted_executable') as trusted:
            supervisor = ModelSupervisor(self.policy, ['/trusted/model'], spawn=self.spawn,
                monotonic=lambda: self.now, worker_uid=501, worker_gid=20, settle_seconds=1)
            supervisor.step()
            self.now += 1
            supervisor.step()
            self.spawn.assert_called_once_with(['/trusted/model'], shell=False,
                stdin=subprocess.DEVNULL, close_fds=True, user=501, group=20, extra_groups=[])
            self.assertEqual(trusted.call_count, 2)
            self.policy.set_model_running.assert_called_with(True)
            supervisor.close()
            self.policy.set_model_running.assert_called_with(False)

    def test_root_missing_or_root_identity_rejected(self):
        with patch('worker.model_supervisor.os.geteuid', return_value=0):
            for kwargs in ({}, {'worker_uid': 0, 'worker_gid': 20},
                           {'worker_uid': 501}, {'worker_uid': 501, 'worker_gid': 0}):
                with self.subTest(kwargs=kwargs), self.assertRaises(ValueError):
                    ModelSupervisor(self.policy, ['/trusted/model'], **kwargs)

    def test_trusted_executable_rejects_symlinks_writers_and_owners(self):
        def info(mode, uid=0):
            return os.stat_result((mode, 1, 1, 1, uid, 0, 0, 0, 0, 0))
        for bad in (info(stat.S_IFLNK | 0o777), info(stat.S_IFDIR | 0o777),
                    info(stat.S_IFDIR | 0o755, 501)):
            def inspect(path):
                if str(path) == '/trusted':
                    return bad
                return info((stat.S_IFREG if path.name == 'model' else stat.S_IFDIR) | 0o755)
            with patch.object(Path, 'lstat', inspect), self.assertRaises(PermissionError):
                trusted_executable('/trusted/model')
        for bad in (info(stat.S_IFLNK | 0o777), info(stat.S_IFREG | 0o775),
                    info(stat.S_IFREG | 0o755, 501), info(stat.S_IFREG | 0o644)):
            with patch.object(Path, 'lstat', lambda path: bad if path.name == 'model'
                              else info(stat.S_IFDIR | 0o755)), self.assertRaises(PermissionError):
                trusted_executable('/trusted/model')
        with self.assertRaises(PermissionError):
            trusted_executable('model')

    def test_unreaped_child_retains_running_allocation_state(self):
        self.start()
        self.child.wait.side_effect = subprocess.TimeoutExpired('owned', 1)
        self.policy.set_model_running.reset_mock()
        with self.assertRaises(subprocess.TimeoutExpired):
            self.supervisor.close()
        self.policy.set_model_running.assert_not_called()

    def test_invalid_configuration(self):
        for argv in ('shell command', [], [''], ['a\0b'], [1]):
            with self.subTest(argv=argv), self.assertRaises(ValueError):
                ModelSupervisor(self.policy, argv)
        for seconds in (0, -1, True, float('nan'), 61):
            with self.subTest(seconds=seconds), self.assertRaises(ValueError):
                ModelSupervisor(self.policy, ['model'], terminate_seconds=seconds)


if __name__ == '__main__':
    unittest.main()
