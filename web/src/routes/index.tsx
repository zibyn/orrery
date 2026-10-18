import { createFileRoute, redirect } from '@tanstack/react-router'

// The console lands on the Sandbox list (api.md §8).
export const Route = createFileRoute('/')({
  beforeLoad: () => {
    throw redirect({ to: '/sandboxes' })
  },
})
