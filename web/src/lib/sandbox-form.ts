import { z } from 'zod'

import type { CreateSandboxRequest } from '@/api'

// The create form covers exactly the fields api.md §8 lists; resources,
// secrets and labels stay CLI-only. Inputs hold strings, so the numeric fields
// coerce.
export const createSandboxSchema = z.object({
  name: z.string().trim().min(1, 'Enter a name'),
  snapshot: z.string(),
  cpu: z.coerce.number().positive('CPU must be greater than zero'),
  memoryMiB: z.coerce.number().int().positive('Memory must be greater than zero'),
  gpuCount: z.coerce.number().int().min(0, 'GPU count cannot be negative'),
  gpuModel: z.string(),
  workspaceId: z.string(),
  workspaceMode: z.enum(['rw', 'ro']),
  idleStop: z.coerce.number().int().min(0, 'Idle stop cannot be negative'),
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
