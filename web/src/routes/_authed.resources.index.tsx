import { useQuery } from '@tanstack/react-query'
import { createFileRoute, Link } from '@tanstack/react-router'
import { Package } from 'lucide-react'

import { api, type ListResponse, type Resource } from '@/api'
import { EmptyState } from '@/components/empty-state'
import { ListCard, ListRow, listTitleClass } from '@/components/list'
import { PageHeader } from '@/components/page-header'
import { Problem } from '@/components/problem'
import { useLocale } from '@/lib/i18n'
import { formatTimestamp } from '@/lib/tokens'
import { m } from '@/paraglide/messages'

export const Route = createFileRoute('/_authed/resources/')({
  component: ResourcesPage,
})

function ResourcesPage() {
  useLocale() // re-render on a language switch (see lib/i18n.tsx)
  const resources = useQuery({
    queryKey: ['resources'],
    queryFn: () => api<ListResponse<Resource>>('GET', '/resources?limit=200'),
  })

  const count = resources.data?.items.length

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        title={m.resources_title()}
        description={m.resources_description()}
        meta={
          count === undefined
            ? undefined
            : count === 1
              ? m.resources_count_one({ count })
              : m.resources_count_other({ count })
        }
      />

      {resources.isPending && (
        <p className="py-8 text-sm text-muted-foreground">{m.common_loading()}</p>
      )}
      {resources.error && (
        <p className="text-sm text-destructive" role="alert">
          {m.resources_load_error()}: <Problem error={resources.error} />
        </p>
      )}

      {resources.data?.items.length === 0 && (
        <EmptyState
          icon={<Package />}
          title={m.resources_empty_title()}
          description={m.resources_empty_description()}
        />
      )}

      {resources.data && resources.data.items.length > 0 && (
        <ListCard>
          {resources.data.items.map((resource) => (
            <ListRow
              key={resource.id}
              data-testid={`resource-${resource.id}`}
              icon={<Package />}
              title={
                <Link
                  to="/resources/$id"
                  params={{ id: resource.id }}
                  className={`${listTitleClass} hover:underline`}
                >
                  {resource.name}
                </Link>
              }
              description={m.common_created({ time: formatTimestamp(resource.created_at) })}
              trailing={
                <div className="hidden min-w-0 flex-col items-end gap-0.5 sm:flex">
                  <span className="text-[11px] text-muted-foreground">
                    {m.resources_current_version()}
                  </span>
                  <span className="max-w-48 truncate font-mono text-xs">
                    {resource.current_version_id ?? '—'}
                  </span>
                </div>
              }
            />
          ))}
        </ListCard>
      )}
    </div>
  )
}
