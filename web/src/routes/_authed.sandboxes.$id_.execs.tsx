import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { createFileRoute, Link } from '@tanstack/react-router'
import { useEffect, useState } from 'react'
import type { Terminal } from '@xterm/xterm'

import { api, type Exec, type ExecResultFrame, type ListResponse } from '@/api'
import { TerminalView } from '@/components/terminal'
import { Button, buttonVariants } from '@/components/ui/button'
import { execListPollInterval, followExecOutput, formatExecResult } from '@/lib/exec-stream'
import { cn, messageOf } from '@/lib/utils'

export const Route = createFileRoute('/_authed/sandboxes/$id_/execs')({
  validateSearch: (search: Record<string, unknown>): { exec?: string } => ({
    exec: typeof search.exec === 'string' ? search.exec : undefined,
  }),
  component: ExecsPage,
})

function ExecsPage() {
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
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="flex flex-col gap-1">
          <Link
            to="/sandboxes/$id"
            params={{ id }}
            className="text-sm text-muted-foreground hover:underline"
          >
            ← Sandbox
          </Link>
          <h1 className="text-2xl font-semibold">Execs</h1>
        </div>
        <Link
          to="/sandboxes/$id/terminal"
          params={{ id }}
          className={buttonVariants({ variant: 'outline' })}
        >
          Open terminal
        </Link>
      </div>

      {execs.isPending && <p className="text-sm text-muted-foreground">Loading…</p>}
      {execs.error && (
        <p className="text-sm text-destructive">Could not load Execs: {execs.error.message}</p>
      )}
      {execs.data?.items.length === 0 && (
        <p className="text-sm text-muted-foreground">No Execs for this Sandbox yet.</p>
      )}

      {execs.data && execs.data.items.length > 0 && (
        <ul className="divide-y divide-border rounded-md border border-border">
          {execs.data.items.map((exec) => (
            <li
              key={exec.id}
              className={cn(
                'flex flex-wrap items-center justify-between gap-3 p-4',
                selectedId === exec.id && 'bg-muted',
              )}
            >
              <Link
                to="/sandboxes/$id/execs"
                params={{ id }}
                search={{ exec: exec.id }}
                aria-current={selectedId === exec.id ? 'true' : undefined}
                className="flex flex-col gap-1 hover:underline"
              >
                <span className="font-mono text-sm">{exec.argv.join(' ')}</span>
                <span className="text-xs text-muted-foreground">
                  {exec.id} · {exec.accepted_at}
                </span>
              </Link>
              <span className="flex items-center gap-2">
                <span
                  className="rounded-full border border-border px-2 py-0.5 text-xs font-medium"
                  data-testid={`exec-state-${exec.id}`}
                >
                  {exec.state}
                </span>
                <Link
                  to="/sandboxes/$id/terminal"
                  params={{ id }}
                  search={{ exec: exec.id }}
                  data-testid={`exec-attach-${exec.id}`}
                  className={buttonVariants({ variant: 'outline', size: 'sm' })}
                >
                  Attach
                </Link>
              </span>
            </li>
          ))}
        </ul>
      )}

      {selectedId && !selected && !execs.isPending && (
        <p className="text-sm text-muted-foreground">That Exec is not in this Sandbox's list.</p>
      )}

      {selected && (
        <section className="flex flex-col gap-3">
          <div className="flex flex-wrap items-center justify-between gap-3">
            <div className="flex flex-col gap-1">
              <h2 className="font-mono text-sm">{selected.argv.join(' ')}</h2>
              <p className="text-xs text-muted-foreground">
                Full logs beyond the terminal scrollback are available with the CLI.
              </p>
            </div>
            {selected.state !== 'finished' && (
              <Button
                variant="outline"
                disabled={cancel.isPending}
                onClick={() => cancel.mutate(selected.id)}
              >
                Cancel Exec
              </Button>
            )}
          </div>
          {cancelError && (
            <p className="text-sm text-destructive" role="alert">
              {cancelError}
            </p>
          )}
          <ExecLog exec={selected} />
        </section>
      )}
    </div>
  )
}

function ExecLog({ exec }: { exec: Exec }) {
  const [terminal, setTerminal] = useState<Terminal | null>(null)
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
        closedMessage:
          'The output stream closed before the Exec finished. A PTY Exec has no log page; attach its terminal instead.',
      },
    )
  }, [terminal, exec.id])

  return (
    <div className="flex flex-col gap-2">
      <p className="text-sm" data-testid="exec-result">
        {result
          ? `Finished: ${formatExecResult(result)}`
          : exec.result
            ? `Finished: ${formatExecResult(exec.result)}`
            : `State: ${exec.state}`}
      </p>
      {error && <p className="text-sm text-destructive">{error}</p>}
      <TerminalView readOnly onReady={setTerminal} className="h-[28rem]" />
    </div>
  )
}
