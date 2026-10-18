import { useForm } from '@tanstack/react-form'
import { createFileRoute } from '@tanstack/react-router'
import { useState } from 'react'
import { z } from 'zod'

import { api, ApiError, type Token } from '@/api'
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
    <div className="mx-auto flex min-h-screen max-w-sm flex-col justify-center gap-6 px-4">
      <div>
        <h1 className="text-2xl font-semibold">stars-sandbox</h1>
        <p className="text-sm text-muted-foreground">Sign in to the console.</p>
      </div>
      <form
        className="flex flex-col gap-4"
        onSubmit={(e) => {
          e.preventDefault()
          form.handleSubmit()
        }}
      >
        <form.Field name="username">
          {(field) => (
            <div className="flex flex-col gap-2">
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
            <div className="flex flex-col gap-2">
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
        {error && <p className="text-sm text-destructive">{error}</p>}
        <Button type="submit" disabled={form.state.isSubmitting}>
          Sign in
        </Button>
      </form>
      <div className="flex items-center gap-3 text-xs text-muted-foreground">
        <span className="h-px flex-1 bg-border" />
        or
        <span className="h-px flex-1 bg-border" />
      </div>
      <a
        href={`/api/v1/oidc/login?return=${encodeURIComponent(safeReturn(returnTo))}`}
        className={buttonVariants({ variant: 'outline' })}
      >
        Sign in with SSO
      </a>
    </div>
  )
}
