import { z } from 'zod'

import type { CreateSnapshotVersionRequest } from '@/api'
import { m } from '@/paraglide/messages'

// The new-version form covers build and import from api.md §2; build contexts
// are set with the CLI (api.md §8) and only carried over from an earlier
// version. Validation messages resolve in the current language.
const secretRow = z
  .object({ id: z.string().trim(), name: z.string().trim() })
  .refine((row) => !row.id === !row.name, { error: () => m.snapshot_form_secret_incomplete() })

const buildSchema = z.object({
  source: z.literal('build'),
  dockerfile: z.string().refine((text) => text.trim() !== '', {
    error: () => m.snapshot_form_dockerfile_required(),
  }),
  base: z.string(),
  contexts: z.record(z.string(), z.string()),
  secrets: z.array(secretRow),
})

// The server's `invalid` stays authoritative; it resolves a tag to its digest.
const importSchema = z.object({
  source: z.literal('import'),
  ref: z.string().trim().min(1, { error: () => m.snapshot_form_ref_required() }),
  authSecret: z.string(),
})

export const snapshotVersionSchema = z.discriminatedUnion('source', [buildSchema, importSchema])

export type SnapshotVersionFormValues = z.infer<typeof snapshotVersionSchema>

// The form's fields hold both sources at once, so switching source keeps input.
export type SnapshotVersionFormFields = {
  source: 'build' | 'import'
  dockerfile: string
  base: string
  contexts: Record<string, string>
  secrets: { id: string; name: string }[]
  ref: string
  authSecret: string
}

// formFieldsFromRequest fills the form from the body an earlier version was
// created from; no body gives an empty build form.
export function formFieldsFromRequest(
  request: CreateSnapshotVersionRequest | undefined,
): SnapshotVersionFormFields {
  const fields: SnapshotVersionFormFields = {
    source: 'build',
    dockerfile: '',
    base: '',
    contexts: {},
    secrets: [],
    ref: '',
    authSecret: '',
  }
  if (request && 'import' in request) {
    return {
      ...fields,
      source: 'import',
      ref: request.import.ref,
      authSecret: request.import.auth_secret ?? '',
    }
  }
  if (request && 'build' in request) {
    const { dockerfile, base, contexts, secrets } = request.build
    return {
      ...fields,
      dockerfile,
      base: base ?? '',
      contexts: contexts ?? {},
      secrets: Object.entries(secrets ?? {}).map(([id, name]) => ({ id, name })),
    }
  }
  return fields
}

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
      ...(Object.keys(values.contexts).length > 0 ? { contexts: values.contexts } : {}),
      ...(Object.keys(secrets).length > 0 ? { secrets } : {}),
    },
  }
}
