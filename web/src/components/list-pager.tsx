import { Link, useSearch } from '@tanstack/react-router'

import {
  Pagination,
  PaginationContent,
  PaginationEllipsis,
  PaginationItem,
  PaginationLink,
  PaginationNext,
  PaginationPrevious,
} from '@/components/ui/pagination'
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

// Page numbers to show: the first, the last and the current one with its
// neighbours; null marks a gap drawn as an ellipsis.
export function pageNumbers(page: number, pageCount: number): (number | null)[] {
  const shown = [...new Set([1, page - 1, page, page + 1, pageCount])]
    .filter((n) => n >= 1 && n <= pageCount)
    .sort((a, b) => a - b)
  return shown.flatMap((n, i) => (i > 0 && n - shown[i - 1] > 1 ? [null, n] : [n]))
}

// The current page of a list whose route uses pageSearch. Its ListPager
// renders links, so Back and open-in-new-tab work.
export function usePagedList<T>(items: readonly T[] | undefined) {
  const { page } = useSearch({ strict: false })
  return paginate(items ?? [], page)
}

// Without onPageChange the page lives in `?page=` and every target is a Link;
// with it (a page kept in state, or the audit cursor stack) they are buttons.
// pageCount is unknown for a server-cursor list: hasNext says whether the
// server reported another page.
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
  onPageChange?: (page: number) => void
  className?: string
}) {
  const canNext = pageCount === undefined ? Boolean(hasNext) : page < pageCount
  if (page <= 1 && !canNext) return null

  const target = (to: number, enabled = true) =>
    !enabled ? (
      <button type="button" disabled />
    ) : onPageChange ? (
      <button type="button" onClick={() => onPageChange(to)} />
    ) : (
      <Link
        to="."
        // Page 1 has no ?page=; without this every page would count as page 1.
        activeOptions={{ explicitUndefined: true }}
        search={(prev: Record<string, unknown>) => ({ ...prev, page: to > 1 ? to : undefined })}
      />
    )

  return (
    <Pagination aria-label={m.pager_label()} className={cn('justify-end', className)}>
      <PaginationContent>
        <PaginationItem>
          <PaginationPrevious
            text={m.pager_previous()}
            aria-label={m.pager_previous()}
            render={target(page - 1, page > 1)}
          />
        </PaginationItem>
        {(pageCount === undefined ? [page] : pageNumbers(page, pageCount)).map((n, i) => (
          <PaginationItem key={n ?? `gap-${i}`}>
            {n === null ? (
              <PaginationEllipsis />
            ) : (
              <PaginationLink isActive={n === page} render={target(n)}>
                {n}
              </PaginationLink>
            )}
          </PaginationItem>
        ))}
        <PaginationItem>
          <PaginationNext
            text={m.pager_next()}
            aria-label={m.pager_next()}
            render={target(page + 1, canNext)}
          />
        </PaginationItem>
      </PaginationContent>
    </Pagination>
  )
}
