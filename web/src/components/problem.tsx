import { problemOf } from '@/lib/labels'

// A problem's localized title with the server's own detail underneath
// (api.md §8). A client-side string has no detail.
export function Problem({ error, className }: { error: unknown; className?: string }) {
  const { title, detail } = problemOf(error)
  return (
    <span className={className}>
      {title}
      {detail && <span className="mt-0.5 block font-mono text-xs opacity-80">{detail}</span>}
    </span>
  )
}
