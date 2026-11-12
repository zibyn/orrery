import { useMutation } from '@tanstack/react-query'
import { createFileRoute, Link, Outlet, redirect, useRouterState } from '@tanstack/react-router'
import {
  Boxes,
  GitBranch,
  HardDrive,
  KeyRound,
  Layers,
  LogOut,
  Package,
  ScrollText,
  Server,
  Terminal,
  Users,
} from 'lucide-react'
import { useEffect } from 'react'
import type * as React from 'react'

import { api, ApiError, getMe, type User } from '@/api'
import { LocaleSwitch } from '@/components/locale-switch'
import { Logo } from '@/components/logo'
import {
  Sidebar,
  SidebarContent,
  SidebarFooter,
  SidebarGroup,
  SidebarGroupContent,
  SidebarGroupLabel,
  SidebarHeader,
  SidebarInset,
  SidebarMenu,
  SidebarMenuAction,
  SidebarMenuButton,
  SidebarMenuItem,
  SidebarProvider,
  SidebarTrigger,
  useSidebar,
} from '@/components/ui/sidebar'
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
        throw redirect({ to: '/login', search: { return: location.href } })
      }
      throw err
    }
  },
  component: AuthedShell,
})

type NavItem = {
  to: '/sandboxes' | '/snapshots' | '/workspaces' | '/resources' | '/secrets' | '/git' | '/cli-auth' | '/admin/users' | '/admin/nodes' | '/admin/audit'
  label: () => string
  icon: React.ComponentType
}

// The console pages of api.md §8. Admin pages are UI-gated on is_admin; the
// server still enforces scope.
const NAV: { label: () => string; items: readonly NavItem[] }[] = [
  {
    label: () => m.nav_group_workspace(),
    items: [
      { to: '/sandboxes', label: () => m.nav_sandboxes(), icon: Boxes },
      { to: '/snapshots', label: () => m.nav_snapshots(), icon: Layers },
      { to: '/workspaces', label: () => m.nav_workspaces(), icon: HardDrive },
      { to: '/resources', label: () => m.nav_resources(), icon: Package },
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
]

const ADMIN_NAV: readonly NavItem[] = [
  { to: '/admin/users', label: () => m.nav_users(), icon: Users },
  { to: '/admin/nodes', label: () => m.nav_nodes(), icon: Server },
  { to: '/admin/audit', label: () => m.nav_audit(), icon: ScrollText },
]

function AuthedShell() {
  useLocale() // re-render on a language switch (see lib/i18n.tsx)
  const { user } = Route.useRouteContext()
  const pathname = useRouterState({ select: (state) => state.location.pathname })
  const logout = useMutation({
    mutationFn: () => api('DELETE', '/sessions/current'),
    onSuccess: () => {
      window.location.assign('/login')
    },
  })

  return (
    <SidebarProvider>
      <a
        href="#main"
        className="sr-only focus:not-sr-only focus:absolute focus:top-4 focus:left-4 focus:z-50 focus:rounded-md focus:bg-primary focus:px-3 focus:py-1.5 focus:text-sm focus:text-primary-foreground"
      >
        {m.nav_skip()}
      </a>

      <AppSidebar
        user={user}
        pathname={pathname}
        onSignOut={() => logout.mutate()}
        signingOut={logout.isPending}
      />

      <SidebarInset>
        <header className="sticky top-0 z-20 flex h-14 shrink-0 items-center gap-2 border-b bg-background/85 px-4 backdrop-blur md:rounded-t-xl lg:px-6">
          <SidebarTrigger className="-ml-1" />
          <Link to="/sandboxes" className="flex items-center gap-2.5 md:hidden">
            <Logo />
            <span className="text-sm font-semibold tracking-tight">Orrery</span>
          </Link>
        </header>

        <main id="main" className="mx-auto w-full max-w-6xl px-4 py-6 sm:px-6 lg:px-8 lg:py-8">
          <Outlet />
        </main>
      </SidebarInset>
    </SidebarProvider>
  )
}

// The console sidebar. On a phone it is a sheet, closed again after each
// navigation so the page the user picked is visible.
function AppSidebar({
  user,
  pathname,
  onSignOut,
  signingOut,
}: {
  user: User
  pathname: string
  onSignOut: () => void
  signingOut: boolean
}) {
  const { setOpenMobile } = useSidebar()
  useEffect(() => setOpenMobile(false), [pathname, setOpenMobile])

  const groups = [
    ...NAV,
    ...(user.is_admin ? [{ label: () => m.nav_group_admin(), items: ADMIN_NAV }] : []),
  ]

  return (
    <Sidebar variant="inset">
      <SidebarHeader>
        <SidebarMenu>
          <SidebarMenuItem>
            <SidebarMenuButton size="lg" render={<Link to="/sandboxes" />}>
              <Logo />
              <span className="font-semibold tracking-tight">Orrery</span>
            </SidebarMenuButton>
          </SidebarMenuItem>
        </SidebarMenu>
      </SidebarHeader>

      <SidebarContent>
        <nav aria-label={m.nav_console()} className="contents">
          {groups.map((group) => (
            <SidebarGroup key={group.items[0].to}>
              <SidebarGroupLabel>{group.label()}</SidebarGroupLabel>
              <SidebarGroupContent>
                <SidebarMenu>
                  {group.items.map((item) => (
                    <SidebarMenuItem key={item.to}>
                      <SidebarMenuButton
                        isActive={pathname === item.to || pathname.startsWith(`${item.to}/`)}
                        render={<Link to={item.to} />}
                      >
                        <item.icon />
                        <span>{item.label()}</span>
                      </SidebarMenuButton>
                    </SidebarMenuItem>
                  ))}
                </SidebarMenu>
              </SidebarGroupContent>
            </SidebarGroup>
          ))}
        </nav>
      </SidebarContent>

      <SidebarFooter>
        <LocaleSwitch className="self-start" />
        <SidebarMenu>
          <SidebarMenuItem>
            <SidebarMenuButton
              size="lg"
              isActive={pathname === '/account'}
              render={<Link to="/account" />}
            >
              <span
                aria-hidden="true"
                className="flex size-8 shrink-0 items-center justify-center rounded-lg bg-sidebar-accent text-xs font-medium text-sidebar-accent-foreground"
              >
                {user.name.slice(0, 1).toUpperCase()}
              </span>
              <span className="grid flex-1 text-left leading-tight">
                <span className="truncate font-medium">{user.name}</span>
                <span className="truncate text-xs text-muted-foreground">
                  {user.is_admin ? m.user_administrator() : m.user_member()}
                </span>
              </span>
            </SidebarMenuButton>
            <SidebarMenuAction
              aria-label={m.user_sign_out()}
              title={m.user_sign_out()}
              disabled={signingOut}
              onClick={onSignOut}
              className="top-3.5"
            >
              <LogOut />
            </SidebarMenuAction>
          </SidebarMenuItem>
        </SidebarMenu>
      </SidebarFooter>
    </Sidebar>
  )
}
