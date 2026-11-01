import { z } from 'zod'

import type { CreateSnapshotVersionRequest } from '@/api'
import { m } from '@/paraglide/messages'

// The new-version form covers build and import from api.md §2; build contexts
// stay CLI-only (api.md §8). Validation messages resolve in the current language.
const secretRow = z
  .object({ id: z.string().trim(), name: z.string().trim() })
  .refine((row) => !row.id === !row.name, { error: () => m.snapshot_form_secret_incomplete() })

const buildSchema = z.object({
  source: z.literal('build'),
  dockerfile: z.string().refine((text) => text.trim() !== '', {
    error: () => m.snapshot_form_dockerfile_required(),
  }),
  base: z.string(),
  secrets: z.array(secretRow),
})

// The server's `invalid` stays authoritative; this only catches unpinned refs early.
const importSchema = z.object({
  source: z.literal('import'),
  ref: z.string().trim().includes('@sha256:', { error: () => m.snapshot_form_ref_sha256() }),
  authSecret: z.string(),
})

export const snapshotVersionSchema = z.discriminatedUnion('source', [buildSchema, importSchema])

export type SnapshotVersionFormValues = z.infer<typeof snapshotVersionSchema>

export function buildSnapshotVersionBody(
  values: SnapshotVersionFormValues,
): CreateSnapshotVersionRequest {
  if (values.source === 'import') {
    return {
      import: { ref: values.ref, ...(values.authSecret ? { auth_secret: values.authSecret } : {}) },
    }
  }
  const secrets = Object.fromEntries(
    values.secrets.filter((row) => row.id).map((row) => [row.id, row.name]),
  )
  return {
    build: {
      dockerfile: values.dockerfile,
      ...(values.base ? { base: values.base } : {}),
      ...(Object.keys(secrets).length > 0 ? { secrets } : {}),
    },
  }
}
