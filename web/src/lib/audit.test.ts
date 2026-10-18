import { describe, expect, it } from 'vitest'

import { auditPath, toRfc3339 } from './audit'

const EMPTY = { actor: '', action: '', target: '', from: '', to: '' }

describe('auditPath', () => {
  it('omits empty filters and the absent cursor', () => {
    expect(auditPath(EMPTY)).toBe('/audit')
    expect(auditPath({ ...EMPTY, target: '   ' }, null)).toBe('/audit')
  })

  it('serializes every filter and encodes reserved characters', () => {
    expect(
      auditPath({
        actor: 'alice',
        action: 'user.create',
        target: '7f6e5d4c',
        from: '2026-10-18T00:00:00Z',
        to: '2026-10-19T00:00:00Z',
      }),
    ).toBe(
      '/audit?actor=alice&action=user.create&target=7f6e5d4c&from=2026-10-18T00%3A00%3A00Z&to=2026-10-19T00%3A00%3A00Z',
    )
  })

  it('echoes next_cursor as the cursor parameter and trims filters', () => {
    expect(auditPath({ ...EMPTY, actor: ' alice ' }, 'cursor-2')).toBe(
      '/audit?actor=alice&cursor=cursor-2',
    )
  })

  it('accepts an explicit page size', () => {
    expect(auditPath(EMPTY, undefined, 25)).toBe('/audit?limit=25')
  })
})

describe('toRfc3339', () => {
  it('turns a datetime-local value into an RFC 3339 timestamp', () => {
    // The wall-clock time the user typed is preserved; only the zone is added.
    expect(toRfc3339('2026-10-18T12:30')).toMatch(
      /^2026-10-18T12:30:00[+-]\d{2}:\d{2}$/,
    )
  })

  it('drops empty and unparseable values', () => {
    expect(toRfc3339('')).toBe('')
    expect(toRfc3339('not-a-date')).toBe('')
  })
})
