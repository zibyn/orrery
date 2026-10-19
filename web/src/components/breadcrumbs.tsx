import { Link } from '@tanstack/react-router'
import { ChevronRight } from 'lucide-react'
import type { ReactNode } from 'react'

import { m } from '@/paraglide/messages'

// Subpages name their section root here, so every page shows the same
// hierarchy: section / current page.
export type Crumb =
  | {
      label: ReactNode
      to: '/sandboxes' | '/snapshots' | '/admin/users' | '/admin/nodes' | '/admin/audit'
    }
  | { label: ReactNode; to: '/sandboxes/$id'; params: { id: string } }
  | { label: ReactNode }

function CrumbLink({ item }: { item: Crumb }) {
  if (!('to' in item)) return null
  if (item.to === '/sandboxes/$id') {
    return (
      <Link to={item.to} params={item.params} className="truncate transition-colors hover:text-foreground">
        {item.label}
      </Link>
    )
  }
  return (
    <Link to={item.to} className="truncate transition-colors hover:text-foreground">
      {item.label}
    </Link>
  )
}

export function Breadcrumbs({ items }: { items: Crumb[] }) {
  return (
    <nav aria-label={m.common_breadcrumb()}>
      <ol className="flex flex-wrap items-center gap-1.5 text-xs text-muted-foreground">
        {items.map((item, index) => {
          const last = index === items.length - 1
          return (
            <li key={index} className="flex min-w-0 items-center gap-1.5">
              {index > 0 && (
                <ChevronRight
                  aria-hidden="true"
                  className="size-3.5 shrink-0 text-muted-foreground/50"
                />
              )}
              {last ? (
                <span aria-current="page" className="truncate text-foreground">
                  {item.label}
                </span>
              ) : 'to' in item ? (
                <CrumbLink item={item} />
              ) : (
                <span className="truncate">{item.label}</span>
              )}
            </li>
          )
        })}
      </ol>
    </nav>
  )
}
