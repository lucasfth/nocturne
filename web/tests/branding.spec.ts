import { test, expect } from '@playwright/test'
import { readFile } from 'node:fs/promises'
import { createHash } from 'node:crypto'
for (const colorScheme of ['light','dark'] as const) for (const width of [320,1440]) test(`public brand ${colorScheme} ${width}px without production calls`, async({page})=>{
 await page.emulateMedia({colorScheme}); await page.setViewportSize({width,height:900})
 const html=(await readFile(new URL('../../gateway/landing.html',import.meta.url),'utf8')).replaceAll('{{SERVICE}}','nocturne').replaceAll('{{MODEL}}','test-model')
 await page.route('**/*',route=>{const url=new URL(route.request().url()); if(url.pathname==='/')return route.fulfill({contentType:'text/html',body:html}); if(url.pathname==='/v1/status')return route.fulfill({json:{window_open:false,serving_workers:0,next_window_start:'2026-10-16T17:30:00Z'}});if(url.pathname==='/v1/pricing')return route.fulfill({json:{input_msat_per_1k_tokens:1,output_msat_per_1k_tokens:2}});return route.abort()})
 await page.goto('/');await expect(page.getByRole('heading',{name:'Nocturne',exact:true})).toBeVisible();await expect(page.getByLabel('Amount',{exact:true})).toBeVisible();expect(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth)).toBe(true)
 await expect(page.locator('link[rel="icon"]')).toHaveAttribute('href',/^data:image\/svg\+xml,/)
 await page.screenshot({path:`test-results/brand-public-${colorScheme}-${width}.png`,fullPage:true})
})
for(const surface of ['public','admin']) for(const width of [320,1440]) test(`system theme reacts without reload ${surface} ${width}`,async({page})=>{
 await page.setViewportSize({width,height:900});await page.emulateMedia({colorScheme:'light'})
 if(surface==='public'){
 const html=(await readFile(new URL('../../gateway/landing.html',import.meta.url),'utf8')).replaceAll('{{SERVICE}}','nocturne').replaceAll('{{MODEL}}','test-model')
 await page.route('**/*',r=>r.request().url().endsWith('/')?r.fulfill({contentType:'text/html',body:html}):r.abort());await page.goto('/')
 }else{await page.goto('/admin/');await page.evaluate(()=>localStorage.removeItem('theme'));await page.reload()}
 const bg=()=>page.evaluate(()=>getComputedStyle(document.body).backgroundColor)
 await expect.poll(bg).toBe('rgb(243, 243, 243)')
 const doc=await page.evaluate(()=>performance.timeOrigin)
 await page.emulateMedia({colorScheme:'dark'});await expect.poll(bg).toBe('rgb(5, 17, 17)')
 expect(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth)).toBe(true)
 await page.emulateMedia({colorScheme:'light'});await expect.poll(bg).toBe('rgb(243, 243, 243)')
 expect(await page.evaluate(()=>performance.timeOrigin)).toBe(doc)
 expect(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth)).toBe(true)
})
test('selected original option 2 is an exact 90 degree counterclockwise pixel transpose',async({page})=>{
 const original=await readFile(new URL('../../docs/brand-source/option-2-original-crop.png',import.meta.url))
 const rotated=await readFile(new URL('../src/assets/nocturne-selected.png',import.meta.url))
 const selection=JSON.parse(await readFile(new URL('../../docs/brand-source/selection.json',import.meta.url),'utf8'))
 expect(selection.sourceSheetSha256).toBe('7dd66bc456780c1e9829644ff46ab8c485685818fee56a6ffd57380a1a7744f3')
 expect(createHash('sha256').update(rotated).digest('hex')).toBe('35957fdac390a8754998942c0df0ae4a2750ff78cee6540419fa2436701b7c01')
 await page.goto('/admin/')
 expect(await page.evaluate(async({a,b})=>{
 const pixels=async(base64:string)=>{const image=new Image();image.src='data:image/png;base64,'+base64;await image.decode();const canvas=document.createElement('canvas');canvas.width=canvas.height=368;const ctx=canvas.getContext('2d')!;ctx.drawImage(image,0,0);return ctx.getImageData(0,0,368,368).data}
 const source=await pixels(a),dest=await pixels(b);for(let y=0;y<368;y++)for(let x=0;x<368;x++)for(let c=0;c<4;c++)if(dest[(y*368+x)*4+c]!==source[(x*368+367-y)*4+c])return false;return true
 },{a:original.toString('base64'),b:rotated.toString('base64')})).toBe(true)
})
