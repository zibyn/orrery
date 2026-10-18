import { describe, expect, it } from 'vitest'

import { expiresAtFromInput, formatTimestamp, scopeOptions } from './tokens'

describe('formatTimestamp', () => {
  it('formats an RFC3339 timestamp in UTC', () => {
    expect(formatTimestamp('2026-10-18T12:34:56Z')).toBe('2026-10-18 12:34:56 UTC')
    expect(formatTimestamp('2026-10-18T12:34:56.123Z')).toBe('2026-10-18 12:34:56 UTC')
  })

  it('shows a dash for a missing value', () => {
    expect(formatTimestamp(null)).toBe('—')
    expect(formatTimestamp(undefined)).toBe('—')
    expect(formatTimestamp('')).toBe('—')
  })

  it('returns an unparseable value unchanged', () => {
    expect(formatTimestamp('not-a-date')).toBe('not-a-date')
  })
})

describe('expiresAtFromInput', () => {
  it('converts a datetime-local value to RFC3339', () => {
    // The input has no timezone, so the expected value is computed from the
    // same local wall-clock time without reusing the implementation.
    const input = '2027-01-02T03:04'
    const expected = new Date(2027, 0, 2, 3, 4, 0, 0).toISOString()
    expect(expiresAtFromInput(input)).toBe(expected)
  })

  it('treats an empty input as an optional value', () => {
    expect(expiresAtFromInput('')).toBeUndefined()
  })

  it('rejects an unparseable value', () => {
    expect(expiresAtFromInput('nope')).toBeUndefined()
  })
})

describe('scopeOptions', () => {
  it('offers every API scope, most privileged last', () => {
    expect(scopeOptions).toEqual(['read_only', 'user', 'admin'])
  })
})
