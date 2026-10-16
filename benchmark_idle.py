"""60-second live 12-simworker benchmark; credentials read locally, never printed."""
import asyncio
import json
import os
import time
from pathlib import Path
import httpx
from worker.agent import Agent
from simworker.sim import SAFE

async def main():
    pid=int(os.popen('systemctl --user show nocturne-gateway.service -p MainPID --value').read())
    def cpu():
        r=Path(f'/proc/{pid}/stat').read_text().split()
        return (int(r[13])+int(r[14]))/os.sysconf('SC_CLK_TCK')
    async def leases(agent):
        while True:
            await agent.post('/worker/lease',{})
    agents=[Agent('http://127.0.0.1:8701',f'sim-{i}',Path(f'data/sim-{i}.secret').read_text().strip(),lambda t:SAFE.copy(),20) for i in range(1,13)]
    tasks=[asyncio.create_task(a.run()) for a in agents]
    await asyncio.sleep(3)
    # Closed schedule normally suppresses Agent leases. Exercise the gateway's
    # twelve concurrent idle long-polls explicitly, while their heartbeats run.
    probes=[asyncio.create_task(leases(a)) for a in agents]
    start=time.monotonic(); before=cpu()
    await asyncio.sleep(60)
    print(json.dumps(dict(workers=12,duration_seconds=round(time.monotonic()-start,2),gateway_cpu_percent=round((cpu()-before)/(time.monotonic()-start)*100,3),pid=pid)))
    for t in probes+tasks: t.cancel()
    await asyncio.gather(*probes,*tasks,return_exceptions=True)

if __name__=='__main__': asyncio.run(main())
