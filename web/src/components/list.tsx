import type * as React from 'react'

import { cn } from '@/lib/utils'

// One list surface for the console: pages differ in what a row holds, not in
// the card, row padding or hover treatment.
export const listTitleClass = 'truncate text-sm font-medium'

export function ListCard({ className, ...props }: React.ComponentProps<'ul'>) {
  return (
    <ul
      data-slot="list-card"
      className={cn(
        'divide-y divide-border overflow-hidden rounded-xl border border-border bg-card',
        className,
      )}
      {...props}
    />
  )
}

export function ListRow({
  icon,
  title,
  description,
  trailing,
  selected,
  className,
  ...props
}: Omit<React.ComponentProps<'li'>, 'title'> & {
  icon?: React.ReactNode
  title: React.ReactNode
  description?: React.ReactNode
  trailing?: React.ReactNode
  selected?: boolean
}) {
  return (
    <li
      data-slot="list-row"
      className={cn(
        'flex flex-wrap items-center gap-x-3 gap-y-2 px-4 py-3 transition-colors',
        selected ? 'bg-accent' : 'hover:bg-muted/40',
        className,
      )}
      {...props}
    >
      {icon && (
        <span className="flex size-8 shrink-0 items-center justify-center rounded-lg border border-border bg-muted/50 text-xs font-medium text-muted-foreground [&_svg]:size-4">
          {icon}
        </span>
      )}
      <div className="min-w-0 flex-1">
        <div className="flex min-w-0 flex-wrap items-center gap-2">{title}</div>
        {description && <div className="text-xs text-muted-foreground">{description}</div>}
      </div>
      {trailing && (
        <div className="ml-auto flex shrink-0 flex-wrap items-center justify-end gap-2">
          {trailing}
        </div>
      )}
    </li>
  )
}
