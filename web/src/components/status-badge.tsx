import type * as React from 'react'

import { Badge } from '@/components/ui/badge'
import { stateLabel } from '@/lib/labels'

// Maps a platform state to a Badge variant; the state text carries the meaning,
// so status is never conveyed by colour alone.
const VARIANTS: Record<string, 'success' | 'warning' | 'info' | 'destructive' | 'secondary'> = {
  ready: 'success',
  running: 'success',
  connected: 'success',
  active: 'success',
  healthy: 'success',
  unhealthy: 'destructive',
  finished: 'secondary',
  pending: 'info',
  provisioning: 'info',
  building: 'info',
  queued: 'info',
  connecting: 'info',
  stopping: 'warning',
  stopped: 'secondary',
  paused: 'warning',
  unavailable: 'warning',
  outdated: 'warning',
  canceling: 'warning',
  cancelled: 'warning',
  closed: 'secondary',
  deleting: 'destructive',
  deleted: 'secondary',
  failed: 'destructive',
  lost: 'destructive',
}

export function StatusBadge({
  state,
  ...props
}: { state: string } & Omit<React.ComponentProps<typeof Badge>, 'variant' | 'children'>) {
  return (
    <Badge variant={VARIANTS[state] ?? 'secondary'} {...props}>
      {stateLabel(state)}
    </Badge>
  )
}
