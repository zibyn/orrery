import { z } from 'zod'

import type { CreateSandboxRequest } from '@/api'
import { m } from '@/paraglide/messages'

// The create form covers exactly the fields api.md §8 lists; resources,
// secrets and labels stay CLI-only. Inputs hold strings, so the numeric fields
// coerce. Validation messages resolve in the console's current language.

const RESERVED_MOUNT_DIRS = ['/resources', '/proc', '/sys', '/dev', '/.orrery']

function within(path: string, dir: string): boolean {
  return path === dir || path.startsWith(`${dir}/`)
}

// The server's rules for volumes[].path (api.md §3, mountPathProblem in
// internal/api/volumes.go), checked against the mounts listed before it.
export function volumeMountProblem(path: string, earlier: string[]): string | undefined {
  const segments = path.split('/').slice(1)
  if (path === '/') return m.sandbox_form_volume_path_root()
  if (!path.startsWith('/') || segments.some((s) => s === '' || s === '.' || s === '..')) {
    return m.sandbox_form_volume_path_invalid()
  }
  const reserved = RESERVED_MOUNT_DIRS.find((dir) => within(path, dir))
  if (reserved) return m.sandbox_form_volume_path_reserved({ dir: reserved })
  const other = earlier.find((o) => within(path, o) || within(o, path))
  if (other) return m.sandbox_form_volume_path_overlap({ other })
  return undefined
}

const volumeMountsSchema = z
  .array(z.object({ volume: z.string(), path: z.string().trim() }))
  .superRefine((mounts, ctx) => {
    mounts.forEach((mount, i) => {
      const earlier = mounts.slice(0, i)
      const problem = !mount.volume
        ? m.sandbox_form_volume_required()
        : earlier.some((o) => o.volume === mount.volume)
          ? m.sandbox_form_volume_duplicate()
          : volumeMountProblem(mount.path, earlier.map((o) => o.path))
      if (problem) ctx.addIssue({ code: 'custom', message: problem, path: [i] })
    })
  })

export const createSandboxSchema = z.object({
  name: z.string().trim().min(1, { error: () => m.sandbox_form_name_required() }),
  snapshot: z.string(),
  cpu: z.coerce.number().positive({ error: () => m.sandbox_form_cpu_positive() }),
  memoryMiB: z.coerce.number().int().positive({ error: () => m.sandbox_form_memory_positive() }),
  gpuCount: z.coerce.number().int().min(0, { error: () => m.sandbox_form_gpu_nonnegative() }),
  gpuModel: z.string(),
  volumes: volumeMountsSchema,
  idleStop: z.coerce.number().int().min(0, { error: () => m.sandbox_form_idle_nonnegative() }),
})

export type CreateSandboxFormValues = z.infer<typeof createSandboxSchema>

export function buildCreateSandboxBody(values: CreateSandboxFormValues): CreateSandboxRequest {
  return {
    name: values.name,
    cpu: values.cpu,
    memory: values.memoryMiB * 1024 * 1024,
    idle_stop: values.idleStop,
    ...(values.snapshot ? { snapshot: values.snapshot } : {}),
    ...(values.gpuCount > 0
      ? { gpu: { count: values.gpuCount, ...(values.gpuModel ? { model: values.gpuModel } : {}) } }
      : {}),
    ...(values.volumes.length > 0 ? { volumes: values.volumes } : {}),
  }
}
