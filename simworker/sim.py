import argparse, asyncio, json
from pathlib import Path
from worker.agent import Agent

SAFE=dict(on_ac=True,battery_pct=90,thermal_state='nominal',user_active=False)
def timeline(rows):
    def state(t):
        s=SAFE.copy()
        for row in sorted(rows,key=lambda r:r['t']):
            if t>=row['t']: s.update({k:v for k,v in row.items() if k!='t'})
        return s
    return state

async def main():
    p=argparse.ArgumentParser(); p.add_argument('--workers',type=int,default=3); p.add_argument('--script',default='demo'); p.add_argument('--tokens-per-s',type=float,default=3); p.add_argument('--coordinator-url',default='http://127.0.0.1:8701'); p.add_argument('--secret-dir',default='data'); a=p.parse_args()
    scripts=[[{'t':0,**SAFE},{'t':10,'on_ac':False}],[{'t':0,**SAFE,'battery_pct':25},{'t':10,'battery_pct':15}],[{'t':0,**SAFE}]] if a.script=='demo' else json.loads(Path(a.script).read_text())
    # A JSON list is a common timeline, a list of lists provides per-worker timelines.
    if scripts and isinstance(scripts[0],dict): scripts=[scripts]*a.workers
    agents=[Agent(a.coordinator_url,f'sim-{i+1}',Path(a.secret_dir,f'sim-{i+1}.secret').read_text().strip(),timeline(scripts[i%len(scripts)]),a.tokens_per_s) for i in range(a.workers)]
    await asyncio.gather(*(w.run() for w in agents))
if __name__=='__main__': asyncio.run(main())
