import { useQuery } from '@tanstack/react-query'
import { CircleCheck, Hourglass, LoaderCircle } from 'lucide-react'
import { useEffect, useState } from 'react'
import type { Terminal } from '@xterm/xterm'

import { api, type Exec, type ExecOutputFrame, type ExecResultFrame } from '@/api'
import { TerminalView } from '@/components/terminal'
import {
  followExecOutput,
  formatExecResult,
  frameEnd,
  frameText,
} from '@/lib/exec-stream'
import { messageOf } from '@/lib/labels'
import { buildExecPollInterval, buildLogView } from '@/lib/snapshots'
import { cn } from '@/lib/utils'
import { m } from '@/paraglide/messages'

export type BuildLogProps = {
  execId: string
  mode: 'follow' | 'stored'
}

// The read-only xterm log shared by the version page: live while the build
// runs, then the stored Output pages once it is terminal (api.md §3, §8).
export function BuildLog({ execId, mode: versionMode }: BuildLogProps) {
  const exec = useQuery({
    queryKey: ['exec', execId],
    queryFn: () => api<Exec>('GET', `/execs/${execId}`),
    enabled: versionMode === 'follow',
    refetchInterval: (query) => buildExecPollInterval(query.state.data?.state),
  })
  const view = buildLogView(versionMode, exec.data?.state)
  const mode = view === 'stored' ? 'stored' : 'follow'
  const streaming = view === 'follow' || view === 'stored'
  const [terminal, setTerminal] = useState<Terminal | null>(null)
  const [result, setResult] = useState<ExecResultFrame | null>(null)
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(mode === 'stored')

  useEffect(() => {
    if (!terminal || !streaming) return
    terminal.reset()
    setResult(null)
    setError('')
    setLoading(true)

    if (mode === 'follow') {
      return followExecOutput(
        execId,
        {
          write: (text) => terminal.write(text),
          result: setResult,
          error: (message) => {
            setLoading(false)
            setError(message)
          },
          open: () => setLoading(false),
        },
        {
          closedMessage: m.build_log_closed(),
          failedMessage: m.build_log_failed(),
        },
      )
    }

    let cancelled = false
    void (async () => {
      try {
        let after = 0
        for (;;) {
          const frames = await api<ExecOutputFrame[]>(
            'GET',
            `/execs/${execId}/output?after=${after}`,
          )
          if (cancelled) return
          if (frames.length === 0) break
          for (const frame of frames) {
            terminal.write(frameText(frame))
            after = frameEnd(frame)
          }
        }
        const exec = await api<Exec>('GET', `/execs/${execId}`)
        if (cancelled) return
        if (exec.result) {
          setResult({ ...exec.result, type: 'result', log_truncated: exec.log_truncated })
        }
      } catch (err) {
        if (!cancelled) setError(messageOf(err))
      } finally {
        if (!cancelled) setLoading(false)
      }
    })()
    return () => {
      cancelled = true
    }
  }, [terminal, execId, mode, streaming])

  return (
    <div className="flex flex-col gap-2">
      <p className="flex items-center gap-1.5 text-xs" data-testid="build-log-result">
        {result ? (
          <CircleCheck className="size-3.5 text-success" />
        ) : loading || view === 'loading' ? (
          <LoaderCircle className="size-3.5 animate-spin text-muted-foreground" />
        ) : null}
        <span className={result ? 'text-foreground' : 'text-muted-foreground'}>
          {result
            ? m.build_log_result({ result: formatExecResult(result) })
            : loading || view === 'loading'
              ? m.build_log_loading()
              : m.build_log_title()}
        </span>
      </p>
      {view === 'queued' && (
        <p className="flex items-center gap-1.5 text-sm text-muted-foreground" data-testid="build-log-queued">
          <Hourglass className="size-4" />
          {m.build_log_queued()}
        </p>
      )}
      {exec.error && <p className="text-sm whitespace-pre-line text-destructive">{messageOf(exec.error)}</p>}
      {error && (
        <p className="text-sm whitespace-pre-line text-destructive">{error}</p>
      )}
      {/* Hidden, not unmounted, so the follow stream reuses the same terminal. */}
      <TerminalView
        readOnly
        onReady={setTerminal}
        className={cn('h-[28rem]', view === 'queued' && 'hidden')}
      />
    </div>
  )
}
