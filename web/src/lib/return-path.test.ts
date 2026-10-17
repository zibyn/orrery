import { describe, expect, it } from 'vitest'

import { safeReturn } from './return-path'

describe('safeReturn', () => {
  it('keeps an in-console path', () => {
    expect(safeReturn('/cli-auth')).toBe('/cli-auth')
    expect(safeReturn('/sandboxes?state=ready')).toBe('/sandboxes?state=ready')
  })

  it('falls back to the console root for anything that leaves the origin', () => {
    expect(safeReturn(undefined)).toBe('/')
    expect(safeReturn('')).toBe('/')
    expect(safeReturn('https://evil.example')).toBe('/')
    expect(safeReturn('//evil.example')).toBe('/')
    expect(safeReturn('/\\evil.example')).toBe('/')
    expect(safeReturn('cli-auth')).toBe('/')
  })
})
