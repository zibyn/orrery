import type * as React from 'react'

import { stateLabel } from '@/lib/labels'
import { cn } from '@/lib/utils'

// Status is never conveyed by color alone (UX: color-only): the dot is
// decorative and the state text is the accessible content.
const TONES: Record<string, 'success' | 'warning' | 'info' | 'destructive' | 'muted'> = {
  ready: 'success',
  running: 'success',
  connected: 'success',
  active: 'success',
  healthy: 'success',
  finished: 'muted',
  pending: 'info',
  provisioning: 'info',
  building: 'info',
  queued: 'info',
  connecting: 'info',
  stopping: 'warning',
  stopped: 'muted',
  paused: 'warning',
  unavailable: 'warning',
  outdated: 'warning',
  canceling: 'warning',
  cancelled: 'warning',
  closed: 'muted',
  deleting: 'destructive',
  deleted: 'muted',
  failed: 'destructive',
  lost: 'destructive',
}

const DOT: Record<NonNullable<(typeof TONES)[string]>, string> = {
  success: 'bg-success',
  warning: 'bg-warning',
  info: 'bg-info',
  destructive: 'bg-destructive',
  muted: 'bg-muted-foreground/60',
}

export function StatusBadge({
  state,
  className,
  ...props
}: { state: string } & React.ComponentProps<'span'>) {
  const tone = TONES[state] ?? 'muted'
  const animate = state === 'pending' || state === 'provisioning' || state === 'building'

  return (
    <span
      data-slot="status-badge"
      className={cn(
        'inline-flex w-fit shrink-0 items-center gap-1.5 rounded-md border border-border bg-card px-2 py-0.5 text-xs font-medium text-foreground',
        className,
      )}
      {...props}
    >
      <span
        aria-hidden="true"
        className={cn('size-1.5 rounded-full', DOT[tone], animate && 'animate-pulse')}
      />
      {stateLabel(state)}
    </span>
  )
}
