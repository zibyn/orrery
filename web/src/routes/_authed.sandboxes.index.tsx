import { useQuery } from '@tanstack/react-query'
import { createFileRoute, Link } from '@tanstack/react-router'
import { Box, ChevronRight, Plus } from 'lucide-react'
import { useState } from 'react'

import { listAll, type Sandbox, type User } from '@/api'
import { PageHeader } from '@/components/page-header'
import { StatusBadge } from '@/components/status-badge'
import { buttonVariants } from '@/components/ui/button'
import { Empty, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from '@/components/ui/empty'
import { Field, FieldLabel } from '@/components/ui/field'
import {
  Item,
  ItemActions,
  ItemContent,
  ItemDescription,
  ItemGroup,
  ItemMedia,
  ItemTitle,
} from '@/components/ui/item'
import { NativeSelect, NativeSelectOption } from '@/components/ui/native-select'
import { Spinner } from '@/components/ui/spinner'
import { useLocale } from '@/lib/i18n'
import { workspaceModeLabel } from '@/lib/labels'
import { sandboxListPollInterval } from '@/lib/sandbox'
import { cn } from '@/lib/utils'
import { m } from '@/paraglide/messages'

export const Route = createFileRoute('/_authed/sandboxes/')({
  component: SandboxesPage,
})

function SandboxesPage() {
  useLocale() // re-render on a language switch (see lib/i18n.tsx)
  const { user } = Route.useRouteContext()
  // An admin's list defaults to their own Sandboxes: without `owner` the API
  // already answers with the caller's, so '' means "me" (api.md §8).
  const [owner, setOwner] = useState('')

  const sandboxes = useQuery({
    queryKey: ['sandboxes', owner],
    queryFn: () =>
      listAll<Sandbox>(`/sandboxes${owner ? `?owner=${encodeURIComponent(owner)}` : ''}`),
    refetchInterval: (query) => sandboxListPollInterval(query.state.data),
  })
  const users = useQuery({
    queryKey: ['users'],
    queryFn: () => listAll<User>('/users'),
    enabled: user.is_admin,
  })

  const count = sandboxes.data?.length

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        title={m.sandboxes_title()}
        description={m.sandboxes_description()}
        meta={
          count === undefined
            ? undefined
            : count === 1
              ? m.sandboxes_count_one({ count })
              : m.sandboxes_count_other({ count })
        }
        actions={
          <>
            {user.is_admin && (
              <Field orientation="horizontal" className="w-auto">
                <FieldLabel htmlFor="sandbox-owner" className="whitespace-nowrap">
                  {m.sandboxes_owner()}
                </FieldLabel>
                <NativeSelect
                  id="sandbox-owner"
                  className="min-w-32"
                  value={owner}
                  onChange={(e) => setOwner(e.target.value)}
                >
                  <NativeSelectOption value="">{m.sandboxes_owner_me({ name: user.name })}</NativeSelectOption>
                  {users.data?.map((item) => (
                    <NativeSelectOption key={item.id} value={item.id}>
                      {item.name}
                    </NativeSelectOption>
                  ))}
                </NativeSelect>
              </Field>
            )}
            <Link
              to="/sandboxes/new"
              className={cn(buttonVariants())}
            >
              <Plus data-icon="inline-start" />
              {m.sandboxes_new()}
            </Link>
          </>
        }
      />

      {sandboxes.isPending && (
        <div className="flex items-center gap-2 py-8 text-sm text-muted-foreground">
          <Spinner />
          {m.sandboxes_loading()}
        </div>
      )}

      {sandboxes.data?.length === 0 && (
        <Empty className="border">
          <EmptyHeader>
            <EmptyMedia variant="icon">
              <Box />
            </EmptyMedia>
            <EmptyTitle>{m.sandboxes_empty_title()}</EmptyTitle>
            <EmptyDescription>{m.sandboxes_empty_description()}</EmptyDescription>
          </EmptyHeader>
        </Empty>
      )}

      {sandboxes.data && sandboxes.data.length > 0 && (
        <ItemGroup>
          {sandboxes.data.map((sandbox) => (
            // The listitem wraps the row so the row itself keeps its link role.
            <div key={sandbox.id} role="listitem">
              <Item
                variant="outline"
                render={<Link to="/sandboxes/$id" params={{ id: sandbox.id }} />}
              >
                <ItemMedia variant="icon">
                  <Box />
                </ItemMedia>
                <ItemContent>
                  <ItemTitle>{sandbox.name}</ItemTitle>
                  <ItemDescription>
                    {sandbox.cpu} CPU · {(sandbox.memory / 1024 / 1024).toFixed(0)} MiB
                    {sandbox.gpu ? ` · ${sandbox.gpu.count} GPU` : ''}
                    {sandbox.workspace
                      ? ` · ${m.sandboxes_meta_workspace({ mode: workspaceModeLabel(sandbox.workspace.mode) })}`
                      : ''}
                  </ItemDescription>
                </ItemContent>
                <ItemActions>
                  <StatusBadge state={sandbox.state} data-testid={`sandbox-state-${sandbox.id}`} />
                  <ChevronRight className="text-muted-foreground/50" />
                </ItemActions>
              </Item>
            </div>
          ))}
        </ItemGroup>
      )}
    </div>
  )
}
