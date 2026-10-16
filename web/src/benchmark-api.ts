/** Exact Go admin benchmark wire contract. No worker lease or executable inputs. */
declare const __MOCK__: boolean
export type State = 'queued' | 'running' | 'cancelling' | 'recovery_required' | 'completed' | 'failed' | 'cancelled'
export type Stage = 'pending' | 'preparing' | 'loading' | 'warming' | 'measuring' | 'validating' | 'restoring' | 'done'
export type Failure = '' | 'deadline' | 'restart' | 'execution' | 'quality' | 'model_mismatch' | 'restoration'
export interface Advanced { context: number; parallel: number; budget_seconds: number }
export type ApplyPhase = 'prepared' | 'applied' | 'rolled_back'
export interface Audit { actor?: string; code_revision?: string; config_generation?: string }
export interface Evidence { quality: 'pass' | 'fail'; context: number; parallel: number; prompt_tokens: number; output_tokens: number; output_allowance?: number; reserved_tokens?: number; duration_seconds: number; tokens_per_second: number; model_verified: boolean; model_sha256: string; build_digest: string }
export interface BenchmarkJob {
 id: string; machine: string; options: Advanced & { model_sha256: string; build_digest: string };
 provenance: Audit & { schema_version: number; policy_version: number; fixture_version: number; worker_id: string; created_at: string; deadline_at: string; model_sha256: string; build_digest: string };
 state: State; progress: Stage; sequence: number; failure: Failure; restored: boolean; summary?: Evidence; updated_at: string; finished_at?: string;
 apply_phase?: ApplyPhase; action?: 'apply'; result?: 'applied' | 'rolled_back'; apply_request?: { transaction_id: string; generation: number; audit: Required<Audit>; requested_at: string; deadline_at: string; completed_at: string; completed_sequence: number }
}
export const active = (j: BenchmarkJob) => ['queued','running','cancelling','recovery_required'].includes(j.state)
export class BenchmarkError extends Error {
 constructor(public status: number, public unsupported = false) { super('Benchmark request unavailable') }
}
const digest = (v: unknown) => typeof v === 'string' && /^[a-f0-9]{64}$/.test(v) && !/^0+$/.test(v)
const integer = (v: unknown, min: number, max: number) => typeof v === 'number' && Number.isSafeInteger(v) && v >= min && v <= max
const date = (v: unknown) => typeof v === 'string' && Number.isFinite(Date.parse(v))
/** Context is per slot, with output inside that budget. Never estimate missing values. */
export function contextAllocation(context: unknown, parallel: unknown): string | undefined {
 if (!integer(context,8192,131072) || (context as number) % 1024 !== 0 || !integer(parallel,1,8)) return undefined
 const total = (context as number) * (parallel as number)
 if (!Number.isSafeInteger(total)) return undefined
 return `${(context as number).toLocaleString('en-US')} tokens per slot (including output) · ${parallel} parallel slots · ${total.toLocaleString('en-US')} aggregate context tokens`
}
function validJob(j: BenchmarkJob, machine: string): boolean {
 if (!j || j.machine !== machine || !digest(j.id) ||
 !['queued','running','cancelling','recovery_required','completed','failed','cancelled'].includes(j.state) ||
 !['pending','preparing','loading','warming','measuring','validating','restoring','done'].includes(j.progress) ||
 !['', 'deadline','restart','execution','quality','model_mismatch','restoration'].includes(j.failure) ||
 (j.finished_at !== undefined && !date(j.finished_at)) ||
 !integer(j.sequence,0,10000) || typeof j.restored !== 'boolean' || !j.options || !j.provenance ||
 (j.apply_phase !== undefined && !['prepared','applied','rolled_back'].includes(j.apply_phase)) ||
 (j.action !== undefined && j.action !== 'apply') || (j.result !== undefined && !['applied','rolled_back'].includes(j.result))) return false
 const o = j.options, p = j.provenance
 if (!integer(o.context,8192,131072) || o.context % 1024 !== 0 || !integer(o.parallel,1,8) || !integer(o.budget_seconds,30,1800) || !digest(o.model_sha256) || !digest(o.build_digest) ||
 p.schema_version !== 1 || p.policy_version !== 1 || p.fixture_version !== 1 || p.worker_id !== machine || p.model_sha256 !== o.model_sha256 || p.build_digest !== o.build_digest || !date(p.created_at) || !date(p.deadline_at) || !date(j.updated_at) ||
 (p.actor !== undefined && !/^[a-z0-9][a-z0-9-]{0,31}$/.test(p.actor)) || (p.code_revision !== undefined && !/^(?:[a-f0-9]{40}|[a-f0-9]{64})$/.test(p.code_revision)) || (p.config_generation !== undefined && !digest(p.config_generation))) return false
 if (j.summary) {
  const e = j.summary
  if (e.quality !== 'pass' || e.model_verified !== true || e.context !== o.context || e.parallel !== o.parallel || e.model_sha256 !== o.model_sha256 || e.build_digest !== o.build_digest ||
  !integer(e.prompt_tokens,1,o.context) || !integer(e.output_tokens,1,o.context) || !Number.isFinite(e.duration_seconds) || e.duration_seconds <= 0 || e.duration_seconds > o.budget_seconds || !Number.isFinite(e.tokens_per_second) || e.tokens_per_second <= 0 || e.tokens_per_second > 1000000 || Math.abs(e.tokens_per_second - e.output_tokens/e.duration_seconds) > e.output_tokens/e.duration_seconds * 0.01) return false
  const allowance = e.output_allowance ?? 0, reserve = e.reserved_tokens ?? 0
  if (!integer(allowance,0,o.context) || !integer(reserve,0,104)) return false
  if (!allowance && !reserve) { if (e.prompt_tokens + e.output_tokens !== e.context) return false }
  else { const remaining = e.context - e.prompt_tokens - allowance; if (allowance < e.output_tokens || remaining < reserve || remaining > 104 || remaining-reserve > 96) return false }
 }
 if (j.apply_request) {
  const a = j.apply_request
  if (!digest(a.transaction_id) || !integer(a.generation,1,1) || !date(a.requested_at) || !date(a.deadline_at) || !date(a.completed_at) || !integer(a.completed_sequence,0,10000) || !a.audit || typeof a.audit.actor !== 'string' || !/^[a-z0-9][a-z0-9-]{0,31}$/.test(a.audit.actor) || typeof a.audit.code_revision !== 'string' || !/^(?:[a-f0-9]{40}|[a-f0-9]{64})$/.test(a.audit.code_revision) || !digest(a.audit.config_generation)) return false
 }
 if ((j.apply_request || j.apply_phase) && j.action !== 'apply') return false
 if (j.state === 'completed' && !j.summary || j.action === 'apply' && (!j.summary || !j.apply_request) || j.result && j.action !== 'apply') return false
 if (j.result === 'applied' && (j.state !== 'completed' || j.restored) || j.result === 'rolled_back' && (!j.restored || !['failed','cancelled'].includes(j.state))) return false
 return true
}
async function request(machine: string, suffix: string, signal: AbortSignal, body?: object): Promise<BenchmarkJob | BenchmarkJob[]> {
 if (__MOCK__) throw new BenchmarkError(501)
 const credential = sessionStorage.getItem('s') ?? ''
 const response = await fetch(`/admin/machines/${encodeURIComponent(machine)}/benchmarks${suffix}`, {
  method: body ? 'POST' : 'GET', signal, credentials: 'same-origin', headers: { Authorization: `Bearer ${credential}`, 'Content-Type': 'application/json' }, body: body ? JSON.stringify(body) : undefined,
 })
 if (response.status === 401 && credential === sessionStorage.getItem('s')) { sessionStorage.removeItem('s'); window.dispatchEvent(new Event('unauthorized')) }
 if (!response.ok) {
  // Only recognize this exact, fixed Go sentinel. Never render arbitrary server text.
  const error = await response.json().catch(() => ({}))
  throw new BenchmarkError(response.status, response.status === 409 && error.detail === 'unsupported worker: secure benchmark adapter unavailable')
 }
 const value = await response.json()
 if (body && Array.isArray(value)) throw new BenchmarkError(502)
 const jobs = Array.isArray(value) ? value : [value]
 if (jobs.length > 256 || !jobs.every(j => validJob(j, machine))) throw new BenchmarkError(502)
 return value
}
export const benchmarks = {
 list: async (m: string, s: AbortSignal) => { const v = await request(m,'',s); if (!Array.isArray(v)) throw new BenchmarkError(502); return v },
 start: (m: string, s: AbortSignal, advanced?: Advanced) => request(m,'',s,advanced ? { advanced } : {}) as Promise<BenchmarkJob>,
 change: async (m: string, id: string, action: 'cancel' | 'apply', s: AbortSignal) => { const value = await request(m,`/${encodeURIComponent(id)}/${action}`,s,{}) as BenchmarkJob; if (value.id !== id) throw new BenchmarkError(502); return value },
}
export function benchmarkFailure(error: unknown, operation = 'load') {
 if (!(error instanceof BenchmarkError)) return 'Connection interrupted. Reload history to reconcile the server state before retrying.'
 if (error.unsupported) return 'Benchmark unavailable: this server has no secure benchmark adapter for this Mac. No job was started. An operator must install and enable a trusted adapter.'
 if (error.status === 401) return 'Authentication expired. Sign in again.'
 if (error.status === 403) return 'Access denied. Use the authorized tailnet and same-origin admin connection.'
 if ([404,405,501].includes(error.status)) return 'Benchmark unavailable for this machine or server. The Python gateway and demo mode do not implement these Go routes; disabled or removed machines are unavailable.'
 if (error.status === 409) return operation === 'apply' ? 'Apply unavailable or refused: a trusted Apply binding, matching provenance and an unused, restored recommendation are required. Reload history; nothing is claimed applied.' : 'Operation refused: the machine may already have outstanding work or its adapter is unavailable. Reload history before retrying.'
 if (error.status === 429) return 'Benchmark capacity reached. Wait for other jobs to finish, then reload history.'
 if (error.status === 400) return 'Options rejected. Check the Advanced bounds and retry.'
 return 'Benchmark service unavailable or response unsupported. State is uncertain; reload history. Do not assume work stopped or settings changed.'
}
