import { describe, expect, it } from 'vitest'

import { buildSnapshotVersionBody, snapshotVersionSchema } from './snapshot-form'

const valid = {
  source: 'build',
  dockerfile: 'FROM scratch\n',
  base: '',
  secrets: [] as { id: string; name: string }[],
  ref: '',
  authSecret: '',
}

function parse(overrides: Record<string, unknown> = {}) {
  const result = snapshotVersionSchema.safeParse({ ...valid, ...overrides })
  if (!result.success) throw new Error(result.error.issues[0]?.message)
  return result.data
}

describe('buildSnapshotVersionBody', () => {
  it('sends only the Dockerfile for a bare build', () => {
    expect(buildSnapshotVersionBody(parse())).toEqual({ build: { dockerfile: 'FROM scratch\n' } })
  })

  it('includes a base and the filled secret rows, dropping empty ones', () => {
    expect(
      buildSnapshotVersionBody(
        parse({
          base: 'default',
          secrets: [
            { id: 'npm', name: 'npm-token' },
            { id: '', name: '' },
            { id: ' pip ', name: 'pip-conf' },
          ],
        }),
      ),
    ).toEqual({
      build: {
        dockerfile: 'FROM scratch\n',
        base: 'default',
        secrets: { npm: 'npm-token', pip: 'pip-conf' },
      },
    })
  })

  it('sends an import ref, with the auth Secret only when chosen', () => {
    const ref = 'docker.io/library/alpine@sha256:abc'
    expect(buildSnapshotVersionBody(parse({ source: 'import', ref, dockerfile: '' }))).toEqual({
      import: { ref },
    })
    expect(
      buildSnapshotVersionBody(parse({ source: 'import', ref: ` ${ref} `, authSecret: 'hub' })),
    ).toEqual({ import: { ref, auth_secret: 'hub' } })
  })
})

describe('snapshotVersionSchema', () => {
  it('requires a Dockerfile for a build', () => {
    expect(snapshotVersionSchema.safeParse({ ...valid, dockerfile: '  ' }).success).toBe(false)
  })

  it('requires an import ref pinned by @sha256:', () => {
    const imp = { ...valid, source: 'import' }
    expect(snapshotVersionSchema.safeParse({ ...imp, ref: '' }).success).toBe(false)
    expect(snapshotVersionSchema.safeParse({ ...imp, ref: 'alpine:3' }).success).toBe(false)
    expect(snapshotVersionSchema.safeParse({ ...imp, ref: 'alpine@sha256:abc' }).success).toBe(true)
  })

  it('rejects a secret row with only one side filled', () => {
    expect(
      snapshotVersionSchema.safeParse({ ...valid, secrets: [{ id: 'npm', name: '' }] }).success,
    ).toBe(false)
  })
})
