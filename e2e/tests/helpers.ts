import { expect, type Page } from '@playwright/test'

/** Long enough for the server's minimum, fixed so every spec shares one admin. */
export const ADMIN_PASSWORD = 'e2e-admin-password'

/** Routes the SPA serves, all reachable once signed in. */
export const ROUTES = ['#/queue', '#/library', '#/settings', '#/api'] as const

/**
 * Records every CSP violation the page reports. Registered as an init script so
 * it is listening before the app's first paint, and kept on `window` so it
 * accumulates across the SPA's hash routing (which never reloads the document).
 *
 * This is the class of check unit tests structurally cannot make: whether the
 * policy the server sends and the page the browser builds actually agree.
 */
export async function collectCspViolations(page: Page): Promise<void> {
  await page.addInitScript(() => {
    const w = window as unknown as { __csp?: string[] }
    w.__csp = []
    document.addEventListener('securitypolicyviolation', (e) => {
      w.__csp?.push(`${e.violatedDirective} blocked=${e.blockedURI} @${e.sourceFile}:${e.lineNumber}`)
    })
  })
}

export async function cspViolations(page: Page): Promise<string[]> {
  return page.evaluate(() => (window as unknown as { __csp?: string[] }).__csp ?? [])
}

/**
 * Signs in, completing first-run setup when this is a fresh install. Every spec
 * can call it: the specs share one server, so which screen appears depends on
 * whichever spec ran first.
 */
export async function signIn(page: Page): Promise<void> {
  await page.goto('/')
  const setupField = page.locator('#new-password')
  const loginField = page.locator('#password')
  await expect(setupField.or(loginField).first()).toBeVisible()

  if (await setupField.count()) {
    await setupField.fill(ADMIN_PASSWORD)
    await page.locator('#confirm-password').fill(ADMIN_PASSWORD)
    await page.getByRole('button', { name: 'Set password' }).click()
  } else {
    await loginField.fill(ADMIN_PASSWORD)
    await page.getByRole('button', { name: 'Log in' }).click()
  }
  await expect(page.getByRole('heading', { name: 'Queue', level: 1 })).toBeVisible()
}
