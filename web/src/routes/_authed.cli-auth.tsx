import { createFileRoute } from '@tanstack/react-router'
import { CircleCheck, Terminal } from 'lucide-react'
import { useState } from 'react'

import { api, type CreatedToken } from '@/api'
import { PageHeader } from '@/components/page-header'
import { Problem } from '@/components/problem'
import { TokenReveal } from '@/components/token-reveal'
import { Card, CardContent } from '@/components/ui/card'
import { useLocale } from '@/lib/i18n'
import { m } from '@/paraglide/messages'

// The page itself creates the token with the Session (api.md §8). A loader,
// unlike an effect, runs once, so StrictMode cannot mint two tokens.
function createCliToken(): Promise<CreatedToken> {
  const name = `cli-${new Date().toISOString().replace(/[-:]/g, '').replace(/\..+/, '')}-${crypto.randomUUID().slice(0, 8)}`
  return api('POST', '/users/me/tokens', { name, scope: 'user' })
}

export const Route = createFileRoute('/_authed/cli-auth')({
  loader: ({ context }) =>
    context.queryClient.fetchQuery({
      queryKey: ['cli-token'],
      queryFn: createCliToken,
      staleTime: Infinity,
      gcTime: Infinity,
      retry: false,
    }),
  component: CliAuthPage,
  errorComponent: CliAuthError,
})

function CliAuthError({ error }: { error: unknown }) {
  useLocale() // re-render on a language switch (see lib/i18n.tsx)
  return (
    <div className="mx-auto flex max-w-xl flex-col gap-3 py-6">
      <PageHeader title={m.cli_title()} />
      <p className="text-sm text-destructive" role="alert">
        {m.cli_create_error()}: <Problem error={error} />
      </p>
    </div>
  )
}

function CliAuthPage() {
  useLocale() // re-render on a language switch (see lib/i18n.tsx)
  const token = Route.useLoaderData()
  const [done, setDone] = useState(false)

  if (done) {
    return (
      <div className="mx-auto flex max-w-xl flex-col gap-3 py-6">
        <PageHeader title={m.cli_title()} description={m.cli_token_created()} />
        <Card>
          <CardContent className="flex items-center gap-3 py-6">
            <span className="flex size-9 items-center justify-center rounded-lg border border-success/30 bg-success/10 text-success">
              <CircleCheck className="size-4" />
            </span>
            <div>
              <p className="text-sm font-medium">{m.cli_signed_in()}</p>
              <p className="text-xs text-muted-foreground">
                {m.cli_paste_hint()}
              </p>
            </div>
          </CardContent>
        </Card>
      </div>
    )
  }

  return (
    <div className="mx-auto flex max-w-xl flex-col gap-6 py-6">
      <PageHeader
        above={
          <span className="inline-flex items-center gap-1 text-xs text-muted-foreground">
            <Terminal className="size-3.5" />
            orrery login
          </span>
        }
        title={m.cli_title()}
        description={m.cli_description()}
      />
      <Card>
        <CardContent className="py-5">
          <TokenReveal token={token.token} onDone={() => setDone(true)} />
        </CardContent>
      </Card>
    </div>
  )
}
