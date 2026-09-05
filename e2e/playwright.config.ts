import { defineConfig, devices } from '@playwright/test'
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

// The server under test is the real binary, serving the bundle in
// internal/webui/static that `go build` embedded into it. That embedding is at
// compile time, so a frontend change is invisible here until BOTH the bundle
// and the binary are rebuilt — `make e2e` does the two in order. Point
// YTM_BINARY somewhere else to test a different build.
const binary = process.env.YTM_BINARY ?? '../bin/yt-dlp-manager'
const port = Number(process.env.YTM_E2E_PORT ?? 18422)

// A private state/config root per run: without it the suite would pick up the
// developer's own config.json — including allow_unauthenticated, which would
// skip the first-run flow these tests exist to cover — and their real queue.
//
// The unix control socket lives here too, so the directory has to stay well
// inside the 108-byte sun_path limit; the system temp dir does, a deep path
// under the repository may not.
const runtimeDir = mkdtempSync(join(tmpdir(), 'ytm-e2e-'))
process.env.YTM_E2E_RUNTIME_DIR = runtimeDir

// Seed the real persistence file with both kinds of unfinished work, plus one
// row in each terminal state. The server must expose the unfinished pair as
// recovery-paused after boot; if either one is left queued, its scheduler will
// start it and the browser regression test fails. The terminal rows are what
// the per-state "Clear …" buttons act on, and they are seeded rather than
// downloaded so the suite never needs the network.
const addedAt = '2026-01-02T03:04:05Z'
writeFileSync(join(runtimeDir, 'state.json'), JSON.stringify({
  version: 1,
  saved_at: addedAt,
  items: [
    {
      id: 'e2e-queued',
      url: 'https://example.com/recovered-queued',
      title: 'Recovered queued item',
      state: 'queued',
      progress: 0,
      added_at: addedAt
    },
    {
      id: 'e2e-active',
      url: 'https://example.com/recovered-active',
      title: 'Recovered active item',
      // A probed thumbnail, so the browser actually requests the proxy. The
      // fetch itself cannot succeed here (the SSRF dialer refuses anything
      // that is not public unicast), which is fine: what this seed exists to
      // exercise is the URL the page asks for.
      thumb_url: 'https://img.example/recovered-active.jpg',
      state: 'downloading',
      progress: 42,
      got: 420,
      total: 1000,
      added_at: addedAt
    },
    {
      id: 'e2e-completed',
      url: 'https://example.com/finished-item',
      title: 'Finished item',
      state: 'completed',
      progress: 100,
      added_at: addedAt,
      done_at: addedAt
    },
    {
      id: 'e2e-failed',
      url: 'https://example.com/broken-item',
      title: 'Broken item',
      state: 'failed',
      error: 'HTTP Error 404: Not Found',
      added_at: addedAt,
      done_at: addedAt
    }
  ],
  order: ['e2e-queued']
}), { mode: 0o600 })

if (!process.env.YTM_E2E_KEEP_STATE) {
  process.on('exit', () => rmSync(runtimeDir, { recursive: true, force: true }))
}

export default defineConfig({
  testDir: './tests',
  fullyParallel: false,
  // One server, one admin account: the specs walk a single install from first
  // run onwards, so they must not race each other for it.
  workers: 1,
  forbidOnly: !!process.env.CI,
  retries: 0,
  reporter: process.env.CI ? [['github'], ['list']] : [['list']],
  use: {
    baseURL: `http://127.0.0.1:${port}`,
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure'
  },
  projects: [
    { name: 'desktop', use: { ...devices['Desktop Chrome'] }, testIgnore: /responsive\.spec\.ts/ },
    { name: 'mobile', use: { ...devices['Pixel 7'] }, testMatch: /responsive\.spec\.ts/ }
  ],
  webServer: {
    command: `"${binary}" server --listen 127.0.0.1:${port} --state "${runtimeDir}/state.json" --socket "${runtimeDir}/ipc.sock"`,
    url: `http://127.0.0.1:${port}/healthz`,
    reuseExistingServer: false,
    // The server's access log is noise in a passing run; its stderr still
    // surfaces startup failures.
    stdout: 'ignore',
    stderr: 'pipe',
    env: {
      XDG_CONFIG_HOME: join(runtimeDir, 'config'),
      XDG_STATE_HOME: join(runtimeDir, 'state'),
      XDG_CACHE_HOME: join(runtimeDir, 'cache')
    }
  }
})
