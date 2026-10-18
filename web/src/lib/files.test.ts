import { describe, expect, it } from 'vitest'

import {
  archivePath,
  downloadHref,
  fileContentPath,
  filesPath,
  formatBytes,
  joinPath,
  normalizePath,
  parentPath,
} from './files'

describe('normalizePath', () => {
  it('treats empty and "." as the Workspace root', () => {
    expect(normalizePath('')).toBe('')
    expect(normalizePath('.')).toBe('')
    expect(normalizePath('./')).toBe('')
  })

  it('strips a leading "./" and trailing slashes', () => {
    expect(normalizePath('./a/b')).toBe('a/b')
    expect(normalizePath('a/b/')).toBe('a/b')
    expect(normalizePath('./a/')).toBe('a')
  })
})

describe('joinPath', () => {
  it('appends a child to the current directory', () => {
    expect(joinPath('', 'notes.txt')).toBe('notes.txt')
    expect(joinPath('docs', 'notes.txt')).toBe('docs/notes.txt')
    expect(joinPath('./docs/', 'notes.txt')).toBe('docs/notes.txt')
  })
})

describe('parentPath', () => {
  it('moves one level up and stops at the root', () => {
    expect(parentPath('')).toBe('')
    expect(parentPath('notes.txt')).toBe('')
    expect(parentPath('docs/notes.txt')).toBe('docs')
    expect(parentPath('docs/')).toBe('')
    expect(parentPath('a/b/c')).toBe('a/b')
  })
})

describe('formatBytes', () => {
  it('uses binary units, matching the rest of the console', () => {
    expect(formatBytes(0)).toBe('0 B')
    expect(formatBytes(1023)).toBe('1023 B')
    expect(formatBytes(1024)).toBe('1.0 KiB')
    expect(formatBytes(1536)).toBe('1.5 KiB')
    expect(formatBytes(1024 * 1024)).toBe('1.0 MiB')
    expect(formatBytes(5 * 1024 ** 3)).toBe('5.0 GiB')
  })

  it('renders unavailable sizes as a dash', () => {
    expect(formatBytes(-1)).toBe('—')
    expect(formatBytes(Number.NaN)).toBe('—')
  })
})

describe('API paths', () => {
  it('builds a directory listing path with an encoded query', () => {
    expect(filesPath('abc', '')).toBe('/sandboxes/abc/files?path=')
    expect(filesPath('abc', './dir with space')).toBe(
      '/sandboxes/abc/files?path=dir%20with%20space',
    )
  })

  it('builds content and archive paths', () => {
    expect(fileContentPath('abc', 'a/b.txt')).toBe(
      '/sandboxes/abc/files/content?path=a%2Fb.txt',
    )
    expect(archivePath('abc', 'docs')).toBe('/sandboxes/abc/files/archive?path=docs')
  })

  it('prefixes same-origin download links with /api/v1', () => {
    expect(downloadHref('file', 'abc', 'dir/a b.txt')).toBe(
      '/api/v1/sandboxes/abc/files/content?path=dir%2Fa%20b.txt',
    )
    expect(downloadHref('dir', 'abc', 'dir')).toBe(
      '/api/v1/sandboxes/abc/files/archive?path=dir',
    )
  })
})
