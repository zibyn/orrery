import { z } from 'zod'

import type { CreateSandboxRequest } from '@/api'
import { m } from '@/paraglide/messages'

// The create form covers exactly the fields api.md §8 lists; resources,
// secrets and labels stay CLI-only. Inputs hold strings, so the numeric fields
// coerce. Validation messages resolve in the console's current language.
export const createSandboxSchema = z.object({
  name: z.string().trim().min(1, { error: () => m.sandbox_form_name_required() }),
  snapshot: z.string(),
  cpu: z.coerce.number().positive({ error: () => m.sandbox_form_cpu_positive() }),
  memoryMiB: z.coerce.number().int().positive({ error: () => m.sandbox_form_memory_positive() }),
  gpuCount: z.coerce.number().int().min(0, { error: () => m.sandbox_form_gpu_nonnegative() }),
  gpuModel: z.string(),
  workspaceId: z.string(),
  workspaceMode: z.enum(['rw', 'ro']),
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
    ...(values.workspaceId
      ? { workspace: { id: values.workspaceId, mode: values.workspaceMode } }
      : {}),
  }
}
