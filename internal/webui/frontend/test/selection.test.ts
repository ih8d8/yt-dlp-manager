import { describe, expect, it } from 'vitest'
import { initialState, rangeBetween, reduce } from '../src/state/downloads'

const ids = ['a', 'b', 'c', 'd', 'e']

describe('rangeBetween', () => {
  it('spans downwards inclusively', () => {
    expect(rangeBetween(ids, 'b', 'd')).toEqual(['b', 'c', 'd'])
  })

  it('spans upwards inclusively', () => {
    expect(rangeBetween(ids, 'd', 'b')).toEqual(['b', 'c', 'd'])
  })

  it('has no range without an anchor, or onto the anchor itself', () => {
    expect(rangeBetween(ids, null, 'c')).toBeNull()
    expect(rangeBetween(ids, 'c', 'c')).toBeNull()
  })

  // Rows leave the queue while the user scrolls (a download finishes, a
  // filter changes). A vanished end must degrade to a plain toggle rather
  // than selecting some other stretch of the list.
  it('has no range when either end has left the list', () => {
    expect(rangeBetween(ids, 'gone', 'c')).toBeNull()
    expect(rangeBetween(ids, 'a', 'gone')).toBeNull()
    expect(rangeBetween([], 'a', 'b')).toBeNull()
  })
})

describe('shift-click selection through the reducer', () => {
  it('selects the whole span, leaving prior selections intact', () => {
    let s = reduce(initialState(), { type: 'toggleSelect', id: 'e' })
    s = reduce(s, { type: 'toggleSelect', id: 'b' })
    s = reduce(s, {
      type: 'selectMany',
      ids: rangeBetween(ids, 'b', 'd')!,
      value: true
    })
    expect([...s.selection].sort()).toEqual(['b', 'c', 'd', 'e'])
  })

  // The row checkbox lets the browser's native toggle stand rather than
  // calling preventDefault (which reverts the tick after Preact has already
  // re-rendered). That is only safe because the clicked row always ends up at
  // exactly the toggle of its previous state — in a range click too, since the
  // clicked row is one end of the span and the span takes its value.
  it('always lands the clicked row on the toggle of its own prior state', () => {
    for (const alreadySelected of [false, true]) {
      const base = alreadySelected
        ? reduce(initialState(), { type: 'toggleSelect', id: 'd' })
        : initialState()
      const span = rangeBetween(ids, 'b', 'd')!
      const after = reduce(base, {
        type: 'selectMany',
        ids: span,
        value: !base.selection.has('d')
      })
      expect(after.selection.has('d')).toBe(!alreadySelected)
    }
  })

  it('clears the whole span when the clicked row was already selected', () => {
    let s = reduce(initialState(), {
      type: 'selectMany',
      ids: ['a', 'b', 'c', 'd'],
      value: true
    })
    s = reduce(s, {
      type: 'selectMany',
      ids: rangeBetween(ids, 'b', 'd')!,
      value: false
    })
    expect([...s.selection]).toEqual(['a'])
  })
})
