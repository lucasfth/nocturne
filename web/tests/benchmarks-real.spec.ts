import { test, expect, type Page } from '@playwright/test'

// Actual Go routes + durable temporary store; no route mocks. All worker reports
// are SYNTHETIC test evidence, not inference/performance or real Apply validation.
// Parent Playwright configuration launches go-benchmark-server.go on loopback.
const base = 'http://127.0.0.1:5201'
const headers = { Authorization: 'Bearer browser-test' }
const path = (machine: string) => `/admin/machines/${machine}/benchmarks`

async function open(page: Page, machine: string) {
 await page.goto(base + '/admin')
 if (await page.getByLabel('Admin secret', { exact: true }).isVisible()) {
  await page.getByLabel('Admin secret', { exact: true }).fill('browser-test')
  await page.getByRole('button', { name: 'Open', exact: true }).click()
 }
 const card = page.locator('[data-slot="card"]').filter({ has: page.getByRole('heading', { name: machine, exact: true }) })
 await card.getByRole('button', { name: 'Benchmark & optimise', exact: true }).click()
 const dialog = page.getByRole('dialog')
 await expect(dialog.getByRole('button', { name: 'Reload history', exact: true })).toBeEnabled()
 return dialog
}

test('real Go synthetic lifecycle: success, explicit Apply, cancellation, recovery and unsupported binding', async ({ page, request }) => {
 test.setTimeout(90_000)
 const errors: string[] = []
 page.on('pageerror', error => errors.push(error.message))
 const latest = async (machine: string) => {
  const response = await request.get(base + path(machine), { headers })
  expect(response.status()).toBe(200)
  const jobs = await response.json()
  expect(JSON.stringify(jobs)).not.toContain('"lease"')
  return jobs[0]
 }
 expect((await request.get(base + path('fixture-success'))).status()).toBe(401)
 let dialog = await open(page, 'fixture-success')
 await dialog.getByRole('button', { name: 'Run benchmark', exact: true }).click()
 await expect(dialog.getByRole('status').filter({ hasText: 'Benchmark: completed' })).toBeVisible({ timeout: 20_000 })
 const completed = await latest('fixture-success')
 expect(completed).toMatchObject({ state: 'completed', restored: true, summary: { quality: 'pass', tokens_per_second: 50 } })
 expect(completed.action).toBeUndefined()
 await dialog.getByRole('button', { name: 'Apply recommendation', exact: true }).click()
 await expect(page.getByRole('alertdialog')).toContainText('32,768 tokens per slot (including output) · 2 parallel slots · 65,536 aggregate context tokens')
 expect((await latest('fixture-success')).action).toBeUndefined()
 await page.getByRole('button', { name: 'Confirm apply', exact: true }).click()
 await expect(dialog.getByRole('status').filter({ hasText: 'Apply: completed' })).toBeVisible({ timeout: 20_000 })
 expect(await latest('fixture-success')).toMatchObject({ id: completed.id, action: 'apply', result: 'applied', summary: completed.summary })

 dialog = await open(page, 'fixture-cancel')
 await dialog.getByRole('button', { name: 'Run benchmark', exact: true }).click()
 await expect(dialog.getByRole('status').filter({ hasText: 'Benchmark: running' })).toBeVisible({ timeout: 20_000 })
 await dialog.getByRole('button', { name: 'Cancel job', exact: true }).click()
 await expect(dialog.getByRole('status').filter({ hasText: 'Benchmark: cancelled' })).toBeVisible({ timeout: 20_000 })
 expect(await latest('fixture-cancel')).toMatchObject({ state: 'cancelled', restored: true })
 await expect(dialog.getByRole('button', { name: 'Run benchmark', exact: true })).toBeEnabled()
 await expect(dialog.getByRole('button', { name: 'Apply recommendation', exact: true })).toHaveCount(0)

 dialog = await open(page, 'fixture-recovery')
 // Retain recovery fencing if the test is retried against the same disposable server.
 if (!(await latest('fixture-recovery'))) {
  await dialog.getByRole('button', { name: 'Run benchmark', exact: true }).click()
 }
 await expect(dialog.getByRole('status').filter({ hasText: 'Benchmark: recovery required' })).toBeVisible({ timeout: 20_000 })
 await expect(dialog.getByRole('alert')).toContainText('Original service state is not verified')
 await expect(dialog.getByRole('button', { name: 'Run benchmark', exact: true })).toBeDisabled()
 expect(await latest('fixture-recovery')).toMatchObject({ state: 'recovery_required', restored: false, failure: 'restoration' })
 expect((await request.post(base + path('fixture-recovery'), { headers, data: {} })).status()).toBe(409)

 dialog = await open(page, 'production-unbound')
 const before = await latest('production-unbound')
 await dialog.getByRole('button', { name: 'Apply recommendation', exact: true }).click()
 const refused = page.waitForResponse(r => r.url().endsWith(`/${before.id}/apply`) && r.request().method() === 'POST')
 await page.getByRole('button', { name: 'Confirm apply', exact: true }).click()
 expect((await refused).status()).toBe(409)
 await expect(dialog.getByRole('alert')).toContainText('Apply unavailable or refused')
 expect(await latest('production-unbound')).toEqual(before)
 await dialog.getByRole('button', { name: 'Reload history', exact: true }).click()
 await dialog.getByRole('button', { name: 'Run benchmark', exact: true }).click()
 await expect(dialog.getByRole('alert')).toContainText('no secure benchmark adapter')
 expect(await latest('production-unbound')).toEqual(before)
 expect(errors).toEqual([])
})
