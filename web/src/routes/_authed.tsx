import { createFileRoute, Outlet, redirect } from '@tanstack/react-router'

import { ApiError, getMe } from '@/api'

// Every page under _authed needs a Session. A 401 sends the browser to the
// login page with the current location as ?return= (ADR-0020).
export const Route = createFileRoute('/_authed')({
  beforeLoad: async ({ context, location }) => {
    try {
      const user = await getMe()
      context.queryClient.setQueryData(['me'], user)
      return { user }
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) {
        throw redirect({ to: '/login', search: { return: location.href } })
      }
      throw err
    }
  },
  component: Outlet,
})
