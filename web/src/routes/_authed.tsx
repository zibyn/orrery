import { useMutation } from '@tanstack/react-query'
import { createFileRoute, Link, Outlet, redirect } from '@tanstack/react-router'

import { api, ApiError, getMe } from '@/api'
import { Button } from '@/components/ui/button'

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
  component: AuthedShell,
})

// Only pages that exist today; each later M6 ticket adds its own entry. Admin
// pages are gated on is_admin when they land (api.md §8).
const NAV = [
  { to: '/sandboxes', label: 'Sandboxes' },
  { to: '/git', label: 'Git accounts' },
  { to: '/cli-auth', label: 'CLI authorization' },
] as const

const navLinkClass =
  'rounded-md px-3 py-1.5 text-sm text-muted-foreground transition-colors hover:bg-muted hover:text-foreground'

function AuthedShell() {
  const { user } = Route.useRouteContext()
  const logout = useMutation({
    mutationFn: () => api('DELETE', '/sessions/current'),
    onSuccess: () => window.location.assign('/login'),
  })

  return (
    <div className="min-h-screen bg-background text-foreground">
      <header className="border-b border-border">
        <div className="mx-auto flex max-w-5xl flex-wrap items-center gap-4 px-4 py-3">
          <Link to="/sandboxes" className="font-semibold">
            stars-sandbox
          </Link>
          <nav className="flex flex-1 flex-wrap items-center gap-1">
            {NAV.map((item) => (
              <Link
                key={item.to}
                to={item.to}
                className={navLinkClass}
                activeProps={{ className: `${navLinkClass} bg-muted text-foreground` }}
              >
                {item.label}
              </Link>
            ))}
          </nav>
          <div className="flex items-center gap-3 text-sm">
            <span className="text-muted-foreground">{user.name}</span>
            <Button
              variant="outline"
              size="sm"
              disabled={logout.isPending}
              onClick={() => logout.mutate()}
            >
              Sign out
            </Button>
          </div>
        </div>
      </header>
      <main className="mx-auto max-w-5xl px-4 py-8">
        <Outlet />
      </main>
    </div>
  )
}
