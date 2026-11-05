import { useQueryClient } from '@tanstack/react-query'
import { createFileRoute, Link, useNavigate } from '@tanstack/react-router'
import { useState } from 'react'

import { api, type Snapshot, type SnapshotVersion } from '@/api'
import { PageHeader } from '@/components/page-header'
import { SnapshotVersionForm } from '@/components/snapshot-version-form'
import {
  Breadcrumb,
  BreadcrumbItem,
  BreadcrumbLink,
  BreadcrumbList,
  BreadcrumbPage,
  BreadcrumbSeparator,
} from '@/components/ui/breadcrumb'
import { Field, FieldLabel } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { useLocale } from '@/lib/i18n'
import { buildSnapshotVersionBody, formFieldsFromRequest } from '@/lib/snapshot-form'
import { m } from '@/paraglide/messages'

export const Route = createFileRoute('/_authed/snapshots/new')({
  component: NewSnapshotPage,
})

// A Snapshot and its first version in one form. If the version is rejected,
// the Snapshot already exists: a resubmit reuses it and keeps its name.
function NewSnapshotPage() {
  useLocale() // re-render on a language switch (see lib/i18n.tsx)
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const [name, setName] = useState('')
  const [created, setCreated] = useState<Snapshot>()
  // One key per form mount: repeated submits cannot create a second version (api.md §2).
  const [idempotencyKey] = useState(() => crypto.randomUUID())

  return (
    <div className="flex max-w-3xl flex-col gap-6">
      <PageHeader
        above={
          <Breadcrumb aria-label={m.common_breadcrumb()}>
            <BreadcrumbList>
              <BreadcrumbItem>
                <BreadcrumbLink render={<Link to="/snapshots" />}>{m.nav_snapshots()}</BreadcrumbLink>
              </BreadcrumbItem>
              <BreadcrumbSeparator />
              <BreadcrumbItem>
                <BreadcrumbPage>{m.snapshots_new()}</BreadcrumbPage>
              </BreadcrumbItem>
            </BreadcrumbList>
          </Breadcrumb>
        }
        title={m.snapshots_new()}
        description={m.snapshots_new_description()}
      />
      <SnapshotVersionForm
        initial={formFieldsFromRequest(undefined)}
        above={
          <Field>
            <FieldLabel htmlFor="snapshot-name">{m.common_name()}</FieldLabel>
            <Input
              id="snapshot-name"
              value={created?.name ?? name}
              disabled={created !== undefined}
              onChange={(e) => setName(e.target.value)}
              autoComplete="off"
            />
          </Field>
        }
        onCancel={() => navigate({ to: '/snapshots' })}
        onSubmit={async (values) => {
          let snapshot = created
          if (!snapshot) {
            if (!name.trim()) throw new Error(m.snapshots_enter_name())
            snapshot = await api<Snapshot>('POST', '/snapshots', { name: name.trim() })
            setCreated(snapshot)
            await queryClient.invalidateQueries({ queryKey: ['snapshots'] })
          }
          const version = await api<SnapshotVersion>(
            'POST',
            `/snapshots/${snapshot.id}/versions`,
            buildSnapshotVersionBody(values),
            { 'Idempotency-Key': idempotencyKey },
          )
          void navigate({
            to: '/snapshots/$id',
            params: { id: snapshot.id },
            search: { version: version.id },
          })
        }}
      />
    </div>
  )
}
