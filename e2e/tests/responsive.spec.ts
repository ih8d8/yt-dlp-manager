import { expect, test } from '@playwright/test'
import { ROUTES, collectCspViolations, cspViolations, signIn } from './helpers'

// Runs in the mobile project only (see playwright.config.ts): the handheld
// layout swaps the sidebar for a bottom nav and restacks the queue rows, which
// is exactly where a style attribute that never applied would show up.
test('the handheld layout has a bottom nav and never scrolls sideways', async ({ page }) => {
  await collectCspViolations(page)
  await signIn(page)

  await expect(page.getByRole('navigation', { name: 'Primary mobile' })).toBeVisible()

  for (const route of ROUTES) {
    await page.goto(`/${route}`)
    const overflow = await page.evaluate(
      () => document.documentElement.scrollWidth - document.documentElement.clientWidth
    )
    expect(overflow, `${route} overflows horizontally by ${overflow}px`).toBeLessThanOrEqual(0)
  }

  expect(await cspViolations(page)).toEqual([])
})

test('download details fit and stay centered on a phone', async ({ page }) => {
  await signIn(page)
  await page.getByRole('button', { name: 'Recovered queued item', exact: true }).click()

  const dialog = page.getByRole('dialog', { name: 'Download details' })
  await expect(dialog).toBeVisible()
  const box = await dialog.boundingBox()
  const viewport = page.viewportSize()
  expect(box).not.toBeNull()
  expect(viewport).not.toBeNull()
  expect(box!.x).toBeGreaterThanOrEqual(0)
  expect(box!.y).toBeGreaterThanOrEqual(0)
  expect(box!.x + box!.width).toBeLessThanOrEqual(viewport!.width)
  expect(box!.y + box!.height).toBeLessThanOrEqual(viewport!.height)
  expect(Math.abs(box!.x + box!.width / 2 - viewport!.width / 2)).toBeLessThanOrEqual(2)
  expect(Math.abs(box!.y + box!.height / 2 - viewport!.height / 2)).toBeLessThanOrEqual(2)
})
