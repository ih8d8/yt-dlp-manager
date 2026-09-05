import { describe, expect, it } from 'vitest'
import { thumbKey, thumbSource } from '../src/components/thumb'

const item = (over: Partial<{ id: string; thumbnail_url?: string; started_at: string | null }> = {}) => ({
  id: 'abc123',
  thumbnail_url: 'https://img.example/a.jpg',
  started_at: '2026-01-01T00:00:00Z',
  ...over
})

describe('thumbSource', () => {
  it('requests the proxy when the item has a probed thumbnail', () => {
    expect(thumbSource(item(), null)).toMatch(/^\/api\/v1\/downloads\/abc123\/thumbnail\?v=[0-9a-z]+$/)
  })

  it('falls back to the placeholder when nothing was probed', () => {
    expect(thumbSource(item({ thumbnail_url: undefined }), null)).toBeNull()
  })

  it('stops asking for an image that just failed to load', () => {
    const d = item()
    expect(thumbSource(d, thumbKey(d))).toBeNull()
  })

  // The original regression: a row that failed while probing has no
  // thumbnail, and a retry re-probes and produces one. Latching on "something
  // failed" left that new preview permanently unrequested.
  it('asks again once a retry has probed a different thumbnail', () => {
    const before = item({ thumbnail_url: 'https://img.example/old.jpg' })
    const after = item({ thumbnail_url: 'https://img.example/new.jpg' })
    expect(thumbSource(after, thumbKey(before))).not.toBeNull()
  })

  // The src string itself has to change, or Preact leaves the attribute alone
  // and the browser answers from its own cache for the 24 hours the response
  // advertises — the tagged file on the server never gets asked for.
  it('gives a re-probed thumbnail a different src', () => {
    const before = thumbSource(item({ thumbnail_url: 'https://img.example/old.jpg' }), null)
    const after = thumbSource(item({ thumbnail_url: 'https://img.example/new.jpg' }), null)
    expect(after).not.toBe(before)
  })

  it('keeps one stable src for one thumbnail url', () => {
    expect(thumbSource(item(), null)).toBe(thumbSource(item({ started_at: 'later' }), null))
  })

  // A retry usually re-probes to the SAME url, so keying the latch on the url
  // alone would leave a transient 502 or timeout showing the placeholder for
  // the component's whole life. Each download attempt gets one fresh try.
  it('asks again on a new attempt even when the thumbnail url is unchanged', () => {
    const first = item({ started_at: '2026-01-01T00:00:00Z' })
    const retried = item({ started_at: '2026-01-02T09:30:00Z' })
    expect(thumbSource(retried, thumbKey(first))).not.toBeNull()
  })

  // ...but not on every render inside one attempt, which would hammer a
  // genuinely missing thumbnail.
  it('does not retry within the same attempt', () => {
    const d = item()
    const failed = thumbKey(d)
    expect(thumbSource({ ...d }, failed)).toBeNull()
  })

  it('escapes the id into the path', () => {
    expect(thumbSource(item({ id: 'a/b' }), null)).toContain('/api/v1/downloads/a%2Fb/thumbnail?v=')
  })
})
