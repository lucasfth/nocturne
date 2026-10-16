<script setup lang="ts">
import { reactive, ref, watch } from 'vue'
import { parseBootstrap, validTunnelKey, type BootstrapPreview } from '@/lib/bootstrap'
import { api, type Machine, type Meta, type MachinePatch } from '@/api'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Textarea } from '@/components/ui/textarea'
import { Dialog, DialogContent, DialogHeader, DialogTitle, DialogDescription } from '@/components/ui/dialog'
const props = defineProps<{ open: boolean; machine: Machine | null }>()
const emit = defineEmits<{ 'update:open': [value: boolean]; saved: [] }>()
const fields: [keyof Meta,string][] = [['owner','Owner'],['model','Hardware description (optional)'],['user','macOS user (optional)'],['host','Host (optional)'],['ip','LAN IP (optional)'],['jump','Jump host (optional)'],['admin_user','Admin user (optional)']]
const id = ref(''), digest = ref(''), pubkey = ref(''), paste = ref(''), error = ref(''), busy = ref(false)
const meta = reactive<Record<keyof Meta,string>>({ owner:'',agent:'',model:'',user:'',host:'',ip:'',jump:'',admin_user:'' })
watch(() => props.open, open => { if (!open) return; id.value=props.machine?.worker_id || ''; digest.value=''; pubkey.value=''; paste.value=''; error.value=''; for (const [key] of fields) meta[key]=props.machine?.meta[key] || '' })
const preview = ref<BootstrapPreview | null>(null)
function clearPaste() { paste.value=''; preview.value=null }
watch(paste, () => { preview.value=null; error.value='' })
watch(() => props.open, open => { if(!open) { clearPaste(); digest.value=''; pubkey.value='' } })
function parseOutput() {
 error.value=''; preview.value=null
 try { preview.value=parseBootstrap(paste.value, props.machine?.worker_id) } catch(e) { error.value=e instanceof Error ? e.message : 'Invalid setup output.' }
}
function applyOutput() {
 if(!preview.value)return
 const p=preview.value
 // Never silently overwrite any manually entered value, including machine identity.
 if((p.workerId && id.value && id.value!==p.workerId)||(p.digest && digest.value && digest.value!==p.digest)||(p.pubkey && pubkey.value && pubkey.value!==p.pubkey)||Object.entries(p.meta||{}).some(([key,value])=>meta[key as keyof Meta] && meta[key as keyof Meta]!==value)) {
  error.value='Existing fields differ. Clear the fields you want to replace, then apply again.'; return
 }
 if(p.workerId)id.value=p.workerId
 for(const [key,value] of Object.entries(p.meta||{}))meta[key as keyof Meta]=value
 if(p.digest)digest.value=p.digest
 if(p.pubkey)pubkey.value=p.pubkey
 clearPaste();error.value=''
}
async function submit() {
 error.value=''
 if(paste.value.trim()) { error.value='Review and apply the pasted output, or clear it before saving.'; return }
 if (!/^[a-z0-9][a-z0-9-]{0,31}$/.test(id.value)) { error.value='Worker id must be 1–32 lowercase letters, numbers or hyphens, starting with a letter or number.'; return }
 if ((!props.machine || digest.value) && !/^[a-fA-F0-9]{64}$/.test(digest.value)) { error.value='Secret SHA256 must contain 64 hexadecimal characters.'; return }
 if (pubkey.value && !validTunnelKey(pubkey.value)) { error.value='Use an ssh-ed25519 public key.'; return }
 busy.value=true
 const data: MachinePatch = { meta: Object.fromEntries(fields.filter(([key])=>!props.machine || meta[key] !== (props.machine.meta[key] || '')).map(([key])=>[key,meta[key].trim() || null])) }
 if (digest.value) data.secret_sha256=digest.value
 if (pubkey.value) data.tunnel_pubkey=pubkey.value
 try { if (props.machine) await api.patch(id.value,data); else await api.add({ worker_id:id.value, secret_sha256:digest.value, tunnel_pubkey:pubkey.value || undefined, meta:data.meta || {} }); emit('saved'); emit('update:open',false) } catch (e) { error.value=e instanceof Error ? e.message : 'Could not save machine' } finally { busy.value=false }
}
</script>
<template>
 <Dialog :open="open" @update:open="emit('update:open',$event)"><DialogContent class="max-h-[90dvh] w-[calc(100vw-2rem)] overflow-x-hidden overflow-y-auto sm:max-w-xl"><DialogHeader><DialogTitle>{{ machine ? 'Edit machine' : 'Add machine' }}</DialogTitle><DialogDescription>{{ machine ? 'Update machine details. Existing credentials are kept unless explicitly replaced.' : 'Register a Mac using its owner-approved bootstrap output.' }}</DialogDescription></DialogHeader>
  <form class="min-w-0 space-y-4 [&>*]:min-w-0" @submit.prevent="submit">
   <fieldset :disabled="busy" class="min-w-0 space-y-4">
   <section class="min-w-0 space-y-3 rounded-lg border p-3" aria-label="Setup registration prefill">
    <Label for="bootstrap">Paste setup output</Label><Textarea id="bootstrap" v-model="paste" placeholder="DIGEST mac-anna …&#10;PUBKEY mac-anna ssh-ed25519 …" class="min-w-0 resize-y break-all font-mono text-xs [field-sizing:fixed]" rows="4" autocomplete="off" spellcheck="false" :maxlength="16385" />
     <p class="text-xs muted">Paste a legacy nocturne Mac report or DIGEST / PUBKEY output, not commands. Parsed locally in memory; never executed or stored. Review before applying; saving remains a separate step.</p>
     <div class="flex flex-wrap gap-2"><Button type="button" variant="outline" @click="parseOutput">Parse setup output</Button><Button type="button" variant="ghost" @click="clearPaste">Clear pasted output</Button></div>
     <div v-if="preview" class="min-w-0 space-y-2 rounded border p-3" aria-label="Setup preview"><p class="break-all text-sm">Worker id: {{ preview.workerId || 'not in this report · choose manually' }}</p><p class="text-xs muted">SHA256 digest: {{ preview.digest ? 'valid · ready to prefill' : 'not supplied · existing field unchanged' }}</p><p class="text-xs muted">Tunnel public key: {{ preview.pubkey ? 'valid ed25519 · ready to prefill' : 'not supplied · existing field unchanged' }}</p><template v-if="preview.reportVersion"><p class="text-sm font-medium">Mac system report detected · v{{ preview.reportVersion }}</p><p v-for="[key,value] in Object.entries(preview.meta || {})" :key="key" class="break-all text-xs">{{ fields.find(([field])=>field===key)?.[1] || key }}: {{ value }}</p><p class="text-xs muted">This owner report contains machine facts, not worker registration credentials. Import these details now; choose Worker id manually. New registration still needs the SHA256 digest from a separately approved worker installation. A host fingerprint is not a tunnel public key. Do not rerun privileged installation just to import this report.</p></template><p class="text-xs muted">{{ preview.ignoredLines }} non-registration lines ignored. Only the reviewed fields above are imported.</p><Button type="button" @click="applyOutput">Apply to empty or matching fields</Button></div>
   </section>
   <div v-if="!machine" class="space-y-2"><Label for="worker-id">Worker id</Label><Input id="worker-id" v-model="id" required placeholder="mac-anna" /><p class="text-xs muted">Unique machine name. Cannot be changed after registration.</p></div>
   <div v-else class="space-y-1"><p class="text-xs muted">Machine identity (cannot be changed)</p><code class="select-all break-all text-sm">{{ id }}</code></div>
   <div class="space-y-2"><Label for="meta-owner">Owner</Label><Input id="meta-owner" v-model="meta.owner" placeholder="Who uses this Mac?" /></div>
   <details class="rounded-lg border p-3"><summary class="cursor-pointer text-sm font-medium">Advanced · optional machine details</summary>
    <p class="my-3 text-xs muted">Manual details for display and copyable SSH shortcuts only. Not required for a registered worker to connect. Hardware description is a manual label, not detected hardware.</p>
    <div class="grid gap-4 sm:grid-cols-2"><div v-for="[key,label] in fields.filter(([key])=>key !== 'owner')" :key="key" class="min-w-0 space-y-2"><Label :for="'meta-'+key">{{ label }}</Label><Input :id="'meta-'+key" v-model="meta[key]" :placeholder="key === 'admin_user' ? 'nocturne-admin' : ''" /></div></div>
   </details>
   <details class="rounded-lg border p-3" :open="!machine"><summary class="cursor-pointer text-sm font-medium">{{ machine ? 'Credentials · replace only if needed' : 'Credentials · required for registration' }}</summary>
    <p class="my-3 text-xs muted">{{ machine ? 'Leave blank to keep existing credentials. Replacing the digest disconnects the worker. Stored credentials are never displayed.' : 'A secret SHA256 digest is required. A tunnel public key is needed when using the SSH tunnel setup.' }}</p>
    <div class="space-y-4">
     <details><summary class="cursor-pointer text-sm">Setup helper · script and instructions</summary><div class="my-3 space-y-2 text-xs muted"><p>Owner preparation uses native macOS tools and needs no Python. Privileged worker installation is a separate, consented step and still requires Python. Use these files from the same reviewed release:</p><ul class="list-inside list-disc space-y-1"><li><a class="underline" href="https://github.com/lucasfth/nocturne/blob/owner-setup-v1/worker/nocturne-mac-setup.sh" target="_blank" rel="noopener noreferrer">Owner setup script</a></li><li><a class="underline" href="https://github.com/lucasfth/nocturne/blob/owner-setup-v1/worker/mac-setup.example.json" target="_blank" rel="noopener noreferrer">Public-key configuration example</a></li><li><a class="underline" href="https://github.com/lucasfth/nocturne/blob/owner-setup-v1/docs/mac-setup.md" target="_blank" rel="noopener noreferrer">Mac setup guide and installation prerequisites</a></li></ul><p>Owner prep does not create a hidden administrator or change sleep/update policy. Follow the guide for protected installer staging and explicit coordinator settings, including NOCTURNE_COORDINATOR_USER. The paste box above also accepts older nocturne Mac system reports to prefill their explicit owner and machine facts. The current owner preparation script emits no legacy report or registration credentials. Registration DIGEST / PUBKEY output comes only from a separate approved worker installation; gateway settings are not Mac connection details. Ordinary edits do not require setup.</p></div>
     </details>
     <div class="space-y-2"><Label for="digest">Secret SHA256 {{ machine ? '(optional · replace)' : '' }}</Label><Input id="digest" v-model="digest" :required="!machine" class="min-w-0 truncate font-mono text-xs" /></div>
     <div class="space-y-2"><Label for="pubkey">Tunnel public key (optional{{ machine ? ' · replace' : '' }})</Label><Input id="pubkey" v-model="pubkey" class="min-w-0 truncate font-mono text-xs" /></div>
    </div>
   </details>
   <p v-if="error" role="alert" class="text-sm text-destructive">{{ error }}</p>
   <div class="flex justify-end gap-2"><Button type="button" variant="outline" :disabled="busy" @click="emit('update:open',false)">Cancel</Button><Button type="submit" :disabled="busy">{{ busy ? 'Saving…' : machine ? 'Save changes' : 'Add machine' }}</Button></div>
   </fieldset>
  </form>
 </DialogContent></Dialog>
</template>
