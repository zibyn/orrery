import { useForm } from '@tanstack/react-form'
import { createFileRoute } from '@tanstack/react-router'
import { useState } from 'react'
import { z } from 'zod'

import { api, ApiError, type Token } from '@/api'
import { Logo } from '@/components/logo'
import { Button, buttonVariants } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { rememberCurrentSession } from '@/lib/current-session'
import { safeReturn } from '@/lib/return-path'

const loginSchema = z.object({
  username: z.string().trim().min(1, 'Enter your username'),
  password: z.string().min(1, 'Enter your password'),
})

export const Route = createFileRoute('/login')({
  validateSearch: (search: Record<string, unknown>) => ({
    return: typeof search.return === 'string' ? search.return : undefined,
  }),
  component: LoginPage,
})

function LoginPage() {
  const { return: returnTo } = Route.useSearch()
  const [error, setError] = useState('')
  const form = useForm({
    defaultValues: { username: '', password: '' },
    onSubmit: async ({ value }) => {
      setError('')
      const parsed = loginSchema.safeParse(value)
      if (!parsed.success) {
        setError(parsed.error.issues[0]?.message ?? 'Invalid input')
        return
      }
      try {
        // The response is the Session's Token DTO; its id lets the account
        // page mark this Session. OIDC logins never see this id.
        const session = await api<Token>('POST', '/sessions', parsed.data)
        rememberCurrentSession(session.id)
      } catch (err) {
        if (err instanceof ApiError && err.status === 401) {
          setError('Wrong username or password.')
        } else {
          setError(err instanceof Error ? err.message : 'Login failed')
        }
        return
      }
      // A full reload: the guard re-reads the Session for the target page.
      window.location.assign(safeReturn(returnTo))
    },
  })

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
      <div className="relative w-full max-w-sm">
        <div className="mb-6 flex flex-col items-center gap-3 text-center">
          <Logo />
          <div className="space-y-1">
            <h1 className="text-lg font-semibold tracking-tight">stars-sandbox</h1>
            <p className="text-sm text-muted-foreground">Sign in to the console.</p>
          </div>
        </div>

        <div className="rounded-xl border border-border bg-card p-5 shadow-sm">
          <form
            className="flex flex-col gap-4"
            onSubmit={(e) => {
              e.preventDefault()
              form.handleSubmit()
            }}
          >
            <form.Field name="username">
              {(field) => (
                <div className="flex flex-col gap-1.5">
                  <Label htmlFor={field.name}>Username</Label>
                  <Input
                    id={field.name}
                    value={field.state.value}
                    onChange={(e) => field.handleChange(e.target.value)}
                    autoComplete="username"
                    autoFocus
                  />
                </div>
              )}
            </form.Field>
            <form.Field name="password">
              {(field) => (
                <div className="flex flex-col gap-1.5">
                  <Label htmlFor={field.name}>Password</Label>
                  <Input
                    id={field.name}
                    type="password"
                    value={field.state.value}
                    onChange={(e) => field.handleChange(e.target.value)}
                    autoComplete="current-password"
                  />
                </div>
              )}
            </form.Field>
            {error && (
              <p className="text-sm text-destructive" role="alert">
                {error}
              </p>
            )}
            <Button type="submit" className="w-full" disabled={form.state.isSubmitting}>
              Sign in
            </Button>
          </form>

          <div className="my-4 flex items-center gap-3 text-xs text-muted-foreground">
            <span className="h-px flex-1 bg-border" />
            or
            <span className="h-px flex-1 bg-border" />
          </div>

          <a
            href={`/api/v1/oidc/login?return=${encodeURIComponent(safeReturn(returnTo))}`}
            className={buttonVariants({ variant: 'outline', className: 'w-full' })}
          >
            Sign in with SSO
          </a>
        </div>

        <p className="mt-6 text-center text-xs text-muted-foreground">
          Agent execution and remote development platform
        </p>
      </div>
    </div>
  )
}
