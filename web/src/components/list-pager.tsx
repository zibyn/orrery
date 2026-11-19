import { useNavigate, useSearch } from '@tanstack/react-router'
import { ChevronLeft, ChevronRight } from 'lucide-react'

import { Button } from '@/components/ui/button'
import { cn } from '@/lib/utils'
import { m } from '@/paraglide/messages'

export const PAGE_SIZE = 20

// validateSearch for a paged list: `?page=` is 1-based and omitted on page 1.
export function pageSearch(search: Record<string, unknown>): { page?: number } {
  const page = Number(search.page)
  return { page: Number.isInteger(page) && page > 1 ? page : undefined }
}

// shortcut: pages a list already fetched whole by listAll, so the header keeps
// its total count; move to server cursors if a list reaches thousands of rows.
export function paginate<T>(items: readonly T[], page = 1) {
  const pageCount = Math.max(1, Math.ceil(items.length / PAGE_SIZE))
  const current = Math.min(page, pageCount)
  return {
    items: items.slice((current - 1) * PAGE_SIZE, current * PAGE_SIZE),
    page: current,
    pageCount,
  }
}

// The current page of a list whose route uses pageSearch; changing page pushes
// history, so Back returns to the previous page.
export function usePagedList<T>(items: readonly T[] | undefined) {
  const { page } = useSearch({ strict: false })
  const navigate = useNavigate()
  return {
    ...paginate(items ?? [], page),
    onPageChange: (next: number) =>
      void navigate({
        to: '.',
        search: (prev: Record<string, unknown>) => ({ ...prev, page: next > 1 ? next : undefined }),
      }),
  }
}

// pageCount is unknown for a server-cursor list (audit): hasNext says whether
// the server reported another page.
export function ListPager({
  page,
  pageCount,
  hasNext,
  onPageChange,
  className,
}: {
  page: number
  pageCount?: number
  hasNext?: boolean
  onPageChange: (page: number) => void
  className?: string
}) {
  const canNext = pageCount === undefined ? Boolean(hasNext) : page < pageCount
  if (page <= 1 && !canNext) return null
  return (
    <nav aria-label={m.pager_label()} className={cn('flex items-center justify-end gap-2', className)}>
      <span className="mr-2 text-sm text-muted-foreground tabular-nums">
        {pageCount === undefined ? m.pager_status_open({ page }) : m.pager_status({ page, pageCount })}
      </span>
      <Button variant="outline" size="sm" disabled={page <= 1} onClick={() => onPageChange(page - 1)}>
        <ChevronLeft data-icon="inline-start" />
        {m.pager_previous()}
      </Button>
      <Button variant="outline" size="sm" disabled={!canNext} onClick={() => onPageChange(page + 1)}>
        {m.pager_next()}
        <ChevronRight data-icon="inline-end" />
      </Button>
    </nav>
  )
}
