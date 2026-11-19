import * as React from 'react'
import { createPortal } from 'react-dom'

import { Breadcrumb, BreadcrumbItem, BreadcrumbList, BreadcrumbPage } from '@/components/ui/breadcrumb'
import { cn } from '@/lib/utils'
import { m } from '@/paraglide/messages'

// The console shell's sticky top bar, where each page puts its header.
export const PageHeaderSlotContext = React.createContext<HTMLElement | null>(null)

// One heading block per page: breadcrumb, title, description, inline meta and
// right-aligned actions. Inside the console shell it renders into the top bar
// as breadcrumb, meta and actions (the title stays for screen readers, the
// description is dropped); `inline` (or no shell) keeps it in the page flow.
export function PageHeader({
  title,
  description,
  meta,
  actions,
  above,
  inline,
  className,
}: {
  title: React.ReactNode
  description?: React.ReactNode
  meta?: React.ReactNode
  actions?: React.ReactNode
  above?: React.ReactNode
  inline?: boolean
  className?: string
}) {
  const slot = React.useContext(PageHeaderSlotContext)

  if (inline || !slot) {
    return (
      <div className={cn('flex flex-col gap-3', className)}>
        {above}
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div className="flex min-w-0 flex-col gap-1">
            <h1 className="truncate text-lg font-semibold tracking-tight sm:text-xl">{title}</h1>
            {description && <p className="max-w-2xl text-sm text-muted-foreground">{description}</p>}
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

  // A top-level page has no trail; its own name is the whole breadcrumb.
  const crumbs = above ?? (
    <Breadcrumb aria-label={m.common_breadcrumb()}>
      <BreadcrumbList>
        <BreadcrumbItem>
          <BreadcrumbPage>{title}</BreadcrumbPage>
        </BreadcrumbItem>
      </BreadcrumbList>
    </Breadcrumb>
  )
  return createPortal(
    <>
      <h1 className="sr-only">{title}</h1>
      <div className="min-w-0 max-w-[50%] shrink-0 truncate">{crumbs}</div>
      {meta && (
        <div className="hidden shrink-0 items-center gap-2 text-xs text-muted-foreground lg:flex">
          {meta}
        </div>
      )}
      {actions && (
        <div className={cn('ml-auto flex min-w-0 items-center gap-2 overflow-x-auto', className)}>
          {actions}
        </div>
      )}
    </>,
    slot,
  )
}
