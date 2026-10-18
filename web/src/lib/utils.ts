import { clsx, type ClassValue } from 'clsx'
import { twMerge } from 'tailwind-merge'

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs))
}

// Problem details are shown to the user; a thrown non-Error still gets text.
export function messageOf(error: unknown): string {
  return error instanceof Error ? error.message : String(error)
}

// Native <select> styled like the shadcn input.
export const selectClass =
  'flex h-9 w-full rounded-md border border-input bg-transparent px-3 py-1 text-sm shadow-xs focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-50'
