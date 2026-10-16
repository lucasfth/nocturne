import { test, expect, type Page } from '@playwright/test'
import summaryFixture from '../fixtures/summary.json' with { type: 'json' }
import machinesFixture from '../fixtures/machines.json' with { type: 'json' }

async function setup(page: Page, overviewFails = false) {
 const machines = structuredClone(machinesFixture)
 const writes: { method: string; body: Record<string, unknown> | null }[] = []
 let failWrite = false
 const errors: string[]=[]
 page.on('pageerror',error=>errors.push(error.message))
 // Match the production Python/Go page policy, including form-action 'none'.
 await page.route('**/admin/', async route => {
  const response=await route.fetch()
  await route.fulfill({ response, headers: { ...response.headers(), 'content-security-policy': "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'" } })
 })
 await page.route('**/admin/summary',route=>route.fulfill({status: overviewFails ? 503 : 200,json: overviewFails ? {detail:'overview unavailable'} : summaryFixture}))
 await page.route('**/admin/machines**',async route=>{
  const request=route.request(), method=request.method()
  expect(request.headers().authorization).toBe('Bearer browser-test')
  if (method==='GET') return route.fulfill({json:machines})
  const body=request.postDataJSON() as Record<string, unknown> | null
  writes.push({method,body})
  if (failWrite) return route.fulfill({status:409,json:{detail:'machine already exists'}})
  const id=decodeURIComponent(new URL(request.url()).pathname.split('/').pop()!)
  let machine=machines.find(machine=>machine.worker_id===id)
  if (method==='POST') { machine={worker_id:String(body!.worker_id), enabled:true,added:0,meta:body!.meta as typeof machines[number]['meta'],tunnel_key_fingerprint:null}; machines.push(machine) }
  if (method==='PATCH') { const meta={...machine!.meta,...body!.meta as object}; Object.assign(machine!,body,{meta}) }
  if (method==='DELETE') machines.splice(machines.indexOf(machine!),1)
  await route.fulfill({status:method==='POST'?201:200,json: method==='DELETE' ? {deleted:id} : machine})
 })
 await page.goto('/admin/')
 await page.getByLabel('Admin secret',{exact:true}).fill('browser-test')
 await page.getByRole('button',{name:'Open',exact:true}).click()
 await expect(page.getByRole('button',{name:'Add machine',exact:true})).toBeVisible()
 return { machines,writes,errors, fail:()=>{failWrite=true} }
}
function card(page: Page,id='mac-anna') { return page.locator('[data-slot="card"]').filter({has:page.getByRole('heading',{name:id,exact:true})}) }

test('add, edit without rotating credentials, disable/enable and confirmed remove refresh the registry',async({page})=>{
 const {writes,errors}=await setup(page)
 await page.getByRole('button',{name:'Add machine',exact:true}).click()
 const dialog=page.getByRole('dialog')
 await page.getByLabel('Worker id',{exact:true}).fill('a') // canonical contract allows one character
 await page.getByLabel('Secret SHA256',{exact:true}).fill('a'.repeat(64))
 await page.getByLabel('Owner',{exact:true}).fill('Test operator')
 await dialog.getByRole('button',{name:'Add machine',exact:true}).click()
 await expect(card(page,'a')).toBeVisible() // intentionally absent from summary
 await card(page,'a').getByRole('button',{name:'Edit machine',exact:true}).click()
 await expect(page.getByRole('dialog')).toContainText('Machine identity (cannot be changed)')
 await page.getByText('Credentials · replace only if needed',{exact:true}).click()
 await expect(page.locator('#digest')).toHaveValue('')
 await expect(page.locator('#pubkey')).toHaveValue('')
 await page.getByLabel('Owner',{exact:true}).fill('Edited operator')
 await dialog.getByRole('button',{name:'Save changes'}).click()
 await expect(card(page,'a')).toContainText('Edited operator')
 expect(writes[1].body).not.toHaveProperty('secret_sha256')
 expect(writes[1].body).not.toHaveProperty('tunnel_pubkey')
 await card(page,'a').getByRole('button',{name:'Disable',exact:true}).click()
 await expect(card(page,'a').getByRole('button',{name:'Enable',exact:true})).toBeEnabled()
 await card(page,'a').getByRole('button',{name:'Enable',exact:true}).click()
 await expect(card(page,'a').getByRole('button',{name:'Disable',exact:true})).toBeEnabled()
 await card(page,'a').getByRole('button',{name:'Remove',exact:true}).click()
 await page.getByRole('alertdialog').getByRole('button',{name:'Cancel'}).click()
 expect(writes.filter(write=>write.method==='DELETE')).toHaveLength(0)
 await card(page,'a').getByRole('button',{name:'Remove',exact:true}).click()
 await page.getByRole('alertdialog').getByRole('button',{name:'Remove machine'}).click()
 await expect(card(page,'a')).toHaveCount(0)
 expect(errors).toEqual([])
})

test('machine controls survive overview failures and remain discoverable',async({page})=>{
 await setup(page,true)
 await expect(page.getByRole('alert')).toContainText('overview unavailable')
 await expect(card(page).getByRole('button',{name:'Edit machine'})).toBeVisible()
 await card(page).getByRole('button',{name:'Edit machine'}).click()
 await expect(page.getByLabel('Owner',{exact:true})).toHaveValue(machinesFixture[0]!.meta.owner)
 await page.screenshot({path: process.env.NOCTURNE_SCREENSHOT || 'test-results/admin-edit.png',fullPage:true})
})

test('validation and API failures keep form editable with actionable errors',async({page})=>{
 const {writes,fail}=await setup(page)
 await page.getByRole('button',{name:'Add machine',exact:true}).click()
 await page.getByLabel('Worker id',{exact:true}).fill('BAD ID')
 await page.locator('#digest').fill('a'.repeat(64))
 await page.getByRole('dialog').getByRole('button',{name:'Add machine',exact:true}).click()
 await expect(page.getByRole('dialog').getByRole('alert')).toContainText('Worker id')
 expect(writes).toHaveLength(0)
 await page.getByLabel('Worker id',{exact:true}).fill('new-mac')
 fail()
 await page.getByRole('dialog').getByRole('button',{name:'Add machine',exact:true}).click()
 await expect(page.getByRole('dialog').getByRole('alert')).toHaveText('machine already exists')
 await expect(page.getByRole('dialog').getByRole('button',{name:'Add machine',exact:true})).toBeEnabled()
})

test('bootstrap mismatch cannot rotate credentials, explicit replacement is sent',async({page})=>{
 const {writes}=await setup(page)
 await card(page).getByRole('button',{name:'Edit machine'}).click()
 await page.getByText('Credentials · replace only if needed',{exact:true}).click()
 await page.locator('#bootstrap').fill(`DIGEST other-mac ${'b'.repeat(64)}`)
 await page.getByRole('button',{name:'Parse setup output'}).click()
 await expect(page.getByRole('dialog').getByRole('alert')).toHaveText('Bootstrap worker IDs do not match')
 expect(writes).toHaveLength(0)
 await page.locator('#bootstrap').fill('')
 await page.locator('#digest').fill('b'.repeat(64))
 await page.getByRole('button',{name:'Save changes'}).click()
 await expect(page.getByRole('dialog')).toHaveCount(0)
 expect(writes[0]!.body!.secret_sha256).toBe('b'.repeat(64))
})


test('overview orders connected Macs first, alphabetically within each group, independently of eligibility', async ({page}) => {
 await page.clock.install()
 const states = [
  {worker_id:'mac-zulu',online:false,eligible:true,enabled:true},
  {worker_id:'mac-ready',online:true,eligible:true,enabled:true,busy:0},
  {worker_id:'mac-missing',enabled:true},
  {worker_id:'mac-paused',online:true,eligible:false,enabled:true},
  {worker_id:'mac-in-use',online:true,eligible:false,user_active:true,enabled:true},
  {worker_id:'mac-disabled',online:true,eligible:false,enabled:false},
  {worker_id:'mac-alpha',online:false,eligible:false,enabled:true},
  {worker_id:'mac-disabled-offline',online:false,eligible:false,enabled:false},
  {worker_id:'mac-tie',online:true,eligible:false,enabled:true},
  {worker_id:'mac-Tie',online:true,eligible:false,enabled:true},
 ]
 const registry=states.map(state=>({...machinesFixture[0]!,worker_id:state.worker_id,enabled:state.enabled}))
 const summary={...summaryFixture,workers:states.filter(state=>'online' in state).map(state=>({...summaryFixture.workers[0]!,...state}))}
 let summaryUnavailable=false
 await page.route('**/admin/summary',route=>route.fulfill(summaryUnavailable ? {status:503,json:{detail:'overview unavailable'}} : {json:summary}))
 await page.route('**/admin/machines',route=>route.fulfill({json:registry}))
 await page.goto('/admin/')
 await page.getByLabel('Admin secret',{exact:true}).fill('browser-test')
 await page.getByRole('button',{name:'Open',exact:true}).click()
 const headings=page.getByRole('tabpanel').getByRole('heading',{level:3})
 await expect(headings).toHaveText(['mac-disabled','mac-in-use','mac-paused','mac-ready','mac-Tie','mac-tie','mac-alpha','mac-disabled-offline','mac-missing','mac-zulu'])
 for (const [id,status] of [['mac-disabled','disabled'],['mac-disabled-offline','disabled'],['mac-in-use','in use'],['mac-paused','paused'],['mac-ready','ready'],['mac-missing','offline'],['mac-zulu','offline']]) {
  await expect(card(page,id).locator('[data-slot="badge"]')).toHaveText(status!)
 }
 summary.workers.find(worker=>worker.worker_id==='mac-zulu')!.online=true
 summary.workers.find(worker=>worker.worker_id==='mac-ready')!.online=false
 await page.clock.fastForward(15000)
 await expect(headings).toHaveText(['mac-disabled','mac-in-use','mac-paused','mac-Tie','mac-tie','mac-zulu','mac-alpha','mac-disabled-offline','mac-missing','mac-ready'])
 await page.screenshot({path:'test-results/overview-online-first.png',fullPage:true})
 await page.getByRole('button',{name:'Log out',exact:true}).click()
 summaryUnavailable=true
 await page.getByLabel('Admin secret',{exact:true}).fill('browser-test')
 await page.getByRole('button',{name:'Open',exact:true}).click()
 await expect(headings).toHaveText(['mac-alpha','mac-disabled','mac-disabled-offline','mac-in-use','mac-missing','mac-paused','mac-ready','mac-Tie','mac-tie','mac-zulu'])
})


test('simple edits preserve hidden metadata and optional advanced details are keyboard accessible',async({page})=>{
 const {writes}=await setup(page)
 await card(page).getByRole('button',{name:'Edit machine'}).click()
 const dialog=page.getByRole('dialog')
 await expect(page.getByLabel('Agent',{exact:true})).toHaveCount(0)
 await expect(page.locator('#meta-host')).not.toBeVisible()
 await expect(page.locator('#digest')).not.toBeVisible()
 await page.getByLabel('Owner',{exact:true}).fill('New owner')
 await dialog.getByRole('button',{name:'Save changes'}).click()
 expect(writes[0]!.body).toEqual({meta:{owner:'New owner'}})
 await expect(card(page)).toContainText(machinesFixture[0]!.meta.agent!)
 await card(page).getByRole('button',{name:'Edit machine'}).click()
 const advanced=dialog.locator('summary').filter({hasText:'Advanced · optional machine details'})
 await advanced.focus()
 await page.keyboard.press('Enter')
 await expect(page.getByLabel('Host (optional)',{exact:true})).toHaveValue(machinesFixture[0]!.meta.host)
 await page.getByLabel('Host (optional)',{exact:true}).fill('new-mac.local')
 await dialog.getByRole('button',{name:'Save changes'}).click()
 expect(writes[1]!.body).toEqual({meta:{host:'new-mac.local'}})
 expect(writes.every(write=>!('agent' in (write.body?.meta as object || {})))).toBe(true)
})

for(const colorScheme of ['light','dark'] as const)test(`setup paste preview is local, non-overwriting and separate from save ${colorScheme}`,async({page})=>{
 await page.emulateMedia({colorScheme});await page.setViewportSize({width:320,height:900})
 const {writes}=await setup(page)
 await page.getByRole('button',{name:'Add machine',exact:true}).click()
 await expect(page.getByLabel('Paste setup output',{exact:true})).toBeVisible()
 await page.getByLabel('Owner',{exact:true}).fill('Manually chosen owner')
 await page.getByLabel('Worker id',{exact:true}).fill('manual-mac')
 const text=`status: installing\r\nDIGEST parsed-mac ${'c'.repeat(64)}\r\n`
 await page.locator('#bootstrap').fill(text)
 await expect(page.locator('#digest')).toHaveValue('')
 await page.getByRole('button',{name:'Parse setup output'}).click()
 await expect(page.getByLabel('Setup preview')).toContainText('parsed-mac')
 expect(writes).toHaveLength(0)
 await page.getByRole('button',{name:'Apply to empty or matching fields'}).click()
 await expect(page.getByRole('dialog').getByRole('alert')).toContainText('Existing fields differ')
 await expect(page.getByLabel('Worker id',{exact:true})).toHaveValue('manual-mac')
 await page.getByLabel('Worker id',{exact:true}).fill('')
 await page.getByRole('button',{name:'Apply to empty or matching fields'}).click()
 await expect(page.locator('#bootstrap')).toHaveValue('')
 await expect(page.getByLabel('Worker id',{exact:true})).toHaveValue('parsed-mac')
 await expect(page.locator('#digest')).toHaveValue('c'.repeat(64))
 await expect(page.getByLabel('Owner',{exact:true})).toHaveValue('Manually chosen owner')
 expect(writes).toHaveLength(0)
 expect(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth)).toBe(true)
 const stored=await page.evaluate(()=>JSON.stringify({...localStorage,...sessionStorage}))
 expect(stored).not.toContain('c'.repeat(64));expect(stored).not.toContain('parsed-mac')
 await page.getByRole('dialog').getByRole('button',{name:'Add machine',exact:true}).click()
 expect(writes).toHaveLength(1);expect(writes[0]!.body!.secret_sha256).toBe('c'.repeat(64))
 await expect(card(page,'parsed-mac')).toBeVisible()
})
test('editing pasted text invalidates preview; closing clears transient credentials and submit never parses implicitly',async({page})=>{
 const {writes}=await setup(page)
 await page.getByRole('button',{name:'Add machine',exact:true}).click()
 await page.getByText('Setup helper · script and instructions',{exact:true}).click()
 await page.getByLabel('Worker id',{exact:true}).fill('test-mac');await page.locator('#digest').fill('e'.repeat(64))
 await page.locator('#bootstrap').fill(`DIGEST test-mac ${'e'.repeat(64)}`)
 await page.getByRole('button',{name:'Parse setup output'}).click();await expect(page.getByLabel('Setup preview')).toBeVisible()
 await page.locator('#bootstrap').fill(`DIGEST test-mac ${'f'.repeat(64)}`)
 await expect(page.getByLabel('Setup preview')).toHaveCount(0)
 await page.getByRole('dialog').getByRole('button',{name:'Add machine',exact:true}).click()
 await expect(page.getByRole('dialog').getByRole('alert')).toContainText('Review and apply')
 expect(writes).toHaveLength(0)
 await page.getByRole('button',{name:'Cancel',exact:true}).click()
 await page.getByRole('button',{name:'Add machine',exact:true}).click()
 await expect(page.locator('#digest')).toHaveValue('');await expect(page.locator('#bootstrap')).toHaveValue('')
})

for(const colorScheme of ['light','dark'] as const)for(const width of [320,1440])test(`Add machine opens with paste visible before fields ${colorScheme} ${width}`,async({page})=>{
 await page.emulateMedia({colorScheme});await page.setViewportSize({width,height:900});const {writes}=await setup(page)
 await page.getByRole('button',{name:'Add machine',exact:true}).click()
 const dialog=page.getByRole('dialog'),paste=page.getByLabel('Paste setup output',{exact:true})
 await expect(paste).toBeVisible();await expect(dialog.locator('#bootstrap')).toHaveCount(1)
 expect(await paste.evaluate(el=>!!(el.compareDocumentPosition(document.querySelector('#worker-id')!)&Node.DOCUMENT_POSITION_FOLLOWING))).toBe(true)
 expect(await paste.evaluate(el=>el.closest('details')===null)).toBe(true)
 await expect(dialog.getByText('Owner preparation uses native macOS tools and needs no Python.',{exact:false})).not.toBeVisible()
 await paste.fill(`DIGEST visible-mac ${'d'.repeat(64)}`)
 await page.getByRole('button',{name:'Parse setup output'}).click()
 await expect(page.getByLabel('Setup preview')).toContainText('visible-mac')
 expect(writes).toHaveLength(0)
 await page.getByRole('button',{name:'Apply to empty or matching fields'}).click()
 await expect(page.getByLabel('Worker id',{exact:true})).toHaveValue('visible-mac')
 await expect(page.locator('#digest')).toHaveValue('d'.repeat(64))
 expect(writes).toHaveLength(0)
 expect(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth)).toBe(true)
 await page.screenshot({path:`test-results/add-machine-paste-visible-${colorScheme}-${width}.png`,fullPage:true})
})

test('legacy owner report prefills facts without inventing identity or credentials and preserves manual values',async({page})=>{
 const {readFile}=await import('node:fs/promises')
 const report=await readFile(new URL('../fixtures/legacy-mac-report.txt',import.meta.url),'utf8')
 const {writes}=await setup(page);await page.getByRole('button',{name:'Add machine',exact:true}).click()
 await page.locator('#bootstrap').fill(report);await page.getByRole('button',{name:'Parse setup output'}).click()
 await expect(page.getByLabel('Setup preview')).toContainText('Mac system report detected')
 await expect(page.getByLabel('Setup preview')).toContainText('not worker registration credentials')
 await page.getByLabel('Owner',{exact:true}).fill('Keep manual owner')
 await page.getByRole('button',{name:'Apply to empty or matching fields'}).click()
 await expect(page.getByRole('dialog').getByRole('alert')).toContainText('Existing fields differ')
 await page.getByLabel('Owner',{exact:true}).fill('');await page.getByRole('button',{name:'Apply to empty or matching fields'}).click()
 await expect(page.getByLabel('Owner',{exact:true})).toHaveValue('Example Owner')
 await expect(page.getByLabel('Worker id',{exact:true})).toHaveValue('');await expect(page.locator('#digest')).toHaveValue('');await expect(page.locator('#pubkey')).toHaveValue('')
 await page.getByText('Advanced · optional machine details',{exact:true}).click()
 await expect(page.locator('#meta-user')).toHaveValue('example');await expect(page.locator('#meta-host')).toHaveValue('Example-MacBook.local');await expect(page.locator('#meta-ip')).toHaveValue('192.0.2.25')
 expect(writes).toHaveLength(0)
})
