import { createFileRoute } from '@tanstack/react-router'

import { LocaleSwitch } from '@/components/locale-switch'
import { Logo } from '@/components/logo'
import { buttonVariants } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { useLocale } from '@/lib/i18n'
import { safeReturn } from '@/lib/return-path'
import { cn } from '@/lib/utils'
import { m } from '@/paraglide/messages'

export const Route = createFileRoute('/login')({
  validateSearch: (search: Record<string, unknown>) => ({
    return: typeof search.return === 'string' ? search.return : undefined,
  }),
  component: LoginPage,
})

// Polaris is the only way in (ADR-0029).
function LoginPage() {
  useLocale() // re-render on a language switch (see lib/i18n.tsx)
  const { return: returnTo } = Route.useSearch()

  return (
    <div className="relative flex min-h-svh flex-col items-center justify-center overflow-hidden px-4 py-10">
      <div
        aria-hidden="true"
        className="pointer-events-none absolute inset-0"
        style={{
          background:
            'radial-gradient(ellipse 70% 45% at 50% -10%, color-mix(in oklch, var(--brand) 16%, transparent), transparent)',
        }}
      />
      <div className="absolute top-4 right-4 z-10">
        <LocaleSwitch />
      </div>
      <div className="relative w-full max-w-sm">
        <div className="mb-6 flex flex-col items-center gap-3 text-center">
          <Logo />
          <div className="flex flex-col gap-1">
            <h1 className="text-lg font-semibold tracking-tight">Orrery</h1>
            <p className="text-sm text-muted-foreground">{m.login_subtitle()}</p>
          </div>
        </div>

        <Card>
          <CardContent>
            <a
              href={`/api/v1/oidc/login?return=${encodeURIComponent(safeReturn(returnTo))}`}
              className={cn(buttonVariants(), 'w-full')}
            >
              {m.login_polaris()}
            </a>
          </CardContent>
        </Card>

        <p className="mt-6 text-center text-xs text-muted-foreground">{m.login_tagline()}</p>
      </div>
    </div>
  )
}
