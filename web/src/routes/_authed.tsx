import { useMutation } from '@tanstack/react-query'
import { createFileRoute, Link, Outlet, redirect } from '@tanstack/react-router'
import {
  Boxes,
  GitBranch,
  KeyRound,
  Layers,
  LogOut,
  ScrollText,
  Server,
  Terminal,
  UserRound,
  Users,
} from 'lucide-react'

import { api, ApiError, getMe } from '@/api'
import { LocaleSwitch } from '@/components/locale-switch'
import { Logo } from '@/components/logo'
import { Button } from '@/components/ui/button'
import { forgetCurrentSession } from '@/lib/current-session'
import { useLocale } from '@/lib/i18n'
import { m } from '@/paraglide/messages'

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
        forgetCurrentSession()
        throw redirect({ to: '/login', search: { return: location.href } })
      }
      throw err
    }
  },
  component: AuthedShell,
})

// The console pages of api.md §8. Admin pages are UI-gated on is_admin; the
// server still enforces scope.
const NAV = [
  {
    label: () => m.nav_group_workspace(),
    items: [
      { to: '/sandboxes', label: () => m.nav_sandboxes(), icon: Boxes },
      { to: '/snapshots', label: () => m.nav_snapshots(), icon: Layers },
    ],
  },
  {
    label: () => m.nav_group_developer(),
    items: [
      { to: '/secrets', label: () => m.nav_secrets(), icon: KeyRound },
      { to: '/git', label: () => m.nav_git(), icon: GitBranch },
      { to: '/cli-auth', label: () => m.nav_cli(), icon: Terminal },
    ],
  },
] as const

const ADMIN_NAV = [
  { to: '/admin/users', label: () => m.nav_users(), icon: Users },
  { to: '/admin/nodes', label: () => m.nav_nodes(), icon: Server },
  { to: '/admin/audit', label: () => m.nav_audit(), icon: ScrollText },
] as const

const navLinkClass =
  'relative flex items-center gap-2.5 rounded-md px-2.5 py-1.5 text-sm text-sidebar-foreground transition-colors hover:bg-sidebar-accent hover:text-sidebar-accent-foreground [&_svg]:size-4 [&_svg]:shrink-0 [&_svg]:opacity-70'
const navActiveClass =
  navLinkClass +
  ' bg-sidebar-accent font-medium text-sidebar-accent-foreground before:absolute before:inset-y-1.5 before:left-0 before:w-0.5 before:rounded-full before:bg-brand [&_svg]:opacity-100'

const mobileLinkClass =
  'shrink-0 rounded-md px-2.5 py-1 text-xs font-medium text-muted-foreground transition-colors hover:bg-muted hover:text-foreground'
const mobileActiveClass = mobileLinkClass + ' bg-accent text-accent-foreground'

function AuthedShell() {
  useLocale() // re-render on a language switch (see lib/i18n.tsx)
  const { user } = Route.useRouteContext()
  const logout = useMutation({
    mutationFn: () => api('DELETE', '/sessions/current'),
    onSuccess: () => {
      forgetCurrentSession()
      window.location.assign('/login')
    },
  })

  const adminItems = user.is_admin ? ADMIN_NAV : []
  const allItems = [
    ...NAV[0].items,
    ...NAV[1].items,
    ...(user.is_admin ? ADMIN_NAV : []),
  ]

  return (
    <div className="min-h-svh bg-background text-foreground">
      <a
        href="#main"
        className="sr-only focus:not-sr-only focus:absolute focus:top-4 focus:left-4 focus:z-50 focus:rounded-md focus:bg-primary focus:px-3 focus:py-1.5 focus:text-sm focus:text-primary-foreground"
      >
        {m.nav_skip()}
      </a>

      <aside className="fixed inset-y-0 left-0 z-30 hidden w-60 flex-col border-r border-sidebar-border bg-sidebar lg:flex">
        <div className="flex h-14 items-center px-4">
          <Link to="/sandboxes" className="flex items-center gap-2.5">
            <Logo />
            <span className="text-sm font-semibold tracking-tight">stars-sandbox</span>
          </Link>
        </div>

        <nav aria-label={m.nav_console()} className="flex flex-1 flex-col gap-5 overflow-y-auto px-2.5 py-3">
          {NAV.map((group) => (
            <div key={group.items[0].to} className="flex flex-col gap-0.5">
              <p className="px-2.5 pb-1 text-[11px] font-medium tracking-wider text-muted-foreground/80 uppercase">
                {group.label()}
              </p>
              {group.items.map((item) => (
                <Link
                  key={item.to}
                  to={item.to}
                  className={navLinkClass}
                  activeProps={{ className: navActiveClass }}
                >
                  <item.icon />
                  {item.label()}
                </Link>
              ))}
            </div>
          ))}

          {adminItems.length > 0 && (
            <div className="flex flex-col gap-0.5">
              <p className="px-2.5 pb-1 text-[11px] font-medium tracking-wider text-muted-foreground/80 uppercase">
                {m.nav_group_admin()}
              </p>
              {adminItems.map((item) => (
                <Link
                  key={item.to}
                  to={item.to}
                  className={navLinkClass}
                  activeProps={{ className: navActiveClass }}
                >
                  <item.icon />
                  {item.label()}
                </Link>
              ))}
            </div>
          )}
        </nav>

        <div className="flex flex-col gap-2 border-t border-sidebar-border p-2.5">
          <LocaleSwitch className="self-start" />
          <div className="flex items-center gap-2.5">
            <Link
              to="/account"
              className="flex min-w-0 flex-1 items-center gap-2.5 rounded-md p-1 transition-colors hover:bg-sidebar-accent"
            >
              <span
                aria-hidden="true"
                className="flex size-7 shrink-0 items-center justify-center rounded-full bg-sidebar-accent text-xs font-medium text-sidebar-accent-foreground"
              >
                {user.name.slice(0, 1).toUpperCase()}
              </span>
              <div className="min-w-0 flex-1">
                <p className="truncate text-sm font-medium">{user.name}</p>
                <p className="truncate text-[11px] text-muted-foreground">
                  {user.is_admin ? m.user_administrator() : m.user_member()}
                </p>
              </div>
            </Link>
            <Button
              variant="ghost"
              size="icon-sm"
              aria-label={m.user_sign_out()}
              title={m.user_sign_out()}
              disabled={logout.isPending}
              onClick={() => logout.mutate()}
            >
              <LogOut />
            </Button>
          </div>
        </div>
      </aside>

      <div className="lg:pl-60">
        <header className="sticky top-0 z-20 border-b border-border bg-background/85 backdrop-blur lg:hidden">
          <div className="flex h-14 items-center gap-2.5 px-4">
            <Link to="/sandboxes" className="flex items-center gap-2.5">
              <Logo />
              <span className="text-sm font-semibold tracking-tight">stars-sandbox</span>
            </Link>
            <div className="ml-auto flex items-center gap-1.5">
              <Link
                to="/account"
                aria-label={m.account_title()}
                className="rounded-md p-1.5 text-muted-foreground transition-colors hover:bg-muted hover:text-foreground"
              >
                <UserRound className="size-4" />
              </Link>
              <span className="hidden max-w-32 truncate text-xs text-muted-foreground sm:inline">
                {user.name}
              </span>
              <LocaleSwitch />
              <Button
                variant="ghost"
                size="icon-sm"
                aria-label={m.user_sign_out()}
                disabled={logout.isPending}
                onClick={() => logout.mutate()}
              >
                <LogOut />
              </Button>
            </div>
          </div>
          <nav
            aria-label={m.nav_console()}
            className="flex gap-1 overflow-x-auto px-3 pb-2 [scrollbar-width:none] [&::-webkit-scrollbar]:hidden"
          >
            {allItems.map((item) => (
              <Link
                key={item.to}
                to={item.to}
                className={mobileLinkClass}
                activeProps={{ className: mobileActiveClass }}
              >
                {item.label()}
              </Link>
            ))}
          </nav>
        </header>

        <main id="main" className="mx-auto w-full max-w-6xl px-4 py-6 sm:px-6 lg:px-8 lg:py-8">
          <Outlet />
        </main>
      </div>
    </div>
  )
}
