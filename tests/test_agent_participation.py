import asyncio
import unittest
from unittest.mock import AsyncMock, patch
from worker.participation import ParticipationGate
from worker.agent import Agent, safe

SENSOR = dict(on_ac=True, battery_pct=0, user_active=False, thermal_state='nominal')

class AgentParticipationTests(unittest.IsolatedAsyncioTestCase):
    def test_desktop_ac(self):
        self.assertTrue(safe(SENSOR))
        self.assertFalse(safe(dict(SENSOR, on_ac=False)))

    def test_fail_closed_gate(self):
        agent = Agent('http://unused', 'fixture', 'fake', participation=lambda: False)
        agent.state = SENSOR
        self.assertFalse(agent.locally_ready())

    async def test_unavailable_ui_cannot_use_gateway_readiness(self):
        gate = ParticipationGate('/nonexistent/nocturne-fixture.sock', 0)
        agent = Agent('http://unused', 'fixture', 'fake', sensors=lambda: SENSOR, participation=gate)
        agent.backend_ok = AsyncMock(return_value=True)
        sent = []
        async def post(path, body):
            sent.append((path, body))
            return {'serve': True, 'active_requests': []}
        agent.post = post
        with patch('worker.agent.admission_allowed', return_value=True), \
                patch('worker.agent.asyncio.sleep', side_effect=asyncio.CancelledError):
            with self.assertRaises(asyncio.CancelledError):
                await agent.heartbeat()
        self.assertTrue(agent.serving)  # The gateway cannot override local denial.
        self.assertEqual(sent[0][0], '/worker/heartbeat')
        self.assertFalse(sent[0][1]['ready'])
        agent.backend_ok.assert_not_awaited()
        self.assertFalse(agent.locally_ready())

    async def test_benchmark_denial_still_blocks_participation(self):
        agent = Agent('http://unused', 'fixture', 'fake', tokens_per_s=1000, participation=lambda: True)
        agent.state = SENSOR
        agent.post = AsyncMock(return_value={'accepted': True})
        with patch('worker.agent.admission_allowed', return_value=False):
            await agent.execute(dict(request_id='r', lease_id='l', body={'max_tokens': 2, 'messages': []}))
        agent.post.assert_awaited_once_with('/worker/fail', {'request_id': 'r', 'lease_id': 'l'})

    async def test_stream_cancels_and_reports_lease_failure(self):
        events = []
        agent = Agent('http://unused', 'fixture', 'fake', tokens_per_s=1000, participation=lambda: False)
        agent.state = SENSOR
        async def post(path, body):
            events.append((path, body)); return {'accepted': True}
        agent.post = post
        await agent.execute(dict(request_id='r', lease_id='l', body={'max_tokens': 2, 'messages': []}))
        self.assertEqual([path for path, _ in events], ['/worker/fail'])

    async def test_gate_changes_after_lease_fails_instead_of_execute(self):
        allow = True
        agent = Agent('http://unused', 'fixture', 'fake', participation=lambda: allow)
        async def heartbeat():
            agent.state = SENSOR; agent.serving = True
            await asyncio.Event().wait()
        agent.heartbeat = heartbeat
        failed = asyncio.Event()
        async def post(path, body):
            nonlocal allow
            if path.endswith('/lease'):
                allow = False
                return dict(request_id='r', lease_id='l')
            if path.endswith('/fail'): failed.set()
            return {}
        agent.post = post
        running = asyncio.create_task(agent.run())
        await asyncio.wait_for(failed.wait(), 3)
        self.assertFalse(agent.tasks)
        running.cancel()
        await asyncio.gather(running, return_exceptions=True)

if __name__ == '__main__': unittest.main()
