import { useMutation, useQuery } from '@tanstack/react-query'
import { createFileRoute, Link } from '@tanstack/react-router'
import { ScrollText } from 'lucide-react'
import { useEffect, useRef, useState } from 'react'
import type { Terminal as XTerminal } from '@xterm/xterm'

import { api, type CreateExecRequest, type Exec, type PtyExitFrame, type Sandbox } from '@/api'
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
import { Card, CardContent } from '@/components/ui/card'
import { Field, FieldDescription, FieldError, FieldGroup, FieldLabel } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { consoleWebSocketUrl, formatExecResult, parseArgv } from '@/lib/exec-stream'
import { useLocale } from '@/lib/i18n'
import { messageOf } from '@/lib/labels'
import { cn } from '@/lib/utils'
import { m } from '@/paraglide/messages'

export const Route = createFileRoute('/_authed/sandboxes/$id_/terminal')({
  validateSearch: (search: Record<string, unknown>): { exec?: string } => ({
    exec: typeof search.exec === 'string' ? search.exec : undefined,
  }),
  component: TerminalPage,
})

const DEFAULT_ARGV = '/bin/sh -l'

type Status = 'connecting' | 'connected' | 'closed'

function TerminalPage() {
  useLocale() // re-render on a language switch (see lib/i18n.tsx)
  const { id } = Route.useParams()
  const { exec: existingExec } = Route.useSearch()

  const [argvText, setArgvText] = useState(DEFAULT_ARGV)
  const [execId, setExecId] = useState<string | null>(existingExec ?? null)
  const [terminal, setTerminal] = useState<XTerminal | null>(null)
  const [status, setStatus] = useState<Status>('connecting')
  const [exit, setExit] = useState('')
  const [error, setError] = useState('')
  const socketRef = useRef<WebSocket | null>(null)
  // Shares the detail page's cache, so the breadcrumb shows the name.
  const sandbox = useQuery({
    queryKey: ['sandbox', id],
    queryFn: () => api<Sandbox>('GET', `/sandboxes/${id}`),
  })

  const create = useMutation({
    mutationFn: () => {
      const body: CreateExecRequest = {
        argv: parseArgv(argvText),
        pty: { rows: terminal?.rows ?? 24, cols: terminal?.cols ?? 80 },
      }
      return api<Exec>('POST', `/sandboxes/${id}/execs`, body)
    },
    onSuccess: (exec) => {
      setError('')
      setExecId(exec.id)
    },
    onError: (err) => setError(messageOf(err)),
  })

  useEffect(() => {
    if (!terminal || !execId) return
    terminal.reset()
    setExit('')
    setError('')
    setStatus('connecting')

    let canceled = false
    let finished = false
    let socket: WebSocket | null = null

    const sendControl = (message: unknown) => {
      if (socket?.readyState === WebSocket.OPEN) socket.send(JSON.stringify(message))
    }

    // The Supervisor record may not exist while the Exec is pending; the CLI
    // waits for the same transition before attaching (api.md §4).
    const attach = async () => {
      let exec: Exec
      try {
        exec = await api<Exec>('GET', `/execs/${execId}`)
        for (let attempt = 0; exec.state === 'pending' && attempt < 40; attempt += 1) {
          await new Promise((resolve) => window.setTimeout(resolve, 250))
          if (canceled) return
          exec = await api<Exec>('GET', `/execs/${execId}`)
        }
      } catch (err) {
        if (!canceled) setError(messageOf(err))
        return
      }
      if (canceled) return
      if (exec.state === 'pending') {
        setError(m.terminal_start_timeout())
        return
      }

      socket = new WebSocket(consoleWebSocketUrl(`/execs/${execId}/pty`), 'sandbox.v1')
      socket.binaryType = 'arraybuffer'
      socketRef.current = socket
      socket.onopen = () => {
        if (canceled) return
        setStatus('connected')
        sendControl({ type: 'resize', rows: terminal.rows, cols: terminal.cols })
        terminal.focus()
      }
      socket.onmessage = (event) => {
        if (typeof event.data === 'string') {
          let message: unknown
          try {
            message = JSON.parse(event.data)
          } catch {
            return
          }
          if (typeof message === 'object' && message !== null && 'type' in message) {
            const frame = message as PtyExitFrame
            if (frame.type === 'exit') {
              finished = true
              setExit(formatExecResult(frame))
              setStatus('closed')
            }
          }
          return
        }
        terminal.write(new Uint8Array(event.data as ArrayBuffer))
      }
      socket.onclose = () => {
        if (!canceled && !finished) setStatus('closed')
      }
      socket.onerror = () => {
        if (canceled) return
        setError(
          existingExec
            ? m.terminal_connect_failed_attached()
            : m.terminal_connect_failed(),
        )
      }
    }
    attach()

    return () => {
      canceled = true
      socketRef.current = null
      socket?.close()
    }
  }, [terminal, execId])

  const sendInput = (data: string) => {
    const socket = socketRef.current
    if (socket?.readyState === WebSocket.OPEN) socket.send(new TextEncoder().encode(data))
  }

  const sendResize = (rows: number, cols: number) => {
    const socket = socketRef.current
    if (socket?.readyState === WebSocket.OPEN) socket.send(JSON.stringify({ type: 'resize', rows, cols }))
  }

  return (
    <div className="flex flex-col gap-4">
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
                <BreadcrumbPage>{m.terminal_title()}</BreadcrumbPage>
              </BreadcrumbItem>
            </BreadcrumbList>
          </Breadcrumb>
        }
        title={m.terminal_title()}
        actions={
          <Link
            to="/sandboxes/$id/execs"
            params={{ id }}
            search={execId ? { exec: execId } : {}}
            className={cn(buttonVariants({ variant: 'outline' }))}
          >
            <ScrollText data-icon="inline-start" />
            {m.terminal_view_execs()}
          </Link>
        }
      />

      {execId ? (
        <div className="flex flex-wrap items-center gap-3 text-sm">
          <span className="text-xs text-muted-foreground">
            {existingExec && execId === existingExec
              ? m.terminal_attached({ id: execId })
              : m.terminal_exec({ id: execId })}
          </span>
          <StatusBadge state={status} data-testid="terminal-status" />
          {exit && (
            <span data-testid="terminal-exit" className="text-xs text-muted-foreground">
              {m.terminal_exited({ result: exit })}
            </span>
          )}
          {error && (
            <span className="whitespace-pre-line text-xs text-destructive" role="alert">
              {error}
            </span>
          )}
        </div>
      ) : (
        <Card>
          <CardContent>
            <form
              onSubmit={(event) => {
                event.preventDefault()
                if (parseArgv(argvText).length > 0) create.mutate()
              }}
            >
              <FieldGroup>
                <div className="flex flex-wrap items-end gap-3">
                  <Field className="min-w-64 flex-1">
                    <FieldLabel htmlFor="terminal-argv">{m.terminal_command()}</FieldLabel>
                    <Input
                      id="terminal-argv"
                      className="font-mono"
                      value={argvText}
                      onChange={(event) => setArgvText(event.target.value)}
                      placeholder={DEFAULT_ARGV}
                    />
                    <FieldDescription>{m.terminal_runs_in()}</FieldDescription>
                  </Field>
                  <Button type="submit" disabled={create.isPending || parseArgv(argvText).length === 0}>
                    {m.terminal_open()}
                  </Button>
                </div>
                {error && <FieldError className="whitespace-pre-line">{error}</FieldError>}
              </FieldGroup>
            </form>
          </CardContent>
        </Card>
      )}

      <div className="overflow-hidden rounded-xl border border-border bg-card p-1.5">
        <TerminalView onReady={setTerminal} onData={sendInput} onResize={sendResize} className="h-[32rem] border-0" />
      </div>
    </div>
  )
}
