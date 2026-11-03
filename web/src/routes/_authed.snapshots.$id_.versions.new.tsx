import { useQuery, useQueryClient } from '@tanstack/react-query'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { useState } from 'react'

import { api, type Snapshot, type SnapshotVersion } from '@/api'
import { Breadcrumbs } from '@/components/breadcrumbs'
import { PageHeader } from '@/components/page-header'
import { Problem } from '@/components/problem'
import { SnapshotVersionForm } from '@/components/snapshot-version-form'
import { useLocale } from '@/lib/i18n'
import { buildSnapshotVersionBody, formFieldsFromRequest } from '@/lib/snapshot-form'
import { m } from '@/paraglide/messages'

export const Route = createFileRoute('/_authed/snapshots/$id_/versions/new')({
  // from: the version whose request fills the form.
  validateSearch: (search: Record<string, unknown>): { from?: string } => ({
    from: typeof search.from === 'string' ? search.from : undefined,
  }),
  component: NewSnapshotVersionPage,
})

function NewSnapshotVersionPage() {
  useLocale() // re-render on a language switch (see lib/i18n.tsx)
  const { id } = Route.useParams()
  const { from } = Route.useSearch()
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  // One key per form mount: repeated submits cannot create a second version (api.md §2).
  const [idempotencyKey] = useState(() => crypto.randomUUID())

  const snapshot = useQuery({
    queryKey: ['snapshot', id],
    queryFn: () => api<Snapshot>('GET', `/snapshots/${id}`),
  })
  const source = useQuery({
    queryKey: ['snapshot-version', id, from],
    queryFn: () => api<SnapshotVersion>('GET', `/snapshots/${id}/versions/${from}`),
    enabled: from !== undefined,
  })
  const name = snapshot.data?.name ?? id
  const loadError = snapshot.error ?? source.error

  return (
    <div className="flex max-w-3xl flex-col gap-6">
      <PageHeader
        above={
          <Breadcrumbs
            items={[
              { label: m.nav_snapshots(), to: '/snapshots' },
              { label: name, to: '/snapshots/$id', params: { id } },
              { label: m.snapshot_new_version() },
            ]}
          />
        }
        title={m.snapshot_new_version()}
        description={
          from ? m.version_new_from_description({ version: from }) : m.version_new_description()
        }
      />

      {loadError && (
        <p className="text-sm text-destructive" role="alert">
          <Problem error={loadError} />
        </p>
      )}

      {(from === undefined || source.data) && (
        <SnapshotVersionForm
          key={from ?? ''}
          initial={formFieldsFromRequest(source.data?.request)}
          snapshotId={id}
          onCancel={() => navigate({ to: '/snapshots/$id', params: { id } })}
          onSubmit={async (values) => {
            const created = await api<SnapshotVersion>(
              'POST',
              `/snapshots/${id}/versions`,
              buildSnapshotVersionBody(values),
              { 'Idempotency-Key': idempotencyKey },
            )
            await queryClient.invalidateQueries({ queryKey: ['snapshot-versions', id] })
            await queryClient.invalidateQueries({ queryKey: ['snapshot', id] })
            void navigate({ to: '/snapshots/$id', params: { id }, search: { version: created.id } })
          }}
        />
      )}
    </div>
  )
}
