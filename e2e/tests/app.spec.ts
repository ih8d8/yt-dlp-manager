import { expect, test } from '@playwright/test'
import { ADMIN_PASSWORD, ROUTES, collectCspViolations, cspViolations, signIn } from './helpers'

test.describe.configure({ mode: 'serial' })

test('first run asks for a password and no setup token over loopback', async ({ page }) => {
  await collectCspViolations(page)
  await page.goto('/')

  await expect(page.getByRole('heading', { name: 'Set administrator password' })).toBeVisible()
  // The token is only demanded of a client the server cannot see as local; a
  // browser on the same machine must never be asked for one.
  await expect(page.locator('#setup-token')).toHaveCount(0)

  await page.locator('#new-password').fill('short')
  await expect(page.getByRole('button', { name: 'Set password' })).toBeDisabled()

  await page.locator('#new-password').fill(ADMIN_PASSWORD)
  await page.locator('#confirm-password').fill('something-else')
  await expect(page.getByText('Passwords do not match')).toBeVisible()

  await page.locator('#confirm-password').fill(ADMIN_PASSWORD)
  await page.getByRole('button', { name: 'Set password' }).click()

  await expect(page.getByRole('heading', { name: 'Queue', level: 1 })).toBeVisible()
  expect(await cspViolations(page)).toEqual([])
})

test('every route renders under the strict CSP with no inline styles', async ({ page }) => {
  await collectCspViolations(page)
  await signIn(page)

  for (const route of ROUTES) {
    await page.goto(`/${route}`)
    await expect(page.getByRole('main')).toBeVisible()
    // Layout belongs in the stylesheet, not in a style attribute. Preact
    // applies a string style prop via CSSOM, which slips past style-src, so
    // this is the only place the rule can be enforced.
    expect(await page.locator('[style]').count(),
      `${route} rendered an inline style attribute; move it to a CSS class`).toBe(0)
  }

  expect(await cspViolations(page), 'CSP violations across the app').toEqual([])
})

test('About omits unavailable build metadata and obsolete footer copy', async ({ page }) => {
  await signIn(page)
  await page.goto('/#/settings')
  await page.getByRole('button', { name: 'About' }).click()

  const build = page.locator('.field-row').filter({ hasText: 'yt-dlp-manager' }).locator('span').nth(1)
  await expect(build).not.toContainText('unknown')
  await expect(page.getByText('Config source:', { exact: false })).toHaveCount(0)
  await expect(page.getByText('Not affiliated with YouTube', { exact: false })).toHaveCount(0)
})

test('the session cookie is HttpOnly and SameSite=Strict', async ({ page }) => {
  await signIn(page)
  const cookie = (await page.context().cookies()).find((c) => c.name === 'ytdlp_session')
  expect(cookie, 'session cookie').toBeDefined()
  expect(cookie!.httpOnly).toBe(true)
  expect(cookie!.sameSite).toBe('Strict')
})

test('a malformed URL is refused with visible feedback', async ({ page }) => {
  await collectCspViolations(page)
  await signIn(page)

  await page.locator('#add-url').fill('not a url')
  await page.getByRole('button', { name: 'Add download' }).click()

  await expect(page.getByRole('alert')).toBeVisible()
  expect(await cspViolations(page)).toEqual([])
})

test('restart pauses all unfinished work and queue actions behave cleanly', async ({ page }) => {
  await signIn(page)

  const rows = page.locator('.queue-list > li').filter({ has: page.locator('.st-paused') })
  await expect(rows).toHaveCount(2)
  await expect(page.locator('.st-queued')).toHaveCount(0)

  const requests: Array<{ action: string; ids: string[] }> = []
  await page.route('**/api/v1/downloads/actions', async (route) => {
    const body = route.request().postDataJSON() as { action: string; ids: string[] }
    requests.push(body)
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ results: body.ids.map((id) => ({ id, ok: true })) })
    })
  })

  await page.getByRole('button', { name: 'Resume all paused (2)' }).click()
  await expect(page.getByText('Resumed 2 paused downloads')).toBeVisible()
  expect(requests[0].action).toBe('resume')
  expect([...requests[0].ids].sort()).toEqual(['e2e-active', 'e2e-queued'])

  await page.getByRole('checkbox', { name: 'Select Recovered queued item' }).check()
  await page.getByRole('checkbox', { name: 'Select Recovered active item' }).check()
  await expect(page.getByRole('toolbar', { name: 'Selection actions' })).toContainText('2 selected')
  await page.getByRole('button', { name: 'Force start/resume', exact: true }).click()

  await expect(page.getByRole('toolbar', { name: 'Selection actions' })).toHaveCount(0)
  await expect(page.getByRole('checkbox', { name: 'Select Recovered queued item' })).not.toBeChecked()
  await expect(page.getByRole('checkbox', { name: 'Select Recovered active item' })).not.toBeChecked()
  expect(requests[1].action).toBe('start_now')
  expect([...requests[1].ids].sort()).toEqual(['e2e-active', 'e2e-queued'])
})

test('download details open as a wide centered dialog', async ({ page }) => {
  await signIn(page)
  await page.getByRole('button', { name: 'Recovered queued item', exact: true }).click()

  const dialog = page.getByRole('dialog', { name: 'Download details' })
  await expect(dialog).toBeVisible()
  const box = await dialog.boundingBox()
  const viewport = page.viewportSize()
  expect(box).not.toBeNull()
  expect(viewport).not.toBeNull()
  expect(box!.width).toBeGreaterThanOrEqual(700)
  expect(Math.abs(box!.x + box!.width / 2 - viewport!.width / 2)).toBeLessThanOrEqual(2)
  expect(Math.abs(box!.y + box!.height / 2 - viewport!.height / 2)).toBeLessThanOrEqual(2)
  const overflow = await dialog.evaluate((el) => el.scrollHeight - el.clientHeight)
  expect(overflow, `details dialog overflows vertically by ${overflow}px`).toBeLessThanOrEqual(0)
  const preview = dialog.locator('.thumb-large')
  await expect(preview).toBeVisible()
  expect((await preview.boundingBox())!.width).toBeLessThanOrEqual(600)
  // A live download adds a Started row. Keep enough headroom for that exact
  // transition rather than only checking the shorter recovered-paused fixture.
  const liveOverflow = await dialog.locator('.detail-list').evaluate((list) => {
    const label = document.createElement('dt')
    label.textContent = 'Started'
    const value = document.createElement('dd')
    value.textContent = new Date().toLocaleString()
    list.append(label, value)
    const panel = list.closest('[role="dialog"]') as HTMLElement
    return panel.scrollHeight - panel.clientHeight
  })
  expect(liveOverflow, `live details dialog overflows by ${liveOverflow}px`).toBeLessThanOrEqual(0)
  await expect(page.getByTitle('Force start/resume').first()).toBeVisible()
})

test('logout, a wrong password, then a real sign-in', async ({ page }) => {
  await signIn(page)

  await page.getByRole('button', { name: 'Log out' }).first().click()
  await expect(page.locator('#password')).toBeVisible()

  await page.locator('#password').fill('not-the-admin-password')
  await page.getByRole('button', { name: 'Log in' }).click()
  await expect(page.getByRole('alert')).toContainText('invalid password')
  await expect(page.getByRole('heading', { name: 'Queue', level: 1 })).toHaveCount(0)

  // A single failure already arms the exponential login backoff, so the next
  // attempt — correct password or not — is refused for a moment. Retrying
  // until it expires is the honest way to test this: an immediate assertion
  // would be asserting that the rate limiter does not work.
  await expect(async () => {
    await page.locator('#password').fill(ADMIN_PASSWORD)
    await page.getByRole('button', { name: 'Log in' }).click()
    await expect(page.getByRole('heading', { name: 'Queue', level: 1 })).toBeVisible({ timeout: 2000 })
  }).toPass({ timeout: 30000 })
})

// A failed row is a two-column layout (everything it knows, then its actions)
// living in a list whose other rows have eight. Several breakpoints re-state
// .row-main's column list, and matching specificity meant the later rule won:
// at this very viewport the title and the error reason were squeezed into a
// 20px column, one character per line. Measured, not screenshotted, so the
// check says what actually went wrong.
test('a failed row gives its title and reason the full row width', async ({ page }) => {
  await signIn(page)

  const failed = page.locator('.row-failed')
  await expect(failed).toHaveCount(1)
  await expect(failed.getByText('Broken item')).toBeVisible()

  // Both widths that the specificity clash broke: the 1151-1320px desktop
  // band and the narrow-phone band under 430px.
  for (const width of [1280, 400]) {
    await page.setViewportSize({ width, height: 900 })
    const row = (await failed.boundingBox())!
    const meta = (await failed.locator('.row-meta').boundingBox())!
    expect(meta.width, `at ${width}px the failed row's text column collapsed`)
      .toBeGreaterThan(row.width * 0.6)

    // The reason reads as a line of prose, not a column of single characters.
    const reason = (await failed.locator('.text-danger').boundingBox())!
    expect(reason.width, `at ${width}px the failure reason wrapped per character`)
      .toBeGreaterThan(reason.height)
    // And the title is not ellipsised down to a letter or two.
    await expect(failed.locator('.row-title')).toHaveText('Broken item')
  }
})

// The per-state clear buttons. They must sit to the LEFT of the broader
// "Clear finished", narrow to a single state, and — like every clear — leave
// unfinished work alone. Kept last in the file: it consumes the seeded
// terminal rows, and the specs share one server.
test('clear completed and clear failed narrow the queue clear', async ({ page }) => {
  await signIn(page)

  const toolbar = page.getByRole('toolbar', { name: 'Queue-wide actions' })
  const labels = await toolbar.getByRole('button').allInnerTexts()
  expect(labels.map((l) => l.replace(/\s*\(\d+\)$/, ''))).toEqual([
    'Resume all paused',
    'Clear completed',
    'Clear failed',
    'Clear finished',
    'Clear all'
  ])

  // The scope goes to the server as a name, never as a list of ids: that is
  // what keeps a row which finished mid-click from being left behind.
  const scopes: string[] = []
  page.on('request', (req) => {
    if (req.url().endsWith('/api/v1/downloads/clear') && req.method() === 'POST') {
      scopes.push((req.postDataJSON() as { scope: string }).scope)
    }
  })

  await expect(toolbar.getByRole('button', { name: 'Clear failed (1)' })).toBeEnabled()
  await toolbar.getByRole('button', { name: 'Clear failed (1)' }).click()
  await expect(page.getByText('Cleared 1 failed entry')).toBeVisible()
  await expect(page.getByRole('region', { name: 'Failed downloads' })).toHaveCount(0)

  // The completed row is untouched by clearing failures, and still counted.
  const clearCompleted = toolbar.getByRole('button', { name: 'Clear completed (1)' })
  await expect(clearCompleted).toBeEnabled()
  await clearCompleted.click()
  await expect(page.getByText('Cleared 1 completed entry')).toBeVisible()

  expect(scopes).toEqual(['failed', 'completed'])

  // Both narrow buttons are now empty and disabled, and the recovery-paused
  // rows this suite depends on survived every clear.
  await expect(toolbar.getByRole('button', { name: 'Clear completed' })).toBeDisabled()
  await expect(toolbar.getByRole('button', { name: 'Clear failed' })).toBeDisabled()
  await expect(page.locator('.queue-list > li').filter({ has: page.locator('.st-paused') })).toHaveCount(2)
})

// The proxy path is keyed by item id alone, so without a version in the query
// a re-probed thumbnail would keep the byte-identical src: Preact would leave
// the attribute untouched, and a request that did go out would be answered
// from the browser's own cache for the 24 hours the response advertises. This
// asserts the request the browser actually makes, which is the half a unit
// test cannot see.
test('the thumbnail request carries a version derived from the probed url', async ({ page }) => {
  const asked: string[] = []
  await page.route('**/api/v1/downloads/*/thumbnail*', async (route) => {
    asked.push(route.request().url())
    await route.fulfill({ status: 502, body: '' })
  })
  await signIn(page)

  await expect(async () => {
    expect(asked.length).toBeGreaterThan(0)
  }).toPass({ timeout: 5000 })

  const versions = asked.map((u) => new URL(u).searchParams.get('v'))
  expect(versions.every((v) => v && /^[0-9a-z]+$/.test(v))).toBe(true)

  // A failed fetch falls back to the inline placeholder rather than a broken
  // image, and does not retry on every render.
  const before = asked.length
  await page.getByRole('button', { name: 'Recovered queued item', exact: true }).click()
  await page.getByRole('button', { name: 'Close details' }).click()
  expect(asked.length).toBe(before)
  const src = await page.locator('.row-main').filter({ hasText: 'Recovered active item' })
    .locator('.thumb img').getAttribute('src')
  expect(src).toMatch(/^data:image\/svg\+xml/)
})
