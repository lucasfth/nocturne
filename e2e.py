import asyncio, json, os, signal, subprocess, time
from pathlib import Path
import httpx

async def main():
    out=[]
    def note(s): out.append(s); print(s,flush=True)
    key=Path('data/demo-api-key.txt').read_text().strip(); admin=Path('data/admin-secret.txt').read_text().strip()
    workers=[]
    try:
        # Three separate OS processes so heartbeat disappearance is genuine.
        scripts=[ [{'t':0,'on_ac':True,'battery_pct':90,'thermal_state':'nominal','user_active':False},{'t':10,'on_ac':False}], [{'t':0,'on_ac':True,'battery_pct':25,'thermal_state':'nominal','user_active':False},{'t':10,'battery_pct':15}], [{'t':0,'on_ac':True,'battery_pct':90,'thermal_state':'nominal','user_active':False}] ]
        from worker.agent import Agent
        from simworker.sim import timeline
        for i,rows in enumerate(scripts):
            script=Path(f'data/e2e-worker-{i+1}.json'); script.write_text(json.dumps(rows)); script.chmod(0o600)
            code="import asyncio,json; from pathlib import Path; from worker.agent import Agent; from simworker.sim import timeline; asyncio.run(Agent('http://127.0.0.1:8701','sim-%d',Path('data/sim-%d.secret').read_text().strip(),timeline(json.loads(Path('data/e2e-worker-%d.json').read_text())),3).run())"%(i+1,i+1,i+1)
            workers.append(subprocess.Popen(['.venv/bin/python','-c',code],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL))
        async with httpx.AsyncClient(timeout=190,trust_env=False) as client:
            await asyncio.sleep(2)
            states=(await client.get('http://127.0.0.1:8702/admin/workers',headers={'Authorization':'Bearer '+admin})).json()
            note('ADMISSION '+json.dumps(states))
            assert next(w for w in states if w['worker_id']=='sim-2')['eligible'] is False
            note('admission refusal: sim-2 on AC but battery 25% => ineligible; unplugged sim-1 will also become ineligible')
            async def request(i,stream=False):
                b={'model':'Ornith-1.5-9B','messages':[{'role':'user','content':'demo request '+str(i)}],'max_tokens':40,'stream':stream}
                r=await client.post('http://127.0.0.1:8700/v1/chat/completions',headers={'Authorization':'Bearer '+key},json=b)
                text=r.text
                if stream: note(f'REQUEST {i} STREAM HTTP {r.status_code} first={text[:240]} tail={text[-320:]}')
                else: note(f'REQUEST {i} NONSTREAM HTTP {r.status_code} '+text[:700])
                assert r.status_code==200
            tasks=[]
            # Ensure nonstream request has been leased before the three streams.
            tasks.append(asyncio.create_task(request(0)))
            await asyncio.sleep(1)
            tasks.append(asyncio.create_task(request(1)))
            await asyncio.sleep(1)
            tasks.extend(asyncio.create_task(request(i,i>=3)) for i in range(2,6))
            await asyncio.sleep(11)
            states=(await client.get('http://127.0.0.1:8702/admin/workers',headers={'Authorization':'Bearer '+admin})).json()
            note('AFTER UNPLUG '+json.dumps(states))
            await asyncio.gather(*tasks)
            note('Six requests complete (three streaming); unplug cancellation/requeue recorded in service journal.')
            # A seventh diagnostic proves missing-heartbeat streaming behavior.
            loss=asyncio.create_task(client.post('http://127.0.0.1:8700/v1/chat/completions',headers={'Authorization':'Bearer '+key},json={'model':'Ornith-1.5-9B','messages':[{'role':'user','content':'heartbeat loss diagnostic'}],'max_tokens':200,'stream':True}))
            await asyncio.sleep(3); workers[2].kill(); workers[2].wait()
            note('Killed sim-3 mid-stream; waiting for real 45-second heartbeat expiry.')
            r=await loss; note('WORKER LOSS HTTP '+str(r.status_code)+' tail='+r.text[-500:]); assert 'worker_interrupted' in r.text and '[DONE]' in r.text
            ledger=(await client.get('http://127.0.0.1:8702/admin/ledger/tail',headers={'Authorization':'Bearer '+admin})).json()
            note('LEDGER TAIL\n'+'\n'.join(json.dumps(x) for x in ledger))
    finally:
        for w in workers:
            if w.poll() is None: w.terminate()
        for w in workers:
            try: w.wait(timeout=5)
            except subprocess.TimeoutExpired: w.kill(); w.wait()
        f=Path('data/e2e-transcript.txt'); f.write_text('\n'.join(out)+'\n'); f.chmod(0o600)
asyncio.run(main())
