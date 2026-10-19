import type * as React from 'react'

import { cn } from '@/lib/utils'

// One heading block per page: optional breadcrumb/back line, title,
// description, inline meta and right-aligned actions.
export function PageHeader({
  title,
  description,
  meta,
  actions,
  above,
  className,
}: {
  title: React.ReactNode
  description?: React.ReactNode
  meta?: React.ReactNode
  actions?: React.ReactNode
  above?: React.ReactNode
  className?: string
}) {
  return (
    <div className={cn('flex flex-col gap-3', className)}>
      {above}
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0 space-y-1">
          <h1 className="truncate text-lg font-semibold tracking-tight sm:text-xl">{title}</h1>
          {description && (
            <p className="max-w-2xl text-sm text-muted-foreground">{description}</p>
          )}
          {meta && (
            <div className="flex flex-wrap items-center gap-2 pt-1 text-xs text-muted-foreground">
              {meta}
            </div>
          )}
        </div>
        {actions && <div className="flex flex-wrap items-center gap-2">{actions}</div>}
      </div>
    </div>
  )
}
