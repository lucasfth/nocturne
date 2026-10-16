/** Parses only the two public registration lines emitted by worker/bootstrap-remote.sh.
 * No execution, network, persistence, or interpretation of shell syntax. */
export interface BootstrapPreview { workerId: string; digest?: string; pubkey?: string; ignoredLines: number; reportVersion?: string; meta?: Partial<Record<'owner'|'user'|'host'|'ip'|'model'|'admin_user',string>> }
const workerId = /^[a-z0-9][a-z0-9-]{0,31}$/
export function validTunnelKey(value: string): boolean {
 const parts=value.trim().split(/\s+/)
 if(parts[0]!=='ssh-ed25519' || !parts[1] || parts[1].length>128 || !/^[A-Za-z0-9+/]+={0,2}$/.test(parts[1])) return false
 try {
  const blob=atob(parts[1]); const u32=(i:number)=>((blob.charCodeAt(i)*0x1000000)+(blob.charCodeAt(i+1)<<16)+(blob.charCodeAt(i+2)<<8)+blob.charCodeAt(i+3))
  return blob.length===51 && u32(0)===11 && blob.slice(4,15)==='ssh-ed25519' && u32(15)===32
 } catch { return false }
}
export function parseBootstrap(text: string, expectedId?: string): BootstrapPreview {
 if(text.length>16384) throw new Error('Setup output is too large (maximum 16 KiB).')
 const reportHeaders=[...text.matchAll(/^=== nocturne mac report v([0-9][0-9.-]*) ===\s*$/gm)]
 if(reportHeaders.length && /^[ \t]*(?:DIGEST|PUBKEY)(?:\s|$)/m.test(text))throw new Error('Mixed report and registration output. Import the Mac report and DIGEST / PUBKEY output separately.')
 if(reportHeaders.length && (text.match(/^=== end ===[ \t]*\r?$/gm)||[]).length!==1)throw new Error('Ambiguous Mac report boundaries. Paste one complete report only.')
 if(reportHeaders.length>1)throw new Error('Multiple Mac reports. Paste one Mac’s output only.')
 let reportMeta: BootstrapPreview['meta']
 if(reportHeaders.length) {
  const start=text.indexOf(reportHeaders[0]![0])+reportHeaders[0]![0].length
  const end=text.indexOf('=== end ===',start)
  if(end<0)throw new Error('Incomplete Mac report. Include the === end === line.')
  reportMeta={};const seenLabels=new Set<string>()
  const mapping={owner_name:'owner',ssh_user:'user',hostname:'host',lan_ip:'ip',model:'model',admin_user:'admin_user'} as const
  for(const raw of text.slice(start,end).split(/\r?\n/)) {
   const m=/^([a-z_]+):[ \t]*(.*)$/.exec(raw.trim());if(!m)continue
   const label=m[1]!;if(!Object.hasOwn(mapping,label))continue
   if(seenLabels.has(label))throw new Error('Duplicate machine report fields.')
   seenLabels.add(label);let value=m[2]!.trim();if(!value||['none','?','not installed'].includes(value))continue
   if(label==='lan_ip')value=value.replace(/[ \t]+\(wifi:.*\)$/,'')
   if(label==='admin_user')value=value.replace(/[ \t]+\([^)]*\)$/,'')
   if(label==='ssh_user'||label==='admin_user') {if(!/^[a-z_][a-z0-9_-]{0,31}[$]?$/.test(value))throw new Error('Invalid user field in Mac report.')}
   else if(label==='hostname') {if(value.length>253||!value.split('.').every(v=>/^[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$/.test(v)))throw new Error('Invalid hostname in Mac report.')}
   else if(label==='lan_ip') {const parts=value.split('.');if(parts.length!==4||!parts.every(v=>/^\d{1,3}$/.test(v)&&Number(v)<=255))throw new Error('Invalid LAN IPv4 address in Mac report.')}
   else if(value.length>200||/[\x00-\x1f\x7f]/.test(value))throw new Error('Invalid display field in Mac report.')
   reportMeta[mapping[label as keyof typeof mapping]]=value
  }
  if(!Object.keys(reportMeta).length)throw new Error('Mac report has no supported machine fields.')
 }
 let result: BootstrapPreview={workerId:'',ignoredLines:0}; const seen=new Set<string>()
 for(const raw of text.split(/\r?\n/)) {
  const line=raw.trim(); if(!line)continue
  if(!/^(?:DIGEST|PUBKEY)(?:\s|$)/.test(line)){result.ignoredLines++;continue}
  const match=/^(DIGEST|PUBKEY)[ \t]+([^\s]+)[ \t]+([^\r\n]+)$/.exec(line)
  if(!match)throw new Error('Malformed registration line. Copy only DIGEST / PUBKEY output, not commands.')
  const label=match[1]!, id=match[2]!, value=match[3]!
  if(seen.has(label))throw new Error('Duplicate registration labels. Paste one Mac’s output only.')
  seen.add(label)
  if(!workerId.test(id))throw new Error('Invalid worker id in setup output.')
  if((result.workerId && result.workerId!==id)||(expectedId && expectedId!==id))throw new Error('Bootstrap worker IDs do not match')
  result.workerId=id
  if(label==='DIGEST') {
   if(!/^[a-fA-F0-9]{64}$/.test(value))throw new Error('Invalid SHA256 digest in setup output.')
   result.digest=value.toLowerCase()
  } else {
   if(!validTunnelKey(value))throw new Error('Invalid ssh-ed25519 public key in setup output.')
   result.pubkey=value.trim().split(/\s+/).slice(0,2).join(' ')
  }
 }
 if(reportHeaders.length) {result.reportVersion=reportHeaders[0]![1];result.meta=reportMeta}
 if(!result.workerId && !reportHeaders.length)throw new Error('No DIGEST / PUBKEY registration lines found. Paste bootstrap output, not shell commands or the owner-preparation status.')
 return result
}
