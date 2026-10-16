<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { benchmarks, active, benchmarkFailure, contextAllocation, type BenchmarkJob, type Advanced } from '@/benchmark-api'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent, DialogHeader, DialogTitle, DialogDescription } from '@/components/ui/dialog'
import { AlertDialog, AlertDialogContent, AlertDialogHeader, AlertDialogTitle, AlertDialogDescription, AlertDialogFooter, AlertDialogCancel, AlertDialogAction } from '@/components/ui/alert-dialog'
const props = defineProps<{ machine: string; enabled: boolean }>()
const open = defineModel<boolean>('open', { default: false })
const jobs = ref<BenchmarkJob[]>([]), selected = ref(''), error = ref(''), pending = ref(false), loaded = ref(false), confirm = ref(false), confirmedId = ref('')
const advanced = ref(false), options = ref<Advanced>({ context: 32768, parallel: 2, budget_seconds: 600 })
const job = computed(() => jobs.value.find(j => j.id === selected.value) ?? jobs.value[0])
const outstanding = computed(() => jobs.value.find(active))
const allocation = computed(() => contextAllocation(job.value?.summary?.context, job.value?.summary?.parallel))
const validOptions = computed(() => !!contextAllocation(options.value.context, options.value.parallel) && options.value.context <= 32768 && options.value.parallel <= 2 && Number.isSafeInteger(options.value.budget_seconds) && options.value.budget_seconds >= 30 && options.value.budget_seconds <= 600)
const canApply = computed(() => !!job.value && job.value.state === 'completed' && job.value.restored && !!job.value.summary && !!allocation.value && !!job.value.provenance.actor && !!job.value.provenance.code_revision && !!job.value.provenance.config_generation && !job.value.action && !outstanding.value)
let generation = 0, controller: AbortController | undefined, timer: ReturnType<typeof setTimeout> | undefined, delay = 2000, failures = 0
function stop() { generation++; controller?.abort(); clearTimeout(timer); confirm.value = false }
function schedule() {
 clearTimeout(timer)
 if (open.value && props.enabled && outstanding.value && failures < 5) timer = setTimeout(() => void load(true), delay)
}
async function load(poll = false) {
 if (pending.value || !open.value || !props.enabled) return
 const epoch = generation, machine = props.machine
 controller = new AbortController(); pending.value = true
 if (!poll) { error.value = ''; failures = 0; delay = 2000 }
 try {
  const next = await benchmarks.list(machine, controller.signal)
  if (epoch !== generation) return
  jobs.value = next; if (!next.some(j => j.id === selected.value)) selected.value = next[0]?.id ?? ''; loaded.value = true; failures = 0; error.value = ''
  delay = Math.min(15000, Math.round(delay * 1.5))
 } catch (e) {
  if (epoch !== generation) return
  loaded.value = false; confirm.value = false; error.value = benchmarkFailure(e); failures++; delay = Math.min(30000, delay * 2)
 } finally { if (epoch === generation) { pending.value = false; schedule() } }
}
async function mutate(action: 'start' | 'cancel' | 'apply') {
 if (pending.value || !props.enabled || !open.value || (action !== 'cancel' && !loaded.value)) return
 if (action === 'cancel' && (!job.value || !active(job.value))) return
 if (action === 'start' && advanced.value && !validOptions.value) return
 if (action === 'start' && outstanding.value || action === 'apply' && (!canApply.value || confirmedId.value !== job.value?.id)) return
 const epoch = generation, machine = props.machine, id = job.value?.id
 clearTimeout(timer); controller = new AbortController(); pending.value = true; error.value = ''
 try {
  const next = action === 'start' ? await benchmarks.start(machine, controller.signal, advanced.value ? options.value : undefined) : await benchmarks.change(machine, id!, action, controller.signal)
  if (epoch !== generation) return
  jobs.value = [next, ...jobs.value.filter(j => j.id !== next.id)]; selected.value = next.id; delay = 2000; failures = 0
 } catch (e) { if (epoch === generation) { error.value = benchmarkFailure(e, action); loaded.value = false } }
 finally { if (epoch === generation) { pending.value = false; schedule() } }
}
watch(() => [open.value, props.machine, props.enabled], () => {
 stop(); jobs.value = []; selected.value = ''; error.value = ''; loaded.value = false; pending.value = false; advanced.value = false; failures = 0; delay = 2000
 options.value = { context: 32768, parallel: 2, budget_seconds: 600 }
 if (open.value && props.enabled) void load()
}, { immediate: true })
watch(selected, () => { confirm.value = false })
onBeforeUnmount(stop)
const reason: Record<string,string> = { deadline: 'Deadline exceeded', restart: 'Coordinator restarted; restoration required', execution: 'Execution rejected or failed', quality: 'Quality checks rejected the candidate', model_mismatch: 'Model or build verification rejected the candidate', restoration: 'Restoration could not be verified' }
const label = (s: string) => s.replaceAll('_', ' ')
</script>
<template>
 <Dialog v-model:open="open"><DialogContent class="max-h-[90dvh] overflow-y-auto sm:max-w-2xl">
  <DialogHeader><DialogTitle>Benchmark &amp; optimise</DialogTitle><DialogDescription>{{ machine }} · Automatic checks, then a recommendation. Apply is always your separate decision.</DialogDescription></DialogHeader>
  <div class="min-w-0 space-y-5">
   <div class="rounded-lg border p-3 text-sm"><p class="font-medium">Safety first</p><p>New inference is paused while work is outstanding. Existing work must drain; the worker checks fresh AC power, battery ≥50%, nominal thermals and no active human before execution. Cancellation waits for verified restoration.</p><p class="mt-2 muted">Requires a trusted Go worker adapter. Registration alone does not install one. No model downloads or automatic Apply.</p></div>
   <p v-if="!enabled" role="status">This machine is disabled or no longer available. Enable it before opening benchmark history.</p>
   <p v-if="error" role="alert" class="text-sm text-destructive">{{ error }}</p>
   <p v-if="failures >= 5" role="status" class="text-sm">Automatic refresh paused after repeated failures. Reload history to resume. Outstanding work may still be running.</p>
   <div class="flex flex-wrap gap-2"><Button variant="outline" :disabled="pending || !enabled" @click="load()">Reload history</Button><span v-if="pending" role="status" class="text-sm muted">Checking server…</span></div>
   <form class="space-y-3" @submit.prevent="mutate('start')">
    <details class="rounded-lg border p-3"><summary class="cursor-pointer text-sm font-medium">Advanced / Nerd</summary>
     <label class="my-3 flex items-center gap-2 text-sm"><input v-model="advanced" type="checkbox" :disabled="pending || !!outstanding"/>Override automatic requirements</label>
     <fieldset v-if="advanced" :disabled="pending || !!outstanding" class="grid gap-3 sm:grid-cols-3">
      <label class="text-sm">Context per slot (including output)<input v-model.number="options.context" aria-label="Context per slot (including output)" type="number" min="8192" max="32768" step="1024" required class="mt-1 w-full rounded-md border bg-transparent p-2"/></label>
      <label class="text-sm">Parallel slots<input v-model.number="options.parallel" aria-label="Parallel slots" type="number" min="1" max="2" step="1" required class="mt-1 w-full rounded-md border bg-transparent p-2"/></label>
      <label class="text-sm">Budget (seconds)<input v-model.number="options.budget_seconds" aria-label="Budget (seconds)" type="number" min="30" max="600" step="1" required class="mt-1 w-full rounded-md border bg-transparent p-2"/></label>
     </fieldset><p v-if="advanced" class="mt-2 text-xs muted">{{ contextAllocation(options.context, options.parallel) ?? 'Invalid context allocation. Enter supported whole-number values.' }}</p><p class="mt-2 text-xs muted">Automatic: {{ contextAllocation(32768, 2) }} · 600 seconds. Overrides cannot select paths, models, commands or bypass safety.</p>
    </details>
    <Button type="submit" :disabled="pending || !enabled || !loaded || !!outstanding || (advanced && !validOptions)">Run benchmark</Button>
   </form>
   <p v-if="loaded && !jobs.length" class="text-sm muted">No retained jobs. Run benchmark to request an automatic recommendation. Server admission determines availability.</p>
   <template v-if="job">
    <div class="space-y-2"><label for="benchmark-history" class="text-sm font-medium">Job history (retained by server)</label><select id="benchmark-history" v-model="selected" :disabled="pending" class="w-full min-w-0 rounded-md border bg-background p-2"><option v-for="j in jobs" :key="j.id" :value="j.id">{{ j.provenance.created_at }} · {{ j.action === 'apply' ? 'Apply' : 'Benchmark' }} · {{ label(j.state) }}</option></select></div>
    <section aria-label="Job status" class="space-y-3 rounded-lg border p-3 text-sm">
     <p>Job updated: {{ job.updated_at }} · job sequence: {{ job.sequence }}</p>
     <p role="status" aria-live="polite" class="font-medium">{{ job.action === 'apply' ? 'Apply' : 'Benchmark' }}: {{ label(job.state) }} · {{ label(job.progress) }}</p>
     <p v-if="active(job)">Stage reported by worker, not a percentage or time estimate. Scheduling remains fenced until the server acknowledges a safe terminal outcome.</p>
     <p v-if="job.state === 'recovery_required'" role="alert" class="font-medium text-destructive">Recovery required. Original service state is not verified. Keep this Mac out of inference and ask an operator to reconcile it. This page cannot force-unfence or retry execution.</p>
     <p v-if="job.failure">{{ reason[job.failure] }}</p>
     <p v-if="job.result === 'applied'">Worker reports candidate applied and service health verified. This is a worker attestation, not an independent browser health check.</p>
     <p v-else-if="job.result === 'rolled_back'">Health/change did not complete: worker verified rollback to the prior configuration and service health.</p>
     <p v-else-if="job.restored">Worker verified prior configuration and service restoration. Benchmark completion does not apply settings.</p>
     <p v-else-if="!active(job)">No restoration acknowledgment. An undispatched cancellation or expiry does not claim execution or rollback.</p>
     <Button v-if="active(job)" variant="destructive" :disabled="pending || job.state === 'cancelling'" @click="mutate('cancel')">{{ job.state === 'cancelling' ? 'Cancellation requested' : 'Cancel job' }}</Button>
    </section>
    <section v-if="job.summary" aria-label="Recommendation" class="space-y-3 rounded-lg border p-3 text-sm">
     <h3 class="font-semibold">Recommendation</h3><p>{{ allocation ?? 'Invalid context allocation' }} · quality {{ job.summary.quality }}</p>
     <p>One long, non-warmup measured row reported by the authenticated worker. Not aggregate throughput or independently verified performance.</p>
     <dl class="grid grid-cols-2 gap-2"><dt>Decode throughput</dt><dd>{{ job.summary.tokens_per_second }} tok/s</dd><dt>Decode duration</dt><dd>{{ job.summary.duration_seconds }} s</dd><dt>Actual prompt tokens</dt><dd>{{ job.summary.prompt_tokens }}</dd><dt>Actual output tokens</dt><dd>{{ job.summary.output_tokens }}</dd><dt>Output allowance</dt><dd>{{ job.summary.output_allowance ?? 'Not recorded' }}</dd><dt>Reserved tokens</dt><dd>{{ job.summary.reserved_tokens ?? 'Not recorded' }}</dd><dt>Model verified by worker</dt><dd>{{ job.summary.model_verified ? 'Yes' : 'No' }}</dd></dl>
     <Button v-if="!job.action" :disabled="pending || !loaded || !canApply" @click="confirmedId = job.id; confirm = true">Apply recommendation</Button><p v-if="!job.action" class="text-xs muted">Requires server-enabled Apply and matching model, build, code and configuration. The server may refuse unsupported or stale evidence.</p>
    </section>
    <details class="rounded-lg border p-3 text-sm"><summary class="cursor-pointer font-medium">Evidence &amp; provenance</summary><section aria-label="Benchmark provenance"><h3 class="mt-3 font-semibold">Benchmark provenance</h3><dl class="mt-3 space-y-2 break-all"><template v-for="(value, key) in { job: job.id, worker_id: job.provenance.worker_id, schema_version: job.provenance.schema_version, policy_version: job.provenance.policy_version, fixture_version: job.provenance.fixture_version, model_sha256: job.provenance.model_sha256, build_digest: job.provenance.build_digest, created_at: job.provenance.created_at, deadline_at: job.provenance.deadline_at, benchmark_completed_at: job.apply_request?.completed_at ?? job.finished_at ?? 'Not recorded', benchmark_completed_sequence: job.apply_request?.completed_sequence ?? (job.state === 'completed' ? job.sequence : 'Not recorded'), actor: job.provenance.actor ?? 'Unknown (legacy)', code_revision: job.provenance.code_revision ?? 'Unknown (legacy)', config_generation: job.provenance.config_generation ?? 'Unknown (legacy)' }" :key="key"><dt class="muted">{{ label(key) }}</dt><dd>{{ value }}</dd></template></dl></section><section v-if="job.apply_request" aria-label="Apply provenance"><h3 class="mt-3 font-semibold">Apply provenance</h3><dl class="mt-3 space-y-2 break-all"><template v-for="(value, key) in { actor: job.apply_request.audit.actor, code_revision: job.apply_request.audit.code_revision, config_generation: job.apply_request.audit.config_generation, transaction_id: job.apply_request.transaction_id, generation: job.apply_request.generation, apply_phase: job.apply_phase ? label(job.apply_phase) : 'Not yet recorded', requested_at: job.apply_request.requested_at, deadline_at: job.apply_request.deadline_at }" :key="key"><dt class="muted">{{ label(key) }}</dt><dd>{{ value }}</dd></template></dl></section><p class="mt-3 muted">The API exposes one summary and fixed rejection codes, not a per-candidate trial log. Missing measurements are not estimated.</p></details>
   </template>
  </div>
 </DialogContent></Dialog>
 <AlertDialog v-model:open="confirm"><AlertDialogContent><AlertDialogHeader><AlertDialogTitle>Apply recommendation to {{ machine }}?</AlertDialogTitle><AlertDialogDescription><span class="mb-2 block font-medium">{{ allocation ?? 'Invalid context allocation; Apply unavailable.' }}</span>This changes the worker configuration, pauses new inference and checks service health. Failed health must roll back; unverified rollback requires operator recovery and stays fenced. Only the pinned recommendation is submitted. No settings change until the server accepts this explicit request.</AlertDialogDescription></AlertDialogHeader><AlertDialogFooter><AlertDialogCancel>Keep current settings</AlertDialogCancel><AlertDialogAction :disabled="pending || !canApply" @click="mutate('apply')">Confirm apply</AlertDialogAction></AlertDialogFooter></AlertDialogContent></AlertDialog>
</template>
