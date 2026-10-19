import { CircleCheck, LoaderCircle } from 'lucide-react'
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

export type BuildLogProps = {
  execId: string
  mode: 'follow' | 'stored'
}

// The read-only xterm log shared by the version page: live while the build
// runs, then the stored Output pages once it is terminal (api.md §3, §8).
export function BuildLog({ execId, mode }: BuildLogProps) {
  const [terminal, setTerminal] = useState<Terminal | null>(null)
  const [result, setResult] = useState<ExecResultFrame | null>(null)
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(mode === 'stored')

  useEffect(() => {
    if (!terminal) return
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
          closedMessage: 'The build log stream closed before the build finished.',
          failedMessage: 'The build log stream failed.',
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
        if (!cancelled) setError(err instanceof Error ? err.message : String(err))
      } finally {
        if (!cancelled) setLoading(false)
      }
    })()
    return () => {
      cancelled = true
    }
  }, [terminal, execId, mode])

  return (
    <div className="flex flex-col gap-2">
      <p className="flex items-center gap-1.5 text-xs" data-testid="build-log-result">
        {result ? (
          <CircleCheck className="size-3.5 text-success" />
        ) : loading ? (
          <LoaderCircle className="size-3.5 animate-spin text-muted-foreground" />
        ) : null}
        <span className={result ? 'text-foreground' : 'text-muted-foreground'}>
          {result
            ? `Finished: ${formatExecResult(result)}`
            : loading
              ? 'Loading build log…'
              : 'Build log'}
        </span>
      </p>
      {error && <p className="text-sm text-destructive">{error}</p>}
      <TerminalView readOnly onReady={setTerminal} className="h-[28rem]" />
    </div>
  )
}
