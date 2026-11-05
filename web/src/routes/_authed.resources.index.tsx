import { useQuery } from '@tanstack/react-query'
import { createFileRoute, Link } from '@tanstack/react-router'
import { Package } from 'lucide-react'

import { api, type ListResponse, type Resource } from '@/api'
import { PageHeader } from '@/components/page-header'
import { Problem } from '@/components/problem'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Empty, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from '@/components/ui/empty'
import {
  Item,
  ItemActions,
  ItemContent,
  ItemDescription,
  ItemGroup,
  ItemMedia,
  ItemTitle,
} from '@/components/ui/item'
import { Spinner } from '@/components/ui/spinner'
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
        <div className="flex items-center gap-2 py-8 text-sm text-muted-foreground">
          <Spinner />
          {m.common_loading()}
        </div>
      )}
      {resources.error && (
        <Alert variant="destructive">
          <AlertDescription>
            {m.resources_load_error()}: <Problem error={resources.error} />
          </AlertDescription>
        </Alert>
      )}

      {resources.data?.items.length === 0 && (
        <Empty className="border">
          <EmptyHeader>
            <EmptyMedia variant="icon">
              <Package />
            </EmptyMedia>
            <EmptyTitle>{m.resources_empty_title()}</EmptyTitle>
            <EmptyDescription>{m.resources_empty_description()}</EmptyDescription>
          </EmptyHeader>
        </Empty>
      )}

      {resources.data && resources.data.items.length > 0 && (
        <ItemGroup>
          {resources.data.items.map((resource) => (
            // The row itself is the link, so the listitem role goes on a wrapper.
            <div key={resource.id} role="listitem">
              <Item
                variant="outline"
                data-testid={`resource-${resource.id}`}
                render={<Link to="/resources/$id" params={{ id: resource.id }} />}
              >
                <ItemMedia variant="icon">
                  <Package />
                </ItemMedia>
                <ItemContent>
                  <ItemTitle>{resource.name}</ItemTitle>
                  <ItemDescription>
                    {m.common_created({ time: formatTimestamp(resource.created_at) })}
                  </ItemDescription>
                </ItemContent>
                <ItemActions className="hidden flex-col items-end gap-0.5 sm:flex">
                  <span className="text-xs text-muted-foreground">
                    {m.resources_current_version()}
                  </span>
                  {resource.current_version_id ? (
                    <span className="font-mono text-xs">{resource.current_version_id.slice(0, 8)}</span>
                  ) : (
                    <span className="text-xs text-muted-foreground">{m.snapshot_no_current()}</span>
                  )}
                </ItemActions>
              </Item>
            </div>
          ))}
        </ItemGroup>
      )}
    </div>
  )
}
