import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { createFileRoute, Link } from '@tanstack/react-router'
import { Terminal } from 'lucide-react'
import { useEffect, useState } from 'react'
import type { Terminal as XTerminal } from '@xterm/xterm'

import { api, type Exec, type ExecResultFrame, type ListResponse, type Sandbox } from '@/api'
import { PageHeader } from '@/components/page-header'
import { StatusBadge } from '@/components/status-badge'
import { TerminalView } from '@/components/terminal'
import {
  Breadcrumb,
  BreadcrumbItem,
  BreadcrumbLink,
  BreadcrumbList,
  BreadcrumbPage,
  BreadcrumbSeparator,
} from '@/components/ui/breadcrumb'
import { Button, buttonVariants } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Empty, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from '@/components/ui/empty'
import {
  Item,
  ItemActions,
  ItemContent,
  ItemDescription,
  ItemGroup,
  ItemTitle,
} from '@/components/ui/item'
import { Spinner } from '@/components/ui/spinner'
import { execListPollInterval, followExecOutput, formatExecResult } from '@/lib/exec-stream'
import { useLocale } from '@/lib/i18n'
import { stateLabel } from '@/lib/labels'
import { notifyError } from '@/lib/notify'
import { formatLocalDateTime } from '@/lib/tokens'
import { cn } from '@/lib/utils'
import { m } from '@/paraglide/messages'

export const Route = createFileRoute('/_authed/sandboxes/$id_/execs')({
  validateSearch: (search: Record<string, unknown>): { exec?: string } => ({
    exec: typeof search.exec === 'string' ? search.exec : undefined,
  }),
  component: ExecsPage,
})

function ExecsPage() {
  useLocale() // re-render on a language switch (see lib/i18n.tsx)
  const { id } = Route.useParams()
  const { exec: selectedId } = Route.useSearch()
  const queryClient = useQueryClient()

  // Shares the detail page's cache, so the breadcrumb shows the name.
  const sandbox = useQuery({
    queryKey: ['sandbox', id],
    queryFn: () => api<Sandbox>('GET', `/sandboxes/${id}`),
  })
  const execs = useQuery({
    queryKey: ['execs', id],
    queryFn: () => api<ListResponse<Exec>>('GET', `/sandboxes/${id}/execs`),
    refetchInterval: (query) => execListPollInterval(query.state.data?.items),
  })
  const selected = execs.data?.items.find((exec) => exec.id === selectedId)

  const cancel = useMutation({
    mutationFn: (execId: string) => api<Exec>('POST', `/execs/${execId}/cancel`),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['execs', id] })
    },
  })

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        above={
          <Breadcrumb aria-label={m.common_breadcrumb()}>
            <BreadcrumbList>
              <BreadcrumbItem>
                <BreadcrumbLink render={<Link to="/sandboxes" />}>{m.nav_sandboxes()}</BreadcrumbLink>
              </BreadcrumbItem>
              <BreadcrumbSeparator />
              <BreadcrumbItem>
                <BreadcrumbLink render={<Link to="/sandboxes/$id" params={{ id }} />}>
                  {sandbox.data?.name ?? id}
                </BreadcrumbLink>
              </BreadcrumbItem>
              <BreadcrumbSeparator />
              <BreadcrumbItem>
                <BreadcrumbPage>{m.execs_title()}</BreadcrumbPage>
              </BreadcrumbItem>
            </BreadcrumbList>
          </Breadcrumb>
        }
        title={m.execs_title()}
        actions={
          <Link
            to="/sandboxes/$id/terminal"
            params={{ id }}
            className={cn(buttonVariants({ variant: 'outline' }))}
          >
            <Terminal data-icon="inline-start" />
            {m.execs_open_terminal()}
          </Link>
        }
      />

      {execs.isPending && (
        <div className="flex items-center gap-2 py-8 text-sm text-muted-foreground">
          <Spinner />
          {m.execs_loading()}
        </div>
      )}

      {execs.data?.items.length === 0 && (
        <Empty className="border">
          <EmptyHeader>
            <EmptyMedia variant="icon">
              <Terminal />
            </EmptyMedia>
            <EmptyTitle>{m.execs_empty_title()}</EmptyTitle>
            <EmptyDescription>{m.execs_empty_description()}</EmptyDescription>
          </EmptyHeader>
        </Empty>
      )}

      {execs.data && execs.data.items.length > 0 && (
        <div className="grid items-start gap-4 xl:grid-cols-[22rem_minmax(0,1fr)]">
          <Card className="overflow-hidden">
            <CardHeader>
              <CardTitle>{m.execs_recent()}</CardTitle>
              <CardDescription>{m.execs_recent_description()}</CardDescription>
            </CardHeader>
            <CardContent>
              <ItemGroup className="gap-1">
                {execs.data.items.map((exec) => (
                  <Item
                    key={exec.id}
                    role="listitem"
                    size="sm"
                    className={cn('items-start', selectedId === exec.id ? 'bg-accent' : 'hover:bg-muted/40')}
                  >
                    {/* Two links per row (select, attach), so the row itself is not a link. */}
                    <ItemContent className="min-w-0">
                      <Link
                        to="/sandboxes/$id/execs"
                        params={{ id }}
                        search={{ exec: exec.id }}
                        aria-current={selectedId === exec.id ? 'true' : undefined}
                        className="flex min-w-0 flex-col gap-1"
                      >
                        <ItemTitle className="font-mono text-xs">{exec.argv.join(' ')}</ItemTitle>
                        <ItemDescription className="truncate text-xs">
                          {formatLocalDateTime(exec.accepted_at)}
                        </ItemDescription>
                      </Link>
                    </ItemContent>
                    <ItemActions className="flex-col items-end gap-1.5">
                      <StatusBadge state={exec.state} data-testid={`exec-state-${exec.id}`} />
                      {/* A finished Exec has nothing left to attach to. */}
                      {exec.state !== 'finished' && (
                        <Link
                          to="/sandboxes/$id/terminal"
                          params={{ id }}
                          search={{ exec: exec.id }}
                          data-testid={`exec-attach-${exec.id}`}
                          className="text-xs text-muted-foreground hover:text-foreground hover:underline"
                        >
                          {m.execs_attach()}
                        </Link>
                      )}
                    </ItemActions>
                  </Item>
                ))}
              </ItemGroup>
            </CardContent>
          </Card>

          <div className="min-w-0">
            {selectedId && !selected && !execs.isPending && (
              <p className="text-sm text-muted-foreground">{m.execs_not_in_list()}</p>
            )}

            {selected && (
              <Card>
                <CardHeader className="flex-row items-start justify-between gap-3">
                  <div className="min-w-0">
                    <CardTitle className="truncate font-mono text-xs">
                      {selected.argv.join(' ')}
                    </CardTitle>
                    <CardDescription>{m.execs_logs_hint()}</CardDescription>
                  </div>
                  {selected.state !== 'finished' && (
                    <Button
                      variant="outline"
                      size="sm"
                      disabled={cancel.isPending}
                      onClick={() => cancel.mutate(selected.id)}
                    >
                      {m.execs_cancel()}
                    </Button>
                  )}
                </CardHeader>
                <CardContent>
                  <ExecLog exec={selected} />
                </CardContent>
              </Card>
            )}

            {!selected && !selectedId && (
              <Empty className="h-full border">
                <EmptyHeader>
                  <EmptyMedia variant="icon">
                    <Terminal />
                  </EmptyMedia>
                  <EmptyTitle>{m.execs_no_selected_title()}</EmptyTitle>
                  <EmptyDescription>{m.execs_no_selected_description()}</EmptyDescription>
                </EmptyHeader>
              </Empty>
            )}
          </div>
        </div>
      )}
    </div>
  )
}

function ExecLog({ exec }: { exec: Exec }) {
  const [terminal, setTerminal] = useState<XTerminal | null>(null)
  const [result, setResult] = useState<ExecResultFrame | null>(null)

  useEffect(() => {
    if (!terminal) return
    terminal.reset()
    setResult(null)

    return followExecOutput(
      exec.id,
      {
        write: (text) => terminal.write(text),
        result: setResult,
        error: notifyError,
      },
      {
        // A disconnected socket is retried twice; the offset is kept so the
        // log resumes without replaying bytes.
        retries: 2,
        closedMessage: m.execs_stream_closed(),
      },
    )
  }, [terminal, exec.id])

  return (
    <div className="flex flex-col gap-2">
      <p className="text-sm" data-testid="exec-result">
        {result
          ? m.execs_result_finished({ result: formatExecResult(result) })
          : exec.result
            ? m.execs_result_finished({ result: formatExecResult(exec.result) })
            : m.execs_result_state({ state: stateLabel(exec.state) })}
      </p>
      <TerminalView readOnly onReady={setTerminal} className="h-[28rem]" />
    </div>
  )
}
