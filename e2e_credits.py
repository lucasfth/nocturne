"""Live curl credit flow. Secrets are sent via curl stdin, never printed or argv."""
import json
import os
import subprocess
import time
from pathlib import Path

ROOT=Path(__file__).resolve().parent
os.chdir(ROOT)

def curl(port,path,method='GET',body=None,key=None):
    config=f'url = "http://127.0.0.1:{port}{path}"\nrequest = "{method}"\nsilent\nshow-error\nfail-with-body\nmax-time = 60\n'
    if key: config+='header = '+json.dumps('Authorization: Bearer '+key)+'\n'
    if body is not None:
        config+='header = "Content-Type: application/json"\ndata = '+json.dumps(json.dumps(body))+'\n'
    r=subprocess.run(['curl','--config','-'],input=config,text=True,capture_output=True)
    if r.returncode: raise RuntimeError('curl request failed: '+path+' '+r.stdout)
    return json.loads(r.stdout)

transcript=[]
def record(label,data): transcript.append(label+' '+json.dumps(data,sort_keys=True))

if __name__=='__main__':
    cfg=json.loads((ROOT/'config.json').read_text())
    key=curl(8700,'/v1/keys','POST',{'invite':cfg['signup_invite']})
    token=key.pop('api_key_plaintext_once'); record('POST /v1/keys',{**key,'api_key_plaintext_once':'[REDACTED]'})
    invoice=curl(8700,'/v1/credits/invoice','POST',{'amount_msat':100000},token); record('POST /v1/credits/invoice',invoice)
    i=invoice['invoice_id']; record('pending',curl(8700,'/v1/credits/invoice/'+i,key=token))
    admin=(ROOT/'data/admin-secret.txt').read_text().strip()
    record('mark-paid',curl(8702,'/admin/invoices/'+i+'/mark-paid','POST',key=admin))
    record('mark-paid duplicate',curl(8702,'/admin/invoices/'+i+'/mark-paid','POST',key=admin))
    record('paid',curl(8700,'/v1/credits/invoice/'+i,key=token))
    before=curl(8700,'/v1/credits/balance',key=token); record('balance before',before)
    with (ROOT/'data/e2e-simworker.log').open('a') as f:
        worker=subprocess.Popen([str(ROOT/'.venv/bin/python'),'-m','simworker.sim','--workers','1','--script','demo','--tokens-per-s','100'],stdout=f,stderr=f)
        try:
            for _ in range(20):
                if curl(8700,'/healthz')['eligible_workers']: break
                time.sleep(.2)
            result=curl(8700,'/v1/chat/completions','POST',{'model':cfg['model'],'messages':[{'role':'user','content':'Explain prepaid inference briefly.'}],'max_tokens':20},token)
            record('chat completion',{'id':result['id'],'usage':result['usage'],'finish_reason':result['choices'][0]['finish_reason']})
        finally:
            worker.terminate(); worker.wait(timeout=10)
    after=curl(8700,'/v1/credits/balance',key=token); record('balance after',after)
    assert before['balance_msat']==100000 and 0<after['balance_msat']<100000
    with (ROOT/'data/e2e-credits-transcript.txt').open('a') as f: f.write(time.strftime('%Y-%m-%dT%H:%M:%SZ',time.gmtime())+'\n'+'\n'.join(transcript)+'\n')
    print('\n'.join(transcript))
