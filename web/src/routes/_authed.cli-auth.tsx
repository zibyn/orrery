import { createFileRoute } from '@tanstack/react-router'
import { CircleCheck, Terminal } from 'lucide-react'
import { useState } from 'react'

import { api, type CreatedToken } from '@/api'
import { PageHeader } from '@/components/page-header'
import { TokenReveal } from '@/components/token-reveal'
import { Card, CardContent } from '@/components/ui/card'

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
  errorComponent: ({ error }) => (
    <div className="mx-auto flex max-w-xl flex-col gap-3 py-6">
      <PageHeader title="CLI authorization" />
      <p className="text-sm text-destructive" role="alert">
        Could not create a token: {error instanceof Error ? error.message : String(error)}
      </p>
    </div>
  ),
})

function CliAuthPage() {
  const token = Route.useLoaderData()
  const [done, setDone] = useState(false)

  if (done) {
    return (
      <div className="mx-auto flex max-w-xl flex-col gap-3 py-6">
        <PageHeader
          title="CLI authorization"
          description="Token created. You can close this tab and return to the terminal."
        />
        <Card>
          <CardContent className="flex items-center gap-3 py-6">
            <span className="flex size-9 items-center justify-center rounded-lg border border-success/30 bg-success/10 text-success">
              <CircleCheck className="size-4" />
            </span>
            <div>
              <p className="text-sm font-medium">Signed in to the CLI</p>
              <p className="text-xs text-muted-foreground">
                Paste the token into the <code className="font-mono">stars-sandbox login</code>{' '}
                prompt if you have not already.
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
            stars-sandbox login
          </span>
        }
        title="CLI authorization"
        description="This token appears once. Paste it into the stars-sandbox login prompt."
      />
      <Card>
        <CardContent className="py-5">
          <TokenReveal token={token.token} onDone={() => setDone(true)} />
        </CardContent>
      </Card>
    </div>
  )
}
