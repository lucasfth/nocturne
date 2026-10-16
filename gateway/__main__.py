import asyncio, logging, os
from pathlib import Path
import uvicorn
from .app import Coordinator
from .protocol import EdgeH11Protocol

async def main():
    logging.basicConfig(level=logging.INFO,format='%(asctime)s %(message)s')
    c=Coordinator(Path(os.environ.get('SERVICE_ROOT','.')))
    servers=[uvicorn.Server(uvicorn.Config(app,host='127.0.0.1',port=port,access_log=False,log_level='warning',limit_concurrency=c.config.get('public_limit_concurrency',64) if port==8700 else 256,backlog=64 if port==8700 else 128,timeout_keep_alive=5,server_header=False,proxy_headers=False,http=EdgeH11Protocol)) for app,port in [(c.public,8700),(c.worker,8701),(c.admin,8702)]]
    monitor=asyncio.create_task(c.maintenance())
    invoices=asyncio.create_task(c.invoice_maintenance())
    try: await asyncio.gather(*(s.serve() for s in servers))
    finally: monitor.cancel(); invoices.cancel()

asyncio.run(main())
