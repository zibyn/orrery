import { createFileRoute } from '@tanstack/react-router'
import { useState } from 'react'

import { api, type CreatedToken } from '@/api'
import { TokenReveal } from '@/components/token-reveal'

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
    <div className="mx-auto flex min-h-screen max-w-xl flex-col justify-center gap-2 px-4">
      <h1 className="text-2xl font-semibold">CLI authorization</h1>
      <p className="text-sm text-destructive">
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
      <div className="mx-auto flex min-h-screen max-w-xl flex-col justify-center gap-2 px-4">
        <h1 className="text-2xl font-semibold">CLI authorization</h1>
        <p className="text-sm text-muted-foreground">
          Token created. You can close this tab and return to the terminal.
        </p>
      </div>
    )
  }

  return (
    <div className="mx-auto flex min-h-screen max-w-xl flex-col justify-center gap-6 px-4">
      <div>
        <h1 className="text-2xl font-semibold">CLI authorization</h1>
        <p className="text-sm text-muted-foreground">
          This token appears once. Paste it into the <code className="font-mono">stars-sandbox login</code> prompt.
        </p>
      </div>
      <TokenReveal token={token.token} onDone={() => setDone(true)} />
    </div>
  )
}
