import { test, expect, type Page } from '@playwright/test'
const initial = () => ({schema_version:1,runtime:'python',revision:'revision-one',fields:{service_name:{type:'string',apply:'live',minimum:null,maximum:null,default:'inference'},public_limit_concurrency:{type:'integer',apply:'restart',minimum:1,maximum:4096,default:64}},values:{service_name:'inference',public_limit_concurrency:64},effective:{service_name:'inference',public_limit_concurrency:64},restart_required:[] as string[]})
async function setup(page: Page) {
 let state=initial(), status=200, delay=0
 const writes: any[]=[]
 await page.route('**/admin/summary',r=>r.fulfill({status:503,json:{detail:'overview unavailable'}}))
 await page.route('**/admin/machines',r=>r.fulfill({json:[]}))
 await page.route('**/admin/config',async route=>{
  expect(route.request().headers().authorization).toBe('Bearer browser-test')
  if (route.request().method()==='PATCH') {
   const body=route.request().postDataJSON(); writes.push(body)
   if(delay) await new Promise(resolve=>setTimeout(resolve,delay))
   if(status!==200) return route.fulfill({status,json:{detail:'PRIVATE ERROR MUST NOT BE SHOWN'}})
   expect(body.revision).toBe(state.revision)
   state.values={...state.values,...body.changes}; state.effective.service_name=state.values.service_name
   state.restart_required=state.values.public_limit_concurrency!==state.effective.public_limit_concurrency?['public_limit_concurrency']:[]
   state.revision+='-new'
  } else if(status!==200 && status!==409) return route.fulfill({status,json:{detail:'PRIVATE ERROR MUST NOT BE SHOWN'}})
  await route.fulfill({json:state})
 })
 await page.goto('/admin/'); await page.getByLabel('Admin secret',{exact:true}).fill('browser-test'); await page.getByRole('button',{name:'Open',exact:true}).click()
 await page.getByRole('tab',{name:'Settings',exact:true}).click()
 await expect(page.getByLabel('Service name',{exact:true})).toHaveValue('inference')
 return {writes,setStatus:(s:number)=>{status=s},setDelay:(d:number)=>{delay=d},changeElsewhere:()=>{state.values.public_limit_concurrency=100;state.revision='external-revision'}}
}
const save=(page:Page)=>page.getByRole('button',{name:'Save settings',exact:true})
test('settings survive failed overview; typed changed-only save shows live and pending effects',async({page})=>{
 const {writes,setDelay}=await setup(page)
 await expect(save(page)).toBeDisabled()
 await expect(page.getByLabel('Public concurrency limit',{exact:true})).toHaveAttribute('max','4096')
 await page.getByLabel('Service name',{exact:true}).fill('Community inference')
 setDelay(350)
 await save(page).click(); await expect(page.getByRole('button',{name:'Saving…'})).toBeDisabled()
 await expect(page.getByTestId('effective-service_name')).toHaveText('Effective: Community inference')
 expect(writes[0]).toEqual({revision:'revision-one',changes:{service_name:'Community inference'}})
 await page.getByLabel('Public concurrency limit',{exact:true}).fill('32'); await save(page).click()
 await expect(page.getByText('Restart pending: Public concurrency limit is saved but not effective.')).toBeVisible()
 await expect(page.getByTestId('effective-public_limit_concurrency')).toHaveText('Effective: 64')
 await expect(page.getByTestId('saved-public_limit_concurrency')).toHaveText('Saved: 32')
 expect(writes[1].changes).toEqual({public_limit_concurrency:32})
 await expect(page.getByRole('button',{name:/^Restart/})).toHaveCount(0)
 await page.screenshot({path:'test-results/admin-settings.png',fullPage:true})
})
test('invalid values never mutate; failed saves redact error and preserve edits',async({page})=>{
 const {writes,setStatus}=await setup(page)
 for(const value of ['0','4097','1.5','']) {
  await page.getByLabel('Public concurrency limit',{exact:true}).fill(value); await save(page).click()
  await expect(page.getByRole('alert').filter({hasText:'must be an integer'})).toBeVisible()
 }
 await page.getByLabel('Public concurrency limit',{exact:true}).fill('64')
 await page.getByLabel('Service name',{exact:true}).fill('<unsafe>'); await save(page).click()
 expect(writes).toHaveLength(0)
 await page.getByLabel('Service name',{exact:true}).fill('My service'); setStatus(400); await save(page).click()
 await expect(page.getByText('The server rejected these values. Check the field constraints.')).toBeVisible()
 await expect(page.getByLabel('Service name',{exact:true})).toHaveValue('My service')
 await expect(page.getByTestId('saved-service_name')).toHaveText('Saved: inference')
 await expect(page.getByText('PRIVATE ERROR MUST NOT BE SHOWN')).toHaveCount(0)
})
test('409 requires explicit reload and review, preserving only changed fields',async({page})=>{
 const {writes,setStatus,changeElsewhere}=await setup(page)
 await page.getByLabel('Service name',{exact:true}).fill('My intent'); changeElsewhere(); setStatus(409)
 await save(page).click(); await expect(save(page)).toBeDisabled()
 await expect(page.getByRole('alert').filter({hasText:'Configuration changed elsewhere'})).toBeVisible()
 expect(writes).toHaveLength(1)
 await page.getByRole('button',{name:'Reload latest (keep edits)'}).click()
 await expect(page.getByLabel('Service name',{exact:true})).toHaveValue('My intent')
 await expect(page.getByLabel('Public concurrency limit',{exact:true})).toHaveValue('100')
 expect(writes).toHaveLength(1)
 setStatus(200); await save(page).click()
 await expect(page.getByTestId('effective-service_name')).toHaveText('Effective: My intent')
 expect(writes[1]).toEqual({revision:'external-revision',changes:{service_name:'My intent'}})
})
for(const status of [401,403,404,503]) test(`settings ${status} is explicit and cannot mutate`,async({page})=>{
 const {setStatus,writes}=await setup(page); setStatus(status)
 await page.getByRole('button',{name:'Reload latest (keep edits)'}).click()
 if(status!==401) await expect(save(page)).toBeDisabled()
 if(status===401) await expect(page.getByLabel('Admin secret',{exact:true})).toBeVisible()
 else await expect(page.getByRole('alert').filter({hasText:status===403?'Access denied':status===404?'not supported':'server unavailable'})).toBeVisible()
 expect(writes).toHaveLength(0)
 await expect(page.getByText('PRIVATE ERROR MUST NOT BE SHOWN')).toHaveCount(0)
})

test('unavailable save blocks blind retries until reload and retains user intent',async({page})=>{
 const {setStatus,writes}=await setup(page)
 await page.getByLabel('Service name',{exact:true}).fill('Preserved name'); setStatus(503)
 await save(page).click(); await expect(save(page)).toBeDisabled()
 expect(writes).toHaveLength(1)
 setStatus(200); await page.getByRole('button',{name:'Reload latest (keep edits)'}).click()
 await expect(page.getByLabel('Service name',{exact:true})).toHaveValue('Preserved name')
 await expect(save(page)).toBeEnabled(); expect(writes).toHaveLength(1)
})

test('reverted numeric field is not dirty and does not overwrite external changes on reload',async({page})=>{
 const {writes,changeElsewhere}=await setup(page)
 await page.getByLabel('Public concurrency limit',{exact:true}).fill('32')
 await expect(save(page)).toBeEnabled()
 await page.getByLabel('Public concurrency limit',{exact:true}).fill('64')
 await expect(save(page)).toBeDisabled()
 await page.getByLabel('Service name',{exact:true}).fill('Keep only this')
 changeElsewhere()
 await page.getByRole('button',{name:'Reload latest (keep edits)'}).click()
 await expect(page.getByLabel('Public concurrency limit',{exact:true})).toHaveValue('100')
 await save(page).click()
 await expect(save(page)).toBeDisabled()
 expect(writes[0].changes).toEqual({service_name:'Keep only this'})
})
