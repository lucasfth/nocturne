import summaryFixture from '../fixtures/summary.json'
import machinesFixture from '../fixtures/machines.json'
import { ApiError, type Summary, type Machine, type MachineInput, type MachinePatch } from './api'
const summary: Summary = structuredClone(summaryFixture)
const machines: Machine[] = structuredClone(machinesFixture)
export async function mockRequest(path: string, method: string, body?: MachineInput | MachinePatch): Promise<unknown> {
 if (path === 'summary') return structuredClone(summary)
 if (path === 'machines' && method === 'GET') return structuredClone(machines)
 if (method === 'POST' && body && 'worker_id' in body) {
  if (machines.some(m => m.worker_id === body.worker_id)) throw new ApiError('Machine already exists', 409)
  const machine: Machine = { worker_id: body.worker_id, enabled: true, added: Date.now()/1000, meta: body.meta, tunnel_key_fingerprint: null }
  machines.push(machine)
  summary.workers.push({ worker_id: body.worker_id, enabled: true, online: false, eligible: false, heartbeat_age_s: null, battery_pct: null, on_ac: null, thermal_state: null, user_active: null, tokens_per_sec: null, busy: 0, capacity: null, metrics: {}, mac: body.meta })
  return structuredClone(machine)
 }
 const id = decodeURIComponent(path.split('/')[1] || '')
 const index = machines.findIndex(m => m.worker_id === id)
 const machine = machines[index]
 if (!machine) throw new ApiError('Machine not found', 404)
 const worker = summary.workers.find(w => w.worker_id === id)
 if (method === 'DELETE') { machines.splice(index, 1); summary.workers = summary.workers.filter(w => w.worker_id !== id) }
 if (method === 'PATCH' && body) {
  machine.meta = { ...machine.meta, ...body.meta }
  if ('enabled' in body && body.enabled !== undefined) machine.enabled = body.enabled
  if (worker) { worker.mac = machine.meta; worker.enabled = machine.enabled; if (!machine.enabled) { worker.online = false; worker.eligible = false; worker.busy = 0 } }
 }
 summary.energy.serving_workers = summary.workers.filter(w => w.online && w.enabled && w.eligible).length
 return structuredClone(method === 'DELETE' ? { deleted: id } : machine)
}
