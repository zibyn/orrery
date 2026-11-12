import { describe, expect, it } from 'vitest'

import type { CreateSnapshotVersionRequest } from '@/api'

import {
  buildSnapshotVersionBody,
  formFieldsFromRequest,
  snapshotVersionSchema,
} from './snapshot-form'

const valid = {
  source: 'build',
  dockerfile: 'FROM scratch\n',
  base: '',
  contexts: {},
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

  it('requires an import ref, by tag or digest', () => {
    const imp = { ...valid, source: 'import' }
    expect(snapshotVersionSchema.safeParse({ ...imp, ref: '' }).success).toBe(false)
    expect(snapshotVersionSchema.safeParse({ ...imp, ref: '  ' }).success).toBe(false)
    expect(snapshotVersionSchema.safeParse({ ...imp, ref: 'alpine:3' }).success).toBe(true)
    expect(snapshotVersionSchema.safeParse({ ...imp, ref: 'alpine@sha256:abc' }).success).toBe(true)
  })

  it('rejects a secret row with only one side filled', () => {
    expect(
      snapshotVersionSchema.safeParse({ ...valid, secrets: [{ id: 'npm', name: '' }] }).success,
    ).toBe(false)
  })

  it('rejects two rows with the same secret id, on the later row', () => {
    const result = snapshotVersionSchema.safeParse({
      ...valid,
      secrets: [
        { id: 'npm', name: 'a' },
        { id: 'pip', name: 'b' },
        { id: ' npm ', name: 'c' },
      ],
    })
    expect(result.success).toBe(false)
    expect(result.error?.issues.map((issue) => issue.path)).toEqual([['secrets', 2, 'id']])
  })

  it('accepts distinct secret ids', () => {
    const secrets = [
      { id: 'npm', name: 'a' },
      { id: 'pip', name: 'a' },
    ]
    expect(snapshotVersionSchema.safeParse({ ...valid, secrets }).success).toBe(true)
  })
})

describe('formFieldsFromRequest', () => {
  it('starts an empty build without an earlier body', () => {
    expect(formFieldsFromRequest(undefined)).toMatchObject({ source: 'build', dockerfile: '' })
  })

  // Resubmitting the filled form must send the earlier body back unchanged.
  it.each<CreateSnapshotVersionRequest>([
    { import: { ref: 'python:3.12', auth_secret: 'hub' } },
    { import: { ref: 'python:3.12' } },
    {
      build: {
        dockerfile: 'FROM base\n',
        base: 'py312',
        contexts: { assets: 'weights@018f0000-0000-7000-8000-000000000001' },
        secrets: { npm: 'npm-token' },
      },
    },
    { build: { dockerfile: 'FROM scratch\n' } },
  ])('round-trips %j', (request) => {
    const parsed = snapshotVersionSchema.parse(formFieldsFromRequest(request))
    expect(buildSnapshotVersionBody(parsed)).toEqual(request)
  })
})
