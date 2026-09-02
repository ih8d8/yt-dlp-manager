import { describe, expect, it } from 'vitest'
import { buildOptions, formatLabel, previewArgs } from '../src/components/add-options-dialog'
import type { Format } from '../src/api/types'

const base = {
  mode: 'preset' as const,
  preset: '' as const,
  videoId: '',
  audioId: '',
  container: '',
  audioFormat: '',
  subtitles: '' as const,
  subLangs: '',
  extraArgs: ''
}

describe('buildOptions', () => {
  it('sends nothing when nothing was overridden', () => {
    expect(buildOptions(base)).toBeUndefined()
    expect(previewArgs(undefined)).toContain('Settings')
  })

  it('sends a preset as a ceiling', () => {
    expect(buildOptions({ ...base, preset: '1080p' })).toEqual({ preset: '1080p' })
  })

  it('sends hand-picked streams as ids, not as an expression', () => {
    const o = buildOptions({ ...base, mode: 'streams', videoId: '137', audioId: '140' })
    expect(o).toEqual({ format_id: '137', audio_format_id: '140' })
  })

  it('never pairs audio-only with a video format or a container', () => {
    const o = buildOptions({
      ...base,
      preset: 'audio',
      container: 'mp4',
      audioFormat: 'mp3'
    })
    // merge_container and format_id would both be rejected by the server for
    // an audio-only request, so the dialog must not be able to send them.
    expect(o).toEqual({ preset: 'audio', audio_only: true, audio_format: 'mp3' })
  })

  it('splits subtitle languages and drops the blanks', () => {
    const o = buildOptions({ ...base, subtitles: 'on', subLangs: 'en, , de ' })
    expect(o).toEqual({ subtitles: 'on', sub_langs: ['en', 'de'] })
  })

  it('sends subtitles off explicitly rather than staying silent', () => {
    expect(buildOptions({ ...base, subtitles: 'off' })).toEqual({ subtitles: 'off' })
  })
})

describe('previewArgs', () => {
  it('mirrors the arguments the server builds for chosen streams', () => {
    const o = buildOptions({
      ...base,
      mode: 'streams',
      videoId: '137',
      audioId: '140',
      container: 'mp4'
    })
    expect(previewArgs(o)).toBe('--format 137+140 --merge-output-format mp4')
  })

  it('shows extraction for an audio-only request', () => {
    const o = buildOptions({ ...base, preset: 'audio', audioFormat: 'opus' })
    expect(previewArgs(o)).toBe('--format ba/b --extract-audio --audio-format opus')
  })

  it('shows a preset as a height ceiling with fallbacks', () => {
    expect(previewArgs(buildOptions({ ...base, preset: '720p' }))).toBe(
      '--format bv*[height<=720]+ba/b[height<=720]/wv*[height<=720]+ba/w[height<=720]'
    )
  })

  // yt-dlp has no negation for -x, so the preview must never claim one: a
  // live run failed with "no such option: --no-extract-audio".
  it('never claims an --extract-audio negation that does not exist', () => {
    const shown = [
      previewArgs(buildOptions({ ...base, preset: '1080p' })),
      previewArgs(buildOptions({ ...base, mode: 'streams', videoId: '137' })),
      previewArgs(buildOptions({ ...base, subtitles: 'off' }))
    ].join(' ')
    expect(shown).not.toContain('--no-extract-audio')
  })
})

describe('extra arguments', () => {
  it('sends nothing when the field is blank or whitespace', () => {
    expect(buildOptions({ ...base, extraArgs: '   ' })).toBeUndefined()
  })

  it('carries the text through as typed', () => {
    expect(buildOptions({ ...base, extraArgs: ' --limit-rate 2M ' })).toEqual({
      extra_args: '--limit-rate 2M'
    })
  })

  // They are the last word on the command line, which is what makes a
  // per-download argument able to override the same option set globally.
  it('previews them last, after every structured choice', () => {
    const o = buildOptions({
      ...base,
      preset: '720p',
      container: 'mp4',
      extraArgs: '--limit-rate 2M'
    })
    expect(previewArgs(o)).toBe(
      '--format bv*[height<=720]+ba/b[height<=720]/wv*[height<=720]+ba/w[height<=720]' +
        ' --merge-output-format mp4 --limit-rate 2M'
    )
  })
})

describe('stream picker classification', () => {
  // A progressive format already carries audio; asking to merge a second audio
  // stream into it yields a duplicate track or a merge failure.
  it('drops the audio pick when the video stream already has audio', () => {
    const o = buildOptions({
      ...base,
      mode: 'streams',
      videoId: '18',
      audioId: '',
      container: 'mp4'
    })
    expect(o).toEqual({ format_id: '18', merge_container: 'mp4' })
  })

  // Audio-only in streams mode is the chosen stream as-is: no merge container,
  // and deliberately no --extract-audio, which would re-encode what was picked.
  it('sends an audio stream on its own without a container or extraction', () => {
    const o = buildOptions({ ...base, mode: 'streams', videoId: '', audioId: '140' })
    expect(o).toEqual({ audio_format_id: '140' })
    expect(previewArgs(o)).toBe('--format 140')
  })
})

describe('formatLabel', () => {
  const f = (over: Partial<Format>): Format => ({
    format_id: 'x',
    has_video: false,
    has_audio: false,
    ...over
  })

  it('marks an estimated size so it does not read as a promise', () => {
    const label = formatLabel(
      f({
        format_id: '137',
        has_video: true,
        resolution: '1920x1080',
        ext: 'mp4',
        filesize: 133000000,
        filesize_approximate: true
      })
    )
    expect(label).toContain('~')
    expect(label).toContain('[137]')
  })

  it('does not mark a known size', () => {
    const label = formatLabel(
      f({ format_id: '140', has_audio: true, ext: 'm4a', filesize: 10271496, tbr: 129 })
    )
    expect(label).not.toContain('~')
    expect(label).toContain('129k')
  })
})
