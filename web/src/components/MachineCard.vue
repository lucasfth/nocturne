<script setup lang="ts">
import { computed, ref } from 'vue'
import type { Worker } from '@/api'
import { nf } from '@/format'
import { Copy, Ellipsis, Laptop } from '@lucide/vue'
import { toast } from 'vue-sonner'
import { Card, CardContent } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { DropdownMenu, DropdownMenuTrigger, DropdownMenuContent, DropdownMenuItem } from '@/components/ui/dropdown-menu'
import { AlertDialog, AlertDialogContent, AlertDialogHeader, AlertDialogTitle, AlertDialogDescription, AlertDialogFooter, AlertDialogCancel, AlertDialogAction } from '@/components/ui/alert-dialog'
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip'
import Meter from './Meter.vue'
import BenchmarkPanel from './BenchmarkPanel.vue'
const props = defineProps<{ worker: Worker; busy?: boolean }>()
const emit = defineEmits<{ edit: [id: string]; toggle: [id: string, enabled: boolean]; remove: [id: string] }>()
const confirm = ref(false), benchmarkOpen = ref(false)
const status = computed(() => { const w = props.worker; return !w.enabled ? 'disabled' : !w.online ? 'offline' : w.eligible ? w.busy ? 'working' : 'ready' : w.user_active ? 'in use' : 'paused' })
const commands = computed(() => { const m = props.worker.mac, admin = m.admin_user || 'nocturne-admin'; return [ ['admin', m.host ? `ssh ${admin}@${m.host}` : ''], ['user', m.host && m.user ? `ssh ${m.user}@${m.host}` : ''], ['remote', m.jump && (m.ip || m.host) ? `ssh -J ${m.jump} ${admin}@${m.ip || m.host}` : ''] ].filter(row => row[1]) })
async function copy(text: string) { try { await navigator.clipboard.writeText(text); toast.success('Copied') } catch { toast.error('Could not copy to clipboard') } }
</script>
<template>
 <Card class="min-w-0"><CardContent class="space-y-5">
  <div class="flex items-center gap-2"><Laptop class="size-4 text-violet-500"/><h3 class="min-w-0 flex-1 truncate font-semibold">{{ worker.worker_id }}</h3><Badge :variant="status === 'offline' ? 'secondary' : 'outline'" :class="{ 'text-emerald-600': status === 'ready' || status === 'working', 'text-amber-600': status === 'in use' }">{{ status }}</Badge>
   <DropdownMenu><DropdownMenuTrigger as-child><Button :disabled="busy" variant="ghost" size="icon" :aria-label="`Actions for ${worker.worker_id}`"><Ellipsis class="size-4"/></Button></DropdownMenuTrigger><DropdownMenuContent align="end"><DropdownMenuItem @select="emit('edit',worker.worker_id)">Edit</DropdownMenuItem><DropdownMenuItem @select="emit('toggle',worker.worker_id,!worker.enabled)">{{ worker.enabled ? 'Disable' : 'Enable' }}</DropdownMenuItem><DropdownMenuItem class="text-destructive" @select="confirm=true">Remove</DropdownMenuItem></DropdownMenuContent></DropdownMenu>
  </div>
  <div class="flex flex-wrap gap-2" aria-label="Machine management">
   <Button variant="outline" size="sm" :disabled="busy" @click="emit('edit',worker.worker_id)">Edit machine</Button>
   <Button variant="outline" size="sm" :disabled="busy" @click="emit('toggle',worker.worker_id,!worker.enabled)">{{ worker.enabled ? 'Disable' : 'Enable' }}</Button>
   <Button variant="destructive" size="sm" :disabled="busy" @click="confirm=true">Remove</Button>
   <span v-if="busy" role="status" class="text-xs muted">Updating…</span>
  </div>
  <Button variant="outline" class="w-full" :disabled="busy || !worker.enabled" @click="benchmarkOpen = true">Benchmark &amp; optimise</Button>
  <div class="space-y-3"><Meter label="Battery" :value="worker.battery_pct" battery/><div class="flex justify-between text-xs muted"><span>{{ worker.on_ac === null ? 'Power unknown' : worker.on_ac ? '🔌 charging' : '🔋 battery' }}</span><span>{{ worker.thermal_state || '—' }}</span></div><div class="flex justify-between text-xs muted"><span>{{ worker.tokens_per_sec == null ? '—' : nf(worker.tokens_per_sec,1) }} tok/s</span><span>{{ worker.busy }}/{{ worker.capacity ?? '—' }} slots · {{ worker.heartbeat_age_s ?? '—' }}s</span></div></div>
  <div class="space-y-3"><Meter label="Memory" :value="worker.metrics.mem_pressure_pct" :suffix="worker.metrics.vm_pressure_level === 4 ? ' critical' : worker.metrics.vm_pressure_level === 2 ? ' warn' : ''"/><Meter label="CPU" :value="worker.metrics.cpu_pct"/><Meter label="GPU" :value="worker.metrics.gpu_pct"/><div class="flex justify-between text-xs muted"><span>{{ worker.metrics.mem_total_gb == null ? '—' : nf(worker.metrics.mem_total_gb) }} GB RAM</span><span>swap {{ worker.metrics.swap_used_mb == null ? '—' : nf(worker.metrics.swap_used_mb/1024,1) }} GB</span></div></div>
  <div class="space-y-2 border-t pt-4"><p class="text-xs muted leading-relaxed">{{ [worker.mac.owner, worker.mac.agent ? 'agent '+worker.mac.agent : '',worker.mac.model].filter(Boolean).join(' · ') || 'No machine details' }}</p>
   <div v-for="row in commands" :key="row[0]" class="flex items-center gap-2"><span class="w-10 shrink-0 text-xs muted">{{ row[0] }}</span><code class="min-w-0 flex-1 overflow-x-auto whitespace-nowrap rounded-md bg-muted p-2 text-[10px]">{{ row[1] }}</code><Tooltip><TooltipTrigger as-child><Button variant="ghost" size="icon" :aria-label="`Copy ${row[0]} SSH command`" @click="copy(row[1]!)"><Copy class="size-3.5"/></Button></TooltipTrigger><TooltipContent>Copy command</TooltipContent></Tooltip></div>
   <p v-if="worker.mac.ip" class="text-[11px] muted">LAN {{ worker.mac.ip }} (DHCP, may change)</p>
  </div>
 </CardContent></Card>
 <BenchmarkPanel v-model:open="benchmarkOpen" :machine="worker.worker_id" :enabled="worker.enabled"/>
 <AlertDialog v-model:open="confirm"><AlertDialogContent><AlertDialogHeader><AlertDialogTitle>Remove {{ worker.worker_id }}?</AlertDialogTitle><AlertDialogDescription>Removes auth and tunnel key; the Mac can no longer connect. This cannot be undone.</AlertDialogDescription></AlertDialogHeader><AlertDialogFooter><AlertDialogCancel>Cancel</AlertDialogCancel><AlertDialogAction @click="emit('remove',worker.worker_id)">Remove machine</AlertDialogAction></AlertDialogFooter></AlertDialogContent></AlertDialog>
</template>
