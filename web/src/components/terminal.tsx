import { FitAddon } from '@xterm/addon-fit'
import { Terminal } from '@xterm/xterm'
import { useEffect, useRef } from 'react'

import { cn } from '@/lib/utils'

import '@xterm/xterm/css/xterm.css'

export type TerminalViewProps = {
  readOnly?: boolean
  className?: string
  onReady?: (terminal: Terminal) => void
  onData?: (data: string) => void
  onResize?: (rows: number, cols: number) => void
}

// One xterm renderer shared by the interactive terminal and the read-only Exec
// log (api.md §8). Log scrollback is capped; the full log is read with the CLI.
export function TerminalView({
  readOnly = false,
  className,
  onReady,
  onData,
  onResize,
}: TerminalViewProps) {
  const hostRef = useRef<HTMLDivElement>(null)
  const handlers = useRef({ onReady, onData, onResize })
  handlers.current = { onReady, onData, onResize }

  useEffect(() => {
    const host = hostRef.current
    if (!host) return

    const terminal = new Terminal({
      disableStdin: readOnly,
      convertEol: readOnly,
      cursorBlink: !readOnly,
      fontSize: 13,
      fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Consolas, monospace',
      scrollback: 2000,
    })
    const fit = new FitAddon()
    terminal.loadAddon(fit)
    terminal.open(host)

    const fitNow = () => {
      try {
        fit.fit()
      } catch {
        // The container can be hidden or not laid out yet; the observer retries.
      }
    }
    fitNow()

    const dataSubscription = terminal.onData((data) => handlers.current.onData?.(data))
    const resizeSubscription = terminal.onResize(({ rows, cols }) =>
      handlers.current.onResize?.(rows, cols),
    )
    handlers.current.onReady?.(terminal)

    const observer = new ResizeObserver(fitNow)
    observer.observe(host)
    return () => {
      observer.disconnect()
      resizeSubscription.dispose()
      dataSubscription.dispose()
      terminal.dispose()
    }
  }, [readOnly])

  return (
    <div
      ref={hostRef}
      data-testid="terminal"
      className={cn(
        'h-96 w-full overflow-hidden rounded-md border border-border bg-black p-1',
        className,
      )}
    />
  )
}
