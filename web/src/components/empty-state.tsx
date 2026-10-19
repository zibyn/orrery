import type * as React from 'react'

import { cn } from '@/lib/utils'

export function EmptyState({
  icon,
  title,
  description,
  action,
  className,
}: {
  icon?: React.ReactNode
  title: string
  description?: string
  action?: React.ReactNode
  className?: string
}) {
  return (
    <div
      className={cn(
        'flex flex-col items-center justify-center gap-2 rounded-xl border border-dashed border-border px-6 py-12 text-center',
        className,
      )}
    >
      {icon && (
        <div className="mb-1 flex size-9 items-center justify-center rounded-lg border border-border bg-muted/50 text-muted-foreground [&_svg]:size-4">
          {icon}
        </div>
      )}
      <p className="text-sm font-medium">{title}</p>
      {description && <p className="max-w-sm text-xs text-muted-foreground">{description}</p>}
      {action && <div className="pt-2">{action}</div>}
    </div>
  )
}
