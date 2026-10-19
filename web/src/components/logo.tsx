import { Star } from 'lucide-react'

export function Logo() {
  return (
    <span
      aria-hidden="true"
      className="flex size-7 shrink-0 items-center justify-center rounded-lg bg-brand text-primary-foreground shadow-sm"
    >
      <Star className="size-3.5" fill="currentColor" />
    </span>
  )
}
