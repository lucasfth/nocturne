import { test, expect, type Page } from '@playwright/test'
import summary from '../fixtures/summary.json' with { type: 'json' }
import machines from '../fixtures/machines.json' with { type: 'json' }
// Synthetic wire fixtures only. These are not inference measurements.
const a='a'.repeat(64), b='b'.repeat(64)
function job(state='completed') { return { id:'1'.repeat(64), machine:'mac-anna', options:{context:32768,parallel:2,budget_seconds:600,model_sha256:a,build_digest:b}, provenance:{schema_version:1,policy_version:1,fixture_version:1,worker_id:'mac-anna',created_at:'2026-10-15T20:00:00Z',deadline_at:'2026-10-15T20:10:00Z',model_sha256:a,build_digest:b,actor:'admin-bearer',code_revision:'c'.repeat(40),config_generation:'d'.repeat(64)},state,progress:state==='completed'?'done':'measuring',sequence:1,failure:'',restored:state==='completed',updated_at:'2026-10-15T20:00:02Z',...(state==='completed'?{summary:{quality:'pass',context:32768,parallel:2,prompt_tokens:32668,output_tokens:100,duration_seconds:2,tokens_per_second:50,model_verified:true,model_sha256:a,build_digest:b}}:{}) } }
async function setup(page: Page, initial: any[] = []) {
 let jobs=initial, fail=0, applyFail=false, gets=0
 const writes: {url:string;body:any}[]=[]
 await page.route('**/admin/summary',r=>r.fulfill({json:summary}))
 await page.route('**/admin/machines',r=>r.fulfill({json:machines}))
 await page.route('**/admin/machines/*/benchmarks**',async r=>{
  expect(r.request().headers().authorization).toBe('Bearer browser-test')
  if(r.request().method()==='GET') { gets++; return r.fulfill({status:fail||200,json:fail?{detail:'private-secret-path'}:jobs}) }
  writes.push({url:r.request().url(),body:r.request().postDataJSON()})
  if(fail || applyFail && r.request().url().endsWith('/apply')) return r.fulfill({status:fail||409,json:{detail:fail===409?'unsupported worker: secure benchmark adapter unavailable':'private-secret-path'}})
  const next=r.request().url().endsWith('/cancel')?{...jobs[0],state:'cancelling',progress:'restoring'}:r.request().url().endsWith('/apply')?{...jobs[0],action:'apply',state:'queued',progress:'pending',restored:false,sequence:0,apply_request:{transaction_id:'e'.repeat(64),generation:1,audit:jobs[0].provenance,requested_at:'2026-10-15T20:00:03Z',deadline_at:'2026-10-15T20:10:03Z',completed_at:'2026-10-15T20:00:02Z',completed_sequence:1}}:job('queued')
  jobs=[next]; await r.fulfill({status:202,json:next})
 })
 await page.goto('/admin/'); await page.getByLabel('Admin secret',{exact:true}).fill('browser-test'); await page.getByRole('button',{name:'Open',exact:true}).click()
 await page.getByRole('button',{name:'Benchmark & optimise',exact:true}).first().click()
 await expect(page.getByRole('button',{name:'Run benchmark',exact:true})).toBeVisible()
 return { writes, set:(v:any[])=>{jobs=v}, fail:(v:number)=>{fail=v}, refuseApply:()=>{applyFail=true}, gets:()=>gets }
}
test('automatic default request, collapsed requirements, honest unsupported and redaction',async({page})=>{
 const f=await setup(page)
 await expect(page.getByLabel('Override automatic requirements')).not.toBeVisible()
 f.fail(409); await page.getByRole('button',{name:'Run benchmark',exact:true}).click()
 expect(f.writes[0]!.body).toEqual({})
 await expect(page.getByRole('alert')).toContainText('No job was started')
 await expect(page.getByText('private-secret-path')).toHaveCount(0)
})
test('advanced bounds and exact typed options',async({page})=>{
 const f=await setup(page); await page.getByText('Advanced / Nerd',{exact:true}).click(); await page.getByLabel('Override automatic requirements').check()
 await page.getByLabel('Context per slot (including output)',{exact:true}).fill('8192'); await page.getByLabel('Parallel slots',{exact:true}).fill('1'); await page.getByLabel('Budget (seconds)',{exact:true}).fill('30')
 await page.getByRole('button',{name:'Run benchmark',exact:true}).click()
 expect(f.writes[0]!.body).toEqual({advanced:{context:8192,parallel:1,budget_seconds:30}})
 await expect(page.getByRole('button',{name:'Cancel job',exact:true})).toBeVisible()
})
test('results need explicit Apply confirmation and server capability remains authoritative',async({page})=>{
 const f=await setup(page,[job()]); f.refuseApply()
 await expect(page.getByRole('region',{name:'Recommendation'})).toContainText('50 tok/s')
 await page.getByRole('button',{name:'Apply recommendation',exact:true}).click(); expect(f.writes).toHaveLength(0)
 await expect(page.getByRole('alertdialog')).toContainText('32,768 tokens per slot (including output) · 2 parallel slots · 65,536 aggregate context tokens')
 await page.screenshot({path:'test-results/benchmark-context-confirmation.png',fullPage:true})
 await page.getByRole('button',{name:'Keep current settings'}).click(); expect(f.writes).toHaveLength(0)
 await page.getByRole('button',{name:'Apply recommendation',exact:true}).click(); await page.getByRole('button',{name:'Confirm apply'}).click()
 expect(f.writes[0]!.body).toEqual({}); await expect(page.getByRole('alert')).toContainText('Apply unavailable or refused')
 await expect(page.getByText('private-secret-path')).toHaveCount(0)
})
test('cancel stays fenced until restoration and recovery is not success',async({page})=>{
 const f=await setup(page,[job('running')]); await page.getByRole('button',{name:'Cancel job',exact:true}).click()
 await expect(page.getByRole('button',{name:'Cancellation requested'})).toBeDisabled()
 f.set([{...job('recovery_required'),progress:'restoring',failure:'restoration'}]); await page.getByRole('button',{name:'Reload history'}).click()
 await expect(page.getByRole('alert')).toContainText('Recovery required')
 await expect(page.getByRole('button',{name:'Run benchmark',exact:true})).toBeDisabled()
})
test('terminal jobs stop polling; malformed evidence fails closed; narrow dialog and focus',async({page})=>{
 await page.setViewportSize({width:320,height:740}); const f=await setup(page,[job()]); const n=f.gets()
 await page.waitForTimeout(2300); expect(f.gets()).toBe(n)
 await expect(page.getByRole('dialog')).toBeVisible()
 expect(await page.evaluate(()=>document.documentElement.scrollWidth<=window.innerWidth)).toBe(true)
 await page.screenshot({path:'test-results/benchmark-320-test-data.png',fullPage:true})
 f.set([{...job(),summary:{...job().summary,context:null}}]); await page.getByRole('button',{name:'Reload history'}).click()
 await expect(page.getByRole('alert')).toContainText('response unsupported'); await expect(page.getByRole('button',{name:'Apply recommendation'})).toBeDisabled()
 await page.keyboard.press('Escape'); await expect(page.getByRole('dialog')).toHaveCount(0)
 await expect(page.getByRole('button',{name:'Benchmark & optimise',exact:true}).first()).toBeFocused()
})
test('unmount aborts polling and next machine cannot inherit evidence',async({page})=>{
 const f=await setup(page,[job('running')]); await page.keyboard.press('Escape'); const n=f.gets(); await page.waitForTimeout(2300); expect(f.gets()).toBe(n)
 await page.getByRole('button',{name:'Benchmark & optimise',exact:true}).nth(1).click()
 await expect(page.getByRole('alert')).toContainText('response unsupported'); await expect(page.getByRole('region',{name:'Recommendation'})).toHaveCount(0)
})
test('Apply health rollback and failed rollback have distinct safety outcomes',async({page})=>{
 const base=job(), apply_request={transaction_id:'e'.repeat(64),generation:1,audit:base.provenance,requested_at:'2026-10-15T20:00:03Z',deadline_at:'2026-10-15T20:10:03Z',completed_at:'2026-10-15T20:00:02Z',completed_sequence:1}
 const f=await setup(page,[{...base,action:'apply',apply_request,state:'failed',failure:'execution',result:'rolled_back'}])
 await expect(page.getByText('Health/change did not complete:',{exact:false})).toContainText('verified rollback')
 f.set([{...base,action:'apply',apply_request,state:'recovery_required',progress:'restoring',failure:'restoration',restored:false}]); await page.getByRole('button',{name:'Reload history'}).click()
 await expect(page.getByRole('alert')).toContainText('Recovery required')
 await expect(page.getByRole('button',{name:'Run benchmark',exact:true})).toBeDisabled()
})
test('server failures cannot leak bodies and stale Apply stays disabled',async({page})=>{
 const f=await setup(page,[job()]); f.fail(503); await page.getByRole('button',{name:'Reload history'}).click()
 await expect(page.getByRole('alert')).toContainText('State is uncertain'); await expect(page.getByText('private-secret-path')).toHaveCount(0)
 await expect(page.getByRole('button',{name:'Apply recommendation'})).toBeDisabled()
})
test('known active job remains cancellable after failed refresh',async({page})=>{
 const f=await setup(page,[job('running')]); f.fail(503)
 await page.getByRole('button',{name:'Reload history'}).click(); await expect(page.getByRole('alert')).toContainText('State is uncertain')
 f.fail(0); await page.getByRole('button',{name:'Cancel job',exact:true}).click()
 expect(f.writes).toHaveLength(1); expect(f.writes[0]!.url).toMatch(/\/cancel$/)
 await expect(page.getByRole('button',{name:'Cancellation requested'})).toBeDisabled()
})
test('malformed Apply timestamps never render untrusted values',async({page})=>{
 await setup(page,[{...job(),action:'apply',apply_request:{requested_at:'private-secret-path'}}])
 await expect(page.getByRole('alert')).toContainText('response unsupported')
 await expect(page.getByText('private-secret-path')).toHaveCount(0)
})

test('benchmark and Apply provenance remain distinct and allowlisted',async({page})=>{
 const base=job(); base.provenance.actor='actor-a'
 await setup(page,[{...base,action:'apply',apply_phase:'prepared',sequence:7,updated_at:'2026-10-15T20:00:09Z',state:'running',progress:'validating',restored:false,apply_request:{transaction_id:'e'.repeat(64),generation:1,audit:{...base.provenance,actor:'actor-b',private_field:'private-secret-path'},requested_at:'2026-10-15T20:00:03Z',deadline_at:'2026-10-15T20:10:03Z',completed_at:'2026-10-15T20:00:02Z',completed_sequence:1}}])
 await page.getByText('Evidence & provenance',{exact:true}).click()
 const benchmark=page.getByRole('region',{name:'Benchmark provenance',exact:true}), apply=page.getByRole('region',{name:'Apply provenance',exact:true})
 await expect(benchmark).toContainText('actor-a'); await expect(benchmark).not.toContainText('actor-b'); await expect(benchmark).toContainText('benchmark completed sequence1'); await expect(benchmark).toContainText('2026-10-15T20:00:02Z'); await expect(benchmark).not.toContainText('2026-10-15T20:00:09Z')
 await expect(page.getByRole('region',{name:'Job status'})).toContainText('job sequence: 7')
 await expect(apply).toContainText('actor-b'); await expect(apply).not.toContainText('actor-a')
 await expect(apply).toContainText('transaction id'+'e'.repeat(64)); await expect(apply).toContainText('generation1'); await expect(apply).toContainText('apply phaseprepared')
 await expect(page.getByText('private-secret-path')).toHaveCount(0)
 await page.screenshot({path:'test-results/benchmark-apply-provenance.png',fullPage:true})
})
test('invalid context and unsafe Apply identity fail closed without rendering values',async({page})=>{
 const f=await setup(page,[job()])
 for (const context of [null, 0, -1, 32768.5, '32768', 1e309, Number.MAX_SAFE_INTEGER+1]) {
  f.set([{...job(),options:{...job().options,context},summary:{...job().summary,context}}]); await page.getByRole('button',{name:'Reload history'}).click()
  await expect(page.getByRole('alert')).toContainText('response unsupported'); await expect(page.getByRole('button',{name:'Apply recommendation'})).toBeDisabled()
 }
 for (const patch of [{generation:Number.MAX_SAFE_INTEGER+1},{generation:2},{transaction_id:'private-secret-path'},{audit:{...job().provenance,actor:'private-secret-path/'}},{apply_phase:'private-secret-path'}]) {
  const {apply_phase,...fields}=patch
  f.set([{...job(),action:'apply',apply_phase,apply_request:{transaction_id:'e'.repeat(64),generation:1,audit:job().provenance,requested_at:'2026-10-15T20:00:03Z',deadline_at:'2026-10-15T20:10:03Z',completed_at:'2026-10-15T20:00:02Z',completed_sequence:1,...fields}}]); await page.getByRole('button',{name:'Reload history'}).click()
  await expect(page.getByRole('alert')).toContainText('response unsupported'); await expect(page.getByText('private-secret-path')).toHaveCount(0)
 }
 expect(f.writes).toHaveLength(0)
})

test('default and advanced context allocation are explicit and invalid overrides never submit',async({page})=>{
 const f=await setup(page)
 await page.getByText('Advanced / Nerd',{exact:true}).click()
 await expect(page.getByText('Automatic:',{exact:false})).toContainText('32,768 tokens per slot (including output) · 2 parallel slots · 65,536 aggregate context tokens')
 await page.getByLabel('Override automatic requirements').check()
 await page.getByLabel('Context per slot (including output)',{exact:true}).fill('8192')
 await page.getByLabel('Parallel slots',{exact:true}).fill('1')
 await expect(page.getByText('8,192 tokens per slot (including output) · 1 parallel slots · 8,192 aggregate context tokens',{exact:true})).toBeVisible()
 for (const value of ['', '32768.5', '9007199254740992']) {
  await page.getByLabel('Context per slot (including output)',{exact:true}).fill(value)
  await expect(page.getByRole('button',{name:'Run benchmark',exact:true})).toBeDisabled()
 }
 expect(f.writes).toHaveLength(0)
})

test('malformed benchmark completion timestamp is rejected without disclosure',async({page})=>{
 const f=await setup(page,[{...job(),finished_at:'private-secret-path'}])
 await expect(page.getByRole('alert')).toContainText('response unsupported')
 await expect(page.getByText('private-secret-path')).toHaveCount(0)
 await expect(page.getByRole('button',{name:'Apply recommendation'})).toHaveCount(0)
 expect(f.writes).toHaveLength(0)
})
