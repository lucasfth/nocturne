import { test, expect } from '@playwright/test'
import { parseBootstrap, validTunnelKey } from '../src/lib/bootstrap'
import { readFile } from 'node:fs/promises'
const key='ssh-ed25519 '+Buffer.concat([Buffer.from([0,0,0,11]),Buffer.from('ssh-ed25519'),Buffer.from([0,0,0,32]),Buffer.alloc(32,7)]).toString('base64')
const digest='ab'.repeat(32)
test('canonical shell output labels map only identity and registration credentials',async()=>{
 const source=await readFile(new URL('../../worker/bootstrap-remote.sh',import.meta.url),'utf8')
 expect(source).toContain('echo "DIGEST $ID '); expect(source).toContain('echo "PUBKEY $ID ')
 expect(parseBootstrap(`Installing worker...\r\n DIGEST mac-example ${digest.toUpperCase()}\r\nPUBKEY mac-example ${key} public-comment\r\n`)).toEqual({workerId:'mac-example',digest,pubkey:key,ignoredLines:1})
})
for(const [name,text] of [
 ['duplicate',`DIGEST mac-a ${digest}\nDIGEST mac-a ${digest}`],
 ['conflicting duplicate',`DIGEST mac-a ${digest}\nDIGEST mac-a ${'f'.repeat(64)}`],
 ['mismatched ids',`DIGEST mac-a ${digest}\nPUBKEY mac-b ${key}`],
 ['quoted digest',`DIGEST mac-a "${digest}"`],
 ['malformed digest','DIGEST mac-a nope'],['invalid id',`DIGEST mac-a;echo ${digest}`],
 ['trailing command',`DIGEST mac-a ${digest};touch /tmp/nope`],
 ['invalid key','PUBKEY mac-a ssh-ed25519 AAAA'],['wrong algorithm',`PUBKEY mac-a ssh-rsa AAAA`],
 ['missing value','DIGEST mac-a'],['oversized','x'.repeat(16385)],
 ['script instead of output','echo "DIGEST $ID $(cat secret)"'],['owner status only','Owner SSH managed block updated.'],
 ['unknown host labels','HOST gateway.example.invalid\nPORT 22'],
] as const)test(`parser rejects ${name} without reflecting pasted values`,()=>{
 expect(()=>parseBootstrap(text)).toThrow();try{parseBootstrap(text)}catch(e){expect((e as Error).message).not.toContain(digest)}
})
test('partial output preserves absent values and unknown shell lines stay inert',()=>{
 expect(parseBootstrap(`$(touch /tmp/never-execute)\nDIGEST mac-a ${digest}`)).toEqual({workerId:'mac-a',digest,ignoredLines:1})
 expect(parseBootstrap(`PUBKEY mac-a ${key}`)).toEqual({workerId:'mac-a',pubkey:key,ignoredLines:0})
 expect(()=>parseBootstrap(`DIGEST mac-b ${digest}`,'mac-a')).toThrow('Bootstrap worker IDs do not match')
 expect(validTunnelKey(key)).toBe(true);expect(validTunnelKey(key.replace('AAAAC3','AAAAC4'))).toBe(false)
})

test('yesterday owner system report imports only actual machine fields, never credentials or inferred identity',async()=>{
 const report=await readFile(new URL('../fixtures/legacy-mac-report.txt',import.meta.url),'utf8')
 const p=parseBootstrap(report.replaceAll('\n','\r\n'))
 expect(p.reportVersion).toBe('2026-10-13.2');expect(p.workerId).toBe('');expect(p.digest).toBeUndefined();expect(p.pubkey).toBeUndefined()
 expect(p.meta).toEqual({owner:'Example Owner',user:'example',host:'Example-MacBook.local',ip:'192.0.2.25',admin_user:'nocturne-admin',model:'Mac16,1  Apple M4  16 GB'})
 expect(()=>parseBootstrap(report.replace('=== end ===',''))).toThrow('Ambiguous Mac report boundaries')
 expect(()=>parseBootstrap(report+'\n'+report)).toThrow()
 for(const [a,b] of [['ssh_user:       example','ssh_user:       example;rm'],['Example-MacBook.local','https://gateway.example'],['192.0.2.25','999.0.2.25']])expect(()=>parseBootstrap(report.replaceAll(a!,b!))).toThrow()
 expect(()=>parseBootstrap(report.replace('=== end ===','ssh_user: other\n=== end ==='))).toThrow('Duplicate')
 const partial=parseBootstrap('=== nocturne mac report v2026-10-13.2 ===\nowner_name: Example Owner\n=== end ===')
 expect(partial.meta).toEqual({owner:'Example Owner'})
})

test('report mode rejects mixed credentials and ambiguous report boundaries',async()=>{
 const report=await readFile(new URL('../fixtures/legacy-mac-report.txt',import.meta.url),'utf8')
 for(const text of [report+`DIGEST other-mac ${digest}`,`PUBKEY mac-a ${key}\n`+report,report.replace('owner_name:',`DIGEST mac-a ${digest}\nowner_name:`)])expect(()=>parseBootstrap(text)).toThrow('Mixed report and registration')
 for(const text of ['=== end ===\n'+report,report+'=== end ===\n'])expect(()=>parseBootstrap(text)).toThrow('Ambiguous Mac report boundaries')
 expect(()=>parseBootstrap('=== end ===\n'+report.replace('=== end ===\n',''))).toThrow()
})
