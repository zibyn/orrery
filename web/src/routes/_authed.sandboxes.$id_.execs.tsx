import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { createFileRoute, Link } from '@tanstack/react-router'
import { ArrowLeft, Terminal } from 'lucide-react'
import { useEffect, useState } from 'react'
import type { Terminal as XTerminal } from '@xterm/xterm'

import { api, type Exec, type ExecResultFrame, type ListResponse } from '@/api'
import { EmptyState } from '@/components/empty-state'
import { PageHeader } from '@/components/page-header'
import { Problem } from '@/components/problem'
import { StatusBadge } from '@/components/status-badge'
import { TerminalView } from '@/components/terminal'
import { Button, buttonVariants } from '@/components/ui/button'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { execListPollInterval, followExecOutput, formatExecResult } from '@/lib/exec-stream'
import { useLocale } from '@/lib/i18n'
import { messageOf, stateLabel } from '@/lib/labels'
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

  const execs = useQuery({
    queryKey: ['execs', id],
    queryFn: () => api<ListResponse<Exec>>('GET', `/sandboxes/${id}/execs`),
    refetchInterval: (query) => execListPollInterval(query.state.data?.items),
  })
  const selected = execs.data?.items.find((exec) => exec.id === selectedId)
  const [cancelError, setCancelError] = useState('')

  const cancel = useMutation({
    mutationFn: (execId: string) => api<Exec>('POST', `/execs/${execId}/cancel`),
    onSuccess: () => {
      setCancelError('')
      queryClient.invalidateQueries({ queryKey: ['execs', id] })
    },
    onError: (err) => setCancelError(messageOf(err)),
  })

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        above={
          <Link
            to="/sandboxes/$id"
            params={{ id }}
            className="inline-flex items-center gap-1 text-xs text-muted-foreground hover:text-foreground"
          >
            <ArrowLeft className="size-3.5" />
            {m.execs_back()}
          </Link>
        }
        title={m.execs_title()}
        actions={
          <Link
            to="/sandboxes/$id/terminal"
            params={{ id }}
            className={buttonVariants({ variant: 'outline' })}
          >
            <Terminal />
            {m.execs_open_terminal()}
          </Link>
        }
      />

      {execs.isPending && (
        <p className="py-8 text-sm text-muted-foreground">{m.execs_loading()}</p>
      )}
      {execs.error && (
        <p className="text-sm text-destructive" role="alert">
          {m.execs_load_error()}: <Problem error={execs.error} />
        </p>
      )}

      {execs.data?.items.length === 0 && (
        <EmptyState
          icon={<Terminal />}
          title={m.execs_empty_title()}
          description={m.execs_empty_description()}
        />
      )}

      {execs.data && execs.data.items.length > 0 && (
        <div className="grid items-start gap-4 xl:grid-cols-[22rem_minmax(0,1fr)]">
          <Card className="overflow-hidden">
            <CardHeader>
              <CardTitle>{m.execs_recent()}</CardTitle>
              <CardDescription>{m.execs_recent_description()}</CardDescription>
            </CardHeader>
            <CardContent className="px-0 pb-0">
              <ul className="divide-y divide-border border-t border-border">
                {execs.data.items.map((exec) => (
                  <li key={exec.id}>
                    <div
                      className={cn(
                        'flex items-start gap-2 px-4 py-3 transition-colors',
                        selectedId === exec.id ? 'bg-accent' : 'hover:bg-muted/40',
                      )}
                    >
                      <Link
                        to="/sandboxes/$id/execs"
                        params={{ id }}
                        search={{ exec: exec.id }}
                        aria-current={selectedId === exec.id ? 'true' : undefined}
                        className="flex min-w-0 flex-1 flex-col gap-1"
                      >
                        <span className="truncate font-mono text-xs font-medium">
                          {exec.argv.join(' ')}
                        </span>
                        <span className="truncate text-[11px] text-muted-foreground">
                          {exec.id} · {exec.accepted_at}
                        </span>
                      </Link>
                      <div className="flex shrink-0 flex-col items-end gap-1.5">
                        <StatusBadge
                          state={exec.state}
                          data-testid={`exec-state-${exec.id}`}
                        />
                        <Link
                          to="/sandboxes/$id/terminal"
                          params={{ id }}
                          search={{ exec: exec.id }}
                          data-testid={`exec-attach-${exec.id}`}
                          className="text-[11px] text-muted-foreground hover:text-foreground hover:underline"
                        >
                          {m.execs_attach()}
                        </Link>
                      </div>
                    </div>
                  </li>
                ))}
              </ul>
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
                  {cancelError && (
                    <p className="mb-2 text-sm whitespace-pre-line text-destructive" role="alert">
                      {cancelError}
                    </p>
                  )}
                  <ExecLog exec={selected} />
                </CardContent>
              </Card>
            )}

            {!selected && !selectedId && (
              <EmptyState
                icon={<Terminal />}
                title={m.execs_no_selected_title()}
                description={m.execs_no_selected_description()}
                className="h-full"
              />
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
  const [error, setError] = useState('')

  useEffect(() => {
    if (!terminal) return
    terminal.reset()
    setResult(null)
    setError('')

    return followExecOutput(
      exec.id,
      {
        write: (text) => terminal.write(text),
        result: setResult,
        error: setError,
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
      {error && <p className="text-sm text-destructive">{error}</p>}
      <TerminalView readOnly onReady={setTerminal} className="h-[28rem]" />
    </div>
  )
}
