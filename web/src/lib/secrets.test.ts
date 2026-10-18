import { describe, expect, it } from 'vitest'

import { secretNameError } from './secrets'

describe('secretNameError', () => {
  it('accepts an ordinary name', () => {
    expect(secretNameError('GITHUB_TOKEN')).toBeNull()
    expect(secretNameError('  api-key  ')).toBeNull()
  })

  it('rejects an empty or whitespace-only name', () => {
    expect(secretNameError('')).not.toBeNull()
    expect(secretNameError('   ')).not.toBeNull()
  })

  it('rejects names longer than 255 characters', () => {
    expect(secretNameError('x'.repeat(255))).toBeNull()
    expect(secretNameError('x'.repeat(256))).not.toBeNull()
  })

  it('rejects the reserved names and prefixes the server enforces', () => {
    expect(secretNameError('PATH')).not.toBeNull()
    expect(secretNameError('HOME')).not.toBeNull()
    expect(secretNameError('GIT_ASKPASS')).not.toBeNull()
    expect(secretNameError('LD_PRELOAD')).not.toBeNull()
    expect(secretNameError('STARS_TOKEN')).not.toBeNull()
  })
})
