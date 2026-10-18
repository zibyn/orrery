import { useEffect, useState } from 'react'
import type { Terminal } from '@xterm/xterm'

import { api, type Exec, type ExecOutputFrame, type ExecResultFrame } from '@/api'
import { TerminalView } from '@/components/terminal'
import {
  consoleWebSocketUrl,
  formatExecResult,
  frameEnd,
  frameText,
  isResultFrame,
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
      let closed = false
      let finished = false
      const socket = new WebSocket(consoleWebSocketUrl(`/execs/${execId}/output?follow=true`))
      socket.onopen = () => setLoading(false)
      socket.onmessage = (event) => {
        let frame: unknown
        try {
          frame = JSON.parse(String(event.data))
        } catch {
          return
        }
        if (isResultFrame(frame)) {
          finished = true
          setResult(frame)
          return
        }
        terminal.write(frameText(frame as ExecOutputFrame))
      }
      socket.onclose = () => {
        setLoading(false)
        if (closed || finished) return
        setError('The build log stream closed before the build finished.')
      }
      socket.onerror = () => {
        if (closed || finished) return
        setLoading(false)
        setError('The build log stream failed.')
      }
      return () => {
        closed = true
        socket.close()
      }
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
      <p className="text-sm" data-testid="build-log-result">
        {result
          ? `Finished: ${formatExecResult(result)}`
          : loading
            ? 'Loading build log…'
            : 'Build log'}
      </p>
      {error && <p className="text-sm text-destructive">{error}</p>}
      <TerminalView readOnly onReady={setTerminal} className="h-[28rem]" />
    </div>
  )
}
