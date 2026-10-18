import { useMutation } from '@tanstack/react-query'
import { createFileRoute, Link } from '@tanstack/react-router'
import { useEffect, useRef, useState } from 'react'
import type { Terminal } from '@xterm/xterm'

import { api, type CreateExecRequest, type Exec, type PtyExitFrame } from '@/api'
import { TerminalView } from '@/components/terminal'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { consoleWebSocketUrl, formatExecResult, parseArgv } from '@/lib/exec-stream'

export const Route = createFileRoute('/_authed/sandboxes/$id_/terminal')({
  validateSearch: (search: Record<string, unknown>): { exec?: string } => ({
    exec: typeof search.exec === 'string' ? search.exec : undefined,
  }),
  component: TerminalPage,
})

const DEFAULT_ARGV = '/bin/sh -l'

type Status = 'connecting' | 'connected' | 'closed'

function TerminalPage() {
  const { id } = Route.useParams()
  const { exec: existingExec } = Route.useSearch()

  const [argvText, setArgvText] = useState(DEFAULT_ARGV)
  const [execId, setExecId] = useState<string | null>(existingExec ?? null)
  const [terminal, setTerminal] = useState<Terminal | null>(null)
  const [status, setStatus] = useState<Status>('connecting')
  const [exit, setExit] = useState('')
  const [error, setError] = useState('')
  const socketRef = useRef<WebSocket | null>(null)

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
    onError: (err) => setError(err.message),
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
        if (!canceled) setError(err instanceof Error ? err.message : String(err))
        return
      }
      if (canceled) return
      if (exec.state === 'pending') {
        setError('The Exec did not start in time.')
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
            ? 'The terminal connection failed. This Exec has no PTY, or the Sandbox is not ready.'
            : 'The terminal connection failed. The Sandbox may not be ready.',
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
    <div className="flex flex-col gap-6">
      <div className="flex flex-col gap-1">
        <Link
          to="/sandboxes/$id"
          params={{ id }}
          className="text-sm text-muted-foreground hover:underline"
        >
          ← Sandbox
        </Link>
        <h1 className="text-2xl font-semibold">Terminal</h1>
      </div>

      {execId ? (
        <div className="flex flex-wrap items-center gap-3 text-sm">
          <span className="text-muted-foreground">
            {existingExec && execId === existingExec ? `Attached to Exec ${execId}` : `Exec ${execId}`}
          </span>
          <span data-testid="terminal-status" className="rounded-full border border-border px-2 py-0.5 text-xs">
            {status}
          </span>
          {exit && <span data-testid="terminal-exit">Exited: {exit}</span>}
          <Link
            to="/sandboxes/$id/execs"
            params={{ id }}
            search={{ exec: execId }}
            className="text-muted-foreground hover:underline"
          >
            View Exec list
          </Link>
        </div>
      ) : (
        <form
          className="flex flex-wrap items-end gap-3"
          onSubmit={(event) => {
            event.preventDefault()
            if (parseArgv(argvText).length > 0) create.mutate()
          }}
        >
          <div className="flex min-w-64 flex-1 flex-col gap-1">
            <Label htmlFor="terminal-argv">Command</Label>
            <Input
              id="terminal-argv"
              value={argvText}
              onChange={(event) => setArgvText(event.target.value)}
              placeholder={DEFAULT_ARGV}
            />
            <p className="text-xs text-muted-foreground">
              Runs in a PTY inside the Sandbox Workspace.
            </p>
          </div>
          <Button type="submit" disabled={create.isPending || parseArgv(argvText).length === 0}>
            Open terminal
          </Button>
        </form>
      )}

      {error && <p className="text-sm text-destructive">{error}</p>}

      <TerminalView
        onReady={setTerminal}
        onData={sendInput}
        onResize={sendResize}
        className="h-[28rem]"
      />
    </div>
  )
}
