declare const __MOCK__: boolean
export interface Meta { owner?: string | null; agent?: string | null; model?: string | null; user?: string | null; host?: string | null; ip?: string | null; jump?: string | null; admin_user?: string | null }
export interface Metrics { mem_pressure_pct?: number | null; vm_pressure_level?: number | null; mem_total_gb?: number | null; swap_used_mb?: number | null; load1?: number | null; cpu_count?: number | null; cpu_pct?: number | null; gpu_pct?: number | null }
export interface Worker { worker_id: string; online: boolean; enabled: boolean; eligible: boolean; heartbeat_age_s: number | null; battery_pct: number | null; on_ac: boolean | null; thermal_state: string | null; user_active: boolean | null; tokens_per_sec: number | null; busy: number; capacity: number | null; metrics: Metrics; mac: Meta }
export interface Machine { worker_id: string; enabled: boolean; added: number; meta: Meta; tunnel_key_fingerprint: string | null }
export interface MachineInput { worker_id: string; secret_sha256: string; tunnel_pubkey?: string; meta: Meta }
export type MachinePatch = Partial<Omit<MachineInput, 'worker_id'>> & { enabled?: boolean }
export interface Summary {
 service: string; dkk_per_btc: number; fx?: { dkk_per_btc: number; source: string; age_s: number };
 pricing: { input_msat_per_1k_tokens: number; output_msat_per_1k_tokens: number; min_charge_msat_per_request: number; input_approx_dkk_per_million_tokens: number; output_approx_dkk_per_million_tokens: number; dkk_per_btc: number; thinking: string };
 watts_per_worker: number; wallet: null | { error?: string; balance_sat?: number; fee_credit_sat?: number; channels?: number; inbound_liquidity_sat?: number; node_id?: string };
 energy: { window_open: boolean; next_window_start: string | null; price_area: string; dkk_per_kwh: number | null; tier: string; eligible_workers: number; serving_workers: number };
 workers: Worker[]; queue: { pending: number; active: number };
 days: { day: string; requests: number; test_requests: number; revenue_msat: number; test_msat: number; prompt_tokens: number; completion_tokens: number }[];
 keys: { key_id: string; balance_msat: number; created: number; spent_msat: number; free_credit_msat: number; requests: number; last_used: number | null; internal: boolean; paid_in_msat: number; earned_msat: number }[];
 invoices: { paid_msat: number; paid: number; pending: number }; earned_msat: number; trial_users: number; test_usage_msat: number; customers: number; customer_credit_liability_msat: number;
}
export class ApiError extends Error { constructor(message: string, public status: number) { super(message) } }
async function request<T>(path: string, method = 'GET', body?: MachineInput | MachinePatch | ConfigPatch): Promise<T> {
 const headers = { Authorization: `Bearer ${sessionStorage.getItem('s') ?? ''}`, 'Content-Type': 'application/json' }
 if (__MOCK__ && path === 'config') throw new ApiError('Settings unavailable in mock mode', 501)
 if (__MOCK__) { const { mockRequest } = await import('./mock'); return await mockRequest(path, method, body as MachineInput | MachinePatch) as T }
 const response = await fetch(`/admin/${path}`, { method, headers, body: body ? JSON.stringify(body) : undefined })
 if (response.status === 401) { sessionStorage.removeItem('s'); window.dispatchEvent(new Event('unauthorized')) }
 if (!response.ok) { const error: { detail?: string } = await response.json().catch(() => ({})); throw new ApiError(error.detail || `Error ${response.status}`, response.status) }
 return response.json() as Promise<T>
}
export const api = { config: () => request<ConfigView>('config'), saveConfig: (patch: ConfigPatch) => request<ConfigView>('config', 'PATCH', patch), summary: () => request<Summary>('summary'), machines: () => request<Machine[]>('machines'), add: (data: MachineInput) => request<Machine>('machines', 'POST', data), patch: (id: string, data: MachinePatch) => request<Machine>(`machines/${encodeURIComponent(id)}`, 'PATCH', data), remove: (id: string) => request<{ deleted: string }>(`machines/${encodeURIComponent(id)}`, 'DELETE') }

export type SettingKey = 'service_name' | 'public_limit_concurrency'
export interface ConfigField { type: 'string' | 'integer'; apply: 'live' | 'restart'; minimum: number | null; maximum: number | null; default: string | number }
export interface ConfigValues { service_name: string; public_limit_concurrency: number }
export interface ConfigView { schema_version: number; runtime: string; revision: string; fields: Record<SettingKey, ConfigField>; values: ConfigValues; effective: ConfigValues; restart_required: SettingKey[] }
export interface ConfigPatch { revision: string; changes: Partial<ConfigValues> }
