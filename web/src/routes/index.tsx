import { createFileRoute, redirect } from '@tanstack/react-router'

// The console's first page is the CLI authorization flow; the rest of the
// console lands in later tickets (api.md §8).
export const Route = createFileRoute('/')({
  beforeLoad: () => {
    throw redirect({ to: '/cli-auth' })
  },
})
