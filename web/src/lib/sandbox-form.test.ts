import { describe, expect, it } from 'vitest'

import { m } from '@/paraglide/messages'

import { buildCreateSandboxBody, createSandboxSchema, volumeMountProblem } from './sandbox-form'

const valid = {
  name: '  demo  ',
  snapshot: '',
  cpu: '1',
  memoryMiB: '512',
  gpuCount: '0',
  gpuModel: '',
  volumes: [] as { volume: string; path: string }[],
  idleStop: '900',
}

function parse(overrides: Partial<typeof valid> = {}) {
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

  it('includes a snapshot, a GPU model and Volume mounts when chosen', () => {
    expect(
      buildCreateSandboxBody(
        parse({
          snapshot: 'default',
          gpuCount: '2',
          gpuModel: 'A100',
          volumes: [
            { volume: 'vol-a', path: '/workspace' },
            { volume: 'vol-b', path: ' /data ' },
          ],
          idleStop: '0',
        }),
      ),
    ).toEqual({
      name: 'demo',
      snapshot: 'default',
      cpu: 1,
      memory: 512 * 1024 * 1024,
      gpu: { count: 2, model: 'A100' },
      volumes: [
        { volume: 'vol-a', path: '/workspace' },
        { volume: 'vol-b', path: '/data' },
      ],
      idle_stop: 0,
    })
  })

  it('omits the GPU model when the count is set but no model is chosen', () => {
    expect(buildCreateSandboxBody(parse({ gpuCount: '1' })).gpu).toEqual({ count: 1 })
  })
})

describe('volumeMountProblem', () => {
  it('accepts absolute, normalized paths, /workspace included', () => {
    expect(volumeMountProblem('/workspace', [])).toBeUndefined()
    expect(volumeMountProblem('/data/models', ['/workspace'])).toBeUndefined()
    expect(volumeMountProblem('/resourcesx', [])).toBeUndefined()
  })

  it('rejects what the server rejects (api.md §3 volumes)', () => {
    for (const path of ['', 'data', '/data/', '/a//b', '/a/./b', '/a/../b']) {
      expect(volumeMountProblem(path, [])).toBe(m.sandbox_form_volume_path_invalid())
    }
    expect(volumeMountProblem('/', [])).toBe(m.sandbox_form_volume_path_root())
    for (const dir of ['/resources', '/proc', '/sys', '/dev', '/.orrery']) {
      expect(volumeMountProblem(`${dir}/x`, [])).toBe(m.sandbox_form_volume_path_reserved({ dir }))
      expect(volumeMountProblem(dir, [])).toBe(m.sandbox_form_volume_path_reserved({ dir }))
    }
  })

  it('rejects a path containing or inside another mount', () => {
    expect(volumeMountProblem('/data/x', ['/data'])).toBe(
      m.sandbox_form_volume_path_overlap({ other: '/data' }),
    )
    expect(volumeMountProblem('/data', ['/data/x'])).toBe(
      m.sandbox_form_volume_path_overlap({ other: '/data/x' }),
    )
    expect(volumeMountProblem('/data', ['/data'])).toBe(
      m.sandbox_form_volume_path_overlap({ other: '/data' }),
    )
  })
})

describe('createSandboxSchema volumes', () => {
  const issue = (volumes: { volume: string; path: string }[]) => {
    const result = createSandboxSchema.safeParse({ ...valid, volumes })
    return result.success ? undefined : result.error.issues[0]?.message
  }

  it('needs a Volume chosen on every row', () => {
    expect(issue([{ volume: '', path: '/data' }])).toBe(m.sandbox_form_volume_required())
  })

  it('reports a bad or overlapping path', () => {
    expect(issue([{ volume: 'a', path: 'data' }])).toBe(m.sandbox_form_volume_path_invalid())
    expect(
      issue([
        { volume: 'a', path: '/data' },
        { volume: 'b', path: '/data/x' },
      ]),
    ).toBe(m.sandbox_form_volume_path_overlap({ other: '/data' }))
  })

  it('mounts one Volume once', () => {
    expect(
      issue([
        { volume: 'a', path: '/one' },
        { volume: 'a', path: '/two' },
      ]),
    ).toBe(m.sandbox_form_volume_duplicate())
  })
})
