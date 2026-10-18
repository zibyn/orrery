import { describe, expect, it } from 'vitest'

import { buildCreateSandboxBody, createSandboxSchema } from './sandbox-form'

const valid = {
  name: '  demo  ',
  snapshot: '',
  cpu: '1',
  memoryMiB: '512',
  gpuCount: '0',
  gpuModel: '',
  workspaceId: '',
  workspaceMode: 'rw',
  idleStop: '900',
}

function parse(overrides: Record<string, string> = {}) {
  const result = createSandboxSchema.safeParse({ ...valid, ...overrides })
  if (!result.success) throw new Error(result.error.issues[0]?.message)
  return result.data
}

describe('createSandboxSchema', () => {
  it('accepts the form defaults', () => {
    const parsed = parse()
    expect(parsed.name).toBe('demo')
    expect(parsed.cpu).toBe(1)
    expect(parsed.memoryMiB).toBe(512)
  })

  it('rejects a missing name and non-positive resources', () => {
    expect(createSandboxSchema.safeParse({ ...valid, name: '  ' }).success).toBe(false)
    expect(createSandboxSchema.safeParse({ ...valid, cpu: '0' }).success).toBe(false)
    expect(createSandboxSchema.safeParse({ ...valid, memoryMiB: '0' }).success).toBe(false)
    expect(createSandboxSchema.safeParse({ ...valid, idleStop: '-1' }).success).toBe(false)
    expect(createSandboxSchema.safeParse({ ...valid, gpuCount: '-1' }).success).toBe(false)
    expect(createSandboxSchema.safeParse({ ...valid, cpu: 'many' }).success).toBe(false)
  })
})

describe('buildCreateSandboxBody', () => {
  it('converts MiB to bytes and leaves the optional parts out', () => {
    expect(buildCreateSandboxBody(parse())).toEqual({
      name: 'demo',
      cpu: 1,
      memory: 512 * 1024 * 1024,
      idle_stop: 900,
    })
  })

  it('includes a snapshot, a GPU model and a read-only Workspace when chosen', () => {
    expect(
      buildCreateSandboxBody(
        parse({
          snapshot: 'default',
          gpuCount: '2',
          gpuModel: 'A100',
          workspaceId: 'ws-id',
          workspaceMode: 'ro',
          idleStop: '0',
        }),
      ),
    ).toEqual({
      name: 'demo',
      snapshot: 'default',
      cpu: 1,
      memory: 512 * 1024 * 1024,
      gpu: { count: 2, model: 'A100' },
      workspace: { id: 'ws-id', mode: 'ro' },
      idle_stop: 0,
    })
  })

  it('omits the GPU model when the count is set but no model is chosen', () => {
    expect(buildCreateSandboxBody(parse({ gpuCount: '1' })).gpu).toEqual({ count: 1 })
  })
})
