import { test, expect, type Page } from '@playwright/test'
import { readFile } from 'node:fs/promises'
import summary from '../fixtures/summary.json' with { type: 'json' }
import machines from '../fixtures/machines.json' with { type: 'json' }
async function login(page: Page) {
 await page.goto('/admin/')
 await page.getByLabel('Admin secret', { exact: true }).fill('browser-test')
 await page.getByRole('button', { name: 'Open', exact: true }).click()
}
async function noOverflow(page: Page) { expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true) }
test('canonical icons are byte-identical and served under the real admin route with image types', async ({ page, request }) => {
 await page.goto('http://127.0.0.1:5200/admin')
 const icons = await page.locator('link[rel="icon"], link[rel="apple-touch-icon"]').evaluateAll(nodes => nodes.map(node => ({ href: node.getAttribute('href')!, type: node.getAttribute('type') })))
 expect(icons).toHaveLength(4)
 for (const icon of icons) {
  expect(icon.href).toMatch(/^\/admin\/assets\/((favicon.*|apple-touch-icon.*)\.png|nocturne-.*\.svg)$/)
  const response = await request.get('http://127.0.0.1:5200' + icon.href)
  expect(response.status()).toBe(200)
  expect(response.headers()['content-type']).toMatch(/image\//)
  const source = icon.href.endsWith('.svg') ? 'nocturne.svg' : ['favicon-16x16', 'favicon-32x32', 'apple-touch-icon'].find(name => icon.href.includes('/' + name + '-'))! + '.png'
  expect(await response.body()).toEqual(await readFile(new URL('../src/assets/' + source, import.meta.url)))
  if (icon.href.endsWith('.png') && icon.type) expect(icon.type).toBe('image/png')
 }
})
test('loading, unavailable and genuinely empty registries are distinct and retryable', async ({ page }) => {
 await page.route('**/admin/summary', route => route.fulfill({ json: summary }))
 let release!: () => void
 const gate = new Promise<void>(resolve => { release = resolve })
 let fail = true
 await page.route('**/admin/machines', async route => { await gate; await route.fulfill(fail ? {status:503,json:{detail:'registry unavailable'}} : {json:[]}) })
 await login(page)
 await expect(page.getByText('Loading machines…')).toBeVisible()
 await expect(page.getByText('No machines yet', { exact:true })).toHaveCount(0)
 release()
 await expect(page.getByText('Machine registry unavailable. Retry above to load your fleet.')).toBeVisible()
 await expect(page.getByText('No machines yet', { exact:true })).toHaveCount(0)
 fail = false
 await page.getByRole('button', {name:'Retry',exact:true}).click()
 await expect(page.getByRole('heading', {name:'No machines yet'})).toBeVisible()
 await page.getByRole('button', {name:'Register your first Mac'}).click()
 await expect(page.getByRole('dialog')).toBeVisible()
})
for (const width of [320, 390, 1440]) test(`responsive ${width}px fleet, settings navigation and keyboard-safe dialog`, async ({ page }) => {
 const errors: string[]=[]
 page.on('pageerror', error => errors.push(error.message))
 await page.setViewportSize({width,height:900})
 await page.route('**/admin/summary', route => route.fulfill({json:summary}))
 await page.route('**/admin/machines', route => route.fulfill({json:machines.map((machine,index)=>({...machine,enabled:index>0}))}))
 await page.route('**/admin/config', route => route.fulfill({status:503,json:{detail:'unavailable'}}))
 await login(page)
 await expect(page.getByRole('heading',{name:machines[0]!.worker_id,exact:true})).toBeVisible()
 await expect(page.locator('[data-slot="card"]').filter({has:page.getByRole('heading',{name:machines[0]!.worker_id,exact:true})})).toContainText('disabled')
 await noOverflow(page)
 await page.screenshot({path:`test-results/polish-fleet-${width}.png`,fullPage:true})
 await page.getByRole('tab',{name:'Settings',exact:true}).click()
 await expect(page.getByRole('button',{name:'Reload latest (keep edits)',exact:true})).toBeVisible()
 await noOverflow(page)
 await page.screenshot({path:`test-results/polish-settings-${width}.png`,fullPage:true})
 await page.getByRole('tab',{name:/Machines/}).click()
 const add=page.getByRole('button',{name:'Add machine',exact:true})
 await add.click()
 const dialog=page.getByRole('dialog')
 await expect(dialog).toBeVisible()
 await expect(page.getByLabel('Paste setup output',{exact:true})).toBeFocused()
 await noOverflow(page)
 await page.screenshot({path:`test-results/polish-dialog-${width}.png`,fullPage:true})
 for(let i=0;i<18;i++) { await page.keyboard.press('Tab'); expect(await dialog.evaluate(node=>node.contains(document.activeElement))).toBe(true) }
 await page.keyboard.press('Escape')
 await expect(dialog).toHaveCount(0)
 await expect(add).toBeFocused()
 expect(errors).toEqual([])
})
