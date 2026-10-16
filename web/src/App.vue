<script setup lang="ts">
import { computed, ref, onMounted, onUnmounted } from 'vue'
import { Moon, Sun, Plus, LogOut, Laptop, LoaderCircle } from '@lucide/vue'
import { toast } from 'vue-sonner'
import { api, type Summary, type Machine } from '@/api'
import { nf } from '@/format'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Card, CardContent } from '@/components/ui/card'
import { Tabs, TabsList, TabsTrigger, TabsContent } from '@/components/ui/tabs'
import { TooltipProvider } from '@/components/ui/tooltip'
import { Toaster } from '@/components/ui/sonner'
import BrandMark from '@/components/BrandMark.vue'
import SettingsPanel from '@/components/SettingsPanel.vue'
import Overview from '@/components/Overview.vue'
import MachineCard from '@/components/MachineCard.vue'
import MachineForm from '@/components/MachineForm.vue'
import UsageTables from '@/components/UsageTables.vue'
const data = ref<Summary | null>(null), machines = ref<Machine[]>([]), authenticated = ref(!!sessionStorage.getItem('s'))
const secret = ref(''), error = ref(''), loading = ref(false), updated = ref('')
const pending = ref<string[]>([])
const machinesLoaded = ref(false), machinesFailed = ref(false), refreshingView = ref(false)
const machineNames = new Intl.Collator('en', { sensitivity: 'base', numeric: true })
const fleet = computed(() => machines.value.map(machine => {
 const worker = data.value?.workers.find(worker => worker.worker_id === machine.worker_id)
 return { worker_id: machine.worker_id, online: false, eligible: false, heartbeat_age_s: null, battery_pct: null, on_ac: null, thermal_state: null, user_active: null, tokens_per_sec: null, busy: 0, capacity: null, metrics: {}, ...worker, enabled: machine.enabled, mac: machine.meta }
}).sort((a, b) => Number(b.online) - Number(a.online)
 || machineNames.compare(a.worker_id, b.worker_id)
 || (a.worker_id < b.worker_id ? -1 : a.worker_id > b.worker_id ? 1 : 0)))
const formOpen = ref(false), selected = ref<Machine | null>(null)
const media = window.matchMedia('(prefers-color-scheme: dark)')
const dark = ref(localStorage.getItem('theme') ? localStorage.getItem('theme') === 'dark' : media.matches)
function applyTheme() { document.documentElement.classList.toggle('dark',dark.value); document.documentElement.classList.toggle('light',!dark.value) }
function toggleTheme() { dark.value=!dark.value; localStorage.setItem('theme',dark.value ? 'dark' : 'light'); applyTheme() }
function systemTheme() { if (!localStorage.getItem('theme')) { dark.value=media.matches; applyTheme() } }
applyTheme()
function logout() { sessionStorage.removeItem('s'); authenticated.value=false; data.value=null; machines.value=[]; secret.value=''; formOpen.value=false; machinesLoaded.value=false; machinesFailed.value=false; updated.value='' }
function unauthorized() { logout(); error.value='Wrong secret' }
let refreshing: Promise<void> | null = null
async function refresh() {
 if (!authenticated.value) return
 // A mutation must fetch again after any in-flight polling request finishes.
 if (refreshing) await refreshing
 const token=sessionStorage.getItem('s')
 refreshingView.value=true
 refreshing=(async () => {
  const failures: string[]=[]
  const current=()=>authenticated.value && token === sessionStorage.getItem('s')
  const loadSummary=api.summary().then(summary=>{ if (current()) data.value=summary })
   .catch(reason=>{ if (current()) failures.push(`Overview: ${reason instanceof Error ? reason.message : 'unavailable'}`) })
  const loadMachines=api.machines().then(list=>{ if (current()) { machines.value=list; machinesLoaded.value=true; machinesFailed.value=false } })
   .catch(reason=>{ if (current()) { machinesFailed.value=true; failures.push(`Machines: ${reason instanceof Error ? reason.message : 'unavailable'}`) } })
  await Promise.all([loadSummary,loadMachines])
  if (current()) { if (!failures.length) updated.value=new Date().toLocaleTimeString('da-DK'); error.value=failures.join(' · ') }
 })()
 try { await refreshing } finally { refreshing=null; refreshingView.value=false }
}
async function login() { if (!secret.value) return; loading.value=true; sessionStorage.setItem('s',secret.value); authenticated.value=true; await refresh(); loading.value=false; secret.value='' }
function edit(id: string) { selected.value=machines.value.find(m=>m.worker_id === id) || null; if (!selected.value) { toast.error('Machine details unavailable; refresh and retry'); return } formOpen.value=true }
async function mutate(id: string, action: () => Promise<unknown>, message: string) {
 if (pending.value.includes(id)) return
 pending.value.push(id)
 try { await action(); await refresh(); toast.success(message) }
 catch (e) { toast.error(e instanceof Error ? e.message : 'Update failed') }
 finally { pending.value=pending.value.filter(value => value !== id) }
}
async function toggle(id: string, enabled: boolean) { await mutate(id,()=>api.patch(id,{ enabled }),enabled ? 'Machine enabled' : 'Machine disabled') }
async function remove(id: string) { await mutate(id,()=>api.remove(id),'Machine removed') }
let timer: ReturnType<typeof setInterval>
onMounted(()=>{ window.addEventListener('unauthorized',unauthorized); media.addEventListener('change',systemTheme); void refresh(); timer=setInterval(()=>void refresh(),15000) })
onUnmounted(()=>{ clearInterval(timer); window.removeEventListener('unauthorized',unauthorized); media.removeEventListener('change',systemTheme) })
</script>
<template>
 <TooltipProvider><Toaster :theme="dark ? 'dark' : 'light'" />
 <a href="#fleet-content" class="sr-only focus:not-sr-only focus:fixed focus:top-2 focus:left-2 focus:z-50 rounded-md bg-background p-3 ring-2 ring-ring">Skip to content</a>
 <main id="fleet-content" tabindex="-1" class="mx-auto max-w-[1440px] px-4 py-6 sm:px-8 sm:py-10">
  <header class="mb-9 flex flex-wrap items-center gap-3"><div class="brand-tile grid size-11 place-items-center rounded-xl"><BrandMark class="brand-mark size-8" /></div><div class="min-w-0 flex-1"><h1 class="text-xl font-semibold tracking-tight">nocturne<span class="ml-2 text-xs font-normal muted">ADMIN</span></h1><p class="text-xs muted">Self-hosted inference gateway<span v-if="updated && authenticated"> · updated {{ updated }}</span></p></div><Button variant="outline" size="icon" aria-label="Toggle dark mode" @click="toggleTheme"><Sun v-if="dark" class="size-4"/><Moon v-else class="size-4"/></Button><Button v-if="authenticated" variant="outline" @click="logout(); error='' "><LogOut class="size-4"/>Log out</Button></header>
  <Card v-if="!authenticated" class="mx-auto mt-[12vh] max-w-sm"><CardContent><h2 class="text-xl font-semibold">nocturne admin</h2><p class="my-2 text-sm muted">Your fleet, at a glance.</p><form class="mt-6 space-y-4" @submit.prevent="login"><Label for="secret">Admin secret</Label><Input id="secret" v-model="secret" type="password" placeholder="Admin secret" autocomplete="current-password" required autofocus/><Button class="w-full" :disabled="loading">{{ loading ? 'Opening…' : 'Open' }}</Button><p v-if="error" role="alert" class="text-sm text-destructive">{{ error }}</p><p class="text-xs muted">Stored only in this browser tab.</p></form></CardContent></Card>
  <template v-else>
   <p v-if="error" role="alert" class="mb-4 rounded-lg border border-destructive p-3 text-sm text-destructive">{{ error }} <Button variant="outline" size="sm" :disabled="refreshingView" @click="refresh">{{ refreshingView ? 'Retrying…' : 'Retry' }}</Button></p>
   <template v-if="data">
    <div class="mb-5 flex flex-wrap items-end justify-between gap-3"><div><p class="eyebrow mb-2">Fleet</p><h2 class="text-2xl font-semibold tracking-tight">Overview</h2></div><Badge variant="outline" class="px-3 py-1.5"><span class="mr-1 size-2 rounded-full" :class="data.energy.window_open && data.energy.serving_workers > 0 ? 'bg-emerald-500' : 'bg-amber-500'"/>{{ data.energy.window_open && data.energy.serving_workers > 0 ? 'Serving' : 'Closed' }} · {{ data.energy.serving_workers }}/{{ data.workers.length }} Macs</Badge></div>
    <Overview :data="data"/>
   </template>
    <Tabs default-value="machines" class="mt-9"><div class="mb-4 flex flex-wrap items-center justify-between gap-3"><TabsList aria-label="Admin sections" class="h-auto w-full flex-wrap justify-start sm:w-auto"><TabsTrigger value="machines">Machines <span class="ml-1 muted">{{ machines.length }}</span></TabsTrigger><TabsTrigger value="customers" :disabled="!data">Customers</TabsTrigger><TabsTrigger value="daily" :disabled="!data">Daily</TabsTrigger><TabsTrigger value="settings">Settings</TabsTrigger></TabsList><Button @click="selected=null; formOpen=true"><Plus class="size-4"/>Add machine</Button></div>
     <TabsContent value="machines"><div class="grid gap-5 md:grid-cols-2 xl:grid-cols-3"><MachineCard v-for="worker in fleet" :key="worker.worker_id" :worker="worker" :busy="pending.includes(worker.worker_id)" @edit="edit" @toggle="toggle" @remove="remove"/></div><div v-if="!machinesLoaded && !machinesFailed" role="status" class="flex items-center justify-center gap-3 rounded-xl border border-dashed p-12 text-sm muted"><LoaderCircle class="size-5 animate-spin"/>Loading machines…</div><div v-else-if="machinesFailed" role="status" class="mt-4 rounded-xl border border-dashed p-6 text-sm muted">{{ machinesLoaded ? 'Showing the last loaded registry. Machine information may be out of date.' : 'Machine registry unavailable. Retry above to load your fleet.' }}</div><Card v-else-if="!machines.length" class="border border-dashed bg-muted/20 py-10 text-center shadow-none"><CardContent class="flex flex-col items-center gap-3"><div class="rounded-full bg-muted p-3"><Laptop class="size-6 muted"/></div><h3 class="text-base font-semibold">No machines yet</h3><p class="max-w-sm text-sm muted">Add your first Mac to get started. Register its bootstrap output, then check its connection here.</p><Button variant="outline" @click="selected=null; formOpen=true"><Plus class="size-4"/>Register your first Mac</Button></CardContent></Card></TabsContent>
     <TabsContent value="settings"><SettingsPanel @saved="refresh"/></TabsContent>
     <TabsContent value="customers"><UsageTables v-if="data" :data="data" kind="customers"/></TabsContent><TabsContent value="daily"><h3 class="mb-3 text-sm muted">Daily (30d)</h3><UsageTables v-if="data" :data="data" kind="daily"/></TabsContent>
    </Tabs>
    <footer v-if="data" class="mt-8 border-t pt-5 text-xs leading-6 muted">Prices: {{ nf(data.pricing.input_approx_dkk_per_million_tokens,2) }} kr in / {{ nf(data.pricing.output_approx_dkk_per_million_tokens,2) }} kr out per 1M tokens · BTC {{ nf(data.dkk_per_btc) }} kr ({{ data.fx?.source }}) · queue {{ data.queue.pending }} waiting, {{ data.queue.active }} running.</footer>

  </template>
  <MachineForm v-model:open="formOpen" :machine="selected" @saved="refresh"/>
 </main></TooltipProvider>
</template>
