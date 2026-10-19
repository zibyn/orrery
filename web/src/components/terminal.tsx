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
      lineHeight: 1.35,
      fontFamily:
        "ui-monospace, 'SF Mono', SFMono-Regular, Menlo, Consolas, 'Liberation Mono', monospace",
      scrollback: 2000,
      // The terminal stays dark in both themes, like a code editor.
      theme: {
        background: '#0b0b0d',
        foreground: '#e5e7eb',
        cursor: '#a5b4fc',
        cursorAccent: '#0b0b0d',
        selectionBackground: '#334155',
        black: '#1f2937',
        brightBlack: '#4b5563',
        red: '#f87171',
        brightRed: '#fca5a5',
        green: '#4ade80',
        brightGreen: '#86efac',
        yellow: '#fbbf24',
        brightYellow: '#fcd34d',
        blue: '#60a5fa',
        brightBlue: '#93c5fd',
        magenta: '#c084fc',
        brightMagenta: '#d8b4fe',
        cyan: '#22d3ee',
        brightCyan: '#67e8f9',
        white: '#e5e7eb',
        brightWhite: '#f9fafb',
      },
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
        'h-96 w-full overflow-hidden rounded-xl border border-border bg-[#0b0b0d] p-3',
        className,
      )}
    />
  )
}
