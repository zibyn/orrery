import * as React from 'react'
import { createPortal } from 'react-dom'

import { Breadcrumb, BreadcrumbItem, BreadcrumbList, BreadcrumbPage } from '@/components/ui/breadcrumb'
import { cn } from '@/lib/utils'
import { m } from '@/paraglide/messages'

// Where the console shell wants each page's header parts: the breadcrumb in
// the sticky top bar, the overview in the band between the bar and the page.
export type PageHeaderSlots = { crumbs: HTMLElement | null; overview: HTMLElement | null }
export const PageHeaderSlotsContext = React.createContext<PageHeaderSlots | null>(null)

// One heading block per page: breadcrumb, title, description, inline meta and
// right-aligned actions. Inside the console shell it renders into the shell's
// header slots; `inline` (or no shell) keeps it in the page flow.
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
  const slots = React.useContext(PageHeaderSlotsContext)
  const overview = (
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
  )

  if (inline || !slots) {
    return (
      <div className={cn('flex flex-col gap-3', className)}>
        {above}
        {overview}
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
  return (
    <>
      {slots.crumbs && createPortal(crumbs, slots.crumbs)}
      {slots.overview &&
        createPortal(
          <div className={cn('mx-auto w-full max-w-6xl px-4 py-5 sm:px-6 lg:px-8', className)}>
            {overview}
          </div>,
          slots.overview,
        )}
    </>
  )
}
